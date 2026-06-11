package main

// elevateAction is the outcome of the privilege-elevation decision (ADR-0023). The
// decision is computed by the pure, un-tagged decideElevation below so it is
// table-testable on any platform; the Linux shell (elevate_linux.go) maps each action
// to a real syscall.Exec/print, and non-Linux uses a no-op stub (elevate_other.go).
type elevateAction int

const (
	// elevateProceed: already root (or nothing to do) — run the command in-process.
	elevateProceed elevateAction = iota
	// elevateExecSudo: not root, a tty is available and sudo exists — re-exec via
	// sudo so it can prompt for the password on the terminal.
	elevateExecSudo
	// elevatePrint: not root and no way to prompt (no tty, or sudo missing) — print
	// the exact ready-to-run command and exit non-zero. NEVER hang waiting on a
	// password in a non-interactive context.
	elevatePrint
	// elevateLoopError: a prior elevation already set FLEETBOX_ELEVATED yet we are
	// still not root — sudo is misconfigured; fail instead of re-exec'ing forever.
	elevateLoopError
)

// decideElevation chooses what a privileged CLI command does given the current
// state. It is the loop-guard and the interactivity gate in one pure place:
//
//   - root already           → proceed
//   - elevated flag yet !root → loop error (re-exec did not yield root)
//   - tty openable & sudo     → exec sudo (it prompts on the tty)
//   - otherwise               → print the command and exit
func decideElevation(euid int, alreadyElevated, ttyOpenable, sudoFound bool) elevateAction {
	if euid == 0 {
		return elevateProceed
	}
	if alreadyElevated {
		return elevateLoopError
	}
	if ttyOpenable && sudoFound {
		return elevateExecSudo
	}
	return elevatePrint
}
