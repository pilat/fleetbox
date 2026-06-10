//go:build fleetbox_fake

package orchestrator

// skipSSHWait reports true under the fleetbox_fake build tag so startOnNetwork
// skips the real SSH readiness wait: the fake backend returns an unroutable
// TEST-NET-3 IP, against which a real ssh.Dial would block for the full timeout.
// The production build (sshwait.go) returns false and the wait runs unchanged
// (ADR-0018).
func skipSSHWait() bool { return true }
