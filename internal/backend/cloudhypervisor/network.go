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
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"

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
	bridge     string
	subnet     netip.Prefix
	gateway    netip.Addr
	masquerade bool // whether the egress iptables rules were installed
	store      *netStore

	mu       sync.Mutex
	taps     []string
	reserved map[netip.Addr]bool // host IPs handed out by Reserve (gateway pre-marked)
}

// CreateNetwork creates a bridge on a free /24, assigns it the gateway address,
// and brings it up. It first reconciles any network whose holder crashed (so
// orphans self-heal on every up), then write-ahead records the new bridge before
// creating it. The first ip command doubles as the CAP_NET_ADMIN probe: without
// the capability it fails with a clear error rather than a cryptic one deep in a
// later step.
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

	if out, err := runIP("link", "add", bridge, "type", "bridge"); err != nil {
		_ = n.store.delete(bridge)
		if isPermissionDenied(out) {
			return nil, fmt.Errorf("create bridge (need CAP_NET_ADMIN; run as root or grant the capability): %w", err)
		}
		return nil, fmt.Errorf("create bridge %s: %w", bridge, err)
	}

	if _, err := runIP("addr", "add", gateway.String()+"/"+strconv.Itoa(subnet.Bits()), "dev", bridge); err != nil {
		_ = n.Close()
		return nil, fmt.Errorf("assign gateway %s: %w", gateway, err)
	}
	if _, err := runIP("link", "set", bridge, "up"); err != nil {
		_ = n.Close()
		return nil, fmt.Errorf("bring bridge %s up: %w", bridge, err)
	}

	// Egress: enable forwarding and masquerade the subnet so members reach the
	// internet. VM↔VM and VM↔host work without this (L2 on the bridge + the
	// gateway address); this is only for outbound traffic (ADR-0011).
	if err := n.enableMasquerade(); err != nil {
		_ = n.Close()
		return nil, fmt.Errorf("enable egress: %w", err)
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

// Close tears the network down: every remaining tap, then the egress rules, then
// the bridge. The write-ahead record is removed last and only once the bridge is
// verified gone, so a failed delete keeps the record for a later reconcile rather
// than orphaning the bridge. It then restores ip_forward if nothing of ours
// remains (ADR-0013). It is the whole-cluster teardown hook; per-VM tap removal
// happens in VM.Stop.
func (n *chNetwork) Close() error {
	n.mu.Lock()
	taps := slices.Clone(n.taps)
	n.taps = nil
	n.mu.Unlock()

	var errs []error
	for _, t := range taps {
		if _, err := runIP("link", "del", t); err != nil && linkExists(t) {
			errs = append(errs, fmt.Errorf("delete tap %s: %w", t, err))
		}
	}
	if err := n.disableMasquerade(); err != nil {
		errs = append(errs, fmt.Errorf("remove egress rules: %w", err))
	}
	if _, err := runIP("link", "del", n.bridge); err != nil && linkExists(n.bridge) {
		errs = append(errs, fmt.Errorf("delete bridge %s: %w", n.bridge, err))
	}

	// Record last, and only once the host is verified clean: a bridge still
	// present means a delete genuinely failed, so keep the record for a retry
	// instead of orphaning it (ADR-0013).
	if !linkExists(n.bridge) {
		if err := n.store.delete(n.bridge); err != nil {
			errs = append(errs, err)
		}
	}
	maybeRestoreIPForward(n.store)

	return errors.Join(errs...)
}

// createTap makes a tap, enslaves it to the bridge, brings it up, and records it
// for teardown. cloud-hypervisor opens the existing tap by name. The tap is
// write-ahead recorded before creation and rolled back if any step fails.
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

	if _, err := runIP("tuntap", "add", "dev", tap, "mode", "tap"); err != nil {
		n.dropTap(tap)
		return "", fmt.Errorf("add tap %s: %w", tap, err)
	}
	if _, err := runIP("link", "set", tap, "master", n.bridge); err != nil {
		_, _ = runIP("link", "del", tap)
		n.dropTap(tap)
		return "", fmt.Errorf("enslave tap %s to %s: %w", tap, n.bridge, err)
	}
	if _, err := runIP("link", "set", tap, "up"); err != nil {
		_, _ = runIP("link", "del", tap)
		n.dropTap(tap)
		return "", fmt.Errorf("bring tap %s up: %w", tap, err)
	}

	return tap, nil
}

// deleteTap removes one tap (a single VM leaving), leaving the bridge for its
// siblings. The host link is removed first; the record is updated only once the
// tap is verified gone (ADR-0013).
func (n *chNetwork) deleteTap(tap string) error {
	_, _ = runIP("link", "del", tap)
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
		Bridge:     n.bridge,
		Subnet:     n.subnet.String(),
		OwnerPID:   os.Getpid(),
		Masquerade: n.masquerade,
		Taps:       cloneTaps(n.taps),
	}
	return n.store.save(rec)
}

// enableMasquerade turns on IPv4 forwarding (only if it was off, recording the
// original for restore) and installs the iptables rules that let the subnet reach
// the internet: a POSTROUTING MASQUERADE for traffic leaving any interface but
// the bridge, plus FORWARD accepts for the bridge. On a partial failure it rolls
// back whatever it added.
func (n *chNetwork) enableMasquerade() error {
	// Only flip ip_forward 0->1, and record the true original once so teardown
	// can restore it. If it is already on (routers, Docker hosts, …) we never
	// touch it and never restore it (ADR-0013).
	if orig, ok := readIPForward(); ok && orig == "0" {
		_ = n.store.saveIPForwardOrig("0")
		if err := writeIPForward("1"); err != nil {
			return fmt.Errorf("enable ip forwarding: %w", err)
		}
	}

	// Insert (-I) rather than append: on hosts where Docker/ufw put a DROP at the
	// top of FORWARD, an appended ACCEPT would never be reached.
	for _, rule := range masqRules("-I", n.subnet.String(), n.bridge) {
		if err := runIPTables(rule...); err != nil {
			n.removeMasqRules() // best-effort cleanup of any rule added so far
			return fmt.Errorf("add iptables rule: %w", err)
		}
	}

	n.mu.Lock()
	n.masquerade = true
	n.mu.Unlock()
	_ = n.persist()
	return nil
}

// disableMasquerade removes the egress rules installed by enableMasquerade. It is
// a no-op if they were never installed. ip_forward is restored separately by
// Close (only once nothing of ours remains).
func (n *chNetwork) disableMasquerade() error {
	n.mu.Lock()
	masq := n.masquerade
	n.mu.Unlock()
	if !masq {
		return nil
	}

	var errs []error
	for _, rule := range masqRules("-D", n.subnet.String(), n.bridge) {
		if err := runIPTables(rule...); err != nil {
			errs = append(errs, err)
		}
	}

	n.mu.Lock()
	n.masquerade = false
	n.mu.Unlock()
	return errors.Join(errs...)
}

// removeMasqRules best-effort deletes every egress rule, ignoring errors. Used to
// roll back a partially-applied enableMasquerade.
func (n *chNetwork) removeMasqRules() {
	for _, rule := range masqRules("-D", n.subnet.String(), n.bridge) {
		_ = runIPTables(rule...)
	}
}

// reconcile removes the host resources of every recorded network whose owning
// holder is no longer alive (a crash before Close), so orphaned bridges, taps,
// and iptables rules self-heal on the next up and via prune. Every teardown step
// is idempotent. When restoreForwarding is set (the prune path) it also returns
// ip_forward to its recorded original once nothing of ours remains; CreateNetwork
// passes false because it is about to need forwarding (ADR-0013).
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
			_, _ = runIP("link", "del", tap)
		}
		for _, rule := range masqRules("-D", rec.Subnet, rec.Bridge) {
			_ = runIPTables(rule...)
		}
		_, _ = runIP("link", "del", rec.Bridge)

		if linkExists(rec.Bridge) {
			errs = append(errs, fmt.Errorf("reconcile: bridge %s still present", rec.Bridge))
			continue // keep the record so a later sweep retries
		}
		if err := store.delete(rec.Bridge); err != nil {
			errs = append(errs, err)
		}
	}

	if restoreForwarding {
		maybeRestoreIPForward(store)
	}
	return errors.Join(errs...)
}

// maybeRestoreIPForward returns ip_forward to its recorded original once no
// fleetbox network remains — neither a persisted record nor an fbx-* bridge on
// the host (a belt-and-suspenders cross-check against another holder's live
// cluster). The marker is first-writer-wins, so this restores the true
// pre-fleetbox value even across holders and crashes (ADR-0013).
func maybeRestoreIPForward(store *netStore) {
	recs, err := store.list()
	if err != nil || len(recs) > 0 {
		return
	}
	if hostHasFbxBridges() {
		return
	}
	orig, ok := store.readIPForwardOrig()
	if !ok {
		return // we never flipped it
	}
	_ = writeIPForward(orig)
	store.clearIPForwardOrig()
}

// masqRules returns the egress iptables rules for the given action ("-I" to add,
// "-D" to remove) scoped to subnet/bridge. The MASQUERADE rule lives in
// nat/POSTROUTING; the accepts in filter/FORWARD. It takes subnet/bridge
// explicitly so reconcile can rebuild a crashed network's rules from its record.
func masqRules(action, subnet, bridge string) [][]string {
	return [][]string{
		{"-t", "nat", action, "POSTROUTING", "-s", subnet, "!", "-o", bridge, "-j", "MASQUERADE"},
		{action, "FORWARD", "-i", bridge, "-j", "ACCEPT"},
		{action, "FORWARD", "-o", bridge, "-j", "ACCEPT"},
	}
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
// cross-process backstop that stops maybeRestoreIPForward from disabling
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

// runIP runs an `ip` subcommand, returning its combined output (for diagnosis)
// and a wrapped error on failure.
func runIP(args ...string) (string, error) {
	out, err := exec.Command("ip", args...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("ip %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// runIPTables runs an `iptables` command, wrapping its combined output into the
// error on failure.
func runIPTables(args ...string) error {
	out, err := exec.Command("iptables", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("iptables %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// isPermissionDenied reports whether an `ip` command's output indicates a
// missing capability (CAP_NET_ADMIN).
func isPermissionDenied(out string) bool {
	low := strings.ToLower(out)
	return strings.Contains(low, "operation not permitted") || strings.Contains(low, "permission denied")
}
