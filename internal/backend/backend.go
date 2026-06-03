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
	// Create creates a new VM with the given configuration.
	Create(cfg Config) (VM, error)

	// NestedVirtSupported returns true if nested virtualization is available.
	NestedVirtSupported() bool
}
