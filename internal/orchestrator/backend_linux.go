//go:build linux

package orchestrator

import (
	"fmt"
	"os"

	"github.com/pilat/fleetbox/internal/store"
)

// helperExe returns the executable a fresh holder is spawned from on Linux: this
// binary itself, re-exec'd with --fleetbox-runner. There is nothing to sign and
// cloud-hypervisor is the downloaded VMM, so the single binary links the CH
// backend and becomes the holder after reexec — the accepted non-sever on Linux
// (ADR-0020).
func helperExe(*store.Store) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("get executable: %w", err)
	}
	return exe, nil
}
