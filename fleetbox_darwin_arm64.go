//go:build darwin && arm64

package fleetbox

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/pilat/fleetbox/internal/control"
	"github.com/pilat/fleetbox/internal/helperdist"
	"github.com/pilat/fleetbox/internal/sshkey"
	"github.com/pilat/fleetbox/internal/store"
)

const (
	// defaultUser is the cloud-init user the helper provisions; the client dials
	// it over SSH directly (the VM's vmnet IP is host-wide reachable, ADR-0017).
	defaultUser = "fleetbox"
	// defaultBootTimeout bounds the client's wait for a member to come up when the
	// caller's context carries no shorter deadline.
	defaultBootTimeout = 5 * time.Minute
)

var (
	_ vmState      = (*darwinVM)(nil)
	_ clusterState = (*darwinCluster)(nil)
)

// Start creates and boots a new VM with the given name. On macOS it spawns the
// downloaded, signed fleetbox-helper as a bound (attached) subprocess that owns
// the VM, waits for the VM to come up, and returns a pure-Go client handle whose
// SSH dials the VM's IP directly (ADR-0017). If the VM already exists, the helper
// boots the existing VM.
func Start(ctx context.Context, name string, options ...Option) (*VM, error) {
	st, sshMgr, err := clientSetup()
	if err != nil {
		return nil, err
	}

	helper, err := helperdist.Ensure(st)
	if err != nil {
		return nil, fmt.Errorf("ensure helper: %w", err)
	}

	sess, err := control.Spawn(st, control.SpawnConfig{
		Exe:     helper,
		Names:   []string{name},
		Options: options,
		Bound:   true,
	})
	if err != nil {
		return nil, fmt.Errorf("spawn helper: %w", err)
	}

	statuses, err := control.WaitMembers(st, []string{name}, bootTimeout(ctx))
	if err != nil {
		_ = sess.Close()
		return nil, fmt.Errorf("wait for vm: %w", err)
	}
	ip := net.ParseIP(statuses[name].IP)
	if ip == nil {
		_ = sess.Close()
		return nil, fmt.Errorf("vm %q came up without an IP", name)
	}

	// A bare Start owns its helper: Stop/Destroy reap it (a solo VM is the last
	// member, so stopping it makes the helper exit).
	return &VM{st: &darwinVM{name: name, ip: ip, st: st, sshMgr: sshMgr, session: sess, ownsSession: true}}, nil
}

// NewCluster prepares a cluster client. No helper is spawned until the first
// member is added — that one helper then owns every member of this cluster on one
// shared vmnet network (ADR-0017, R3).
func NewCluster(options ...Option) (*Cluster, error) {
	st, sshMgr, err := clientSetup()
	if err != nil {
		return nil, err
	}
	return &Cluster{st: &darwinCluster{st: st, sshMgr: sshMgr, options: options}}, nil
}

// clientSetup resolves the pure-Go client dependencies shared by Start and
// NewCluster: the store and the SSH key (ensured here so it exists before the
// helper reads it for the seed).
func clientSetup() (*store.Store, *sshkey.Manager, error) {
	st, err := store.New()
	if err != nil {
		return nil, nil, fmt.Errorf("init store: %w", err)
	}
	sshMgr := sshkey.NewManager(st.SSHKeyPath())
	if _, err := sshMgr.EnsureKey(); err != nil {
		return nil, nil, fmt.Errorf("ensure ssh key: %w", err)
	}
	return st, sshMgr, nil
}

// darwinVM is the client handle for a VM owned by a helper. SSH dials the VM's IP
// directly; lifecycle commands go to the helper over the member socket.
type darwinVM struct {
	name   string
	ip     net.IP
	st     *store.Store
	sshMgr *sshkey.Manager
	// session is the owning helper's session. EVERY member holds it — even cluster
	// members whose Cluster handle the caller discarded (fleetbox.StartN) — so the
	// session, and the bound control connection inside it, stay reachable while any
	// VM is referenced. Otherwise GC could collect the session mid-test and its
	// connection's finalizer would close the fd, which the helper reads as EOF and
	// tears the cluster down (R4). When the last member is unreferenced the session
	// is collected and that same EOF cleanly reaps the helper — the GC-driven
	// teardown the in-process macOS model used (ADR-0008).
	session *control.Session
	// ownsSession is true only for a solo VM created by Start: its Stop/Destroy
	// reaps the helper, because stopping the sole member makes the helper exit. A
	// cluster member leaves it false — the helper outlives any single member until
	// the last one stops (or the Cluster is closed).
	ownsSession bool
}

// Name returns the VM name.
func (d *darwinVM) Name() string { return d.name }

// IP returns the VM's IP address.
func (d *darwinVM) IP() net.IP { return d.ip }

// SSH executes a command on the VM via SSH and returns the output. The VM's
// vmnet IP is host-wide reachable, so the client dials it directly without going
// through the helper (ADR-0017).
func (d *darwinVM) SSH(_ context.Context, cmd string) (string, error) {
	client, err := d.sshMgr.DialIP(d.ip, defaultUser, 30*time.Second)
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

// Stop gracefully shuts down the VM via the helper. The disk is preserved. For a
// solo VM this stops the helper's only member, so the helper exits and is reaped.
func (d *darwinVM) Stop(_ context.Context) error {
	if err := control.Stop(d.st, d.name); err != nil {
		return fmt.Errorf("stop vm: %w", err)
	}
	if d.ownsSession {
		_ = d.session.Close()
	}
	return nil
}

// Destroy stops the VM and removes all its files. It waits for the stop to be
// confirmed (control.Stop polls the pidfile) before deleting the member's store
// files, so files are never removed under a live VM (ADR-0017, R6). It is
// idempotent: a second call finds the VM already gone and returns nil.
func (d *darwinVM) Destroy(_ context.Context) error {
	if err := control.Stop(d.st, d.name); err != nil {
		return fmt.Errorf("stop vm: %w", err)
	}
	if d.ownsSession {
		_ = d.session.Close()
	}
	if !d.st.Exists(d.name) {
		return nil
	}
	if err := d.st.Delete(d.name); err != nil {
		return fmt.Errorf("delete vm files: %w", err)
	}
	return nil
}

// State returns the current VM state as reported by the helper, or "stopped"
// when no helper is serving it.
func (d *darwinVM) State() string {
	status, err := control.GetStatus(d.st, d.name)
	if err != nil {
		return control.StateStopped
	}
	return status.State
}

// darwinCluster is the client handle for a cluster: one bound helper owns every
// member on one shared network. The first Add spawns the helper; later Adds join
// the same helper (R3). All access is serialized so the lazy helper spawn happens
// exactly once.
type darwinCluster struct {
	mu      sync.Mutex
	st      *store.Store
	sshMgr  *sshkey.Manager
	options []Option
	session *control.Session
	primary string // first member's name — the sibling new members join through
	members []*darwinVM
}

// Add boots a member, spawning the cluster's single helper on the first call and
// joining it on later ones. The clustering-capability gate is applied by the
// public Cluster.Add before this runs.
func (c *darwinCluster) Add(ctx context.Context, name string) (vmState, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.session == nil {
		helper, err := helperdist.Ensure(c.st)
		if err != nil {
			return nil, fmt.Errorf("ensure helper: %w", err)
		}
		sess, err := control.Spawn(c.st, control.SpawnConfig{
			Exe:     helper,
			Names:   []string{name},
			Options: c.options,
			Bound:   true,
		})
		if err != nil {
			return nil, fmt.Errorf("spawn helper: %w", err)
		}
		if _, err := control.WaitMembers(c.st, []string{name}, bootTimeout(ctx)); err != nil {
			_ = sess.Close()
			return nil, fmt.Errorf("wait for vm: %w", err)
		}
		c.session = sess
		c.primary = name
	} else if err := control.AddMember(c.st, c.primary, name); err != nil {
		return nil, fmt.Errorf("add member %q: %w", name, err)
	}

	vm, err := c.newMember(name)
	if err != nil {
		return nil, err
	}
	c.members = append(c.members, vm)
	return vm, nil
}

// newMember reads the member's IP from the helper and builds its client handle.
// The handle shares (but does not own) the cluster's session, so the helper
// stays reachable while any member is referenced.
func (c *darwinCluster) newMember(name string) (*darwinVM, error) {
	status, err := control.GetStatus(c.st, name)
	if err != nil {
		return nil, fmt.Errorf("get status %q: %w", name, err)
	}
	ip := net.ParseIP(status.IP)
	if ip == nil {
		return nil, fmt.Errorf("member %q came up without an IP", name)
	}
	// Hold the shared session so it (and its bound control connection) outlives
	// a discarded Cluster handle for as long as any member is referenced.
	return &darwinVM{name: name, ip: ip, st: c.st, sshMgr: c.sshMgr, session: c.session}, nil
}

// VMs returns a snapshot of the cluster's current members in the order added.
func (c *darwinCluster) VMs() []vmState {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]vmState, len(c.members))
	for i, m := range c.members {
		out[i] = m
	}
	return out
}

// Close stops every remaining member via the helper; when the last member stops,
// the helper exits, and Close waits for that exit before returning (ADR-0017,
// R6). It is idempotent.
func (c *darwinCluster) Close() error {
	c.mu.Lock()
	members := c.members
	sess := c.session
	c.members = nil
	c.session = nil
	c.primary = ""
	c.mu.Unlock()

	for _, m := range members {
		_ = control.Stop(c.st, m.name)
	}
	if sess != nil {
		_ = sess.Close()
	}
	return nil
}

// supportsClustering reports whether this host can interconnect VMs: macOS 26+
// (vmnet SharedMode). It is a pure-Go host check so the gate never downloads the
// helper (ADR-0017, R7).
func (c *darwinCluster) supportsClustering() bool { return supportsClusteringHost() }

// bootTimeout derives the readiness wait from the caller's context deadline,
// falling back to defaultBootTimeout.
func bootTimeout(ctx context.Context) time.Duration {
	if dl, ok := ctx.Deadline(); ok {
		if d := time.Until(dl); d > 0 && d < defaultBootTimeout {
			return d
		}
	}
	return defaultBootTimeout
}

// nestedVirtSupported is the pure-Go macOS heuristic: Apple Silicon M3+ on macOS
// 15+. It must answer without VZ and without downloading the helper, because it
// gates the test-skip path (ADR-0017, R7). An unrecognized CPU brand (a future
// chip) is treated optimistically as capable; the helper runs the authoritative
// vz.IsNestedVirtualizationSupported check at boot and errors cleanly if wrong.
func nestedVirtSupported() bool {
	return nestedCapable(macOSMajor(), appleCPUGeneration())
}

// nestedCapable is the pure decision behind nestedVirtSupported, split out so it
// is testable without the host sysctls. nested virtualization needs macOS 15+ and
// Apple Silicon M3+. A 0 generation means the CPU brand was unrecognized (a future
// chip): treat it as capable so a new Mac is not wrongly skipped — the helper's
// authoritative VZ check rejects it at boot if the optimism was wrong (R7).
func nestedCapable(macOSMajor, appleGen int) bool {
	if macOSMajor < 15 {
		return false
	}
	if appleGen == 0 {
		return true
	}
	return appleGen >= 3
}

// supportsClusteringHost reports macOS 26+, where vmnet SharedMode gives VM↔VM
// connectivity (ADR-0008, ADR-0012). Pure Go, no helper download (R7).
func supportsClusteringHost() bool { return macOSMajor() >= 26 }

// prune is a no-op on macOS: vmnet owns its own state and a dead helper's
// in-process VMs die with it, so there is nothing to reclaim (ADR-0013).
func prune() error { return nil }

// macOSMajor returns the host's major macOS version from kern.osproductversion,
// or 0 on a sysctl/parse error (which conservatively reports "not capable").
func macOSMajor() int {
	ver, err := unix.Sysctl("kern.osproductversion")
	if err != nil {
		return 0
	}
	major, _, _ := strings.Cut(ver, ".")
	n, err := strconv.Atoi(major)
	if err != nil {
		return 0
	}
	return n
}

// appleCPUGeneration returns the Apple Silicon generation N from
// machdep.cpu.brand_string ("Apple M3 Pro" → 3), or 0 if unrecognized.
func appleCPUGeneration() int {
	brand, err := unix.Sysctl("machdep.cpu.brand_string")
	if err != nil {
		return 0
	}
	return parseAppleGeneration(brand)
}

// parseAppleGeneration extracts N from an "Apple M<N> ..." CPU brand string. It
// returns 0 when no "M<N>" token is present (a non-Apple or future/unknown brand).
func parseAppleGeneration(brand string) int {
	for f := range strings.FieldsSeq(brand) {
		if len(f) >= 2 && f[0] == 'M' {
			if n, err := strconv.Atoi(f[1:]); err == nil {
				return n
			}
		}
	}
	return 0
}
