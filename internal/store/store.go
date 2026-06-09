// Package store manages VM state directories under
// ~/.fleetbox/clusters/<cluster>/<member>/.
package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// VM represents a stored VM configuration.
type VM struct {
	Name      string    `json:"name"`
	MAC       string    `json:"mac"`
	CPUs      int       `json:"cpus"`
	MemoryMB  int       `json:"memory_mb"`
	DiskMB    int       `json:"disk_mb"`
	Image     string    `json:"image"`
	CreatedAt time.Time `json:"created_at"`
	Fixtures  []Fixture `json:"fixtures,omitempty"`
	// IP is the static IPv4 address assigned at first create on backends that
	// allocate from a known subnet (Linux/cloud-hypervisor). It is the persisted
	// source of truth so a rebooted VM keeps its address and a re-joining cluster
	// member does not collide. Empty on macOS, where addresses come from DHCP.
	IP string `json:"ip,omitempty"`
}

// Fixture is a persisted read-only host directory packed into the guest at boot.
// HostPath is absolute; Label is the stable ext4 volume label assigned at first
// creation (FBFIX<i>, i = position in the fixture list). The label is the single
// source of truth shared between the image's volume label and the guest's
// cloud-init LABEL= mount line, so it is computed once and persisted here rather
// than re-derived in two places (ADR-0015).
type Fixture struct {
	HostPath  string `json:"host_path"`
	GuestPath string `json:"guest_path"`
	Label     string `json:"label"`
}

// Store manages VM storage.
type Store struct {
	baseDir string
}

// New creates a Store at the default location (~/.fleetbox).
func New() (*Store, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("get home dir: %w", err)
	}
	return NewAt(filepath.Join(home, ".fleetbox"))
}

// NewAt creates a Store at the given base directory.
func NewAt(baseDir string) (*Store, error) {
	dirs := []string{
		baseDir,
		filepath.Join(baseDir, "clusters"),
		filepath.Join(baseDir, "images"),
		// run/ holds the holder control sockets. They live here, not in the deeply
		// nested member dir, because a unix socket path must fit the 104-byte
		// sun_path limit, which clusters/<cluster>/<member>/ blows past for long
		// names (amends ADR-0014; see SocketPath).
		filepath.Join(baseDir, "run"),
	}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create dir %s: %w", dir, err)
		}
	}
	return &Store{baseDir: baseDir}, nil
}

// BaseDir returns the base storage directory.
func (s *Store) BaseDir() string {
	return s.baseDir
}

// VMDir returns the member directory for a VM, nested under its cluster:
// <baseDir>/clusters/<cluster>/<member>/. The cluster segment is derived from
// the member name (see clusterName), so a solo VM "dev" lives at
// clusters/dev/dev/ and a cluster member "web-2" at clusters/web/web-2/.
func (s *Store) VMDir(name string) string {
	return filepath.Join(s.baseDir, "clusters", clusterName(name), name)
}

// EnsureDir creates the member directory (and its cluster parent) for a VM.
// It is idempotent and is the single place member directories are created, so
// the holder (which serves a member's socket/pidfile before the VM boots) and
// Create both go through it.
func (s *Store) EnsureDir(name string) error {
	if err := os.MkdirAll(s.VMDir(name), 0o755); err != nil {
		return fmt.Errorf("create vm dir: %w", err)
	}

	return nil
}

// ImagesDir returns the images cache directory.
func (s *Store) ImagesDir() string {
	return filepath.Join(s.baseDir, "images")
}

// BinDir returns the cache directory for downloaded executables and firmware
// (~/.fleetbox/bin). The Linux backend caches the checksum-pinned
// cloud-hypervisor binary and its firmware here; it is created on first download
// rather than by New, so macOS installs never grow an empty bin directory.
func (s *Store) BinDir() string {
	return filepath.Join(s.baseDir, "bin")
}

// NetworkStateDir returns the directory holding the Linux backend's per-network
// write-ahead records and ip_forward marker (~/.fleetbox/networks). It lets a
// crashed cluster's bridges/taps/iptables rules be reclaimed on the next up or
// via prune (ADR-0013); it is created on first network create, so macOS installs
// never grow it.
func (s *Store) NetworkStateDir() string {
	return filepath.Join(s.baseDir, "networks")
}

// SSHKeyPath returns the path to the SSH private key.
func (s *Store) SSHKeyPath() string {
	return filepath.Join(s.baseDir, "id_ed25519")
}

// Exists returns true if a VM with the given name exists (has config.json).
func (s *Store) Exists(name string) bool {
	configPath := filepath.Join(s.VMDir(name), "config.json")
	_, err := os.Stat(configPath)
	return err == nil
}

// Create creates a new VM directory and writes its config.
func (s *Store) Create(vm *VM) error {
	if err := s.EnsureDir(vm.Name); err != nil {
		return err
	}

	return s.Save(vm)
}

// Save writes the VM config to disk.
func (s *Store) Save(vm *VM) error {
	configPath := filepath.Join(s.VMDir(vm.Name), "config.json")

	data, err := json.MarshalIndent(vm, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}

	if err := os.WriteFile(configPath, data, 0o644); err != nil {
		return fmt.Errorf("write config: %w", err)
	}

	return nil
}

// Load reads a VM config from disk.
func (s *Store) Load(name string) (*VM, error) {
	configPath := filepath.Join(s.VMDir(name), "config.json")

	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	var vm VM
	if err := json.Unmarshal(data, &vm); err != nil {
		return nil, fmt.Errorf("unmarshal config: %w", err)
	}

	return &vm, nil
}

// Delete removes a VM's member directory and all its contents, then drops the
// parent cluster directory if it is now empty.
func (s *Store) Delete(name string) error {
	vmDir := s.VMDir(name)

	if _, err := os.Stat(vmDir); os.IsNotExist(err) {
		return fmt.Errorf("vm %q does not exist", name)
	}

	// RemoveAll on the MEMBER dir is correct — it must wipe disk.raw, config.json,
	// seed.iso, pid, etc. (The NEVER-RemoveAll rule below applies only to the
	// shared parent cluster dir, which may hold sibling members.)
	if err := os.RemoveAll(vmDir); err != nil {
		return fmt.Errorf("remove vm dir: %w", err)
	}

	// The control sockets live in run/, not the member dir, so RemoveAll above did
	// not touch them. A live holder retires its own socket on stop; this best-effort
	// sweep clears one a crashed holder may have left behind.
	_ = os.Remove(s.SocketPath(name))
	_ = os.Remove(s.ControlSocketPath(name))

	// Drop the now-maybe-empty cluster directory. os.Remove (NEVER RemoveAll)
	// refuses a non-empty directory, which is exactly the "siblings still
	// present, keep it" case; the error is ignored unconditionally
	// (non-empty, not-exist, perm — all benign here).
	clusterDir := filepath.Join(s.baseDir, "clusters", clusterName(name))
	_ = os.Remove(clusterDir)

	return nil
}

// List returns the names of all stored VM members, walking the two-level
// clusters/<cluster>/<member>/ tree and returning the flat list of member
// names (the unit every caller works in). Results are cluster-sorted then
// member-sorted (os.ReadDir sorts each level), not creation order.
func (s *Store) List() ([]string, error) {
	clustersDir := filepath.Join(s.baseDir, "clusters")

	clusters, err := os.ReadDir(clustersDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read clusters dir: %w", err)
	}

	var names []string
	for _, c := range clusters {
		if !c.IsDir() {
			continue
		}
		members, err := os.ReadDir(filepath.Join(clustersDir, c.Name()))
		if err != nil {
			return nil, fmt.Errorf("read cluster dir %s: %w", c.Name(), err)
		}
		for _, m := range members {
			if m.IsDir() {
				names = append(names, m.Name())
			}
		}
	}

	return names, nil
}

// DiskPath returns the path to the VM's disk image.
func (s *Store) DiskPath(name string) string {
	return filepath.Join(s.VMDir(name), "disk.raw")
}

// SeedPath returns the path to the VM's seed ISO.
func (s *Store) SeedPath(name string) string {
	return filepath.Join(s.VMDir(name), "seed.iso")
}

// FixturePath returns the path to the i-th fixture's ext4 image inside the VM's
// member directory (fixture-<i>.img). It is per-member, not cluster-level, so the
// existing Delete → RemoveAll(memberDir) wipes it for free with no extra teardown
// (ADR-0015).
func (s *Store) FixturePath(name string, i int) string {
	return filepath.Join(s.VMDir(name), fmt.Sprintf("fixture-%d.img", i))
}

// EFIPath returns the path to the VM's EFI variable store.
func (s *Store) EFIPath(name string) string {
	return filepath.Join(s.VMDir(name), "efi.nvram")
}

// PidfilePath returns the path to the VM's holder pidfile, inside its member
// directory. Every member served by one holder writes the same PID (os.Getpid)
// into its own pidfile.
func (s *Store) PidfilePath(name string) string {
	return filepath.Join(s.VMDir(name), "pid")
}

// SocketPath returns the path to a member's holder control socket. It lives in
// ~/.fleetbox/run/ under a hash of the name, NOT in the member directory: a unix
// socket path must fit the 104-byte sun_path limit, and the nested member dir
// (clusters/<cluster>/<member>/) plus a long name and a long home dir blows past
// it. The hash keeps the path short and bounded regardless of name length
// (amends ADR-0014, which had placed it in the member dir).
func (s *Store) SocketPath(name string) string {
	return filepath.Join(s.baseDir, "run", sockHash(name)+".sock")
}

// ControlSocketPath returns the path to a bound helper's holder-wide control
// socket. It is distinct from the per-member SocketPath: the library client holds
// one long-lived connection to it for the helper's lifetime, and the helper
// treats that connection's EOF as "parent gone — tear everything down"
// (ADR-0017, R4). Like SocketPath it lives in run/ under a name hash to stay
// under the sun_path limit.
func (s *Store) ControlSocketPath(primary string) string {
	return filepath.Join(s.baseDir, "run", sockHash(primary)+".ctl")
}

// sockHash maps a VM name to a short, filesystem-safe token (the first 16 hex of
// its SHA-256) so the unix socket path stays well under the 104-byte sun_path
// limit no matter how long the name or home directory is.
func sockHash(name string) string {
	sum := sha256.Sum256([]byte(name))
	return hex.EncodeToString(sum[:8])
}

// SerialLogPath returns the path to the VM's serial log.
func (s *Store) SerialLogPath(name string) string {
	return filepath.Join(s.VMDir(name), "serial.log")
}

// Lock represents an advisory file lock on a VM.
type Lock struct {
	file *os.File
	name string
}

// TryLock attempts to acquire an exclusive lock on the VM.
// Returns nil if locked successfully, error otherwise.
func (s *Store) TryLock(name string) (*Lock, error) {
	lockPath := filepath.Join(s.VMDir(name), ".lock")

	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("vm %q is locked by another process", name)
	}

	return &Lock{file: f, name: name}, nil
}

// Unlock releases the lock.
func (l *Lock) Unlock() error {
	_ = syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)

	if err := l.file.Close(); err != nil {
		return fmt.Errorf("close lock file: %w", err)
	}

	return nil
}

// clusterName derives a member's cluster name by stripping a single trailing
// "-<digits>" group. It is the inverse-free mapping that lets a member's
// directory be located from its name alone, with no persisted cluster field:
//
//	web-3    → web    (normal cluster member)
//	web-12   → web    (multi-digit index)
//	dev      → dev    (no dash: its own cluster)
//	web-1-2  → web-1  (only the last -N is stripped)
//	node-2024→ node   (accepted consequence: a solo VM ending in -<digits>)
//	web-     → web-   (trailing dash, no digits: unchanged)
//	web-x    → web-x  (non-digit suffix: unchanged)
//	-5       → -5     (empty prefix guarded: no empty path segment)
func clusterName(name string) string {
	i := strings.LastIndex(name, "-")
	if i <= 0 {
		return name // no dash, or dash at index 0 ("-5") → name as-is
	}
	suffix := name[i+1:]
	if suffix == "" {
		return name // trailing dash, no digits ("web-") → "web-"
	}
	for _, r := range suffix {
		if r < '0' || r > '9' {
			return name // any non-digit ("web-x") → name as-is
		}
	}
	return name[:i] // strip "-<digits>"
}
