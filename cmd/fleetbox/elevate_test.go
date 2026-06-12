package main

import "testing"

// TestDecideElevation pins the auto-elevation decision (ADR-0023), the only
// automated coverage of the gate — the real re-exec is Linux-host-only. The
// critical row is "no tty" → print-and-exit: a non-interactive context must never
// choose exec-sudo (which could hang waiting on a password).
func TestDecideElevation(t *testing.T) {
	cases := []struct {
		name            string
		euid            int
		alreadyElevated bool
		ttyOpenable     bool
		sudoFound       bool
		want            elevateAction
	}{
		{name: "already root proceeds", euid: 0, want: elevateProceed},
		{name: "root wins even if elevated flag set", euid: 0, alreadyElevated: true, want: elevateProceed},
		{
			name: "interactive non-root execs sudo",
			euid: 1000, ttyOpenable: true, sudoFound: true, want: elevateExecSudo,
		},
		{
			name: "no tty prints instead of hanging",
			euid: 1000, ttyOpenable: false, sudoFound: true, want: elevatePrint,
		},
		{
			name: "sudo missing prints",
			euid: 1000, ttyOpenable: true, sudoFound: false, want: elevatePrint,
		},
		{
			name: "elevated yet not root is a loop error",
			euid: 1000, alreadyElevated: true, ttyOpenable: true, sudoFound: true, want: elevateLoopError,
		},
		{
			name: "loop guard beats the tty path",
			euid: 1000, alreadyElevated: true, ttyOpenable: false, sudoFound: false, want: elevateLoopError,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := decideElevation(tc.euid, tc.alreadyElevated, tc.ttyOpenable, tc.sudoFound)
			if got != tc.want {
				t.Errorf("decideElevation(euid=%d, elevated=%v, tty=%v, sudo=%v) = %d, want %d",
					tc.euid, tc.alreadyElevated, tc.ttyOpenable, tc.sudoFound, got, tc.want)
			}
		})
	}
}

// TestEnsureSbinInPath pins the PATH fix (ADR-0023): the elevated holder needs
// /sbin and /usr/sbin (where ip/iptables live) even when the invoking user's PATH
// omits them — as a stock Debian cloud image's does. Surfaced by dogfooding the
// Linux path inside a fleetbox-booted VM.
func TestEnsureSbinInPath(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "sbin-less user PATH gets sbin appended",
			in:   "/usr/local/bin:/usr/bin:/bin",
			want: "/usr/local/bin:/usr/bin:/bin:/usr/local/sbin:/usr/sbin:/sbin",
		},
		{
			name: "already-present sbin dirs are not duplicated",
			in:   "/usr/sbin:/usr/bin:/sbin:/bin",
			want: "/usr/sbin:/usr/bin:/sbin:/bin:/usr/local/sbin",
		},
		{
			name: "empty PATH yields just the sbin dirs (no cwd-injecting empty entry)",
			in:   "",
			want: "/usr/local/sbin:/usr/sbin:/sbin",
		},
		{
			name: "empty entries are dropped",
			in:   "/usr/bin::/bin:",
			want: "/usr/bin:/bin:/usr/local/sbin:/usr/sbin:/sbin",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ensureSbinInPath(tc.in); got != tc.want {
				t.Errorf("ensureSbinInPath(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
