//go:build fleetbox_fake

package orchestrator

import (
	"github.com/pilat/fleetbox/internal/backend"
	"github.com/pilat/fleetbox/internal/backend/fake"
)

// newBackend selects the fake backend under the fleetbox_fake build tag, severing
// the real hypervisor (vz/cloud-hypervisor) from the binary so the cross-process
// coordination layer can be tested on a CI runner with no VM boot and no codesign
// (ADR-0018). This file is physically absent from a normal `go build ./...`; only
// `-tags fleetbox_fake` links it, and it then redeclares newBackend in place of
// the platform files (which carry `!fleetbox_fake`).
func newBackend() (backend.Backend, error) {
	return fake.New(), nil
}

// nestedVirtSupported reports true: the fake never boots a guest, so the
// capability gate must not skip the coordination tests.
func nestedVirtSupported() bool {
	return true
}

// preflight is a no-op: the fake needs neither /dev/kvm + CAP_NET_ADMIN (Linux)
// nor the VZ entitlement (macOS), so a stock CI runner does not trip it.
func preflight() error {
	return nil
}
