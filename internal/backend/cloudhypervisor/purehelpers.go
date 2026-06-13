package cloudhypervisor

// This file carries the platform-portable slivers of the Linux networking code:
// the nft table-name mapping, the nf_tables errno classifier, and the uplink-name
// selection. The rest of the package is //go:build linux (it talks to netlink and
// nf_tables), so it cannot be unit-tested on the macOS dev box. These three are
// pure — they touch no kernel interface — so they live here, untagged, and get
// darwin-runnable tests (purehelpers_test.go). The Linux code feeds them the
// values it pulls from the kernel.

import (
	"errors"
	"fmt"
	"strings"
	"syscall"
)

// nftTableName maps a bridge name to the name of the nft table that owns its
// firewall rules. nft identifiers dislike hyphens, so the bridge's `fbx-<id>`
// becomes `fbx_<id>`. Create, teardown, and reconcile all route through this one
// helper so they cannot drift onto different table names (Decision 2).
func nftTableName(bridge string) string {
	return strings.ReplaceAll(bridge, "-", "_")
}

// classifyNFTErr turns a raw nf_tables error into a coherent, actionable one,
// discriminating on errno (Decision 8): a non-root host fails the probe with
// EPERM (the same "needs root" story as the netlink bridge probe), while a kernel
// without nf_tables fails with EOPNOTSUPP/ENOENT. The original errno is wrapped so
// callers can still errors.Is it. A nil error passes through as nil.
func classifyNFTErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, syscall.EPERM):
		return fmt.Errorf("nf_tables firewall (needs root): %w", err)
	case errors.Is(err, syscall.EOPNOTSUPP), errors.Is(err, syscall.ENOENT):
		return fmt.Errorf("kernel lacks nf_tables support: %w", err)
	default:
		return fmt.Errorf("nf_tables probe: %w", err)
	}
}

// uplinkName resolves the egress interface's name from the link indices of the
// host's default route(s). It picks the first valid index and resolves it through
// byIndex (net.InterfaceByIndex on Linux). An empty list — or only invalid
// indices — means the host has no default route (offline): it returns ("", nil),
// and the caller skips uplink forwarding (masquerade is moot; VM↔VM and VM↔host
// still work). The byIndex indirection keeps this testable without a real host.
func uplinkName(indices []int, byIndex func(int) (string, error)) (string, error) {
	for _, idx := range indices {
		if idx <= 0 {
			continue
		}
		name, err := byIndex(idx)
		if err != nil {
			return "", fmt.Errorf("resolve uplink interface (index %d): %w", idx, err)
		}
		return name, nil
	}
	return "", nil
}
