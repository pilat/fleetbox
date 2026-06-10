//go:build darwin && arm64

package orchestrator

import (
	"fmt"

	"github.com/pilat/fleetbox/internal/helperdist"
	"github.com/pilat/fleetbox/internal/store"
)

// helperExe returns the downloaded, signed fleetbox-helper the client spawns as
// the VM holder, fetching it on first use (or honoring FLEETBOX_HELPER). This is
// the macOS sever: the client links no vz — only the path to the signed helper,
// which carries the entitlement (ADR-0017/0020).
func helperExe(st *store.Store) (string, error) {
	path, err := helperdist.Ensure(st)
	if err != nil {
		return "", fmt.Errorf("resolve helper: %w", err)
	}
	return path, nil
}
