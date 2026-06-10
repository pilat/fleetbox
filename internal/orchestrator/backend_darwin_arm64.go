//go:build darwin && arm64 && !fleetbox_fake

package orchestrator

import (
	"github.com/pilat/fleetbox/internal/backend"
	"github.com/pilat/fleetbox/internal/backend/vz"
)

func newBackend() (backend.Backend, error) {
	return vz.New(), nil
}

// preflight is a no-op on macOS: the helper holds the virtualization entitlement
// and vmnet needs no special host capability. The authoritative VZ capability
// check happens when the VM boots.
func preflight() error {
	return nil
}

// nestedVirtSupported asks Virtualization.framework directly. It is the
// authoritative check, run inside the helper; the root darwin client uses a
// pure-Go heuristic instead so it can decide to skip a test without downloading
// the helper (ADR-0017, R7).
func nestedVirtSupported() bool {
	return vz.New().NestedVirtSupported()
}
