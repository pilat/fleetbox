//go:build darwin && arm64

package fleetbox

import (
	"github.com/pilat/fleetbox/internal/backend"
	"github.com/pilat/fleetbox/internal/backend/vz"
)

func newBackend() (backend.Backend, error) {
	return vz.New(), nil
}

func nestedVirtSupported() bool {
	return vz.New().NestedVirtSupported()
}
