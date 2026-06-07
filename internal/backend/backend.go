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
	// AssignedIP is the static IPv4 address the VM is configured with via its
	// seed (Linux/cloud-hypervisor backend). Backends that discover the IP from
	// DHCP (vz) leave it empty and find the address themselves in WaitForIP.
	AssignedIP string
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

	// Subnet returns the network's IPv4 CIDR (e.g. "192.168.5.0/24") when the
	// backend assigns static addresses from a known range (Linux). It returns
	// the empty string for backends whose guests obtain addresses via DHCP
	// (vz), which is the signal the orchestrator uses to skip static IP
	// allocation and emit no cloud-init network-config.
	Subnet() string
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

	// WaitForIP blocks until the VM's IPv4 address is known and TCP port 22 on
	// it is reachable, then returns the address. It honors ctx cancellation and
	// any deadline on ctx. vz discovers the address from dhcpd_leases by
	// hostname; cloud-hypervisor returns the statically assigned address after
	// the reachability probe.
	WaitForIP(ctx context.Context) (string, error)
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

	// SupportsClustering reports whether this backend can run more than one VM
	// on a single shared network so the members reach each other. It is false
	// only on macOS releases older than 26, where VZ NAT isolates VMs from one
	// another; the public layer checks it before booting a second member and
	// rejects the request with a clear error.
	SupportsClustering() bool

	// Reconcile reclaims host resources left by a holder that crashed before it
	// could tear its network down — orphaned bridges, taps, and firewall rules.
	// Backends that own no such host state (vz: vmnet manages its own) return
	// nil. It backs `fleetbox prune`; backends may also run it implicitly on
	// network create so orphans self-heal (ADR-0013).
	Reconcile() error
}
