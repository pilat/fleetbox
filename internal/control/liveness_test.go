package control

import (
	"errors"
	"fmt"
	"syscall"
	"testing"
)

// TestSignalMeansAlive pins the cross-uid liveness classification (ADR-0023): a
// kill(pid, 0) probe means the process is alive on nil OR EPERM (it exists but is
// owned by another user — the non-root `ls`/`ssh` probing the root-owned holder),
// and dead only on ESRCH or any other error. The EPERM row is the regression: it
// used to be read as "stopped", hiding a running VM from non-root commands.
func TestSignalMeansAlive(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil → alive (we may signal it)", err: nil, want: true},
		{name: "EPERM → alive (exists, foreign-owned)", err: syscall.EPERM, want: true},
		{name: "EPERM wrapped → alive", err: fmt.Errorf("signal: %w", syscall.EPERM), want: true},
		{name: "ESRCH → dead (no such process)", err: syscall.ESRCH, want: false},
		{name: "other error → dead", err: errors.New("boom"), want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := signalMeansAlive(tc.err); got != tc.want {
				t.Errorf("signalMeansAlive(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
