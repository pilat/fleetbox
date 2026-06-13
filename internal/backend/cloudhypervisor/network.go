//go:build linux

package cloudhypervisor

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"github.com/pilat/fleetbox/internal/backend"
)

// reservedSubnets records every /24 handed out by detectFreeIPv4Subnet in this
// process, so two networks created back-to-back never pick the same range even
// before the first bridge's address is visible to net.Interfaces. It mirrors the
// vz backend's detector (the two cannot share code across the build-tag split).
var (
	reservedSubnetsMu sync.Mutex
	reservedSubnets   = map[netip.Prefix]struct{}{}
)

// chNetwork is a Linux bridge with a private /24 — the shared network a
// cluster's VMs attach to via taps. The gateway (.1) lives on the bridge; every
// member reaches the host, the others, and (with masquerade) the internet on one
// NIC (ADR-0011). Its host resources are mirrored to a write-ahead record under
// store so a crash before Close leaves nothing unrecoverable (ADR-0013).
type chNetwork struct {
	bridge  string
	subnet  netip.Prefix
	gateway netip.Addr
	// uplink is the egress interface whose forwarding flag this network may have
	// flipped ("" when the host is offline or already forwarding globally).
	uplink string
	// uplinkFwdOrig is the uplink's forwarding value as this holder observed it
	// before flipping (write-ahead audit; the restore value comes from the
	// first-writer-wins marker, not this field — ADR-0025).
	uplinkFwdOrig string
	store         *netStore

	mu       sync.Mutex
	taps     []string
	reserved map[netip.Addr]bool // host IPs handed out by Reserve (gateway pre-marked)
}

// CreateNetwork creates a bridge on a free /24, assigns it the gateway address,
// and brings it up via netlink, then enables per-interface forwarding and installs
// the nft egress firewall. It first reconciles any network whose holder crashed
// (so orphans self-heal on every up), then write-ahead records the new bridge
// before creating it. The first netlink write (the bridge LinkAdd) doubles as the
// CAP_NET_ADMIN probe: without the capability it fails with EPERM and a clear
// error rather than a cryptic one deep in a later step (ADR-0025).
func (b *Backend) CreateNetwork() (backend.Network, error) {
	// Self-heal first: clean up any network whose owning holder is gone. Leave
	// forwarding on — we are about to need it.
	_ = b.reconcile(false)

	subnet, err := detectFreeIPv4Subnet()
	if err != nil {
		return nil, fmt.Errorf("detect free subnet: %w", err)
	}

	id, err := shortID()
	if err != nil {
		return nil, err
	}
	bridge := "fbx-" + id
	gateway := subnet.Addr().Next()

	n := &chNetwork{bridge: bridge, subnet: subnet, gateway: gateway, store: newNetStore(b.netDir)}

	// Write-ahead: persist the record naming the bridge BEFORE creating it, so a
	// crash anywhere below still leaves a record a later reconcile can clean
	// (ADR-0013).
	if err := n.persist(); err != nil {
		return nil, fmt.Errorf("persist network record: %w", err)
	}

	// First netlink write — also the CAP_NET_ADMIN probe (Decision 7).
	br := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: bridge}}
	if err := netlink.LinkAdd(br); err != nil {
		_ = n.store.delete(bridge)
		if errors.Is(err, unix.EPERM) {
			return nil, fmt.Errorf("create bridge (needs root): %w", err)
		}
		return nil, fmt.Errorf("create bridge %s: %w", bridge, err)
	}

	// nf_tables preflight AFTER the netlink root probe (Decision 8): a non-root host
	// already failed the bridge create above, so this surfaces "kernel lacks
	// nf_tables" coherently rather than a confusing error mid-firewall-install.
	if err := nftProbe(); err != nil {
		// Only the bridge exists yet (no firewall, no taps). Tear it down directly:
		// routing through Close would call removeFirewall, which on this same
		// nf_tables-less kernel also fails to list tables and would then keep the
		// write-ahead record forever. Delete the record only once the bridge is
		// confirmed gone, so a failed delete still leaves an index for reconcile.
		if delErr := delLink(n.bridge); delErr == nil || !linkExists(n.bridge) {
			_ = n.store.delete(bridge)
		}
		return nil, err
	}

	brLink, err := netlink.LinkByName(bridge)
	if err != nil {
		_ = n.Close()
		return nil, fmt.Errorf("look up bridge %s: %w", bridge, err)
	}
	addr, err := netlink.ParseAddr(gateway.String() + "/" + strconv.Itoa(subnet.Bits()))
	if err != nil {
		_ = n.Close()
		return nil, fmt.Errorf("parse gateway address: %w", err)
	}
	if err := netlink.AddrAdd(brLink, addr); err != nil {
		_ = n.Close()
		return nil, fmt.Errorf("assign gateway %s: %w", gateway, err)
	}
	if err := netlink.LinkSetUp(brLink); err != nil {
		_ = n.Close()
		return nil, fmt.Errorf("bring bridge %s up: %w", bridge, err)
	}

	// Egress: per-interface forwarding (never the global switch — Decision 4) plus
	// the nft masquerade and self-protecting forward filter (Decisions 5, 6). VM↔VM
	// and VM↔host work without either (L2 on the bridge + the gateway address);
	// these are only for outbound traffic and inbound protection (ADR-0011/0025).
	if err := n.enableForwarding(); err != nil {
		_ = n.Close()
		return nil, fmt.Errorf("enable forwarding: %w", err)
	}
	if err := installFirewall(bridge, subnet); err != nil {
		_ = n.Close()
		return nil, fmt.Errorf("install firewall: %w", err)
	}

	return n, nil
}

// Subnet returns the network's IPv4 CIDR; the client derives each member's
// gateway and netmask from it to build the seed's static network-config.
func (n *chNetwork) Subnet() string {
	return n.subnet.String()
}

// Reserve allocates a static host address on this live network. It honors ipHint
// (a member's previously-stored IP) when that address is in-subnet and still free
// — preserving a stopped member's address across reboots — otherwise it picks the
// lowest free host address. The gateway, network, and broadcast addresses are
// reserved, as is every address already handed out in this process. The MAC is
// deterministic in the name (Decisions 5, 6, 8). It is the helper-side successor
// to the orchestrator's old allocateIP; allocation lives with the network's owner.
func (n *chNetwork) Reserve(name, ipHint string) (ip, mac string, err error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.reserved == nil {
		n.reserved = map[netip.Addr]bool{n.gateway: true}
	}
	mac = backend.GenerateMAC(name)

	if ipHint != "" {
		if a, err := netip.ParseAddr(ipHint); err == nil && n.subnet.Contains(a) && a != n.gateway && !n.reserved[a] {
			n.reserved[a] = true
			return a.String(), mac, nil
		}
	}

	broadcast := lastAddr(n.subnet)
	for cand := n.gateway.Next(); n.subnet.Contains(cand) && cand != broadcast; cand = cand.Next() {
		if !n.reserved[cand] {
			n.reserved[cand] = true
			return cand.String(), mac, nil
		}
	}
	return "", "", fmt.Errorf("no free IP in subnet %s", n.subnet)
}

// lastAddr returns the broadcast (all host bits set) address of an IPv4 prefix.
func lastAddr(p netip.Prefix) netip.Addr {
	bytes := p.Addr().As4()
	for i := p.Bits(); i < 32; i++ {
		bytes[i/8] |= 1 << (7 - uint(i%8))
	}
	return netip.AddrFrom4(bytes)
}

// Close tears the network down: every remaining tap, then the nft firewall table,
// then the bridge — all via netlink/nftables (ADR-0025). The write-ahead record is
// removed last and only once the bridge is verified gone, so a failed delete keeps
// the record for a later reconcile rather than orphaning the bridge. It then
// restores the uplink's forwarding flag if nothing of ours remains (ADR-0013). It
// is the whole-cluster teardown hook; per-VM tap removal happens in VM.Stop.
func (n *chNetwork) Close() error {
	n.mu.Lock()
	taps := slices.Clone(n.taps)
	n.taps = nil
	n.mu.Unlock()

	var errs []error
	for _, t := range taps {
		if err := delLink(t); err != nil && linkExists(t) {
			errs = append(errs, fmt.Errorf("delete tap %s: %w", t, err))
		}
	}
	fwRemoved := true
	if err := removeFirewall(n.bridge); err != nil {
		errs = append(errs, fmt.Errorf("remove firewall: %w", err))
		fwRemoved = false
	}
	if err := delLink(n.bridge); err != nil && linkExists(n.bridge) {
		errs = append(errs, fmt.Errorf("delete bridge %s: %w", n.bridge, err))
	}

	// Record last, and only once the host is verified clean — the bridge gone AND
	// the firewall table removed. The record is the only index of that table's name
	// (nftTableName(bridge)), so dropping it while the table lingers would orphan
	// the table; a bridge still present means a delete genuinely failed. Either way,
	// keep the record for a retry instead of orphaning a resource (ADR-0013).
	if fwRemoved && !linkExists(n.bridge) {
		if err := n.store.delete(n.bridge); err != nil {
			errs = append(errs, err)
		}
	}
	maybeRestoreForwarding(n.store)

	return errors.Join(errs...)
}

// createTap makes a persistent tap, enslaves it to the bridge, brings it up, and
// records it for teardown — all via netlink. cloud-hypervisor opens the existing
// tap by name, so the tap must be persistent (the netlink default; NonPersist is
// deliberately not set). The tap is write-ahead recorded before creation and
// rolled back if any step fails.
func (n *chNetwork) createTap() (string, error) {
	id, err := shortID()
	if err != nil {
		return "", err
	}
	tap := "fbt-" + id

	// Write-ahead: record the tap before creating it.
	n.mu.Lock()
	n.taps = append(n.taps, tap)
	n.mu.Unlock()
	if err := n.persist(); err != nil {
		n.dropTap(tap)
		return "", fmt.Errorf("persist tap %s: %w", tap, err)
	}

	rollback := func() {
		_ = delLink(tap)
		n.dropTap(tap)
	}

	link := &netlink.Tuntap{LinkAttrs: netlink.LinkAttrs{Name: tap}, Mode: netlink.TUNTAP_MODE_TAP}
	if err := netlink.LinkAdd(link); err != nil {
		rollback()
		return "", fmt.Errorf("add tap %s: %w", tap, err)
	}

	tapLink, err := netlink.LinkByName(tap)
	if err != nil {
		rollback()
		return "", fmt.Errorf("look up tap %s: %w", tap, err)
	}
	brLink, err := netlink.LinkByName(n.bridge)
	if err != nil {
		rollback()
		return "", fmt.Errorf("look up bridge %s: %w", n.bridge, err)
	}
	if err := netlink.LinkSetMaster(tapLink, brLink); err != nil {
		rollback()
		return "", fmt.Errorf("enslave tap %s to %s: %w", tap, n.bridge, err)
	}
	if err := netlink.LinkSetUp(tapLink); err != nil {
		rollback()
		return "", fmt.Errorf("bring tap %s up: %w", tap, err)
	}

	return tap, nil
}

// deleteTap removes one tap (a single VM leaving), leaving the bridge for its
// siblings. The host link is removed first; the record is updated only once the
// tap is verified gone (ADR-0013).
func (n *chNetwork) deleteTap(tap string) error {
	_ = delLink(tap)
	if linkExists(tap) {
		return fmt.Errorf("delete tap %s: still present after delete", tap)
	}
	n.dropTap(tap)
	return nil
}

// dropTap removes tap from the in-memory list and rewrites the record.
func (n *chNetwork) dropTap(tap string) {
	n.mu.Lock()
	n.taps = slices.DeleteFunc(n.taps, func(t string) bool { return t == tap })
	n.mu.Unlock()
	_ = n.persist()
}

// persist writes the network's current state to its write-ahead record. The lock
// is held across the snapshot and the file write so concurrent tap creations
// cannot race a stale snapshot over a fresher one.
func (n *chNetwork) persist() error {
	if n.store == nil {
		return nil
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	rec := &netRecord{
		Bridge:        n.bridge,
		Subnet:        n.subnet.String(),
		OwnerPID:      os.Getpid(),
		Uplink:        n.uplink,
		UplinkFwdOrig: n.uplinkFwdOrig,
		Taps:          cloneTaps(n.taps),
	}
	return n.store.save(rec)
}

// reconcile removes the host resources of every recorded network whose owning
// holder is no longer alive (a crash before Close), so orphaned bridges, taps, and
// nft tables self-heal on the next up and via prune. Every teardown step is
// idempotent. When restoreForwarding is set (the prune path) it also returns the
// uplink's forwarding flag to its recorded original once nothing of ours remains;
// CreateNetwork passes false because it is about to need forwarding (ADR-0013).
func (b *Backend) reconcile(restoreForwarding bool) error {
	store := newNetStore(b.netDir)
	recs, err := store.list()
	if err != nil {
		return err
	}

	var errs []error
	for _, rec := range recs {
		if pidAlive(rec.OwnerPID) {
			continue // a live holder still owns this network
		}
		for _, tap := range rec.Taps {
			killProcsUsingTap(tap) // stop the orphaned VM before pulling its NIC
			_ = delLink(tap)
		}
		fwErr := removeFirewall(rec.Bridge)
		_ = delLink(rec.Bridge)

		if linkExists(rec.Bridge) {
			errs = append(errs, fmt.Errorf("reconcile: bridge %s still present", rec.Bridge))
			continue // keep the record so a later sweep retries
		}
		if fwErr != nil {
			// Keep the record so a later sweep retries the table delete — it is the
			// only index of the table's name, so deleting it now would orphan the
			// nft table (ADR-0013).
			errs = append(errs, fmt.Errorf("reconcile: remove firewall for %s: %w", rec.Bridge, fwErr))
			continue
		}
		if err := store.delete(rec.Bridge); err != nil {
			errs = append(errs, err)
		}
	}

	if restoreForwarding {
		maybeRestoreForwarding(store)
	}
	return errors.Join(errs...)
}

// delLink deletes a network interface by name, idempotently: a link that is
// already gone is not an error, so teardown and reconcile can run repeatedly.
// Callers that need to confirm the delete gate on linkExists.
func delLink(name string) error {
	link, err := netlink.LinkByName(name)
	if errors.As(err, &netlink.LinkNotFoundError{}) {
		return nil // already gone — nothing to delete
	}
	if err != nil {
		return fmt.Errorf("look up link %s: %w", name, err)
	}
	if err := netlink.LinkDel(link); err != nil {
		return fmt.Errorf("delete link %s: %w", name, err)
	}
	return nil
}

// detectFreeIPv4Subnet returns a /24 in 192.168.0.0/16 that overlaps no host
// interface and has not been handed out in this process.
func detectFreeIPv4Subnet() (netip.Prefix, error) {
	occupied, err := occupiedPrivatePrefixes()
	if err != nil {
		return netip.Prefix{}, err
	}

	reservedSubnetsMu.Lock()
	defer reservedSubnetsMu.Unlock()

	for octet := range 256 {
		candidate := netip.PrefixFrom(netip.AddrFrom4([4]byte{192, 168, byte(octet), 0}), 24)
		if _, taken := reservedSubnets[candidate]; taken {
			continue
		}
		if slices.ContainsFunc(occupied, candidate.Overlaps) {
			continue
		}
		reservedSubnets[candidate] = struct{}{}
		return candidate, nil
	}

	return netip.Prefix{}, errors.New("no free /24 subnet available in 192.168.0.0/16")
}

func occupiedPrivatePrefixes() ([]netip.Prefix, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("list interfaces: %w", err)
	}

	target := netip.MustParsePrefix("192.168.0.0/16")
	var occupied []netip.Prefix
	for i := range ifaces {
		addrs, err := ifaces[i].Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			prefix, err := netip.ParsePrefix(ipNet.String())
			if err != nil {
				continue
			}
			if target.Overlaps(prefix) {
				occupied = append(occupied, prefix)
			}
		}
	}

	return occupied, nil
}

// hostHasFbxBridges reports whether any fbx-* interface still exists, the
// cross-process backstop that stops maybeRestoreForwarding from disabling
// forwarding under another holder's live cluster.
func hostHasFbxBridges() bool {
	ifaces, err := net.Interfaces()
	if err != nil {
		return false
	}
	for i := range ifaces {
		if strings.HasPrefix(ifaces[i].Name, "fbx-") {
			return true
		}
	}
	return false
}

// linkExists reports whether a network interface with the given name is present.
func linkExists(name string) bool {
	_, err := net.InterfaceByName(name)
	return err == nil
}

// shortID returns 8 hex chars of randomness for an interface name.
func shortID() (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("read random: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}
