//go:build linux

package main

import (
	"fmt"
	"os"

	"github.com/pilat/fleetbox/internal/holder"
	"github.com/pilat/fleetbox/internal/store"
)

// maybeRunHolder runs the in-process holder when this process was re-exec'd as
// one (--fleetbox-runner). On Linux the CLI is its own VM host — there is nothing
// to sign and cloud-hypervisor is the downloaded VMM — so it re-execs itself
// rather than a separate helper (ADR-0017, R9).
func maybeRunHolder() (bool, error) {
	if holder.IsRunner() {
		return true, holder.Run() //nolint:wrapcheck // transparent delegate; the holder wraps its own errors
	}
	return false, nil
}

// holderExe returns the executable a fresh holder is spawned from: the CLI
// itself, re-exec'd with --fleetbox-runner.
func holderExe(*store.Store) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("get executable: %w", err)
	}
	return exe, nil
}
