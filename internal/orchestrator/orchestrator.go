// Package orchestrator owns the VM lifecycle CLIENT-SIDE: it resolves the
// per-call dependencies (store, SSH key, image), spawns the VM helper, creates
// the network and boots/waits/tears down members by driving that helper over the
// control protocol. Since ADR-0020 it runs in the client on BOTH platforms and
// links NO concrete hypervisor — its backend is the pure-Go remote proxy
// (internal/backend/remote); the real vz/cloud-hypervisor backend lives only
// behind the helper (internal/holder). The helper binary is the downloaded signed
// fleetbox-helper on darwin and a self-reexec of the client binary on linux.
//
// The clustering-capability gate and the public ErrClustersUnsupported sentinel
// live in the root package, not here: a caller is expected to have checked
// SupportsClustering before adding a second member, so Cluster.Add boots
// unconditionally.
package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/pilat/fleetbox/internal/backend"
	"github.com/pilat/fleetbox/internal/backend/remote"
	"github.com/pilat/fleetbox/internal/control"
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

// VM represents a running virtual machine driven by the orchestrator. Since
// ADR-0020 the orchestrator is a client: it owns the pure-Go prep and drives a
// helper over the control protocol via a remote-proxy backend, so VM.backend is a
// remote handle, not a hypervisor.
type VM struct {
	name    string
	ip      net.IP
	store   *store.Store
	sshMgr  *sshkey.Manager
	backend backend.VM
	config  *store.VM
	// session is the owning helper's control session. EVERY VM holds it — even a
	// cluster member whose Cluster handle the caller discarded (StartN) — so the
	// session, and the bound control connection inside it, stay reachable while any
	// VM is referenced; GC collecting the last reference is what reaps a bound
	// helper (ADR-0017 R4).
	session *control.Session
	// ownsSession is true only for a solo VM created by Start: its Stop/Destroy
	// reaps the helper (stopping the sole member makes it exit). A cluster member
	// leaves it false — the Cluster reaps via Close.
	ownsSession bool
	// createdThisCall is true when this VM's files were created by the current
	// call (it did not exist in the store beforehand). The detached-cluster
	// rollback consults it so a failed `up` only Destroys members it freshly made
	// and merely Stops pre-existing ones, never deleting a persisted disk.
	createdThisCall bool
}

// startDeps holds the once-per-call client prep shared by every VM in a Start or
// StartN. They are resolved a single time (resolveStartDeps) and reused for each
// VM, so a StartN cluster does not redo store/key/image setup per node.
type startDeps struct {
	options   *opts.Options
	store     *store.Store
	sshMgr    *sshkey.Manager
	pubKey    string
	imagePath string
}

// Cluster is a set of VMs sharing one helper-owned network, so every member
// reaches the others by IP — a vmnet SharedMode network on macOS, a Linux bridge
// on Linux (ADR-0008, ADR-0011). The shared network is a runtime object in the
// helper, tied to the Cluster's lifetime — never persisted. The helper is spawned
// lazily on the first Add (it needs the first member's name); later Adds reserve
// and boot on the same live helper.
type Cluster struct {
	mu      sync.Mutex
	deps    *startDeps
	bound   bool // library (bound, reaped with the caller) vs CLI (detached, persistent)
	backend backend.Backend
	network backend.Network
	session *control.Session
	primary string
	vms     []*VM
}

// Start creates and boots a new VM with the given name on its own one-member
// helper. If the VM already exists, the helper boots the existing VM. The helper
// is bound (reaped when the caller goes away).
func Start(ctx context.Context, name string, optFns ...opts.Option) (*VM, error) {
	deps, err := resolveStartDeps(optFns...)
	if err != nil {
		return nil, err
	}

	sess, b, err := spawnHelper(deps.store, []string{name}, true)
	if err != nil {
		return nil, err
	}

	nw, err := b.CreateNetwork()
	if err != nil {
		_ = sess.Close()
		return nil, fmt.Errorf("create network: %w", err)
	}

	vm, err := startOnNetwork(ctx, name, nw, deps, b)
	if err != nil {
		_ = sess.Close() // sole owner failed to boot: reap the helper it spawned
		return nil, err
	}
	// A bare Start owns its helper, so Stop/Destroy reap it.
	vm.session = sess
	vm.ownsSession = true
	return vm, nil
}

// spawnHelper launches the VM helper for the given members and returns its
// control session plus a remote-proxy backend that drives it. The helper binary
// is the downloaded, signed fleetbox-helper on macOS and a self-reexec of this
// binary on Linux (helperExe); bound is the library lifetime (reaped with the
// caller), false the CLI's detached/persistent mode.
func spawnHelper(st *store.Store, names []string, bound bool) (*control.Session, backend.Backend, error) {
	exe, err := helperExe(st)
	if err != nil {
		return nil, nil, err
	}
	sess, err := control.Spawn(st, control.SpawnConfig{Exe: exe, Names: names, Bound: bound})
	if err != nil {
		return nil, nil, fmt.Errorf("spawn helper: %w", err)
	}
	return sess, remote.New(st, names[0]), nil
}

// deriveNetworkConfig builds the seed's static network-config from the helper's
// subnet and the member's reserved IP/MAC: the gateway is the subnet's .1 and the
// netmask comes from its prefix length (Linux static-addressing path).
func deriveNetworkConfig(subnetCIDR, ip, mac string) (*seed.NetworkConfig, error) {
	prefix, err := netip.ParsePrefix(subnetCIDR)
	if err != nil {
		return nil, fmt.Errorf("parse subnet %q: %w", subnetCIDR, err)
	}
	prefix = prefix.Masked()
	gateway := prefix.Addr().Next()
	netmask := net.IP(net.CIDRMask(prefix.Bits(), 32)).String()
	return &seed.NetworkConfig{MAC: mac, IP: ip, Gateway: gateway.String(), Netmask: netmask}, nil
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

	return &startDeps{
		options:   options,
		store:     st,
		sshMgr:    sshMgr,
		pubKey:    pubKey,
		imagePath: imagePath,
	}, nil
}

// startOnNetwork performs the per-VM work — store config load/create, disk copy,
// seed ISO, backend create+boot, IP and SSH readiness — attaching the VM to the
// already-created network nw. Shared setup is done once by resolveStartDeps and
// passed in via deps. The returned VM retains nw so the network is not released
// by GC while the VM lives (ADR-0008, R3).
func startOnNetwork(
	ctx context.Context, name string, nw backend.Network, deps *startDeps, b backend.Backend,
) (vm *VM, err error) {
	st := deps.store
	options := deps.options

	existing := st.Exists(name)
	var vmConfig *store.VM
	if existing {
		loaded, err := st.Load(name)
		if err != nil {
			return nil, fmt.Errorf("load vm config: %w", err)
		}
		vmConfig = loaded
	}

	// Reserve this member's address on the helper-owned network BEFORE building the
	// seed (Decision 5). A previously-stored IP (existing VM) rides along as a hint
	// so a stopped member keeps its address while its /24 stays free; a new VM
	// passes no hint. The helper returns the MAC it will set on the NIC, so the
	// seed's network-config and the NIC agree without both sides recomputing
	// (Decision 6). On the DHCP/vz path the IP comes back empty.
	var ipHint string
	if vmConfig != nil {
		ipHint = vmConfig.IP
	}
	reservedIP, mac, err := nw.Reserve(name, ipHint)
	if err != nil {
		return nil, fmt.Errorf("reserve address: %w", err)
	}

	if !existing {
		// Fixtures are frozen at birth: validated, absolutized, and labeled once
		// here, then persisted and re-applied verbatim on every later boot
		// (ADR-0015). Validation is a create-time concern only — the existing
		// branch above never re-checks the host dirs.
		fixtures, err := toStoreFixtures(options.Fixtures)
		if err != nil {
			return nil, fmt.Errorf("prepare fixtures: %w", err)
		}

		vmConfig = &store.VM{
			Name:      name,
			MAC:       mac,
			IP:        reservedIP, // empty on the DHCP/vz path
			CPUs:      options.CPUs,
			MemoryMB:  options.MemGB * 1024,
			DiskMB:    options.DiskGB * 1024,
			Image:     options.Image,
			CreatedAt: time.Now(),
			Fixtures:  fixtures,
		}

		// On a static-addressing backend (Linux, non-empty subnet) bake the
		// reserved IP plus the gateway/netmask derived from the subnet into the
		// seed's network-config; a DHCP backend (vz) reports an empty subnet and
		// the guest stays on DHCP with no network-config emitted.
		var netCfg *seed.NetworkConfig
		if subnet := nw.Subnet(); subnet != "" {
			netCfg, err = deriveNetworkConfig(subnet, reservedIP, mac)
			if err != nil {
				return nil, err
			}
		}

		if err := st.Create(vmConfig); err != nil {
			return nil, fmt.Errorf("create vm store: %w", err)
		}

		// From here this call owns freshly created on-disk state (member dir, disk,
		// seed, fixtures). If any later step fails, startOnNetwork returns no *VM, so
		// Add never records it in c.vms and the cluster rollback can't reach it —
		// delete it on the way out so a partial member never leaks. Only fires for a
		// just-created member: an existing one being re-upped keeps its disk.
		defer func() {
			if err != nil {
				_ = st.Delete(name)
			}
		}()

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
		// it also carries the static network-config built above; on macOS the
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

	// Create backend VM on the shared network. The backend opens the serial log
	// itself from SerialLogPath — the file is VM state owned by the backend (the
	// helper, once the orchestrator moves client-side), not the orchestrator
	// (Decision 7); it is released by backend.Stop on every teardown path below.
	backendCfg := backend.Config{
		Name:          name,
		DiskPath:      st.DiskPath(name),
		SeedPath:      st.SeedPath(name),
		EFIPath:       st.EFIPath(name),
		MAC:           vmConfig.MAC,
		CPUs:          vmConfig.CPUs,
		MemoryBytes:   uint64(vmConfig.MemoryMB) * 1024 * 1024,
		SerialLogPath: st.SerialLogPath(name),
		// Re-attach the read-only fixture images built above, so both a freshly
		// created and a rebooted VM get identical fixtures (ADR-0015).
		FixturePaths: fixturePaths,
		// The persisted static IP (empty on the DHCP/vz path); the Linux backend
		// returns it from WaitForIP after a reachability probe. With the remote
		// proxy the helper ignores cfg.MAC/AssignedIP (it holds them from the
		// reservation it made) — they are set here only for the in-helper backend.
		AssignedIP: vmConfig.IP,
	}
	backendVM, err := b.Create(backendCfg, nw)
	if err != nil {
		return nil, fmt.Errorf("create backend vm: %w", err)
	}

	// Boot the VM
	if err := backendVM.Start(ctx); err != nil {
		_ = backendVM.Stop(ctx) // release any tap/process/serial a partial boot left behind
		return nil, fmt.Errorf("start vm: %w", err)
	}

	vm = &VM{
		name:    name,
		store:   st,
		sshMgr:  deps.sshMgr,
		backend: backendVM,
		config:  vmConfig,
		// existing was read before any file was created, so it cleanly marks
		// whether this call made the VM — the bit the rollback branches on.
		createdThisCall: !existing,
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
		return nil, fmt.Errorf("wait for ip: %w", err)
	}
	ip := net.ParseIP(ipStr)
	if ip == nil {
		_ = backendVM.Stop(ctx)
		return nil, fmt.Errorf("backend returned invalid IP %q", ipStr)
	}
	vm.ip = ip

	// Wait for SSH. Skipped only under the fleetbox_fake build tag (skipSSHWait),
	// where the fake backend's unroutable IP would make the real dial block for the
	// full timeout; production builds always run it (ADR-0018).
	if !skipSSHWait() {
		if err := vm.waitForSSH(ctx, 2*time.Minute); err != nil {
			_ = backendVM.Stop(ctx)
			return nil, fmt.Errorf("wait for ssh: %w", err)
		}
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

// Stop gracefully shuts down the VM via the helper. The disk is preserved; the
// helper closes its own serial log file as part of Stop (Decision 7). For a solo
// VM this stops the helper's only member, so the helper exits, and ownsSession
// reaps it.
func (v *VM) Stop(ctx context.Context) error {
	if err := v.backend.Stop(ctx); err != nil {
		return fmt.Errorf("stop vm: %w", err)
	}
	if v.ownsSession && v.session != nil {
		_ = v.session.Close()
	}
	return nil
}

// Destroy stops the VM and removes all its files. The backend's Stop polls the
// member's pidfile, so the VM is confirmed down before its store files are
// deleted (R6).
func (v *VM) Destroy(ctx context.Context) error {
	_ = v.backend.Stop(ctx)
	if v.ownsSession && v.session != nil {
		_ = v.session.Close()
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

// NewCluster prepares a cluster client; no helper is spawned until the first Add
// (the helper is launched on the first member's name). Shared client prep (store,
// SSH key, image) runs once here and is reused for every Add. The helper is bound
// — reaped when the caller goes away — matching the library lifetime.
func NewCluster(optFns ...opts.Option) (*Cluster, error) {
	return newCluster(true, optFns...)
}

// NewClusterDetached is NewCluster for the CLI: the helper is spawned detached so
// it persists after the command exits, and reconnect-by-name addresses members
// later (ls/ssh/down). Cluster.Close releases the persistent helper rather than
// reaping it.
func NewClusterDetached(optFns ...opts.Option) (*Cluster, error) {
	return newCluster(false, optFns...)
}

func newCluster(bound bool, optFns ...opts.Option) (*Cluster, error) {
	deps, err := resolveStartDeps(optFns...)
	if err != nil {
		return nil, err
	}
	return &Cluster{deps: deps, bound: bound}, nil
}

// ensureHelper spawns the cluster's single helper on the first member and creates
// its shared network, memoized so every later Add reuses the same live helper and
// network. The caller holds c.mu.
func (c *Cluster) ensureHelper(name string) error {
	if c.session != nil {
		return nil
	}
	sess, b, err := spawnHelper(c.deps.store, []string{name}, c.bound)
	if err != nil {
		return err
	}
	nw, err := b.CreateNetwork()
	if err != nil {
		_ = sess.Close()
		return fmt.Errorf("create network: %w", err)
	}
	c.session = sess
	c.backend = b
	c.network = nw
	c.primary = name
	return nil
}

// Add boots an additional VM on the cluster's shared helper and registers it as a
// member. The first Add spawns the helper; later Adds reserve and boot on the same
// live network, so the new VM reaches every existing member by IP. The
// clustering-capability gate lives in the root package's Cluster.Add, which is
// expected to have rejected a second member on a non-clustering host before
// reaching here, so Add boots unconditionally.
func (c *Cluster) Add(ctx context.Context, name string) (*VM, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.ensureHelper(name); err != nil {
		return nil, err
	}
	vm, err := startOnNetwork(ctx, name, c.network, c.deps, c.backend)
	if err != nil {
		return nil, err
	}
	// Members share, but do not own, the cluster's session — the Cluster reaps it
	// via Close (or GC, once the last member is unreferenced).
	vm.session = c.session
	c.vms = append(c.vms, vm)
	return vm, nil
}

// VMs returns a snapshot of the cluster's current members in the order they were
// added.
func (c *Cluster) VMs() []*VM {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.vms)
}

// StartClusterDetached boots the named VMs as one interconnected cluster on a
// single detached helper that persists after this process exits (the CLI's `up`).
// It is all-or-nothing: on any member's failure it rolls back the members already
// started and releases the helper. On success the helper is left running and the
// members are addressed later by name (ls/ssh/down).
//
// Rollback is disk-safe: a re-up of stopped members (which already have persisted
// disks) only stops them, while members this call created fresh are destroyed —
// never delete a disk that existed before this `up` (see Cluster.rollback).
func StartClusterDetached(ctx context.Context, names []string, optFns ...opts.Option) error {
	c, err := NewClusterDetached(optFns...)
	if err != nil {
		return err
	}
	for _, name := range names {
		if _, err := c.Add(ctx, name); err != nil {
			rbErr := c.rollback(ctx)
			_ = c.Close()
			if rbErr != nil {
				return fmt.Errorf("start %s: %w (rollback also failed: %w)", name, err, rbErr)
			}
			return fmt.Errorf("start %s: %w", name, err)
		}
	}
	// Detached: leave the helper running and do not reap it — the members outlive
	// this command (cattle-with-persistence).
	return nil
}

// rollback undoes a partial detached cluster up. For each member already added it
// branches on createdThisCall: a member this call created fresh is Destroyed (its
// files are this call's mess to clean up), while a pre-existing member — a stopped
// node being re-upped — is only Stopped, so its persisted disk survives. This is
// the data-loss fix: the old rollback Destroyed every added member, deleting disks
// that existed before the up.
func (c *Cluster) rollback(ctx context.Context) error {
	var errs error
	for _, vm := range c.VMs() {
		if vm.createdThisCall {
			if err := vm.Destroy(ctx); err != nil {
				errs = errors.Join(errs, fmt.Errorf("rollback destroy %s: %w", vm.name, err))
			}
		} else {
			if err := vm.Stop(ctx); err != nil {
				errs = errors.Join(errs, fmt.Errorf("rollback stop %s: %w", vm.name, err))
			}
		}
	}
	return errs
}

// AddMember boots a new member onto the LIVE helper already serving `sibling`,
// without spawning a new one — how a stopped node re-joins a running cluster's
// network instead of getting an isolated one of its own. It drives the live helper
// through the sibling's socket: createnetwork is idempotent (returns the existing
// subnet), then the member is reserved and booted on that network.
func AddMember(ctx context.Context, sibling, name string, optFns ...opts.Option) error {
	deps, err := resolveStartDeps(optFns...)
	if err != nil {
		return err
	}
	b := remote.New(deps.store, sibling)
	nw, err := b.CreateNetwork()
	if err != nil {
		return fmt.Errorf("reach cluster network via %s: %w", sibling, err)
	}
	if _, err := startOnNetwork(ctx, name, nw, deps, b); err != nil {
		return fmt.Errorf("add member %s: %w", name, err)
	}
	return nil
}

// Close releases the cluster's helper. For a bound (library) cluster it closes the
// control connection and reaps the helper, which tears down the shared network
// helper-side; for a detached (CLI) cluster it releases the persistent helper. It
// is idempotent.
func (c *Cluster) Close() error {
	c.mu.Lock()
	sess := c.session
	c.session = nil
	c.backend = nil
	c.network = nil
	c.primary = ""
	c.vms = nil
	c.mu.Unlock()

	if sess != nil {
		_ = sess.Close()
	}
	return nil
}

// Prune reclaims the inert host network state a helper leaves behind if it dies
// without running its teardown — on Linux, orphaned bridges, taps, nft firewall
// tables, and a left-on uplink forwarding flag. It spawns a short-lived helper that
// reconciles and exits, because reconcile needs CAP_NET_ADMIN (it programs netlink
// and nf_tables), which the Linux helper carries. On macOS the root's prune is a
// no-op and never calls this (vmnet owns its own state) — ADR-0013/0020/0025.
func Prune() error {
	st, err := store.New()
	if err != nil {
		return fmt.Errorf("init store: %w", err)
	}
	exe, err := helperExe(st)
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, control.ReconcileFlag)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("prune: %w", err)
	}
	return nil
}
