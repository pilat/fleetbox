package orchestrator

import (
	"strings"
	"testing"
)

// TestRequireRoot pins the honest root gate (ADR-0023): euid==0 passes, anything
// else returns the root-required error. The exact wording is flexible, so this
// asserts on a stable substring, not the full string. Runs on darwin/arm64 under
// `make test` — the only automated coverage of this gate, since the Linux preflight
// opens /dev/kvm first and returns before the euid check on a KVM-less runner.
func TestRequireRoot(t *testing.T) {
	if err := requireRoot(0); err != nil {
		t.Errorf("requireRoot(0) = %v, want nil", err)
	}

	for _, euid := range []int{1, 1000} {
		err := requireRoot(euid)
		if err == nil {
			t.Fatalf("requireRoot(%d) = nil, want error", euid)
		}
		if !strings.Contains(err.Error(), "needs root") {
			t.Errorf("requireRoot(%d) error = %q, want it to mention %q", euid, err, "needs root")
		}
		// The old lie is gone: the message must not suggest granting a file
		// capability (the removed advice that made preflight pass then boot fail).
		if strings.Contains(err.Error(), "capability") {
			t.Errorf("requireRoot(%d) error still suggests a capability fix: %q", euid, err)
		}
	}
}
