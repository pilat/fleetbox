package fleetbox

import (
	"context"
	"errors"
	"testing"

	"github.com/pilat/fleetbox/internal/backend"
)

// fakeBackend lets the clustering-capability gate be tested without booting a VM
// or depending on the host's macOS version. Only SupportsClustering is
// exercised; the create paths return errors because the gate must reject before
// reaching them.
type fakeBackend struct {
	clustering bool
}

func (f fakeBackend) CreateNetwork() (backend.Network, error) {
	return nil, errors.New("fakeBackend: CreateNetwork not implemented")
}

func (f fakeBackend) Create(backend.Config, backend.Network) (backend.VM, error) {
	return nil, errors.New("fakeBackend: Create not implemented")
}

func (f fakeBackend) NestedVirtSupported() bool { return true }

func (f fakeBackend) SupportsClustering() bool { return f.clustering }

func (f fakeBackend) Reconcile() error { return nil }

// TestClusterAddRejectsSecondMemberWithoutClustering pins the macOS <26 guard
// (ADR-0012): adding a second member on a non-clustering backend fails with
// ErrClustersUnsupported before any boot work. This mirrors the same guard in
// StartCluster/StartN.
func TestClusterAddRejectsSecondMemberWithoutClustering(t *testing.T) {
	c := &Cluster{
		deps: &startDeps{backend: fakeBackend{clustering: false}},
		vms:  []*VM{{name: "node-1"}},
	}

	_, err := c.Add(context.Background(), "node-2")
	if !errors.Is(err, ErrClustersUnsupported) {
		t.Fatalf("Add(second member, no clustering) = %v, want ErrClustersUnsupported", err)
	}
}
