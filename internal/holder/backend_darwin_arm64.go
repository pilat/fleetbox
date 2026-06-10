//go:build darwin && arm64 && !fleetbox_fake

package holder

import (
	"github.com/pilat/fleetbox/internal/backend"
	"github.com/pilat/fleetbox/internal/backend/vz"
	"github.com/pilat/fleetbox/internal/store"
)

// newRealBackend selects the macOS backend: Apple Virtualization.framework. This
// is the one place vz is linked — the holder runs only inside the signed
// fleetbox-helper, so the importable client and CLI stay pure Go (ADR-0017/0020).
func newRealBackend(*store.Store) (backend.Backend, error) {
	return vz.New(), nil
}
