package main

import (
	"errors"
	"fmt"
	"os"
	"runtime/debug"
)

// versionDev is the version a non-release ("go build" / "go install") build
// carries until ldflags set a real one; versionString treats it as the trigger to
// fall back to the VCS metadata the toolchain stamps in.
const versionDev = "dev"

// Build metadata, set at release time via -ldflags -X main.* (see .goreleaser.yaml).
// The defaults identify a non-release ("go build" / "go install") build. These are
// the only package-level mutable vars; everything else is constructed locally so the
// command tree carries no global state.
var (
	version = versionDev
	commit  = "none"
	date    = "unknown"
)

// cliExit is the single exit scheme the whole CLI uses. A command's RunE returns
// it to control the process exit code and whether main prints an "error:" line:
// silent is set when the command has already written its own output (a child
// ssh/scp wrote to stderr, or a best-effort bulk loop printed per-target results),
// so main must not double-report on top of it.
type cliExit struct {
	code   int
	silent bool
	err    error
}

// Error implements error. It reports the wrapped message, or empty for a silent
// exit that carries only a code.
func (e *cliExit) Error() string {
	if e.err != nil {
		return e.err.Error()
	}
	return ""
}

func main() {
	// On Linux a re-exec'd holder (--fleetbox-runner/--fleetbox-reconcile) is
	// intercepted by internal/holder's init() — linked here via the root fleetbox
	// package — which runs the holder and exits before this main() is reached. On
	// macOS the holder is the separate downloaded fleetbox-helper, never this binary
	// (ADR-0020). So by the time we get here, this is a real CLI invocation.
	err := newRootCmd().Execute()
	if err == nil {
		return
	}

	// One exit-handling block for the whole CLI. A *cliExit carries its own code
	// and decides whether to print; any other error is a plain failure → exit 1.
	var ce *cliExit
	if errors.As(err, &ce) {
		if !ce.silent && ce.err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", ce.err)
		}
		os.Exit(ce.code)
	}
	fmt.Fprintf(os.Stderr, "error: %v\n", err)
	os.Exit(1)
}

// versionString reports the build version. Release builds carry it via -ldflags
// (-X main.version/commit/date, see .goreleaser.yaml). A plain `go install`/`go build`
// has no ldflags, so fall back to the VCS metadata the Go toolchain stamps into the
// binary — otherwise `go install`ed CLIs would report "dev".
func versionString() string {
	v, c, d := version, commit, date
	if v == versionDev {
		if bi, ok := debug.ReadBuildInfo(); ok {
			if bi.Main.Version != "" && bi.Main.Version != "(devel)" {
				v = bi.Main.Version
			}
			for _, s := range bi.Settings {
				switch s.Key {
				case "vcs.revision":
					if s.Value != "" {
						c = s.Value
					}
				case "vcs.time":
					if s.Value != "" {
						d = s.Value
					}
				}
			}
		}
	}
	return fmt.Sprintf("fleetbox %s (commit %s, built %s)", v, c, d)
}
