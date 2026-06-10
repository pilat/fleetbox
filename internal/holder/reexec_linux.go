//go:build linux

package holder

import (
	"fmt"
	"os"
)

// init intercepts a self-reexec'd holder before Go's test framework or a CLI
// main() runs. On Linux the client spawns os.Executable() with --fleetbox-runner
// (boot) or --fleetbox-reconcile (prune); for a library test binary there is no
// main() we control, so naive reexec would just re-run the test framework. This
// init() runs the holder and exits first (the docker/reexec pattern), making
// self-reexec work for both cmd/fleetbox and a user's test binary. It is a cheap
// no-op when neither sentinel flag is present (ADR-0020).
//
// It is linux-only: macOS keeps a separate signed cmd/fleetbox-helper binary
// whose main() calls Run directly, and the importable macOS client links neither
// this package nor a backend (the sever).
func init() {
	switch {
	case IsReconcile():
		exitFromHolder(RunReconcile())
	case IsRunner():
		exitFromHolder(Run())
	}
}

// exitFromHolder reports the holder's exit status and terminates the process,
// never returning to the test framework or CLI main().
func exitFromHolder(err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "fleetbox-helper: %v\n", err)
		os.Exit(1)
	}
	os.Exit(0)
}
