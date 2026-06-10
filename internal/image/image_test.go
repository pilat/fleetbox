package image

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func writeTemp(t *testing.T, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return p
}

func TestCopyDiskExtend(t *testing.T) {
	content := []byte("hello disk")
	src := writeTemp(t, "src.raw", content)
	dst := filepath.Join(t.TempDir(), "dst.raw")

	want := int64(len(content)) + 4096
	if err := CopyDisk(src, dst, want); err != nil {
		t.Fatalf("CopyDisk: %v", err)
	}

	info, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("stat dst: %v", err)
	}
	if info.Size() != want {
		t.Errorf("dst size = %d, want %d", info.Size(), want)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if !bytes.Equal(got[:len(content)], content) {
		t.Errorf("first %d bytes = %q, want %q", len(content), got[:len(content)], content)
	}
}

func TestCopyDiskEqual(t *testing.T) {
	content := []byte("exact size payload")
	src := writeTemp(t, "src.raw", content)
	dst := filepath.Join(t.TempDir(), "dst.raw")

	if err := CopyDisk(src, dst, int64(len(content))); err != nil {
		t.Fatalf("CopyDisk: %v", err)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("dst = %q, want %q", got, content)
	}
}

func TestCopyDiskShrinkErrorsAndLeavesNoDst(t *testing.T) {
	content := []byte("this is the base image")
	src := writeTemp(t, "src.raw", content)
	dst := filepath.Join(t.TempDir(), "dst.raw")

	err := CopyDisk(src, dst, int64(len(content))-1)
	if err == nil {
		t.Fatal("CopyDisk(shrink) = nil error, want error")
	}
	if !strings.Contains(err.Error(), "smaller than base image") {
		t.Errorf("error %q does not name the shrink cause", err)
	}
	// The guard fires before the destination is created, so no corrupt dst is
	// left behind.
	if _, statErr := os.Stat(dst); !os.IsNotExist(statErr) {
		t.Errorf("dst exists after shrink error (stat err = %v), want not-exist", statErr)
	}
}

func TestCopyDiskShrinkLeavesExistingDstUntouched(t *testing.T) {
	content := []byte("this is the base image")
	src := writeTemp(t, "src.raw", content)
	existing := []byte("PRE-EXISTING DESTINATION")
	dst := writeTemp(t, "dst.raw", existing)

	if err := CopyDisk(src, dst, int64(len(content))-1); err == nil {
		t.Fatal("CopyDisk(shrink) = nil error, want error")
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if !bytes.Equal(got, existing) {
		t.Errorf("dst = %q, want unchanged %q", got, existing)
	}
}

func TestCopyDiskPassthrough(t *testing.T) {
	content := []byte("no truncate when size is zero")
	src := writeTemp(t, "src.raw", content)
	dst := filepath.Join(t.TempDir(), "dst.raw")

	if err := CopyDisk(src, dst, 0); err != nil {
		t.Fatalf("CopyDisk: %v", err)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("dst = %q, want exact copy %q", got, content)
	}
}

// TestCacheName pins the snapshot-stamped raw cache-filename derivation directly.
func TestCacheName(t *testing.T) {
	got := cacheName("debian-12", "20260601-2496", "amd64")
	want := "debian-12-20260601-2496-amd64.raw"
	if got != want {
		t.Errorf("cacheName = %q, want %q", got, want)
	}
}

// TestEnsureCacheHit pins the cache short-circuit for a catalog alias, with no
// network: a pre-seeded snapshot-stamped raw (the name Ensure now derives from the
// catalog's pinned snapshot for the current GOARCH) is returned as-is. The old
// double-".raw.raw" quirk is gone — the name is now <alias>-<snapshot>-<arch>.raw.
func TestEnsureCacheHit(t *testing.T) {
	catalog, err := loadCatalog()
	if err != nil {
		t.Fatalf("loadCatalog: %v", err)
	}
	arch := runtime.GOARCH
	for _, alias := range []string{"debian-12", "ubuntu-24.04"} {
		t.Run(alias, func(t *testing.T) {
			info, ok := catalog[alias]
			if !ok {
				t.Fatalf("catalog missing %q", alias)
			}
			cacheDir := t.TempDir()
			rawPath := filepath.Join(cacheDir, cacheName(alias, info.Snapshot, arch))
			if err := os.WriteFile(rawPath, []byte("cached raw disk"), 0o644); err != nil {
				t.Fatalf("seed cache: %v", err)
			}

			got, err := Ensure(cacheDir, alias)
			if err != nil {
				t.Fatalf("Ensure: %v", err)
			}
			if got != rawPath {
				t.Errorf("Ensure = %q, want cached %q", got, rawPath)
			}
		})
	}
}

// TestEnsureCacheHitLiteralURL locks the unchanged literal-URL branch: a non-alias
// input keeps its basename-derived cache name (here fbtiny -> fbtiny.raw), the same
// derivation coord_test.go / orchestrator_fake_test.go depend on.
func TestEnsureCacheHitLiteralURL(t *testing.T) {
	cacheDir := t.TempDir()
	rawPath := filepath.Join(cacheDir, "fbtiny.raw")
	if err := os.WriteFile(rawPath, []byte("cached raw disk"), 0o644); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	got, err := Ensure(cacheDir, "https://invalid.test/fbtiny")
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if got != rawPath {
		t.Errorf("Ensure = %q, want cached %q", got, rawPath)
	}
}
