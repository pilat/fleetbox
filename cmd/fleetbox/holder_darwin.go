//go:build darwin

package main

import (
	"fmt"

	"github.com/pilat/fleetbox/internal/helperdist"
	"github.com/pilat/fleetbox/internal/store"
)

// maybeRunHolder never runs on macOS: the holder is the separate downloaded,
// signed fleetbox-helper, so the CLI is a pure-Go client that never re-execs
// itself (ADR-0017, R9). Keeping the holder out of this binary is what lets the
// CLI stay free of cgo, vz, and codesign.
func maybeRunHolder() (bool, error) { return false, nil }

// holderExe returns the downloaded, signed fleetbox-helper to spawn as the
// holder, fetching it on first use (or honoring FLEETBOX_HELPER).
func holderExe(st *store.Store) (string, error) {
	path, err := helperdist.Ensure(st)
	if err != nil {
		return "", fmt.Errorf("resolve helper: %w", err)
	}
	return path, nil
}
