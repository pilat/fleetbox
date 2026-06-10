//go:build (darwin && arm64) || linux

package fleetbox

import (
	"context"

	"github.com/pilat/fleetbox/internal/orchestrator"
)

// Start creates and boots a new VM with the given name. The orchestration runs
// client-side (image resolve, disk/seed/fixture build, store) and drives a VM
// helper over a unix socket; the helper is the downloaded, signed binary on macOS
// and a self-reexec of this binary on Linux (ADR-0020). If the VM already exists,
// the helper boots the existing VM. SSH dials the VM's IP directly.
func Start(ctx context.Context, name string, options ...Option) (*VM, error) {
	vm, err := orchestrator.Start(ctx, name, options...)
	if err != nil {
		return nil, err //nolint:wrapcheck // transparent delegate; orchestrator wraps
	}
	return &VM{st: vm}, nil
}

// NewCluster creates a cluster client. The helper is spawned on the first Add (it
// is launched on the first member's name) and then owns every member on one shared
// network. Shared client prep (store, SSH key, image) runs once and is reused for
// every Add.
func NewCluster(options ...Option) (*Cluster, error) {
	c, err := orchestrator.NewCluster(options...)
	if err != nil {
		return nil, err //nolint:wrapcheck // transparent delegate; orchestrator wraps
	}
	return &Cluster{st: clientCluster{c: c}}, nil
}

// clientCluster adapts the client-side orchestrator.Cluster to clusterState — the
// one cluster impl on both platforms now (ADR-0020 collapsed the macOS/Linux
// seam). orchestrator.VM already satisfies vmState via its exported methods, so
// members need no per-VM wrapper.
type clientCluster struct {
	c *orchestrator.Cluster
}

var (
	_ clusterState = clientCluster{}
	// orchestrator.VM satisfies vmState directly (its exported methods match); the
	// check lives here because orchestrator cannot import the root package.
	_ vmState = (*orchestrator.VM)(nil)
)

func (cc clientCluster) Add(ctx context.Context, name string) (vmState, error) {
	vm, err := cc.c.Add(ctx, name)
	if err != nil {
		return nil, err //nolint:wrapcheck // transparent delegate; orchestrator wraps
	}
	return vm, nil
}

func (cc clientCluster) VMs() []vmState {
	vms := cc.c.VMs()
	out := make([]vmState, len(vms))
	for i, v := range vms {
		out[i] = v
	}
	return out
}

func (cc clientCluster) Close() error {
	return cc.c.Close() //nolint:wrapcheck // transparent delegate; orchestrator wraps
}

// supportsClustering is a pure-Go host check (so the gate never spawns the helper,
// R7): macOS 26+ for vmnet SharedMode, always true on Linux.
func (cc clientCluster) supportsClustering() bool { return supportsClusteringHost() }
