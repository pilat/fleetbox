package fleetbox

import (
	"context"
	"errors"
	"net"
	"testing"
)

// fakeClusterState drives the clustering-capability gate (and the public
// VM/Cluster delegation) without a backend or a real VM. It runs on both builds,
// so it is the unit-level guard that the gate behaves identically on macOS and
// Linux; a real boot exercises the rest in fleetboxtest (ADR-0017, R2).
type fakeClusterState struct {
	clustering bool
	members    []vmState
}

func (f *fakeClusterState) Add(_ context.Context, name string) (vmState, error) {
	vm := &fakeVMState{name: name}
	f.members = append(f.members, vm)
	return vm, nil
}

func (f *fakeClusterState) VMs() []vmState { return f.members }

func (f *fakeClusterState) Close() error { return nil }

func (f *fakeClusterState) supportsClustering() bool { return f.clustering }

type fakeVMState struct {
	name      string
	destroyed int
}

func (f *fakeVMState) Name() string                                { return f.name }
func (f *fakeVMState) IP() net.IP                                  { return nil }
func (f *fakeVMState) SSH(context.Context, string) (string, error) { return "", nil }
func (f *fakeVMState) Stop(context.Context) error                  { return nil }
func (f *fakeVMState) Destroy(context.Context) error               { f.destroyed++; return nil }
func (f *fakeVMState) State() string                               { return "running" }

// TestClusterAddRejectsSecondMemberWithoutClustering pins the macOS <26 guard
// (ADR-0012): adding a second member on a non-clustering backend fails with
// ErrClustersUnsupported before any boot work. This mirrors the same guard in
// StartCluster/StartN.
func TestClusterAddRejectsSecondMemberWithoutClustering(t *testing.T) {
	c := &Cluster{st: &fakeClusterState{members: []vmState{&fakeVMState{name: "node-1"}}}}

	_, err := c.Add(context.Background(), "node-2")
	if !errors.Is(err, ErrClustersUnsupported) {
		t.Fatalf("Add(second member, no clustering) = %v, want ErrClustersUnsupported", err)
	}
}

// TestClusterAddAllowsFirstMemberWithoutClustering: a lone VM is always allowed,
// even on a backend that cannot interconnect VMs (a single VM is not a cluster).
func TestClusterAddAllowsFirstMemberWithoutClustering(t *testing.T) {
	c := &Cluster{st: &fakeClusterState{}}
	if _, err := c.Add(context.Background(), "solo"); err != nil {
		t.Fatalf("first Add without clustering = %v, want nil", err)
	}
}

// TestClusterAddAllowsSecondMemberWithClustering: with clustering, a second
// member is admitted and the public VM wraps the impl handle.
func TestClusterAddAllowsSecondMemberWithClustering(t *testing.T) {
	c := &Cluster{st: &fakeClusterState{clustering: true, members: []vmState{&fakeVMState{name: "node-1"}}}}
	vm, err := c.Add(context.Background(), "node-2")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if vm.Name() != "node-2" {
		t.Errorf("VM.Name() = %q, want node-2 (public type must delegate to its impl)", vm.Name())
	}
	if got := c.VMs(); len(got) != 2 {
		t.Errorf("VMs() len = %d, want 2", len(got))
	}
}
