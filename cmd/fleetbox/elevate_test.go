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
