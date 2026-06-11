package orchestrator

import (
	"context"
	"os"
	"testing"

	"github.com/pilat/fleetbox/internal/backend"
	"github.com/pilat/fleetbox/internal/store"
)

var _ backend.VM = (*stubBackendVM)(nil)

// stubBackendVM is a no-op backend.VM for the orchestrator tests: there is no
// reusable in-process backend.VM fake (internal/backend/fake lives behind the
// helper), so the rollback and createdThisCall tests drive a local stub. Stop is
// recorded so a test can assert it ran without inspecting a real hypervisor.
type stubBackendVM struct {
	stopCalls int
}

func (s *stubBackendVM) Start(context.Context) error { return nil }

func (s *stubBackendVM) Stop(context.Context) error {
	s.stopCalls++
	return nil
}

func (s *stubBackendVM) State() backend.State { return backend.StateStopped }

func (s *stubBackendVM) Wait(context.Context) error { return nil }

func (s *stubBackendVM) WaitForIP(context.Context) (string, error) { return "10.0.0.5", nil }

// TestClusterRollback is the seam test for the data-loss fix: rollback must
// Destroy only members this call created and merely Stop pre-existing ones, so a
// re-up of stopped members never deletes a persisted disk. Both members have
// files on disk (a created member is stored before a later member fails); the
// createdThisCall flag — not store state — decides Destroy vs Stop.
func TestClusterRollback(t *testing.T) {
	st, err := store.NewAt(t.TempDir())
	if err != nil {
		t.Fatalf("store.NewAt: %v", err)
	}
	for _, name := range []string{"web-1", "web-2"} {
		if err := st.Create(&store.VM{Name: name}); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}

	preexisting := &VM{name: "web-1", store: st, backend: &stubBackendVM{}, createdThisCall: false}
	created := &VM{name: "web-2", store: st, backend: &stubBackendVM{}, createdThisCall: true}
	c := &Cluster{vms: []*VM{preexisting, created}}

	if err := c.rollback(context.Background()); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	// Pre-existing member: stopped, never deleted — its disk survives.
	if !st.Exists("web-1") {
		t.Error("rollback deleted a pre-existing member's disk (data loss)")
	}
	if _, err := os.Stat(st.VMDir("web-1")); err != nil {
		t.Errorf("pre-existing member dir missing after rollback: %v", err)
	}

	// Created-this-call member: destroyed, files removed.
	if st.Exists("web-2") {
		t.Error("rollback did not clean up a member it created this call")
	}
	if _, err := os.Stat(st.VMDir("web-2")); !os.IsNotExist(err) {
		t.Errorf("created member dir still present after rollback: stat err = %v", err)
	}
}
