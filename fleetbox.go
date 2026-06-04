//go:build darwin && arm64

// Package fleetbox provides Linux VMs as Go test fixtures on macOS (Apple Silicon).
//
// Fleetbox boots stock Linux cloud images via Apple Virtualization.framework,
// configures them with cloud-init, and provides SSH access for testing.
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
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/pilat/fleetbox/internal/backend"
	"github.com/pilat/fleetbox/internal/dhcp"
	"github.com/pilat/fleetbox/internal/image"
	"github.com/pilat/fleetbox/internal/seed"
	"github.com/pilat/fleetbox/internal/sshkey"
	"github.com/pilat/fleetbox/internal/store"
)

const (
	defaultImage  = "debian-12"
	defaultCPUs   = 2
	defaultMemGB  = 4
	defaultDiskGB = 20
	defaultUser   = "fleetbox"
)

// Image aliases for common distributions.
const (
	Debian12   = "debian-12"
	Ubuntu2404 = "ubuntu-24.04"
)

// VM represents a running virtual machine.
type VM struct {
	name      string
	ip        net.IP
	store     *store.Store
	sshMgr    *sshkey.Manager
	backend   backend.VM
	network   backend.Network
	config    *store.VM
	serialLog *os.File
}

// startDeps holds the once-per-call handles shared by every VM in a Start or
// StartN. They are resolved a single time (resolveStartDeps) and reused for
// each VM, so a StartN cluster does not redo store/key/image setup per node.
type startDeps struct {
	options   *Options
	store     *store.Store
	sshMgr    *sshkey.Manager
	pubKey    string
	imagePath string
	backend   backend.Backend
}

// Cluster is a set of VMs sharing one vmnet SharedMode network, so every member
// reaches the others by IP (ADR-0008). The shared network is an in-process
// object tied to the Cluster's lifetime — it is never persisted, so a Cluster is
// a runtime handle, not on-disk state. Members can be added after creation,
// which is what lets a CLI holder process grow a live cluster without recreating
// its network.
type Cluster struct {
	mu      sync.Mutex
	deps    *startDeps
	network backend.Network
	vms     []*VM
}

// Options configures VM creation.
type Options struct {
	Image  string
	CPUs   int
	MemGB  int
	DiskGB int
	Mounts []Mount
}

// Mount is a host↔guest shared directory. The host directory appears live and
// read-write inside the guest at GuestPath; edits on either side are visible on
// the other immediately (Apple Virtualization.framework virtiofs). Mounts are a
// property the VM is born with: they are set at first creation, persisted, and
// re-applied on every boot. Changing a VM's mounts means recreating it (rm + up).
type Mount struct {
	// HostPath is the directory on the macOS host to share. It must exist at
	// creation time and is resolved to an absolute path before persistence.
	HostPath string
	// GuestPath is the absolute path inside the guest where HostPath appears.
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

// WithMount shares the host directory hostPath into the guest at guestPath as a
// live, read-write virtiofs mount. Call it more than once to add several mounts.
// guestPath must be absolute. The mount is applied when the VM is first created
// and re-applied on every subsequent boot; it is ignored when passed to an
// already-existing VM, exactly as cpu/memory/disk options are. In a StartN or
// StartCluster every member receives the same mounts.
func WithMount(hostPath, guestPath string) Option {
	return func(o *Options) {
		o.Mounts = append(o.Mounts, Mount{HostPath: hostPath, GuestPath: guestPath})
	}
}

// Start creates and boots a new VM with the given name.
// If the VM already exists, it boots the existing VM.
func Start(ctx context.Context, name string, opts ...Option) (*VM, error) {
	deps, err := resolveStartDeps(opts...)
	if err != nil {
		return nil, err
	}

	// One vmnet SharedMode network for this VM. A single Start yields a
	// one-member network; the macOS-26 requirement surfaces here, propagated
	// from the backend (ADR-0008).
	nw, err := deps.backend.CreateNetwork()
	if err != nil {
		return nil, fmt.Errorf("create network: %w", err)
	}

	return startOnNetwork(ctx, name, nw, deps)
}

// resolveStartDeps performs the once-per-call setup shared by every VM in a
// Start or StartN: it applies options over defaults, opens the store, ensures
// the per-installation SSH key, ensures the image is cached, and constructs the
// backend. It does not touch any individual VM.
func resolveStartDeps(opts ...Option) (*startDeps, error) {
	options := &Options{
		Image:  defaultImage,
		CPUs:   defaultCPUs,
		MemGB:  defaultMemGB,
		DiskGB: defaultDiskGB,
	}
	for _, opt := range opts {
		opt(options)
	}

	st, err := store.New()
	if err != nil {
		return nil, fmt.Errorf("init store: %w", err)
	}

	sshMgr := sshkey.NewManager(st.SSHKeyPath())
	pubKey, err := sshMgr.EnsureKey()
	if err != nil {
		return nil, fmt.Errorf("ensure ssh key: %w", err)
	}

	imagePath, err := image.Ensure(st.ImagesDir(), options.Image)
	if err != nil {
		return nil, fmt.Errorf("ensure image: %w", err)
	}

	return &startDeps{
		options:   options,
		store:     st,
		sshMgr:    sshMgr,
		pubKey:    pubKey,
		imagePath: imagePath,
		backend:   newBackend(),
	}, nil
}

// startOnNetwork performs the per-VM work — store config load/create, disk copy,
// seed ISO, backend create+boot, IP and SSH readiness — attaching the VM to the
// already-created network nw. Shared setup is done once by resolveStartDeps and
// passed in via deps. The returned VM retains nw so the network is not released
// by GC while the VM lives (ADR-0008, R3).
func startOnNetwork(ctx context.Context, name string, nw backend.Network, deps *startDeps) (*VM, error) {
	st := deps.store
	options := deps.options

	var vmConfig *store.VM
	if st.Exists(name) {
		// Load existing VM config
		loaded, err := st.Load(name)
		if err != nil {
			return nil, fmt.Errorf("load vm config: %w", err)
		}
		vmConfig = loaded
	} else {
		// Mounts are frozen at birth: validated, absolutized, and tagged once
		// here, then persisted and re-applied verbatim on every later boot
		// (ADR-0010). Validation is a create-time concern only — see the loaded
		// branch above, which never re-checks the host dir.
		mounts, err := toStoreMounts(options.Mounts)
		if err != nil {
			return nil, fmt.Errorf("prepare mounts: %w", err)
		}

		// Create new VM
		vmConfig = &store.VM{
			Name:      name,
			MAC:       backend.GenerateMAC(name),
			CPUs:      options.CPUs,
			MemoryMB:  options.MemGB * 1024,
			DiskMB:    options.DiskGB * 1024,
			Image:     options.Image,
			CreatedAt: time.Now(),
			Mounts:    mounts,
		}
		if err := st.Create(vmConfig); err != nil {
			return nil, fmt.Errorf("create vm store: %w", err)
		}

		// Copy disk image
		diskPath := st.DiskPath(name)
		diskSize := int64(options.DiskGB) * 1024 * 1024 * 1024
		if err := image.CopyDisk(deps.imagePath, diskPath, diskSize); err != nil {
			return nil, fmt.Errorf("copy disk: %w", err)
		}

		// Create seed ISO. With mounts, cloud-init also writes the fstab entries
		// (so they survive reboots without a re-run) and pins the guest user's
		// uid to the host uid so virtiofs identity pass-through lines up.
		seedPath := st.SeedPath(name)
		seedCfg := seed.Config{
			Hostname: name,
			User:     defaultUser,
			SSHKey:   deps.pubKey,
		}
		if len(mounts) > 0 {
			seedCfg.Mounts = toSeedMounts(mounts)
			seedCfg.UID = os.Getuid()
		}
		if err := seed.Create(seedPath, seedCfg); err != nil {
			return nil, fmt.Errorf("create seed: %w", err)
		}
	}

	// Create serial log file (owned by VM, closed in Stop/Destroy)
	serialLog, err := os.OpenFile(st.SerialLogPath(name), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open serial log: %w", err)
	}

	// Create backend VM on the shared network
	backendCfg := backend.Config{
		Name:        name,
		DiskPath:    st.DiskPath(name),
		SeedPath:    st.SeedPath(name),
		EFIPath:     st.EFIPath(name),
		MAC:         vmConfig.MAC,
		CPUs:        vmConfig.CPUs,
		MemoryBytes: uint64(vmConfig.MemoryMB) * 1024 * 1024,
		SerialOut:   serialLog,
		// Re-attach the virtiofs devices from the persisted config, so both a
		// freshly created and a rebooted VM get identical mounts (ADR-0010).
		Mounts: toBackendMounts(vmConfig.Mounts),
	}
	backendVM, err := deps.backend.Create(backendCfg, nw)
	if err != nil {
		_ = serialLog.Close()
		return nil, fmt.Errorf("create backend vm: %w", err)
	}

	// Boot the VM
	if err := backendVM.Start(ctx); err != nil {
		_ = serialLog.Close()
		return nil, fmt.Errorf("start vm: %w", err)
	}

	vm := &VM{
		name:      name,
		store:     st,
		sshMgr:    deps.sshMgr,
		backend:   backendVM,
		network:   nw,
		config:    vmConfig,
		serialLog: serialLog,
	}

	// Wait for IP
	ip, err := vm.waitForIP(ctx, 2*time.Minute)
	if err != nil {
		_ = backendVM.Stop(ctx)
		_ = serialLog.Close()
		return nil, fmt.Errorf("wait for ip: %w", err)
	}
	vm.ip = ip

	// Wait for SSH
	if err := vm.waitForSSH(ctx, 2*time.Minute); err != nil {
		_ = backendVM.Stop(ctx)
		_ = serialLog.Close()
		return nil, fmt.Errorf("wait for ssh: %w", err)
	}

	return vm, nil
}

// toStoreMounts validates the public mounts and converts them to persisted store
// mounts, assigning each a stable fbm<i> tag where i is its position in the list
// and resolving host paths to absolute. The tag↔index invariant (ADR-0010)
// depends on order being preserved, so the list is neither reordered nor deduped.
// It returns an error for a non-absolute guest path or a missing host directory —
// the only place create-time mount validation happens.
func toStoreMounts(mounts []Mount) ([]store.Mount, error) {
	if len(mounts) == 0 {
		return nil, nil
	}
	out := make([]store.Mount, 0, len(mounts))
	for i, m := range mounts {
		if !filepath.IsAbs(m.GuestPath) {
			return nil, fmt.Errorf("mount guest path %q must be absolute", m.GuestPath)
		}
		host, err := filepath.Abs(m.HostPath)
		if err != nil {
			return nil, fmt.Errorf("resolve mount host path %q: %w", m.HostPath, err)
		}
		if _, err := os.Stat(host); err != nil {
			return nil, fmt.Errorf("mount host path %q: %w", host, err)
		}
		out = append(out, store.Mount{
			HostPath:  host,
			GuestPath: m.GuestPath,
			Tag:       fmt.Sprintf("fbm%d", i),
		})
	}
	return out, nil
}

// toBackendMounts projects persisted mounts onto the backend's host-side view
// (host path + tag); the guest path is irrelevant to attaching the device.
func toBackendMounts(mounts []store.Mount) []backend.Mount {
	if len(mounts) == 0 {
		return nil
	}
	out := make([]backend.Mount, len(mounts))
	for i, m := range mounts {
		out[i] = backend.Mount{HostPath: m.HostPath, Tag: m.Tag}
	}
	return out
}

// toSeedMounts projects persisted mounts onto the guest-side view cloud-init
// needs (tag + guest path) to write the fstab entries.
func toSeedMounts(mounts []store.Mount) []seed.Mount {
	if len(mounts) == 0 {
		return nil
	}
	out := make([]seed.Mount, len(mounts))
	for i, m := range mounts {
		out[i] = seed.Mount{Tag: m.Tag, GuestPath: m.GuestPath}
	}
	return out
}

// Name returns the VM name.
func (v *VM) Name() string {
	return v.name
}

// IP returns the VM's IP address.
func (v *VM) IP() net.IP {
	return v.ip
}

// SSH executes a command on the VM via SSH and returns the output.
func (v *VM) SSH(ctx context.Context, cmd string) (string, error) {
	client, err := v.sshMgr.DialIP(v.ip, defaultUser, 30*time.Second)
	if err != nil {
		return "", fmt.Errorf("dial: %w", err)
	}
	defer func() { _ = client.Close() }()

	out, err := client.Run(cmd)
	if err != nil {
		return out, fmt.Errorf("run %q: %w", cmd, err)
	}

	return out, nil
}

// Stop gracefully shuts down the VM. The disk is preserved.
func (v *VM) Stop(ctx context.Context) error {
	err := v.backend.Stop(ctx)
	if v.serialLog != nil {
		_ = v.serialLog.Close()
		v.serialLog = nil
	}
	if err != nil {
		return fmt.Errorf("stop vm: %w", err)
	}

	return nil
}

// Destroy stops the VM and removes all its files.
func (v *VM) Destroy(ctx context.Context) error {
	_ = v.backend.Stop(ctx)

	if v.serialLog != nil {
		_ = v.serialLog.Close()
		v.serialLog = nil
	}

	if err := v.store.Delete(v.name); err != nil {
		return fmt.Errorf("delete vm files: %w", err)
	}

	return nil
}

// State returns the current VM state.
func (v *VM) State() string {
	return v.backend.State().String()
}

func (v *VM) waitForIP(ctx context.Context, timeout time.Duration) (net.IP, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		ipStr, err := dhcp.LookupByHostname(v.name)
		if err == nil && ipStr != "" {
			if ip := net.ParseIP(ipStr); ip != nil {
				if isReachable(ip) {
					return ip, nil
				}
			}
		}

		time.Sleep(time.Second)
	}
	return nil, errors.New("timeout waiting for IP")
}

func isReachable(ip net.IP) bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(ip.String(), "22"), 2*time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func (v *VM) waitForSSH(ctx context.Context, timeout time.Duration) error {
	addr := net.JoinHostPort(v.ip.String(), "22")
	if err := v.sshMgr.WaitForSSH(addr, defaultUser, timeout); err != nil {
		return fmt.Errorf("wait for ssh: %w", err)
	}

	return nil
}

// StartN creates and boots N VMs with the given prefix (prefix-1, prefix-2, ...).
// All N VMs share ONE vmnet SharedMode network, so they reach each other by IP —
// the cluster is interconnected (ADR-0008). It is a thin wrapper over
// StartCluster with generated names.
func StartN(ctx context.Context, prefix string, n int, opts ...Option) ([]*VM, error) {
	names := make([]string, n)
	for i := 1; i <= n; i++ {
		names[i-1] = fmt.Sprintf("%s-%d", prefix, i)
	}
	c, err := StartCluster(ctx, names, opts...)
	if err != nil {
		return nil, err
	}
	return c.VMs(), nil
}

// NewCluster creates a cluster's shared vmnet network but boots no VMs. Use Add
// to bring members up on it. Shared setup (store, SSH key, image, backend) runs
// once here and is reused for every Add. The macOS-26 requirement surfaces here,
// propagated from the backend (ADR-0008).
func NewCluster(opts ...Option) (*Cluster, error) {
	deps, err := resolveStartDeps(opts...)
	if err != nil {
		return nil, err
	}

	nw, err := deps.backend.CreateNetwork()
	if err != nil {
		return nil, fmt.Errorf("create network: %w", err)
	}

	return &Cluster{deps: deps, network: nw}, nil
}

// StartCluster boots the named VMs on one shared network and returns the
// Cluster. On any member's failure it destroys the members already started and
// returns the error (all-or-nothing, like StartN). The network is not released
// explicitly — GC reaps it once every referencing VM is unreferenced (R3).
func StartCluster(ctx context.Context, names []string, opts ...Option) (*Cluster, error) {
	c, err := NewCluster(opts...)
	if err != nil {
		return nil, err
	}

	for _, name := range names {
		if _, err := c.Add(ctx, name); err != nil {
			for _, vm := range c.VMs() {
				_ = vm.Destroy(ctx)
			}
			return nil, fmt.Errorf("start %s: %w", name, err)
		}
	}
	return c, nil
}

// Add boots an additional VM on the cluster's shared network and registers it as
// a member. The new VM reaches every existing member by IP. Adding to a live
// cluster is what lets a CLI holder re-join a previously stopped node without
// recreating the network.
func (c *Cluster) Add(ctx context.Context, name string) (*VM, error) {
	vm, err := startOnNetwork(ctx, name, c.network, c.deps)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	c.vms = append(c.vms, vm)
	c.mu.Unlock()
	return vm, nil
}

// VMs returns a snapshot of the cluster's current members in the order they were
// added.
func (c *Cluster) VMs() []*VM {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.vms)
}

// NestedVirtSupported returns true if nested virtualization is available.
// Requires M3+ and macOS 15+.
func NestedVirtSupported() bool {
	return nestedVirtSupported()
}
