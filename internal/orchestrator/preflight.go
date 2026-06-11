package orchestrator

import "errors"

// requireRoot is the pure Linux root gate, split into this un-tagged file so it is
// unit-testable on any platform without actually being root (the linux-tagged
// preflight() calls it; ADR-0023). It returns a clear, copy-pasteable error when
// the process is not root — correct for both consumers: the CLI (which normally
// auto-elevates before this runs) and a library/test user (who must run under
// sudo). Root, not CAP_NET_ADMIN, is the honest gate: the backend shells out to
// ip/iptables (file caps do not survive exec) and writes the DAC-gated
// /proc/sys/net/ipv4/ip_forward, so only root actually works.
func requireRoot(euid int) error {
	if euid == 0 {
		return nil
	}
	return errors.New(
		"fleetbox needs root on Linux (to create the bridge and per-VM taps): run the CLI " +
			"normally and approve the sudo prompt, or for tests run as root " +
			`(e.g. sudo -E env "HOME=$HOME" "PATH=$PATH" go test ./...)`)
}
