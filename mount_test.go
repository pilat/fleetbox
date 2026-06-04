//go:build darwin && arm64

package fleetbox

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/pilat/fleetbox/internal/backend"
	"github.com/pilat/fleetbox/internal/seed"
	"github.com/pilat/fleetbox/internal/store"
)

func TestWithMountAppliesToOptions(t *testing.T) {
	var o Options
	WithMount("a", "/b")(&o)
	WithMount("c", "/d")(&o)

	want := []Mount{{HostPath: "a", GuestPath: "/b"}, {HostPath: "c", GuestPath: "/d"}}
	if len(o.Mounts) != len(want) {
		t.Fatalf("Mounts len = %d, want %d", len(o.Mounts), len(want))
	}
	for i, m := range o.Mounts {
		if m != want[i] {
			t.Errorf("Mounts[%d] = %+v, want %+v", i, m, want[i])
		}
	}
}

func TestToStoreMountsAssignsTagsAndAbsolutizes(t *testing.T) {
	dir := t.TempDir()

	got, err := toStoreMounts([]Mount{
		{HostPath: dir, GuestPath: "/work"},
		{HostPath: dir, GuestPath: "/data"},
	})
	if err != nil {
		t.Fatalf("toStoreMounts: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].Tag != "fbm0" || got[1].Tag != "fbm1" {
		t.Errorf("tags = %q,%q, want fbm0,fbm1", got[0].Tag, got[1].Tag)
	}
	if got[0].GuestPath != "/work" || got[1].GuestPath != "/data" {
		t.Errorf("guest paths = %q,%q", got[0].GuestPath, got[1].GuestPath)
	}
	for i, m := range got {
		if !filepath.IsAbs(m.HostPath) {
			t.Errorf("mount %d host path %q not absolute", i, m.HostPath)
		}
	}
}

func TestToStoreMountsAbsolutizesRelativeHostPath(t *testing.T) {
	// "." is the working directory, which always exists — a relative host path
	// must be absolutized, not rejected.
	got, err := toStoreMounts([]Mount{{HostPath: ".", GuestPath: "/work"}})
	if err != nil {
		t.Fatalf("toStoreMounts: %v", err)
	}
	if !filepath.IsAbs(got[0].HostPath) {
		t.Errorf("host path %q not absolutized", got[0].HostPath)
	}
}

func TestToStoreMountsRejectsRelativeGuestPath(t *testing.T) {
	if _, err := toStoreMounts([]Mount{{HostPath: t.TempDir(), GuestPath: "work"}}); err == nil {
		t.Fatal("expected error for relative guest path")
	}
}

func TestToStoreMountsRejectsMissingHostPath(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope")
	if _, err := toStoreMounts([]Mount{{HostPath: missing, GuestPath: "/work"}}); err == nil {
		t.Fatal("expected error for missing host path")
	}
}

func TestToStoreMountsEmpty(t *testing.T) {
	got, err := toStoreMounts(nil)
	if err != nil {
		t.Fatalf("toStoreMounts(nil): %v", err)
	}
	if got != nil {
		t.Errorf("got %v, want nil", got)
	}
}

// TestMountsSurviveRebootWithoutOptions proves the persistence path: mounts
// assigned at create time round-trip through Save/Load and yield identical
// backend mounts on a simulated reboot — without re-reading options and without
// re-validating the (now-deletable) host directory.
func TestMountsSurviveRebootWithoutOptions(t *testing.T) {
	st, err := store.NewAt(t.TempDir())
	if err != nil {
		t.Fatalf("NewAt: %v", err)
	}

	hostDir := t.TempDir()
	mounts, err := toStoreMounts([]Mount{{HostPath: hostDir, GuestPath: "/work"}})
	if err != nil {
		t.Fatalf("toStoreMounts: %v", err)
	}

	vm := &store.VM{Name: "rebooter", Mounts: mounts}
	if err := st.Create(vm); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Delete the host dir before the reboot path runs: our layer must not
	// re-validate it (create-time concern only), so the load + backend-mount
	// build below must still succeed with the dir gone.
	if err := os.RemoveAll(hostDir); err != nil {
		t.Fatalf("remove host dir: %v", err)
	}

	// Reboot: load from store, build backend mounts. No options consulted.
	loaded, err := st.Load("rebooter")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := toBackendMounts(loaded.Mounts)

	want := []backend.Mount{{HostPath: mounts[0].HostPath, Tag: "fbm0"}}
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("backend mounts = %+v, want %+v", got, want)
	}
}

func TestToSeedMounts(t *testing.T) {
	got := toSeedMounts([]store.Mount{
		{HostPath: "/h0", GuestPath: "/work", Tag: "fbm0"},
		{HostPath: "/h1", GuestPath: "/data", Tag: "fbm1"},
	})
	want := []seed.Mount{{Tag: "fbm0", GuestPath: "/work"}, {Tag: "fbm1", GuestPath: "/data"}}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("seed mount %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}
