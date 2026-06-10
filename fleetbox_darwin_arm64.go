//go:build darwin && arm64

package fleetbox

import (
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// This file holds the macOS host-capability probes that must answer client-side,
// in pure Go, WITHOUT spawning or downloading the helper (ADR-0017, R7): deciding
// to skip a test must not pull a multi-hundred-MB helper. The VM orchestration
// itself lives in the shared fleetbox_supported.go (it drives the helper); only
// these Darwin probes stay platform-specific here (ADR-0020 collapse note).

// nestedVirtSupported is the pure-Go macOS heuristic: Apple Silicon M3+ on macOS
// 15+. It must answer without VZ and without downloading the helper, because it
// gates the test-skip path (ADR-0017, R7). An unrecognized CPU brand (a future
// chip) is treated optimistically as capable; the helper runs the authoritative
// vz.IsNestedVirtualizationSupported check at boot and errors cleanly if wrong.
func nestedVirtSupported() bool {
	return nestedCapable(macOSMajor(), appleCPUGeneration())
}

// nestedCapable is the pure decision behind nestedVirtSupported, split out so it
// is testable without the host sysctls. nested virtualization needs macOS 15+ and
// Apple Silicon M3+. A 0 generation means the CPU brand was unrecognized (a future
// chip): treat it as capable so a new Mac is not wrongly skipped — the helper's
// authoritative VZ check rejects it at boot if the optimism was wrong (R7).
func nestedCapable(macOSMajor, appleGen int) bool {
	if macOSMajor < 15 {
		return false
	}
	if appleGen == 0 {
		return true
	}
	return appleGen >= 3
}

// supportsClusteringHost reports macOS 26+, where vmnet SharedMode gives VM↔VM
// connectivity (ADR-0008, ADR-0012). Pure Go, no helper download (R7).
func supportsClusteringHost() bool { return macOSMajor() >= 26 }

// prune is a no-op on macOS: vmnet owns its own state and a dead helper's
// in-process VMs die with it, so there is nothing to reclaim (ADR-0013).
func prune() error { return nil }

// macOSMajor returns the host's major macOS version from kern.osproductversion,
// or 0 on a sysctl/parse error (which conservatively reports "not capable").
func macOSMajor() int {
	ver, err := unix.Sysctl("kern.osproductversion")
	if err != nil {
		return 0
	}
	major, _, _ := strings.Cut(ver, ".")
	n, err := strconv.Atoi(major)
	if err != nil {
		return 0
	}
	return n
}

// appleCPUGeneration returns the Apple Silicon generation N from
// machdep.cpu.brand_string ("Apple M3 Pro" → 3), or 0 if unrecognized.
func appleCPUGeneration() int {
	brand, err := unix.Sysctl("machdep.cpu.brand_string")
	if err != nil {
		return 0
	}
	return parseAppleGeneration(brand)
}

// parseAppleGeneration extracts N from an "Apple M<N> ..." CPU brand string. It
// returns 0 when no "M<N>" token is present (a non-Apple or future/unknown brand).
func parseAppleGeneration(brand string) int {
	for f := range strings.FieldsSeq(brand) {
		if len(f) >= 2 && f[0] == 'M' {
			if n, err := strconv.Atoi(f[1:]); err == nil {
				return n
			}
		}
	}
	return 0
}
