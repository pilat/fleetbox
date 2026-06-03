//go:build darwin && arm64

package fleetbox

import (
	"github.com/pilat/fleetbox/internal/backend"
	"github.com/pilat/fleetbox/internal/backend/vz"
)

func newBackend() backend.Backend {
	return vz.New()
}

func nestedVirtSupported() bool {
	return vz.New().NestedVirtSupported()
}
