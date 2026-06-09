// Package orchestrator owns the VM lifecycle: it resolves the per-call
// dependencies (store, SSH key, image, backend), creates the backend network,
// and boots, waits for, and tears down VMs. It is the only place the
// internal/backend implementations are wired in, so it is the one package whose
// import graph carries a hypervisor (vz on darwin, cloud-hypervisor on linux).
//
// On linux the root fleetbox package imports the orchestrator and runs it
// in-process. On darwin the root package does NOT import it — the orchestrator
// lives only inside the downloaded, signed fleetbox-helper, and the root package
// is a thin pure-Go client that drives the helper over a socket (ADR-0017). The
// clustering-capability gate and the public ErrClustersUnsupported sentinel live
// in the root package, not here: a caller is expected to have checked
// SupportsClustering before adding a second member, so Cluster.Add boots
// unconditionally.
package orchestrator

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/pilat/fleetbox/internal/backend"
	"github.com/pilat/fleetbox/internal/fixture"
	"github.com/pilat/fleetbox/internal/image"
	"github.com/pilat/fleetbox/internal/opts"
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

// VM represents a running virtual machine owned by the orchestrator.
type VM struct {
	name      string
	ip        net.IP
	store     *store.Store
	sshMgr    *sshkey.Manager
	backend   backend.VM
	network   backend.Network
	config    *store.VM
	serialLog *os.File
	// ownsNetwork is true only for a VM created by a bare Start (its network has
	// a single member), so Destroy may release the network. Cluster members share
	// a network and leave it false — the Cluster owns its teardown (Cluster.Close).
	ownsNetwork bool
}

// startDeps holds the once-per-call handles shared by every VM in a Start or
// StartN. They are resolved a single time (resolveStartDeps) and reused for
// each VM, so a StartN cluster does not redo store/key/image setup per node.
type startDeps struct {
	options   *opts.Options
	store     *store.Store
	sshMgr    *sshkey.Manager
	pubKey    string
	imagePath string
	backend   backend.Backend
}

// Cluster is a set of VMs sharing one network, so every member reaches the
// others by IP — a vmnet SharedMode network on macOS, a Linux bridge on Linux
// (ADR-0008, ADR-0011). The shared network is a runtime object tied to the
// Cluster's lifetime — never persisted, so a Cluster is a runtime handle, not
// on-disk state. Members can be added after creation, which is what lets a CLI
// holder process grow a live cluster without recreating its network.
type Cluster struct {
	mu      sync.Mutex
	deps    *startDeps
	network backend.Network
	vms     []*VM
}

// ipAssignment is a static address allocated from a backend network's subnet:
// the host IP plus the gateway and netmask the guest needs to configure its NIC.
type ipAssignment struct {
	ip      string
	gateway string
	netmask string
}

// Start creates and boots a new VM with the given name on its own one-member
// network. If the VM already exists, it boots the existing VM.
func Start(ctx context.Context, name string, optFns ...opts.Option) (*VM, error) {
	deps, err := resolveStartDeps(optFns...)
	if err != nil {
		return nil, err
	}

	// One network for this VM. A single Start yields a one-member network; the
	// macOS-26 requirement surfaces here, propagated from the backend (ADR-0008).
	nw, err := deps.backend.CreateNetwork()
	if err != nil {
		return nil, fmt.Errorf("create network: %w", err)
	}

	vm, err := startOnNetwork(ctx, name, nw, deps)
	if err != nil {
		_ = nw.Close() // sole owner failed to boot: release the network it made
		return nil, err
	}
	// A bare Start owns its one-member network, so Destroy may release it.
	vm.ownsNetwork = true
	return vm, nil
}

// resolveStartDeps performs the once-per-call setup shared by every VM in a
// Start or StartN: it applies options over defaults, opens the store, ensures
// the per-installation SSH key, ensures the image is cached, and constructs the
// backend. It does not touch any individual VM.
func resolveStartDeps(optFns ...opts.Option) (*startDeps, error) {
	// Fail fast with an actionable message if the host can't run a VM, before the
	// (potentially multi-GB) image download (ADR-0017). No-op on macOS.
	if err := preflight(); err != nil {
		return nil, err
	}

	options := &opts.Options{
		Image:  defaultImage,
		CPUs:   defaultCPUs,
		MemGB:  defaultMemGB,
		DiskGB: defaultDiskGB,
	}
	for _, opt := range optFns {
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

	b, err := newBackend()
	if err != nil {
		return nil, err
	}

	return &startDeps{
		options:   options,
		store:     st,
		sshMgr:    sshMgr,
		pubKey:    pubKey,
		imagePath: imagePath,
		backend:   b,
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
		// Fixtures are frozen at birth: validated, absolutized, and labeled once
		// here, then persisted and re-applied verbatim on every later boot
		// (ADR-0015). Validation is a create-time concern only — see the loaded
		// branch above, which never re-checks the host dirs.
		fixtures, err := toStoreFixtures(options.Fixtures)
		if err != nil {
			return nil, fmt.Errorf("prepare fixtures: %w", err)
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
			Fixtures:  fixtures,
		}

		// On a backend that assigns static addresses (Linux), allocate this VM's
		// IP from the network's subnet now, before the seed is written, and
		// persist it so reboots and re-joining cluster members keep it. A
		// DHCP backend (vz) reports an empty subnet and skips this entirely.
		var netCfg *seed.NetworkConfig
		if subnet := nw.Subnet(); subnet != "" {
			assignment, err := allocateIP(st, subnet)
			if err != nil {
				return nil, fmt.Errorf("allocate ip: %w", err)
			}
			vmConfig.IP = assignment.ip
			netCfg = &seed.NetworkConfig{
				MAC:     vmConfig.MAC,
				IP:      assignment.ip,
				Gateway: assignment.gateway,
				Netmask: assignment.netmask,
			}
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

		// Create seed ISO. The fixture mount lines (by LABEL) are written here, at
		// first create, and persist via the guest's /etc/fstab — so a reboot
		// re-mounts them without re-running cloud-init, the same way the labels
		// stay stable while the images are rebuilt each boot (ADR-0015). On Linux
		// it also carries the static network-config allocated above; on macOS the
		// guest stays on DHCP and that config is omitted.
		seedPath := st.SeedPath(name)
		seedCfg := seed.Config{
			Hostname: name,
			User:     defaultUser,
			SSHKey:   deps.pubKey,
			Network:  netCfg,
			Fixtures: toSeedFixtures(vmConfig.Fixtures),
		}
		if err := seed.Create(seedPath, seedCfg); err != nil {
			return nil, fmt.Errorf("create seed: %w", err)
		}
	}

	// Build (or rebuild) each fixture's read-only ext4 image from its persisted
	// host directory. The fixture SET is frozen at birth (it lives in
	// vmConfig.Fixtures, whether just-created or loaded), but the CONTENT is
	// snapshotted fresh on every boot — no cache (ADR-0015). The member dir exists
	// on both paths, so the images are writable here; the labels are persisted, so
	// they keep matching the seed's LABEL= mount lines across reboots.
	fixturePaths := make([]string, len(vmConfig.Fixtures))
	for i, f := range vmConfig.Fixtures {
		imgPath := st.FixturePath(name, i)
		if err := fixture.BuildImage(imgPath, f.HostPath, f.Label); err != nil {
			return nil, fmt.Errorf("build fixture image %q: %w", f.HostPath, err)
		}
		fixturePaths[i] = imgPath
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
		// Re-attach the read-only fixture images built above, so both a freshly
		// created and a rebooted VM get identical fixtures (ADR-0015).
		FixturePaths: fixturePaths,
		// The persisted static IP (empty on the DHCP/vz path); the Linux backend
		// returns it from WaitForIP after a reachability probe.
		AssignedIP: vmConfig.IP,
	}
	backendVM, err := deps.backend.Create(backendCfg, nw)
	if err != nil {
		_ = serialLog.Close()
		return nil, fmt.Errorf("create backend vm: %w", err)
	}

	// Boot the VM
	if err := backendVM.Start(ctx); err != nil {
		_ = backendVM.Stop(ctx) // release any tap/process a partial boot left behind
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

	// Wait for IP. Discovery is the backend's job (vz parses dhcpd_leases by
	// hostname; cloud-hypervisor returns the static IP it assigned) — the only
	// platform coupling that used to live here. Scope the 2-minute budget to the
	// wait so a slow boot does not consume the caller's whole context.
	ipCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	ipStr, err := backendVM.WaitForIP(ipCtx)
	cancel()
	if err != nil {
		_ = backendVM.Stop(ctx)
		_ = serialLog.Close()
		return nil, fmt.Errorf("wait for ip: %w", err)
	}
	ip := net.ParseIP(ipStr)
	if ip == nil {
		_ = backendVM.Stop(ctx)
		_ = serialLog.Close()
		return nil, fmt.Errorf("backend returned invalid IP %q", ipStr)
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

// toStoreFixtures validates the public fixtures and converts them to persisted
// store fixtures, assigning each a stable FBFIX<i> label (i = position in the
// list) and resolving host paths to absolute. The label↔index invariant
// (ADR-0015) depends on order, so the list is neither reordered nor deduped. It
// errors on a non-absolute guest path, a host path that is missing or not a
// directory, or two fixtures sharing one guest path (cloud-init double-mounting
// one path is undefined) — the only place create-time fixture validation happens.
func toStoreFixtures(fixtures []opts.Fixture) ([]store.Fixture, error) {
	if len(fixtures) == 0 {
		return nil, nil
	}
	out := make([]store.Fixture, 0, len(fixtures))
	seen := make(map[string]bool, len(fixtures))
	for i, f := range fixtures {
		if !filepath.IsAbs(f.GuestPath) {
			return nil, fmt.Errorf("fixture guest path %q must be absolute", f.GuestPath)
		}
		if seen[f.GuestPath] {
			return nil, fmt.Errorf("duplicate fixture guest path %q", f.GuestPath)
		}
		seen[f.GuestPath] = true

		host, err := filepath.Abs(f.HostPath)
		if err != nil {
			return nil, fmt.Errorf("resolve fixture host path %q: %w", f.HostPath, err)
		}
		info, err := os.Stat(host)
		if err != nil {
			return nil, fmt.Errorf("fixture host path %q: %w", host, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("fixture host path %q is not a directory", host)
		}
		out = append(out, store.Fixture{
			HostPath:  host,
			GuestPath: f.GuestPath,
			Label:     fmt.Sprintf("FBFIX%d", i),
		})
	}
	return out, nil
}

// toSeedFixtures projects persisted fixtures onto the guest-side view cloud-init
// needs (label + guest path) to emit the mount line.
func toSeedFixtures(fixtures []store.Fixture) []seed.Fixture {
	if len(fixtures) == 0 {
		return nil
	}
	out := make([]seed.Fixture, len(fixtures))
	for i, f := range fixtures {
		out[i] = seed.Fixture{Label: f.Label, GuestPath: f.GuestPath}
	}
	return out
}

// allocateIP picks the lowest free host address in subnet for a new VM. The
// gateway (.1), network, and broadcast addresses are reserved, as are the IPs
// already persisted to other VMs' configs in the same subnet — so members of a
// cluster get distinct, stable addresses without any cluster-level state. It is
// only called on backends that assign static IPs (Linux); vz reports no subnet.
func allocateIP(st *store.Store, subnetCIDR string) (ipAssignment, error) {
	prefix, err := netip.ParsePrefix(subnetCIDR)
	if err != nil {
		return ipAssignment{}, fmt.Errorf("parse subnet %q: %w", subnetCIDR, err)
	}
	prefix = prefix.Masked()

	gateway := prefix.Addr().Next() // .1
	broadcast := lastAddr(prefix)

	taken := map[netip.Addr]bool{gateway: true}
	names, err := st.List()
	if err != nil {
		return ipAssignment{}, fmt.Errorf("list vms: %w", err)
	}
	for _, n := range names {
		cfg, err := st.Load(n)
		if err != nil || cfg.IP == "" {
			continue
		}
		if a, err := netip.ParseAddr(cfg.IP); err == nil && prefix.Contains(a) {
			taken[a] = true
		}
	}

	netmask := net.IP(net.CIDRMask(prefix.Bits(), 32)).String()
	for candidate := gateway.Next(); prefix.Contains(candidate) && candidate != broadcast; candidate = candidate.Next() {
		if !taken[candidate] {
			return ipAssignment{ip: candidate.String(), gateway: gateway.String(), netmask: netmask}, nil
		}
	}

	return ipAssignment{}, fmt.Errorf("no free IP in subnet %s", subnetCIDR)
}

// lastAddr returns the broadcast (all host bits set) address of an IPv4 prefix.
func lastAddr(p netip.Prefix) netip.Addr {
	bytes := p.Addr().As4()
	for i := p.Bits(); i < 32; i++ {
		bytes[i/8] |= 1 << (7 - uint(i%8))
	}
	return netip.AddrFrom4(bytes)
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
func (v *VM) SSH(_ context.Context, cmd string) (string, error) {
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

	// Release the network only for a sole-owner VM; a cluster member's network is
	// shared and torn down by Cluster.Close (R3). No-op on macOS.
	if v.ownsNetwork && v.network != nil {
		_ = v.network.Close()
		v.network = nil
	}

	// Idempotent: a second Destroy finds the files already gone and returns nil
	// (matches the darwin client, ADR-0017 R2).
	if !v.store.Exists(v.name) {
		return nil
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

func (v *VM) waitForSSH(_ context.Context, timeout time.Duration) error {
	addr := net.JoinHostPort(v.ip.String(), "22")
	if err := v.sshMgr.WaitForSSH(addr, defaultUser, timeout); err != nil {
		return fmt.Errorf("wait for ssh: %w", err)
	}

	return nil
}

// NewCluster creates a cluster's shared network but boots no VMs. Use Add to
// bring members up on it. Shared setup (store, SSH key, image, backend) runs once
// here and is reused for every Add.
func NewCluster(optFns ...opts.Option) (*Cluster, error) {
	deps, err := resolveStartDeps(optFns...)
	if err != nil {
		return nil, err
	}

	nw, err := deps.backend.CreateNetwork()
	if err != nil {
		return nil, fmt.Errorf("create network: %w", err)
	}

	return &Cluster{deps: deps, network: nw}, nil
}

// Close releases the cluster's shared network. Call it once every member has been
// stopped or destroyed — on Linux it tears down the bridge and egress rules; on
// macOS it is a no-op (the vmnet network is released by GC). It is idempotent.
func (c *Cluster) Close() error {
	c.mu.Lock()
	nw := c.network
	c.network = nil
	c.mu.Unlock()

	if nw == nil {
		return nil
	}
	if err := nw.Close(); err != nil {
		return fmt.Errorf("close cluster network: %w", err)
	}
	return nil
}

// Add boots an additional VM on the cluster's shared network and registers it as
// a member. The new VM reaches every existing member by IP. The
// clustering-capability gate lives in the root package's Cluster.Add, which is
// expected to have rejected a second member on a non-clustering backend before
// reaching here, so Add boots unconditionally.
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

// SupportsClustering reports whether this cluster's backend can interconnect VMs
// on its shared network. It is false only on macOS older than 26, where VZ NAT
// isolates VMs from one another (ADR-0008, ADR-0012); the root package's gate
// consults it before booting a second member.
func (c *Cluster) SupportsClustering() bool {
	return c.deps.backend.SupportsClustering()
}

// Prune reclaims the inert host resources a fleetbox holder leaves behind if it
// dies without running its teardown — on Linux, orphaned bridges, taps, and
// firewall rules. On macOS it is a no-op (vmnet owns its state).
func Prune() error {
	b, err := newBackend()
	if err != nil {
		return err
	}
	if err := b.Reconcile(); err != nil {
		return fmt.Errorf("prune: %w", err)
	}
	return nil
}

// NestedVirtSupported reports whether nested virtualization is available on this
// host, asking the backend directly. On darwin it is the authoritative VZ check
// (the root client uses a pure-Go heuristic to avoid downloading the helper just
// to skip a test); on linux it probes /dev/kvm and the KVM nested parameter.
func NestedVirtSupported() bool {
	return nestedVirtSupported()
}
