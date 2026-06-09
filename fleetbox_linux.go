//go:build linux

package fleetbox

import (
	"context"

	"github.com/pilat/fleetbox/internal/orchestrator"
)

// Start creates and boots a new VM with the given name. If the VM already
// exists, it boots the existing VM. On Linux the orchestration runs in-process
// (cloud-hypervisor, pure Go).
func Start(ctx context.Context, name string, options ...Option) (*VM, error) {
	vm, err := orchestrator.Start(ctx, name, options...)
	if err != nil {
		return nil, err //nolint:wrapcheck // transparent delegate; orchestrator wraps
	}
	return &VM{st: vm}, nil
}

// NewCluster creates a cluster's shared network but boots no VMs. Use Cluster.Add
// to bring members up on it. Shared setup (store, SSH key, image, backend) runs
// once here and is reused for every Add.
func NewCluster(options ...Option) (*Cluster, error) {
	c, err := orchestrator.NewCluster(options...)
	if err != nil {
		return nil, err //nolint:wrapcheck // transparent delegate; orchestrator wraps
	}
	return &Cluster{st: linuxCluster{c: c}}, nil
}

// linuxCluster adapts the in-process orchestrator.Cluster to clusterState. The
// only adaptation is wrapping Add/VMs to the vmState seam (orchestrator.VM
// already satisfies vmState via its exported methods, so no per-VM wrapper is
// needed).
type linuxCluster struct {
	c *orchestrator.Cluster
}

var (
	_ clusterState = linuxCluster{}
	// orchestrator.VM satisfies vmState directly (its exported methods match); the
	// check lives here because orchestrator cannot import the root package.
	_ vmState = (*orchestrator.VM)(nil)
)

func (l linuxCluster) Add(ctx context.Context, name string) (vmState, error) {
	vm, err := l.c.Add(ctx, name)
	if err != nil {
		return nil, err //nolint:wrapcheck // transparent delegate; orchestrator wraps
	}
	return vm, nil
}

func (l linuxCluster) VMs() []vmState {
	vms := l.c.VMs()
	out := make([]vmState, len(vms))
	for i, v := range vms {
		out[i] = v
	}
	return out
}

func (l linuxCluster) Close() error {
	return l.c.Close() //nolint:wrapcheck // transparent delegate; orchestrator wraps
}

func (l linuxCluster) supportsClustering() bool { return l.c.SupportsClustering() }

func nestedVirtSupported() bool { return orchestrator.NestedVirtSupported() }

func prune() error {
	return orchestrator.Prune() //nolint:wrapcheck // transparent delegate; orchestrator wraps
}
