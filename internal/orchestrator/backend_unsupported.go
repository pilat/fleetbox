//go:build !linux && !(darwin && arm64)

package orchestrator

import (
	"fmt"
	"runtime"

	"github.com/pilat/fleetbox/internal/store"
)

// helperExe reports that fleetbox has no VM helper for this platform. The message
// names both GOOS and GOARCH so the failure is obvious (e.g. darwin/amd64, where
// Apple never exposed nested virtualization). Start/NewCluster surface it; nothing
// panics.
func helperExe(*store.Store) (string, error) {
	return "", fmt.Errorf("fleetbox: unsupported platform (%s/%s)", runtime.GOOS, runtime.GOARCH)
}
