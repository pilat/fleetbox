//go:build darwin && arm64

// Command fleetbox-helper is the macOS VM holder: the only fleetbox binary that
// links Apple Virtualization.framework, and therefore the only one that must
// carry the com.apple.security.virtualization entitlement (ad-hoc signed by the
// project, downloaded by the library/CLI at first use). It runs the holder server
// loop — owning the VMs of one Start/up call on one shared vmnet network and
// serving the per-member control protocol — so the user's test binary and the
// CLI stay pure Go and need no codesign (ADR-0017).
//
// It is launched, never invoked by hand: the client passes --fleetbox-runner with
// the member names, FLEETBOX_OPTS with the encoded options, and (in library mode)
// FLEETBOX_PARENT_PID so the helper reaps itself when the test process exits.
package main

import (
	"fmt"
	"os"

	"github.com/pilat/fleetbox/internal/holder"
)

func main() {
	if err := holder.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "fleetbox-helper: %v\n", err)
		os.Exit(1)
	}
}
