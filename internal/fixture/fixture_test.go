package fixture

import (
	"os"
	"path/filepath"
	"testing"
)

// TestBuildImage proves go-ext4fs handles the file shapes that defeated ISO9660:
// a nested subdir, a name with a space, a unicode name, an empty file, and a
// symlink. It also asserts the 16 GiB canvas was resized down to fit (a few MiB),
// not left at full size. Kernel-mountability is covered by go-ext4fs's own e2e
// suite and by the VM smoke test (ADR-0015).
func TestBuildImage(t *testing.T) {
	src := t.TempDir()

	sub := filepath.Join(src, "nested dir") // space in a directory name
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(src, "with space.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write spaced file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sub, "ünïcödé.txt"), []byte("payload"), 0o600); err != nil {
		t.Fatalf("write unicode file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(src, "empty"), nil, 0o644); err != nil {
		t.Fatalf("write empty file: %v", err)
	}
	if err := os.Symlink("with space.txt", filepath.Join(src, "link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	img := filepath.Join(t.TempDir(), "fixture-0.img")
	if err := BuildImage(img, src, "FBFIX0"); err != nil {
		t.Fatalf("BuildImage: %v", err)
	}

	info, err := os.Stat(img)
	if err != nil {
		t.Fatalf("stat image: %v", err)
	}
	if info.Size() == 0 {
		t.Fatal("image is empty")
	}
	// The 16 GiB canvas must have been resized down to fit the content. Anything
	// near the canvas size means Resize did not run or did not shrink.
	const maxSize = 64 * 1024 * 1024
	if info.Size() > maxSize {
		t.Errorf("image size %d exceeds %d bytes — resize did not shrink the canvas", info.Size(), maxSize)
	}
}

// TestBuildImageMissingSource guards the reboot path where a fixture's persisted
// host dir was deleted before a later boot: BuildImage must fail with a clear
// error rather than silently producing an empty or partial image (ADR-0015, C5).
func TestBuildImageMissingSource(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	img := filepath.Join(t.TempDir(), "fixture-0.img")
	if err := BuildImage(img, missing, "FBFIX0"); err == nil {
		t.Fatal("expected an error building from a missing source directory")
	}
}
