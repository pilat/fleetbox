package orchestrator

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/pilat/fleetbox/internal/opts"
	"github.com/pilat/fleetbox/internal/seed"
	"github.com/pilat/fleetbox/internal/store"
)

func TestToStoreFixturesAssignsLabelsAndAbsolutizes(t *testing.T) {
	d0, d1 := t.TempDir(), t.TempDir()

	got, err := toStoreFixtures([]opts.Fixture{
		{HostPath: d0, GuestPath: "/work"},
		{HostPath: d1, GuestPath: "/data"},
	})
	if err != nil {
		t.Fatalf("toStoreFixtures: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].Label != "FBFIX0" || got[1].Label != "FBFIX1" {
		t.Errorf("labels = %q,%q, want FBFIX0,FBFIX1", got[0].Label, got[1].Label)
	}
	if got[0].GuestPath != "/work" || got[1].GuestPath != "/data" {
		t.Errorf("guest paths = %q,%q", got[0].GuestPath, got[1].GuestPath)
	}
	for i, f := range got {
		if !filepath.IsAbs(f.HostPath) {
			t.Errorf("fixture %d host path %q not absolute", i, f.HostPath)
		}
	}
}

func TestToStoreFixturesAbsolutizesRelativeHostPath(t *testing.T) {
	// "." is the working directory, which always exists and is a dir — a relative
	// host path must be absolutized, not rejected.
	got, err := toStoreFixtures([]opts.Fixture{{HostPath: ".", GuestPath: "/work"}})
	if err != nil {
		t.Fatalf("toStoreFixtures: %v", err)
	}
	if !filepath.IsAbs(got[0].HostPath) {
		t.Errorf("host path %q not absolutized", got[0].HostPath)
	}
}

func TestToStoreFixturesRejectsRelativeGuestPath(t *testing.T) {
	if _, err := toStoreFixtures([]opts.Fixture{{HostPath: t.TempDir(), GuestPath: "work"}}); err == nil {
		t.Fatal("expected error for relative guest path")
	}
}

func TestToStoreFixturesRejectsMissingHostPath(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope")
	if _, err := toStoreFixtures([]opts.Fixture{{HostPath: missing, GuestPath: "/work"}}); err == nil {
		t.Fatal("expected error for missing host path")
	}
}

func TestToStoreFixturesRejectsFileHostPath(t *testing.T) {
	file := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if _, err := toStoreFixtures([]opts.Fixture{{HostPath: file, GuestPath: "/work"}}); err == nil {
		t.Fatal("expected error for host path that is a file, not a directory")
	}
}

func TestToStoreFixturesRejectsDuplicateGuestPath(t *testing.T) {
	dir := t.TempDir()
	if _, err := toStoreFixtures([]opts.Fixture{
		{HostPath: dir, GuestPath: "/work"},
		{HostPath: dir, GuestPath: "/work"},
	}); err == nil {
		t.Fatal("expected error for duplicate guest path")
	}
}

func TestToStoreFixturesEmpty(t *testing.T) {
	got, err := toStoreFixtures(nil)
	if err != nil {
		t.Fatalf("toStoreFixtures(nil): %v", err)
	}
	if got != nil {
		t.Errorf("got %v, want nil", got)
	}
}

func TestToSeedFixtures(t *testing.T) {
	got := toSeedFixtures([]store.Fixture{
		{HostPath: "/h0", GuestPath: "/work", Label: "FBFIX0"},
		{HostPath: "/h1", GuestPath: "/data", Label: "FBFIX1"},
	})
	want := []seed.Fixture{{Label: "FBFIX0", GuestPath: "/work"}, {Label: "FBFIX1", GuestPath: "/data"}}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("seed fixture %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}
