//go:build !linux && !(darwin && arm64)

package fleetbox

import (
	"context"
	"fmt"
	"runtime"
)

// unsupported reports that fleetbox has no backend for this platform. The message
// names both GOOS and GOARCH so the failure is obvious (e.g. darwin/amd64, where
// Apple never exposed nested virtualization). Start/NewCluster surface it; nothing
// panics.
func unsupported() error {
	return fmt.Errorf("fleetbox: unsupported platform (%s/%s)", runtime.GOOS, runtime.GOARCH)
}

// Start always errors on an unsupported platform.
func Start(context.Context, string, ...Option) (*VM, error) {
	return nil, unsupported()
}

// NewCluster always errors on an unsupported platform.
func NewCluster(...Option) (*Cluster, error) {
	return nil, unsupported()
}

func nestedVirtSupported() bool { return false }

func prune() error { return nil }
