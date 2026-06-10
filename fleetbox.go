// Package fleetbox provides Linux VMs as Go test fixtures.
//
// On macOS (Apple Silicon) fleetbox boots stock Linux cloud images via Apple
// Virtualization.framework; on Linux it boots them via cloud-hypervisor. Either
// way it configures the guest once with cloud-init and provides SSH access for
// testing, through the same backend-neutral Go API.
//
// The orchestration runs client-side on both platforms and drives a VM helper
// over a unix socket; the helper holds only the live VMs/network (ADR-0020). On
// macOS that helper is a separately distributed, ad-hoc-signed fleetbox-helper
// subprocess the library downloads at first use, so the importable package — and
// therefore the user's test binary — is pure Go and needs neither cgo nor codesign
// (ADR-0017). On Linux the single binary self-reexecs into the helper. The public
// API below is identical on both.
//
// Basic usage:
//
//	vm, err := fleetbox.Start(ctx, "myvm")
//	if err != nil {
//		log.Fatal(err)
//	}
//	defer vm.Stop(ctx)
//
//	out, err := vm.SSH(ctx, "uname -a")
package fleetbox

import (
	"context"
	"errors"
	"fmt"
	"net"

	"github.com/pilat/fleetbox/internal/opts"
)

// ErrClustersUnsupported is returned when a second cluster member is requested
// on a backend that cannot interconnect VMs — macOS older than 26, where VZ NAT
// isolates VMs from one another (ADR-0008, ADR-0012). A single VM still works.
var ErrClustersUnsupported = errors.New("fleetbox: clusters require macOS 26+")

// Options configures VM creation.
type Options = opts.Options

// Fixture is a read-only host directory packed into the guest at boot. At first
// creation HostPath is snapshotted into an ext4 image, attached to the VM as a
// read-only block device, and mounted by the stock guest at GuestPath via
// cloud-init. Fixtures are a property the VM is born with: the set is frozen at
// first creation and persisted, but the content is rebuilt from HostPath on every
// boot (so the guest sees the directory as it is at that boot, never live within
// a boot). Files arrive world-readable (0444), directories traversable (0555),
// owned by root; host permission and executable bits are not preserved. It works
// identically on macOS and Linux (ADR-0015).
type Fixture = opts.Fixture

// Option is a functional option for configuring a VM.
type Option = opts.Option

// WithImage sets the image to use (alias or URL).
func WithImage(img string) Option { return opts.WithImage(img) }

// WithCPUs sets the number of CPUs.
func WithCPUs(n int) Option { return opts.WithCPUs(n) }

// WithMemoryGB sets the memory in gigabytes.
func WithMemoryGB(n int) Option { return opts.WithMemoryGB(n) }

// WithDiskGB sets the disk size in gigabytes.
func WithDiskGB(n int) Option { return opts.WithDiskGB(n) }

// WithFixture packs the host directory hostDir into the guest at guestPath as a
// read-only fixture: at boot the directory is snapshotted into an ext4 image,
// attached as a read-only block device, and mounted by the stock guest at
// guestPath. Call it more than once to add several fixtures. guestPath must be
// absolute; hostDir must exist and be a directory, and is resolved to an absolute
// path. The fixture set is frozen when the VM is first created and ignored when
// passed to an already-existing VM (exactly as cpu/memory/disk options are), but
// the content is rebuilt from hostDir on every boot. Files arrive world-readable,
// owned by root. In a StartN or StartCluster every member receives the same
// fixtures (ADR-0015).
func WithFixture(hostDir, guestPath string) Option { return opts.WithFixture(hostDir, guestPath) }

// vmState is the per-platform implementation behind a VM: orchestrator.VM in
// process on linux, a client handle that talks to the downloaded helper on
// darwin. Both expose the same methods, so the public VM is a thin, identical
// delegate on either platform (ADR-0017, R2).
type vmState interface {
	Name() string
	IP() net.IP
	SSH(ctx context.Context, cmd string) (string, error)
	Stop(ctx context.Context) error
	Destroy(ctx context.Context) error
	State() string
}

// clusterState is the per-platform implementation behind a Cluster. Add returns a
// vmState (not *VM) to avoid an import cycle: the public Cluster.Add wraps it. The
// clustering-capability check is part of the seam so the gate below is identical
// on both platforms.
type clusterState interface {
	Add(ctx context.Context, name string) (vmState, error)
	VMs() []vmState
	Close() error
	supportsClustering() bool
}

// VM represents a running virtual machine.
type VM struct {
	st vmState
}

// Name returns the VM name.
func (v *VM) Name() string { return v.st.Name() }

// IP returns the VM's IP address.
func (v *VM) IP() net.IP { return v.st.IP() }

// SSH executes a command on the VM via SSH and returns the output.
func (v *VM) SSH(ctx context.Context, cmd string) (string, error) {
	return v.st.SSH(ctx, cmd) //nolint:wrapcheck // transparent delegate; the platform impl wraps
}

// Stop gracefully shuts down the VM. The disk is preserved.
func (v *VM) Stop(ctx context.Context) error {
	return v.st.Stop(ctx) //nolint:wrapcheck // transparent delegate; the platform impl wraps
}

// Destroy stops the VM and removes all its files.
func (v *VM) Destroy(ctx context.Context) error {
	return v.st.Destroy(ctx) //nolint:wrapcheck // transparent delegate; the platform impl wraps
}

// State returns the current VM state.
func (v *VM) State() string { return v.st.State() }

// Cluster is a set of VMs sharing one network, so every member reaches the
// others by IP — a vmnet SharedMode network on macOS, a Linux bridge on Linux
// (ADR-0008, ADR-0011). The shared network is a runtime object tied to the
// Cluster's lifetime — never persisted, so a Cluster is a runtime handle, not
// on-disk state. Members can be added after creation, which is what lets a CLI
// holder process grow a live cluster without recreating its network.
type Cluster struct {
	st clusterState
}

// Add boots an additional VM on the cluster's shared network and registers it as
// a member. The new VM reaches every existing member by IP. Adding a second
// member on a backend that cannot interconnect VMs (macOS < 26) is rejected with
// ErrClustersUnsupported before any boot work — the same guard StartCluster
// applies up front, here also covering a node re-joining a live cluster
// (ADR-0012). The first member is a lone VM and is always allowed.
func (c *Cluster) Add(ctx context.Context, name string) (*VM, error) {
	if len(c.st.VMs()) >= 1 && !c.st.supportsClustering() {
		return nil, ErrClustersUnsupported
	}
	vs, err := c.st.Add(ctx, name)
	if err != nil {
		return nil, err //nolint:wrapcheck // transparent delegate; the platform impl wraps
	}
	return &VM{st: vs}, nil
}

// VMs returns a snapshot of the cluster's current members in the order they were
// added.
func (c *Cluster) VMs() []*VM {
	states := c.st.VMs()
	out := make([]*VM, len(states))
	for i, s := range states {
		out[i] = &VM{st: s}
	}
	return out
}

// Close releases the cluster's resources. On Linux it tears down the shared
// bridge and egress rules; on macOS it stops every remaining member and waits for
// the helper to exit. Call it once every member has been stopped or destroyed. It
// is idempotent.
func (c *Cluster) Close() error {
	return c.st.Close() //nolint:wrapcheck // transparent delegate; the platform impl wraps
}

// StartN creates and boots N VMs with the given prefix (prefix-1, prefix-2, ...).
// All N VMs share ONE network, so they reach each other by IP — the cluster is
// interconnected (ADR-0008 macOS, ADR-0011 Linux). It is a thin wrapper over
// StartCluster with generated names.
func StartN(ctx context.Context, prefix string, n int, options ...Option) ([]*VM, error) {
	names := make([]string, n)
	for i := 1; i <= n; i++ {
		names[i-1] = fmt.Sprintf("%s-%d", prefix, i)
	}
	c, err := StartCluster(ctx, names, options...)
	if err != nil {
		return nil, err
	}
	return c.VMs(), nil
}

// StartCluster boots the named VMs on one shared network and returns the
// Cluster. On any member's failure it destroys the members already started and
// closes the cluster, then returns the error (all-or-nothing, like StartN).
func StartCluster(ctx context.Context, names []string, options ...Option) (*Cluster, error) {
	c, err := NewCluster(options...)
	if err != nil {
		return nil, err
	}

	// Reject a multi-member cluster up front on a backend that can't interconnect
	// VMs, before booting anything (ADR-0012). A single-member StartCluster is a
	// VM, not a cluster, so it is allowed.
	if len(names) > 1 && !c.st.supportsClustering() {
		_ = c.Close()
		return nil, ErrClustersUnsupported
	}

	for _, name := range names {
		if _, err := c.Add(ctx, name); err != nil {
			for _, vm := range c.VMs() {
				_ = vm.Destroy(ctx)
			}
			_ = c.Close()
			return nil, fmt.Errorf("start %s: %w", name, err)
		}
	}
	return c, nil
}

// NestedVirtSupported returns true if nested virtualization is available — what
// consumers that run KVM inside guests need. On macOS it requires Apple Silicon
// M3+ and macOS 15+; on Linux it requires /dev/kvm and the KVM nested parameter
// enabled. On macOS it is a pure-Go host heuristic (so deciding to skip a test
// never downloads the helper); the helper performs the authoritative VZ check at
// boot (ADR-0017, R7).
func NestedVirtSupported() bool {
	return nestedVirtSupported()
}

// Prune reclaims the inert host resources a fleetbox holder leaves behind if it
// dies without running its teardown — on Linux, orphaned bridges, taps, and
// firewall rules, plus restoring ip_forward once nothing of ours remains. It runs
// automatically on the CLI's down, so cleanup is never the user's job; this
// exported form is for library callers that want to sweep explicitly. On macOS it
// is a no-op: vmnet owns its own state and the in-process VMs of a dead helper die
// with it (ADR-0013, ADR-0017).
func Prune() error {
	return prune()
}
