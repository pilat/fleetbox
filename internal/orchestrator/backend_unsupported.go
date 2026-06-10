//go:build !linux && !(darwin && arm64) && !fleetbox_fake

package orchestrator

import (
	"fmt"
	"runtime"

	"github.com/pilat/fleetbox/internal/backend"
)

// newBackend reports that fleetbox has no backend for this platform. The message
// names both GOOS and GOARCH so the failure is obvious (e.g. darwin/amd64, where
// Apple never exposed nested virtualization). Start/NewCluster surface it;
// nothing panics.
func newBackend() (backend.Backend, error) {
	return nil, fmt.Errorf("fleetbox: unsupported platform (%s/%s)", runtime.GOOS, runtime.GOARCH)
}

func nestedVirtSupported() bool {
	return false
}

// preflight is a no-op on an unsupported platform: newBackend already returns the
// clear "unsupported platform" error.
func preflight() error {
	return nil
}
