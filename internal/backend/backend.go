// Package backend defines the interface for VM backends.
package backend

import (
	"context"
	"io"
)

// Config specifies VM configuration.
type Config struct {
	Name        string
	DiskPath    string
	SeedPath    string
	EFIPath     string
	MAC         string
	CPUs        int
	MemoryBytes uint64
	SerialOut   io.Writer
	Mounts      []Mount
}

// Mount is a host directory shared into the guest via virtiofs. HostPath is the
// absolute host directory; Tag is the virtiofs device tag the guest mounts by.
// The struct is backend-neutral by design — no hypervisor types appear here
// (ADR-0002). The guest path is not present because the host side only needs to
// attach the device; where it mounts inside the guest is the seed's concern.
type Mount struct {
	HostPath string
	Tag      string
}

// Network is an opaque handle to a backend network that VMs attach to.
// VMs sharing one Network can reach each other; pass the same Network to
// several Create calls to build an interconnected cluster. The concrete type
// lives in the backend implementation — no hypervisor types appear here
// (ADR-0002). See ADR-0008 for the vmnet SharedMode network behind it.
type Network interface {
	// Close releases the network. It is reserved for explicit whole-cluster
	// teardown: a Network shared by several running VMs must not be closed
	// while any of them is still alive. Backends may release the network via
	// GC once every VM referencing it is unreferenced, in which case Close is
	// a no-op.
	Close() error
}

// VM represents a running virtual machine.
type VM interface {
	// Start boots the VM.
	Start(ctx context.Context) error

	// Stop gracefully shuts down the VM (ACPI).
	Stop(ctx context.Context) error

	// State returns the current VM state.
	State() State

	// Wait blocks until the VM stops.
	Wait(ctx context.Context) error
}

// State represents the VM's current state.
type State int

const (
	StateUnknown State = iota
	StateStopped
	StateStarting
	StateRunning
	StatePausing
	StatePaused
	StateResuming
	StateStopping
	StateError
)

func (s State) String() string {
	switch s {
	case StateUnknown:
		return "unknown"
	case StateStopped:
		return "stopped"
	case StateStarting:
		return "starting"
	case StateRunning:
		return "running"
	case StatePausing:
		return "pausing"
	case StatePaused:
		return "paused"
	case StateResuming:
		return "resuming"
	case StateStopping:
		return "stopping"
	case StateError:
		return "error"
	default:
		return "unknown"
	}
}

// Backend creates VMs for a specific platform.
type Backend interface {
	// CreateNetwork creates a network for VMs to attach to. VMs created on the
	// same Network can reach one another; reuse one Network across Create calls
	// to build a cluster.
	CreateNetwork() (Network, error)

	// Create creates a new VM with the given configuration, attached to net.
	Create(cfg Config, net Network) (VM, error)

	// NestedVirtSupported returns true if nested virtualization is available.
	NestedVirtSupported() bool
}
