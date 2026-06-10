//go:build fleetbox_fake

package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/pilat/fleetbox/internal/backend/fake"
	"github.com/pilat/fleetbox/internal/opts"
	"github.com/pilat/fleetbox/internal/store"
)

// These tests run the REAL in-process orchestrator (resolveStartDeps →
// startOnNetwork → Cluster) against the fake backend, under the fleetbox_fake
// build tag. They prove the coordination and the artifact glue — store create,
// disk copy, seed ISO, fixture build, network lifecycle — not that a VM boots
// (the fake never boots one). Every assertion targets a recorded backend.Config
// field or an on-disk file the real orchestrator wrote, never a value the fake
// itself invents (e.g. the IP). Real boot/SSH/IP discovery stay covered by the
// VM-boot suites (make test-vm, vm-linux.yml).

// tinyImageURL resolves, in image.Ensure, to the cache filename "fbtiny.raw":
// Ensure takes the URL basename ("fbtiny"), strips any .qcow2/.img suffix, and
// appends ".raw". setupFakeEnv pre-seeds that file so Ensure returns it
// immediately and never reaches the network — and the name is arch-independent,
// unlike the default debian-12-generic-<arch>.raw.
const tinyImageURL = "https://invalid.test/fbtiny"

func TestStartSingleVMRecordsConfigAndWritesArtifacts(t *testing.T) {
	st := setupFakeEnv(t)
	fake.Reset()

	ctx := context.Background()
	vm, err := Start(ctx, "solo", opts.WithImage(tinyImageURL), opts.WithDiskGB(1))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	if got := vm.State(); got != "running" {
		t.Errorf("State() = %q, want running", got)
	}

	created := fake.Created()
	if len(created) != 1 {
		t.Fatalf("Created() len = %d, want 1", len(created))
	}
	cfg := created[0]
	if cfg.Name != "solo" {
		t.Errorf("Config.Name = %q, want solo", cfg.Name)
	}
	// DHCP/vz path: fakeNetwork.Subnet()=="" so startOnNetwork skips allocateIP
	// and emits no static IP. (Static-IP allocation is unit tested in ipalloc_test.)
	if cfg.AssignedIP != "" {
		t.Errorf("Config.AssignedIP = %q, want empty (no static IP on the DHCP path)", cfg.AssignedIP)
	}
	// The real artifact code ran: assert the threaded paths are non-empty AND the
	// files exist on disk (a green test that ignored these would pass with empties).
	if cfg.DiskPath == "" || cfg.SeedPath == "" {
		t.Fatalf("Config disk/seed paths empty: disk=%q seed=%q", cfg.DiskPath, cfg.SeedPath)
	}
	assertFileNonEmpty(t, st.DiskPath("solo")) // real image.CopyDisk
	assertFileNonEmpty(t, st.SeedPath("solo")) // real seed.Create

	// Destroy on a sole-owner VM stops the backend and closes its network.
	if err := vm.Destroy(ctx); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if got := fake.Stopped(); !slices.Contains(got, "solo") {
		t.Errorf("Stopped() = %v, want it to contain solo", got)
	}
	if got := fake.NetworksClosed(); got != 1 {
		t.Errorf("NetworksClosed() = %d, want 1", got)
	}
}

func TestStartWithFixtureBuildsImage(t *testing.T) {
	st := setupFakeEnv(t)
	fake.Reset()

	hostDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(hostDir, "data.txt"), []byte("payload"), 0o644); err != nil {
		t.Fatalf("write fixture source: %v", err)
	}

	ctx := context.Background()
	_, err := Start(ctx, "fix",
		opts.WithImage(tinyImageURL), opts.WithDiskGB(1), opts.WithFixture(hostDir, "/mnt/data"))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	created := fake.Created()
	if len(created) != 1 {
		t.Fatalf("Created() len = %d, want 1", len(created))
	}
	if got := len(created[0].FixturePaths); got != 1 {
		t.Fatalf("Config.FixturePaths len = %d, want 1", got)
	}
	// The real fixture.BuildImage ran and produced the on-disk ext4 image the
	// config points the backend at.
	assertFileNonEmpty(t, st.FixturePath("fix", 0))
	if created[0].FixturePaths[0] != st.FixturePath("fix", 0) {
		t.Errorf("FixturePaths[0] = %q, want %q", created[0].FixturePaths[0], st.FixturePath("fix", 0))
	}
}

func TestClusterAddsMembersAndTearsDown(t *testing.T) {
	setupFakeEnv(t)
	fake.Reset()

	ctx := context.Background()
	c, err := NewCluster(opts.WithImage(tinyImageURL), opts.WithDiskGB(1))
	if err != nil {
		t.Fatalf("NewCluster: %v", err)
	}

	names := []string{"web-1", "web-2", "web-3"}
	for _, name := range names {
		if _, err := c.Add(ctx, name); err != nil {
			t.Fatalf("Add %s: %v", name, err)
		}
	}

	if got := len(c.VMs()); got != len(names) {
		t.Fatalf("VMs() len = %d, want %d", got, len(names))
	}
	created := fake.Created()
	if len(created) != len(names) {
		t.Fatalf("Created() len = %d, want %d", len(created), len(names))
	}
	gotNames := make(map[string]bool, len(created))
	for _, cfg := range created {
		gotNames[cfg.Name] = true
	}
	for _, name := range names {
		if !gotNames[name] {
			t.Errorf("Created() missing member %q (got %v)", name, gotNames)
		}
	}

	// Teardown: stop every member, then close the shared network once.
	for _, vm := range c.VMs() {
		if err := vm.Stop(ctx); err != nil {
			t.Fatalf("Stop %s: %v", vm.Name(), err)
		}
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	stopped := fake.Stopped()
	for _, name := range names {
		if !slices.Contains(stopped, name) {
			t.Errorf("Stopped() = %v, want it to contain %q", stopped, name)
		}
	}
	if got := fake.NetworksClosed(); got != 1 {
		t.Errorf("NetworksClosed() = %d, want 1 (one shared cluster network)", got)
	}
}

func TestStartCreateFailureClosesNetwork(t *testing.T) {
	setupFakeEnv(t)
	fake.Reset()
	fake.FailCreate("boom")

	ctx := context.Background()
	if _, err := Start(ctx, "boom", opts.WithImage(tinyImageURL), opts.WithDiskGB(1)); err == nil {
		t.Fatal("Start = nil error, want the forced Create failure")
	}

	// orchestrator.go closes the sole-owner network when startOnNetwork fails.
	if got := fake.NetworksClosed(); got != 1 {
		t.Errorf("NetworksClosed() = %d, want 1 (network released on Create failure)", got)
	}
	// Nothing booted, so nothing was stopped — this is single-Start failure, not
	// the cluster all-or-nothing teardown (that path lives in the holder; see T4).
	if got := fake.Stopped(); len(got) != 0 {
		t.Errorf("Stopped() = %v, want empty (nothing booted)", got)
	}
}

// setupFakeEnv points HOME at a fresh temp dir, creates the store there, and
// pre-seeds the tiny raw image so image.Ensure serves it from cache (no network).
// It returns a store rooted at the same HOME for path assertions.
func setupFakeEnv(t *testing.T) *store.Store {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	st, err := store.New()
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	raw := filepath.Join(st.ImagesDir(), "fbtiny.raw")
	if err := os.WriteFile(raw, []byte("fleetbox-fake-tiny-image\n"), 0o644); err != nil {
		t.Fatalf("seed image %s: %v", raw, err)
	}
	return st
}

func assertFileNonEmpty(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if info.Size() == 0 {
		t.Fatalf("%s exists but is empty", path)
	}
}
