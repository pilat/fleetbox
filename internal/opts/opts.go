// Package opts holds the backend-neutral VM creation options.
//
// It is a pure-data leaf: it knows nothing about backends, the orchestrator, or
// the store, so the pure-Go client packages can depend on it without dragging a
// hypervisor into their import graph. The root fleetbox package re-exposes these
// types verbatim (type aliases plus thin With* wrappers) so its public signatures
// are unchanged (ADR-0017, R1). Since ADR-0020 the orchestrator consumes Options
// directly client-side; they no longer cross a process boundary as JSON.
package opts

// Options configures VM creation.
type Options struct {
	// Image is the image alias (e.g. "debian-12") or a direct raw/qcow2 image URL.
	Image string
	// CPUs is the number of virtual CPUs.
	CPUs int
	// MemGB is the memory size, in gigabytes.
	MemGB int
	// DiskGB is the disk size, in gigabytes.
	DiskGB int
	// Fixtures are read-only host directories packed into the guest at boot (see Fixture).
	Fixtures []Fixture
}

// Fixture is a read-only host directory packed into the guest at boot. At first
// creation HostPath is snapshotted into an ext4 image, attached to the VM as a
// read-only block device, and mounted by the stock guest at GuestPath via
// cloud-init. Fixtures are a property the VM is born with: the set is frozen at
// first creation and persisted, but the content is rebuilt from HostPath on every
// boot (so the guest sees the directory as it is at that boot, never live within
// a boot). Files arrive world-readable (0444), directories traversable (0555),
// owned by root; host permission and executable bits are not preserved. It works
// identically on macOS and Linux (ADR-0015).
type Fixture struct {
	// HostPath is the host directory to pack. It must exist and be a directory at
	// creation time, and is resolved to an absolute path before persistence.
	HostPath string
	// GuestPath is the absolute path inside the guest where the fixture is mounted.
	GuestPath string
}

// Option is a functional option for configuring a VM.
type Option func(*Options)

// WithImage sets the image to use (alias or URL).
func WithImage(img string) Option {
	return func(o *Options) { o.Image = img }
}

// WithCPUs sets the number of CPUs.
func WithCPUs(n int) Option {
	return func(o *Options) { o.CPUs = n }
}

// WithMemoryGB sets the memory in gigabytes.
func WithMemoryGB(n int) Option {
	return func(o *Options) { o.MemGB = n }
}

// WithDiskGB sets the disk size in gigabytes.
func WithDiskGB(n int) Option {
	return func(o *Options) { o.DiskGB = n }
}

// WithFixture packs the host directory hostDir into the guest at guestPath as a
// read-only fixture. Call it more than once to add several fixtures. See Fixture
// and the root fleetbox.WithFixture wrapper for the full contract (ADR-0015).
func WithFixture(hostDir, guestPath string) Option {
	return func(o *Options) {
		o.Fixtures = append(o.Fixtures, Fixture{HostPath: hostDir, GuestPath: guestPath})
	}
}
