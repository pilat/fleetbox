//go:build linux

package fleetbox

import (
	"os"
	"strings"

	// Blank-import the holder so its self-reexec init() interceptor is linked into
	// every Linux binary that uses fleetbox — including a user's `go test` binary,
	// whose main() is the test framework. When the client spawns os.Executable()
	// with --fleetbox-runner/--fleetbox-reconcile, that init() runs the holder and
	// exits before the test framework starts (ADR-0020). This is what makes the
	// single-binary self-reexec work on Linux; it also links the cloud-hypervisor
	// backend into the client binary, which is the accepted non-sever on Linux.
	_ "github.com/pilat/fleetbox/internal/holder"
	"github.com/pilat/fleetbox/internal/orchestrator"
)

// This file holds the Linux host-capability probes that answer client-side, in
// pure Go, without spawning the helper (ADR-0017, R7). The VM orchestration lives
// in the shared fleetbox_supported.go; only these probes stay platform-specific
// here. The kvm probe is a client-side heuristic — the helper's cloud-hypervisor
// backend runs the authoritative check at boot (ADR-0020 collapse note).

// nestedVirtSupported reports whether /dev/kvm exists and KVM nested
// virtualization is enabled — what consumers running KVM inside guests need. It is
// a pure-Go probe so it never links the cloud-hypervisor backend or spawns the
// helper.
func nestedVirtSupported() bool {
	if _, err := os.Stat("/dev/kvm"); err != nil {
		return false
	}
	return kvmNestedEnabled()
}

// kvmNestedEnabled reports whether KVM's nested parameter is on. It checks the
// x86 submodules (kvm_intel/kvm_amd) and the base kvm module — the latter is where
// arm64 (and the generic case) expose the flag, since arm64 has no kvm_intel/
// kvm_amd. Without it the probe was x86-only and reported false on every arm64
// host, even one with ARMv8.3+ nested virt enabled.
func kvmNestedEnabled() bool {
	for _, p := range []string{
		"/sys/module/kvm_intel/parameters/nested", // x86 Intel
		"/sys/module/kvm_amd/parameters/nested",   // x86 AMD
		"/sys/module/kvm/parameters/nested",       // arm64 / generic
	} {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		switch strings.TrimSpace(string(data)) {
		case "Y", "1":
			return true
		}
	}
	return false
}

// supportsClusteringHost is always true on Linux: cluster members share one bridge
// and reach each other (ADR-0011), unlike the macOS <26 NAT path.
func supportsClusteringHost() bool { return true }

// prune reclaims orphaned host network state by driving a short-lived reconcile
// helper (bridges/taps/nft firewall tables/uplink forwarding) — the helper carries
// CAP_NET_ADMIN (ADR-0013/0020/0025).
func prune() error {
	return orchestrator.Prune() //nolint:wrapcheck // transparent delegate; orchestrator wraps
}
