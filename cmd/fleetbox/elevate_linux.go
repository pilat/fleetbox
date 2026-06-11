//go:build linux

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

// envElevated marks a process that a prior auto-elevation already re-exec'd via
// sudo. It is the loop guard: if it is set and we are still not root, sudo is
// misconfigured and we must not re-exec again (ADR-0023). It is set by the `env`
// wrapper (not sudo -E), so it survives sudo's env_reset.
const envElevated = "FLEETBOX_ELEVATED"

// ensurePrivileged is called first by every privileged CLI command (up/down/rm) on
// Linux, where the network work needs root (ADR-0023). It re-execs the command
// under sudo when interactive, prints the exact command when not, and is a no-op
// when already root. The library NEVER calls this — auto-elevation lives only in
// the CLI (decision 3).
func ensurePrivileged() error {
	action := decideElevation(
		os.Geteuid(),
		os.Getenv(envElevated) == "1",
		ttyOpenable(),
		sudoFound(),
	)
	switch action {
	case elevateProceed:
		return nil
	case elevateLoopError:
		return errors.New(
			"privilege elevation did not result in root; run the command under sudo manually")
	case elevateExecSudo:
		return execUnderSudo()
	case elevatePrint:
		return printElevation()
	}
	return nil // unreachable: decideElevation returns one of the four actions
}

// ttyOpenable reports whether sudo could prompt for a password — i.e. whether a
// controlling terminal exists. It opens /dev/tty specifically (not stdin/stdout/
// stderr), because sudo reads the password from /dev/tty, so a per-std-fd check
// would misfire under partial redirection (e.g. `fleetbox up 2>/dev/null`). When
// there is no controlling terminal (CI, `go test`, `setsid`) the open fails and we
// fall through to printing the command instead of risking a hang.
func ttyOpenable() bool {
	f, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}

// sudoFound reports whether a sudo binary is on PATH.
func sudoFound() bool {
	_, err := exec.LookPath("sudo")
	return err == nil
}

// execUnderSudo replaces this process with the elevated command so sudo can prompt
// on the tty and the child's exit code becomes ours. If sudo vanished between the
// LookPath check and here, it falls through to the print path rather than failing
// opaquely.
func execUnderSudo() error {
	sudoPath, err := exec.LookPath("sudo")
	if err != nil {
		return printElevation()
	}
	argv, err := elevatedArgv()
	if err != nil {
		return err
	}
	// syscall.Exec replaces the image; on success it never returns. The env handed
	// to sudo itself does not matter (sudo resets it) — the `env` wrapper inside
	// argv sets FLEETBOX_ELEVATED and PATH for the final exec, and sudo itself adds
	// SUDO_USER/SUDO_UID/SUDO_GID (which the store and the key/socket fixups read).
	if err := syscall.Exec(sudoPath, argv, os.Environ()); err != nil {
		return fmt.Errorf("exec sudo: %w", err)
	}
	return nil
}

// printElevation writes the exact, ready-to-paste command to stderr and exits
// non-zero, for the non-interactive case (CI, pipes, no tty). It returns a silent
// cliExit so main does not stack a second "error:" line on top of the command we
// printed (the shared cliExit scheme). It must NEVER hang.
func printElevation() error {
	argv, err := elevatedArgv()
	if err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr,
		"fleetbox needs root on Linux but there is no terminal to prompt for the sudo password.")
	fmt.Fprintln(os.Stderr, "Run this command manually:")
	fmt.Fprintln(os.Stderr, "  "+shellJoin(argv))
	return &cliExit{code: 1, silent: true}
}

// elevatedArgv builds the argv for the re-exec:
//
//	sudo env FLEETBOX_ELEVATED=1 PATH=<current> <abs self> <original args...>
//
// FLEETBOX_ELEVATED and PATH go in the `env` prefix, NOT via `sudo -E`: many
// sudoers policies reject -E (SETENV not granted) and env_reset drops the process
// environment, so the loop-guard flag and PATH must be set by `env` (which runs
// after sudo authenticates). The self path is absolute (os.Executable), which is
// what fixes `sudo: fleetbox: command not found` when sudo's secure_path omits the
// Go/mise bin dir. HOME is deliberately NOT forwarded — the store resolves the
// SUDO_USER passwd home, not $HOME, in the root case (Task 2 / ADR-0023).
func elevatedArgv() ([]string, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve own path: %w", err)
	}
	argv := make([]string, 0, 5+len(os.Args)-1)
	argv = append(argv, "sudo", "env", envElevated+"=1", "PATH="+os.Getenv("PATH"), self)
	argv = append(argv, os.Args[1:]...)
	return argv, nil
}

// shellJoin renders an argv as a single, copy-pasteable shell command, quoting only
// the tokens that need it so the common case stays readable.
func shellJoin(argv []string) string {
	quoted := make([]string, len(argv))
	for i, a := range argv {
		quoted[i] = shellQuote(a)
	}
	return strings.Join(quoted, " ")
}

// shellQuote single-quotes a token unless it consists solely of characters that are
// safe unquoted in a POSIX shell. An embedded single quote is escaped the standard
// way ('"'"').
func shellQuote(s string) string {
	if s != "" && !strings.ContainsFunc(s, shellUnsafe) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// shellUnsafe reports whether r needs quoting in a shell word. The allowed set
// covers everything in our argv's common case (paths, PATH=, FLAG=value, flags).
func shellUnsafe(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return false
	case strings.ContainsRune("-_=:/.,+@", r):
		return false
	default:
		return true
	}
}
