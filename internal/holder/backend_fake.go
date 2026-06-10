//go:build fleetbox_fake

package holder

import (
	"github.com/pilat/fleetbox/internal/backend"
	"github.com/pilat/fleetbox/internal/backend/fake"
	"github.com/pilat/fleetbox/internal/store"
)

// newRealBackend selects the fake backend under the fleetbox_fake build tag. The
// fake now lives behind the helper (ADR-0020, moved from the orchestrator): a
// helper built -tags fleetbox_fake serves the real client↔helper protocol with an
// instant, no-VM backend, so the coordination tests exercise the wire on a CI
// runner with no codesign and no /dev/kvm. This file replaces the platform files
// (which carry !fleetbox_fake) and is physically absent from a normal build.
func newRealBackend(*store.Store) (backend.Backend, error) {
	return fake.New(), nil
}
