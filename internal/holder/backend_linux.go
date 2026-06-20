//go:build linux && !fleetbox_fake

package holder

import (
	"github.com/pilat/fleetbox/internal/backend"
	"github.com/pilat/fleetbox/internal/backend/cloudhypervisor"
	"github.com/pilat/fleetbox/internal/store"
)

// newRealBackend selects the Linux backend: cloud-hypervisor. It owns the shared
// bridge/taps and the ADR-0013 write-ahead records under the store's network-state
// dir, and caches the pinned VMM binary under the bin dir. The holder is the only
// thing that links it, reached on Linux by the CLI/test binary re-execing itself as
// the holder (ADR-0020).
func newRealBackend(st *store.Store) (backend.Backend, error) {
	return cloudhypervisor.New(st.BinDir(), st.NetworkStateDir()), nil
}
