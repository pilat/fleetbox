//go:build !darwin && !linux

package main

import (
	"fmt"
	"runtime"

	"github.com/pilat/fleetbox/internal/store"
)

// maybeRunHolder never runs on an unsupported platform.
func maybeRunHolder() (bool, error) { return false, nil }

// holderExe reports that no holder exists for this platform.
func holderExe(*store.Store) (string, error) {
	return "", fmt.Errorf("fleetbox: unsupported platform (%s/%s)", runtime.GOOS, runtime.GOARCH)
}
