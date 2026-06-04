// Package store manages VM state directories under ~/.fleetbox/vms/.
package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
	Mounts    []Mount   `json:"mounts,omitempty"`
}

// Mount is a persisted host↔guest shared directory. HostPath is absolute, Tag is
// the stable virtiofs tag assigned at first creation (fbm<i>, i = position in the
// mount list). The tag is the single source of truth shared between the host
// virtiofs device and the guest fstab entry, so it is computed once and persisted
// here rather than re-derived elsewhere.
type Mount struct {
	HostPath  string `json:"host_path"`
	GuestPath string `json:"guest_path"`
	Tag       string `json:"tag"`
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
		filepath.Join(baseDir, "vms"),
		filepath.Join(baseDir, "images"),
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

// VMDir returns the directory for a VM.
func (s *Store) VMDir(name string) string {
	return filepath.Join(s.baseDir, "vms", name)
}

// ImagesDir returns the images cache directory.
func (s *Store) ImagesDir() string {
	return filepath.Join(s.baseDir, "images")
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
	vmDir := s.VMDir(vm.Name)

	if err := os.MkdirAll(vmDir, 0o755); err != nil {
		return fmt.Errorf("create vm dir: %w", err)
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

// Delete removes a VM directory and all its contents.
func (s *Store) Delete(name string) error {
	vmDir := s.VMDir(name)

	if _, err := os.Stat(vmDir); os.IsNotExist(err) {
		return fmt.Errorf("vm %q does not exist", name)
	}

	if err := os.RemoveAll(vmDir); err != nil {
		return fmt.Errorf("remove vm dir: %w", err)
	}

	return nil
}

// List returns the names of all stored VMs.
func (s *Store) List() ([]string, error) {
	vmsDir := filepath.Join(s.baseDir, "vms")

	entries, err := os.ReadDir(vmsDir)
	if err != nil {
		return nil, fmt.Errorf("read vms dir: %w", err)
	}

	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
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

// EFIPath returns the path to the VM's EFI variable store.
func (s *Store) EFIPath(name string) string {
	return filepath.Join(s.VMDir(name), "efi.nvram")
}

// PidfilePath returns the path to the VM's pidfile (in baseDir, not vmDir).
func (s *Store) PidfilePath(name string) string {
	return filepath.Join(s.baseDir, "pid-"+name)
}

// SocketPath returns the path to the VM's control socket (in baseDir, not vmDir).
func (s *Store) SocketPath(name string) string {
	return filepath.Join(s.baseDir, "sock-"+name)
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
