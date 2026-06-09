// Package opts holds the backend-neutral VM creation options and their
// serialization across the helper-process boundary.
//
// It is a pure-data leaf: it knows nothing about backends, the orchestrator, or
// the store, so both the client half (internal/control) and the server half
// (internal/orchestrator) can depend on it without dragging a hypervisor into
// their import graph. The root fleetbox package re-exposes these types verbatim
// (type aliases plus thin With* wrappers) so its public signatures are unchanged
// (ADR-0017, R1).
package opts

import (
	"encoding/json"
	"fmt"
)

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

// optionsData is the JSON projection of Options that crosses the helper process
// boundary. Option funcs cannot be serialized, so Encode applies them to an
// Options and serializes the resulting values; Decode reconstructs the option
// funcs from the non-zero values.
type optionsData struct {
	Image    string        `json:"image,omitempty"`
	CPUs     int           `json:"cpus,omitempty"`
	MemGB    int           `json:"mem,omitempty"`
	DiskGB   int           `json:"disk,omitempty"`
	Fixtures []fixtureData `json:"fixtures,omitempty"`
}

// fixtureData carries a fixture across the helper process boundary. Only the host
// and guest paths cross — the host path is already absolute (resolved by the
// caller); labels are not serialized because they are assigned at first-create in
// the orchestrator (ADR-0015).
type fixtureData struct {
	HostPath  string `json:"host_path"`
	GuestPath string `json:"guest_path"`
}

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

// Encode serializes a set of options to the JSON string the helper receives in
// FLEETBOX_OPTS. Option funcs cannot be serialized, so they are applied to an
// Options struct and the resulting values are serialized instead.
func Encode(options []Option) (string, error) {
	var o Options
	for _, opt := range options {
		opt(&o)
	}

	data := optionsData{
		Image:  o.Image,
		CPUs:   o.CPUs,
		MemGB:  o.MemGB,
		DiskGB: o.DiskGB,
	}
	for _, f := range o.Fixtures {
		data.Fixtures = append(data.Fixtures, fixtureData(f))
	}

	b, err := json.Marshal(data)
	if err != nil {
		return "", fmt.Errorf("marshal options: %w", err)
	}

	return string(b), nil
}

// Decode reconstructs option funcs from a FLEETBOX_OPTS JSON string. An empty
// string yields no options.
func Decode(s string) ([]Option, error) {
	if s == "" {
		return nil, nil
	}
	var data optionsData
	if err := json.Unmarshal([]byte(s), &data); err != nil {
		return nil, fmt.Errorf("unmarshal options: %w", err)
	}

	var options []Option
	if data.Image != "" {
		options = append(options, WithImage(data.Image))
	}
	if data.CPUs > 0 {
		options = append(options, WithCPUs(data.CPUs))
	}
	if data.MemGB > 0 {
		options = append(options, WithMemoryGB(data.MemGB))
	}
	if data.DiskGB > 0 {
		options = append(options, WithDiskGB(data.DiskGB))
	}
	for _, f := range data.Fixtures {
		options = append(options, WithFixture(f.HostPath, f.GuestPath))
	}
	return options, nil
}
