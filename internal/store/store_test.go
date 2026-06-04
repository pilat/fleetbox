package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStoreBasic(t *testing.T) {
	tmpDir := t.TempDir()
	st, err := NewAt(tmpDir)
	if err != nil {
		t.Fatalf("NewAt: %v", err)
	}

	// Check directories were created
	dirs := []string{
		tmpDir,
		filepath.Join(tmpDir, "vms"),
		filepath.Join(tmpDir, "images"),
	}
	for _, dir := range dirs {
		if _, err := os.Stat(dir); err != nil {
			t.Errorf("directory %s not created: %v", dir, err)
		}
	}

	// Test paths
	if st.VMDir("test") != filepath.Join(tmpDir, "vms", "test") {
		t.Errorf("VMDir wrong: %s", st.VMDir("test"))
	}
	if st.ImagesDir() != filepath.Join(tmpDir, "images") {
		t.Errorf("ImagesDir wrong: %s", st.ImagesDir())
	}
	if st.SSHKeyPath() != filepath.Join(tmpDir, "id_ed25519") {
		t.Errorf("SSHKeyPath wrong: %s", st.SSHKeyPath())
	}
}

func TestStoreCreateLoadDelete(t *testing.T) {
	tmpDir := t.TempDir()
	st, err := NewAt(tmpDir)
	if err != nil {
		t.Fatalf("NewAt: %v", err)
	}

	vm := &VM{
		Name:      "test-vm",
		MAC:       "aa:bb:cc:dd:ee:ff",
		CPUs:      2,
		MemoryMB:  4096,
		DiskMB:    20480,
		Image:     "debian-12",
		CreatedAt: time.Now(),
	}

	// Create
	if err := st.Create(vm); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !st.Exists("test-vm") {
		t.Error("Exists returned false after Create")
	}

	// Load
	loaded, err := st.Load("test-vm")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Name != vm.Name {
		t.Errorf("Name = %q, want %q", loaded.Name, vm.Name)
	}
	if loaded.MAC != vm.MAC {
		t.Errorf("MAC = %q, want %q", loaded.MAC, vm.MAC)
	}
	if loaded.CPUs != vm.CPUs {
		t.Errorf("CPUs = %d, want %d", loaded.CPUs, vm.CPUs)
	}

	// List
	names, err := st.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(names) != 1 || names[0] != "test-vm" {
		t.Errorf("List = %v, want [test-vm]", names)
	}

	// Delete
	if err := st.Delete("test-vm"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if st.Exists("test-vm") {
		t.Error("Exists returned true after Delete")
	}
}

func TestStoreMountsRoundTrip(t *testing.T) {
	tmpDir := t.TempDir()
	st, err := NewAt(tmpDir)
	if err != nil {
		t.Fatalf("NewAt: %v", err)
	}

	vm := &VM{
		Name:      "mounted-vm",
		MAC:       "aa:bb:cc:dd:ee:ff",
		CPUs:      2,
		MemoryMB:  4096,
		DiskMB:    20480,
		Image:     "debian-12",
		CreatedAt: time.Now(),
		Mounts: []Mount{
			{HostPath: "/host/work", GuestPath: "/work", Tag: "fbm0"},
			{HostPath: "/host/data", GuestPath: "/data", Tag: "fbm1"},
		},
	}

	if err := st.Create(vm); err != nil {
		t.Fatalf("Create: %v", err)
	}

	loaded, err := st.Load("mounted-vm")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded.Mounts) != 2 {
		t.Fatalf("loaded %d mounts, want 2", len(loaded.Mounts))
	}
	for i, want := range vm.Mounts {
		if loaded.Mounts[i] != want {
			t.Errorf("Mounts[%d] = %+v, want %+v", i, loaded.Mounts[i], want)
		}
	}

	// config.json must carry the documented json keys.
	data, err := os.ReadFile(filepath.Join(st.VMDir("mounted-vm"), "config.json"))
	if err != nil {
		t.Fatalf("read config.json: %v", err)
	}
	for _, key := range []string{`"mounts"`, `"host_path"`, `"guest_path"`, `"tag"`} {
		if !strings.Contains(string(data), key) {
			t.Errorf("config.json missing key %s\n%s", key, data)
		}
	}
}

func TestStoreNoMountsOmitsField(t *testing.T) {
	tmpDir := t.TempDir()
	st, err := NewAt(tmpDir)
	if err != nil {
		t.Fatalf("NewAt: %v", err)
	}

	vm := &VM{Name: "plain-vm", CreatedAt: time.Now()}
	if err := st.Create(vm); err != nil {
		t.Fatalf("Create: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(st.VMDir("plain-vm"), "config.json"))
	if err != nil {
		t.Fatalf("read config.json: %v", err)
	}
	if strings.Contains(string(data), "mounts") {
		t.Errorf("config.json should omit mounts when empty\n%s", data)
	}
}

func TestStorePaths(t *testing.T) {
	tmpDir := t.TempDir()
	st, err := NewAt(tmpDir)
	if err != nil {
		t.Fatalf("NewAt: %v", err)
	}

	name := "test-vm"
	vmDir := filepath.Join(tmpDir, "vms", name)

	if st.DiskPath(name) != filepath.Join(vmDir, "disk.raw") {
		t.Errorf("DiskPath wrong: %s", st.DiskPath(name))
	}
	if st.SeedPath(name) != filepath.Join(vmDir, "seed.iso") {
		t.Errorf("SeedPath wrong: %s", st.SeedPath(name))
	}
	if st.EFIPath(name) != filepath.Join(vmDir, "efi.nvram") {
		t.Errorf("EFIPath wrong: %s", st.EFIPath(name))
	}
	if st.PidfilePath(name) != filepath.Join(tmpDir, "pid-"+name) {
		t.Errorf("PidfilePath wrong: %s", st.PidfilePath(name))
	}
	if st.SocketPath(name) != filepath.Join(tmpDir, "sock-"+name) {
		t.Errorf("SocketPath wrong: %s", st.SocketPath(name))
	}
	if st.SerialLogPath(name) != filepath.Join(vmDir, "serial.log") {
		t.Errorf("SerialLogPath wrong: %s", st.SerialLogPath(name))
	}
}

func TestLock(t *testing.T) {
	tmpDir := t.TempDir()
	st, err := NewAt(tmpDir)
	if err != nil {
		t.Fatalf("NewAt: %v", err)
	}

	vm := &VM{Name: "test-vm"}
	if err := st.Create(vm); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// First lock should succeed
	lock1, err := st.TryLock("test-vm")
	if err != nil {
		t.Fatalf("TryLock 1: %v", err)
	}

	// Second lock should fail
	_, err = st.TryLock("test-vm")
	if err == nil {
		t.Error("TryLock 2 should have failed")
	}

	// Unlock and try again
	if err := lock1.Unlock(); err != nil {
		t.Fatalf("Unlock: %v", err)
	}

	lock3, err := st.TryLock("test-vm")
	if err != nil {
		t.Fatalf("TryLock 3: %v", err)
	}
	_ = lock3.Unlock()
}
