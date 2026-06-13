//go:build linux

package cloudhypervisor

import (
	"fmt"
	"net"

	"github.com/vishvananda/netlink"
)

// uplinkProbeIP is a public address used only to query the routing table for the
// default-route interface. No packet is sent — RouteGet resolves which link the
// kernel would use to reach it (Decision 4).
const uplinkProbeIP = "1.1.1.1"

// discoverUplink returns the name of the interface carrying the host's default
// route, or "" if the host has none (an offline box) — in which case masquerade is
// moot and only egress is unavailable; VM↔VM and VM↔host still work. It asks the
// routing table for the route to a public address (no packet leaves the host) and
// resolves the resulting link index to a name (Decision 4).
func discoverUplink() (string, error) {
	// A RouteGet failure means no route to the probe address — an offline host
	// (e.g. ENETUNREACH). That is not fatal: an empty result yields no uplink, and
	// the caller skips uplink forwarding.
	routes, _ := netlink.RouteGet(net.ParseIP(uplinkProbeIP))
	indices := make([]int, 0, len(routes))
	for i := range routes {
		indices = append(indices, routes[i].LinkIndex)
	}
	return uplinkName(indices, func(idx int) (string, error) {
		ifi, err := net.InterfaceByIndex(idx)
		if err != nil {
			return "", fmt.Errorf("interface by index %d: %w", idx, err)
		}
		return ifi.Name, nil
	})
}

// enableForwarding turns on per-interface IPv4 forwarding for the bridge and the
// egress uplink so the subnet's masqueraded traffic is actually routed, WITHOUT
// ever touching the global switch (Decision 4). If the host already forwards
// globally it changes nothing (and records nothing to restore). Otherwise it
// write-ahead records the uplink and its observed original BEFORE flipping
// anything, enables forwarding on the bridge (ephemeral — dies with the bridge, no
// restore) and on the uplink (NAT return traffic ingresses there), and preserves
// the uplink's true original first-writer-wins for the last network out to restore.
func (n *chNetwork) enableForwarding() error {
	uplink, err := discoverUplink()
	if err != nil {
		return fmt.Errorf("discover uplink: %w", err)
	}

	// Host already forwards on all interfaces — enable nothing, restore nothing.
	if globalForwardingOn() {
		orig, _ := readForwarding(uplink) // informational only; we flip nothing
		return n.recordUplink(uplink, orig)
	}

	orig, ok := "", false
	if uplink != "" {
		orig, ok = readForwarding(uplink)
	}

	// Write-ahead the uplink + observed original BEFORE flipping the flag (ADR-0013).
	if err := n.recordUplink(uplink, orig); err != nil {
		return err
	}

	// Bridge ingress: ephemeral, no restore.
	if err := writeForwarding(n.bridge, "1"); err != nil {
		return fmt.Errorf("enable forwarding on bridge %s: %w", n.bridge, err)
	}

	// Uplink ingress: only flip 0->1, and record the true original first-writer-wins
	// so the last network out restores it. An uplink already forwarding (orig "1")
	// is left untouched and not restored — it was not our doing.
	if uplink != "" && ok && orig == "0" {
		if err := n.store.saveForwardingOrig(uplink, "0"); err != nil {
			return fmt.Errorf("record uplink forwarding original: %w", err)
		}
		if err := writeForwarding(uplink, "1"); err != nil {
			return fmt.Errorf("enable forwarding on uplink %s: %w", uplink, err)
		}
	}
	return nil
}

// recordUplink stores the uplink and its observed forwarding original on the
// network and rewrites the write-ahead record.
func (n *chNetwork) recordUplink(uplink, orig string) error {
	n.mu.Lock()
	n.uplink = uplink
	n.uplinkFwdOrig = orig
	n.mu.Unlock()
	if err := n.persist(); err != nil {
		return fmt.Errorf("persist uplink record: %w", err)
	}
	return nil
}

// maybeRestoreForwarding returns every uplink fleetbox flipped to its recorded
// original once no fleetbox network remains — neither a persisted record nor an
// fbx-* bridge on the host (a belt-and-suspenders cross-check against another
// holder's live cluster). The markers are first-writer-wins, so this restores the
// true pre-fleetbox value even across holders and crashes (ADR-0013, ADR-0025).
// The bridge's own forwarding flag needs no restore — it died with the bridge.
func maybeRestoreForwarding(store *netStore) {
	recs, err := store.list()
	if err != nil || len(recs) > 0 {
		return
	}
	if hostHasFbxBridges() {
		return
	}
	for uplink, orig := range store.listForwardingOrigs() {
		// Clear the first-writer-wins marker only after the restore actually lands.
		// If writeForwarding fails (e.g. the uplink vanished transiently), keep the
		// marker so a later teardown can still restore the recorded original.
		if err := writeForwarding(uplink, orig); err != nil {
			continue
		}
		store.clearForwardingOrig(uplink)
	}
}
