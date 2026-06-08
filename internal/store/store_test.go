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
		filepath.Join(tmpDir, "clusters"),
		filepath.Join(tmpDir, "images"),
	}
	for _, dir := range dirs {
		if _, err := os.Stat(dir); err != nil {
			t.Errorf("directory %s not created: %v", dir, err)
		}
	}

	// Test paths: a solo VM is a cluster of one (clusters/test/test).
	if st.VMDir("test") != filepath.Join(tmpDir, "clusters", "test", "test") {
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

func TestStoreFixturesRoundTrip(t *testing.T) {
	tmpDir := t.TempDir()
	st, err := NewAt(tmpDir)
	if err != nil {
		t.Fatalf("NewAt: %v", err)
	}

	vm := &VM{
		Name:      "fixtured-vm",
		MAC:       "aa:bb:cc:dd:ee:ff",
		CPUs:      2,
		MemoryMB:  4096,
		DiskMB:    20480,
		Image:     "debian-12",
		CreatedAt: time.Now(),
		Fixtures: []Fixture{
			{HostPath: "/host/work", GuestPath: "/work", Label: "FBFIX0"},
			{HostPath: "/host/data", GuestPath: "/data", Label: "FBFIX1"},
		},
	}

	if err := st.Create(vm); err != nil {
		t.Fatalf("Create: %v", err)
	}

	loaded, err := st.Load("fixtured-vm")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded.Fixtures) != 2 {
		t.Fatalf("loaded %d fixtures, want 2", len(loaded.Fixtures))
	}
	for i, want := range vm.Fixtures {
		if loaded.Fixtures[i] != want {
			t.Errorf("Fixtures[%d] = %+v, want %+v", i, loaded.Fixtures[i], want)
		}
	}

	// config.json must carry the documented json keys.
	data, err := os.ReadFile(filepath.Join(st.VMDir("fixtured-vm"), "config.json"))
	if err != nil {
		t.Fatalf("read config.json: %v", err)
	}
	for _, key := range []string{`"fixtures"`, `"host_path"`, `"guest_path"`, `"label"`} {
		if !strings.Contains(string(data), key) {
			t.Errorf("config.json missing key %s\n%s", key, data)
		}
	}
}

func TestStoreNoFixturesOmitsField(t *testing.T) {
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
	if strings.Contains(string(data), "fixtures") {
		t.Errorf("config.json should omit fixtures when empty\n%s", data)
	}
}

func TestFixturePath(t *testing.T) {
	st, err := NewAt(t.TempDir())
	if err != nil {
		t.Fatalf("NewAt: %v", err)
	}

	// A cluster member's i-th fixture image lives inside its member dir, which
	// nests under the derived cluster (web-2 → cluster web).
	got := st.FixturePath("web-2", 1)
	want := filepath.Join(st.BaseDir(), "clusters", "web", "web-2", "fixture-1.img")
	if got != want {
		t.Errorf("FixturePath(web-2, 1) = %q, want %q", got, want)
	}
}

func TestStorePaths(t *testing.T) {
	tmpDir := t.TempDir()
	st, err := NewAt(tmpDir)
	if err != nil {
		t.Fatalf("NewAt: %v", err)
	}

	// "test-vm" → cluster "test-vm" (trailing "-vm" is not "-<digits>", so the
	// name is its own solo cluster).
	name := "test-vm"
	vmDir := filepath.Join(tmpDir, "clusters", name, name)

	if st.DiskPath(name) != filepath.Join(vmDir, "disk.raw") {
		t.Errorf("DiskPath wrong: %s", st.DiskPath(name))
	}
	if st.SeedPath(name) != filepath.Join(vmDir, "seed.iso") {
		t.Errorf("SeedPath wrong: %s", st.SeedPath(name))
	}
	if st.EFIPath(name) != filepath.Join(vmDir, "efi.nvram") {
		t.Errorf("EFIPath wrong: %s", st.EFIPath(name))
	}
	if st.PidfilePath(name) != filepath.Join(vmDir, "pid") {
		t.Errorf("PidfilePath wrong: %s", st.PidfilePath(name))
	}
	if st.SocketPath(name) != filepath.Join(vmDir, "sock") {
		t.Errorf("SocketPath wrong: %s", st.SocketPath(name))
	}
	if st.SerialLogPath(name) != filepath.Join(vmDir, "serial.log") {
		t.Errorf("SerialLogPath wrong: %s", st.SerialLogPath(name))
	}

	// A cluster member nests under its derived cluster: clusters/web/web-2/.
	if st.DiskPath("web-2") != filepath.Join(tmpDir, "clusters", "web", "web-2", "disk.raw") {
		t.Errorf("DiskPath(web-2) wrong: %s", st.DiskPath("web-2"))
	}
	if st.SocketPath("dev") != filepath.Join(tmpDir, "clusters", "dev", "dev", "sock") {
		t.Errorf("SocketPath(dev) wrong: %s", st.SocketPath("dev"))
	}
}

func TestClusterName(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"web-3", "web"},
		{"web-12", "web"},
		{"web-01", "web"},
		{"dev", "dev"},
		{"5", "5"},
		{"web-1-2", "web-1"},
		{"node-2024", "node"},
		{"web-", "web-"},
		{"web-x", "web-x"},
		{"-5", "-5"},
		{"", ""},
		// Pathological-but-defined: only the last "-<digits>" is stripped, so a
		// doubled dash leaves a trailing dash in the cluster segment. Documented
		// here so the behavior is locked, not accidental.
		{"web--1", "web-"},
		{"--1", "-"},
	}
	for _, c := range cases {
		if got := clusterName(c.in); got != c.want {
			t.Errorf("clusterName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestListTwoLevel(t *testing.T) {
	st, err := NewAt(t.TempDir())
	if err != nil {
		t.Fatalf("NewAt: %v", err)
	}

	for _, name := range []string{"web-1", "web-2", "db"} {
		if err := st.Create(&VM{Name: name, CreatedAt: time.Now()}); err != nil {
			t.Fatalf("Create %s: %v", name, err)
		}
	}

	names, err := st.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	// cluster-sorted then member-sorted: [db] then [web-1, web-2].
	want := []string{"db", "web-1", "web-2"}
	if len(names) != len(want) {
		t.Fatalf("List = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("List = %v, want %v", names, want)
		}
	}
}

func TestListMissingClustersDir(t *testing.T) {
	st, err := NewAt(t.TempDir())
	if err != nil {
		t.Fatalf("NewAt: %v", err)
	}
	if err := os.RemoveAll(filepath.Join(st.BaseDir(), "clusters")); err != nil {
		t.Fatalf("remove clusters dir: %v", err)
	}

	names, err := st.List()
	if err != nil {
		t.Fatalf("List on missing clusters dir: %v", err)
	}
	if len(names) != 0 {
		t.Errorf("List = %v, want empty", names)
	}
}

func TestDeleteEmptyClusterCleanup(t *testing.T) {
	st, err := NewAt(t.TempDir())
	if err != nil {
		t.Fatalf("NewAt: %v", err)
	}

	for _, name := range []string{"web-1", "web-2"} {
		if err := st.Create(&VM{Name: name, CreatedAt: time.Now()}); err != nil {
			t.Fatalf("Create %s: %v", name, err)
		}
	}
	clusterDir := filepath.Join(st.BaseDir(), "clusters", "web")

	// Deleting one member leaves the cluster dir (sibling remains).
	if err := st.Delete("web-1"); err != nil {
		t.Fatalf("Delete web-1: %v", err)
	}
	if _, err := os.Stat(filepath.Join(clusterDir, "web-1")); !os.IsNotExist(err) {
		t.Error("web-1 member dir should be gone")
	}
	if _, err := os.Stat(clusterDir); err != nil {
		t.Errorf("cluster dir should remain while web-2 lives: %v", err)
	}

	// Deleting the last member removes the now-empty cluster dir too.
	if err := st.Delete("web-2"); err != nil {
		t.Fatalf("Delete web-2: %v", err)
	}
	if _, err := os.Stat(clusterDir); !os.IsNotExist(err) {
		t.Error("empty cluster dir should be removed after last member")
	}
}

func TestDeleteSoloRemovesClusterDir(t *testing.T) {
	st, err := NewAt(t.TempDir())
	if err != nil {
		t.Fatalf("NewAt: %v", err)
	}
	if err := st.Create(&VM{Name: "dev", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("Create dev: %v", err)
	}
	if err := st.Delete("dev"); err != nil {
		t.Fatalf("Delete dev: %v", err)
	}
	if _, err := os.Stat(filepath.Join(st.BaseDir(), "clusters", "dev")); !os.IsNotExist(err) {
		t.Error("clusters/dev should be removed after deleting sole member dev")
	}
}

// TestNodeCollision documents the benign lossy-derivation case (D1): two
// independent solo VMs "node" and "node-2024" both resolve to cluster "node"
// and become siblings. Deleting one must leave the other and the shared
// cluster dir intact, because Delete uses os.Remove (refuses non-empty).
func TestNodeCollision(t *testing.T) {
	st, err := NewAt(t.TempDir())
	if err != nil {
		t.Fatalf("NewAt: %v", err)
	}

	for _, name := range []string{"node", "node-2024"} {
		if err := st.Create(&VM{Name: name, CreatedAt: time.Now()}); err != nil {
			t.Fatalf("Create %s: %v", name, err)
		}
	}
	clusterDir := filepath.Join(st.BaseDir(), "clusters", "node")

	if err := st.Delete("node-2024"); err != nil {
		t.Fatalf("Delete node-2024: %v", err)
	}
	if !st.Exists("node") {
		t.Error("node should still exist after deleting node-2024")
	}
	if _, err := os.Stat(clusterDir); err != nil {
		t.Errorf("shared cluster dir should remain while node lives: %v", err)
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
