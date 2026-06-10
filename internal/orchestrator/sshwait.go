//go:build !fleetbox_fake

package orchestrator

// skipSSHWait reports whether startOnNetwork should skip the real SSH readiness
// wait. It is always false in production builds — the wait always runs, dialing
// the booted guest. Only the fleetbox_fake test build overrides it
// (sshwait_fake.go) to short-circuit the dial against the fake backend's
// unroutable TEST-NET-3 IP, which would otherwise block for the full timeout.
func skipSSHWait() bool { return false }
