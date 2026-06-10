//go:build !linux && !(darwin && arm64) && !fleetbox_fake

package holder

import (
	"fmt"
	"runtime"

	"github.com/pilat/fleetbox/internal/backend"
	"github.com/pilat/fleetbox/internal/store"
)

// newRealBackend reports that no VM backend exists for this platform. The holder
// never actually runs here (the client errors before spawning one), but the
// constructor must exist so the package compiles everywhere (ADR-0020).
func newRealBackend(*store.Store) (backend.Backend, error) {
	return nil, fmt.Errorf("fleetbox: unsupported platform (%s/%s)", runtime.GOOS, runtime.GOARCH)
}
