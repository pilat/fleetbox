package image

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
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

func TestVerifyChecksum(t *testing.T) {
	content := []byte("checksum me")
	sum := sha256.Sum256(content)
	digest := hex.EncodeToString(sum[:])
	path := writeTemp(t, "blob.raw", content)

	if err := verifyChecksum(path, digest); err != nil {
		t.Errorf("verifyChecksum(correct) = %v, want nil", err)
	}

	// Case-insensitive: an uppercase expected digest still matches.
	if err := verifyChecksum(path, strings.ToUpper(digest)); err != nil {
		t.Errorf("verifyChecksum(uppercase) = %v, want nil", err)
	}

	wrong := strings.Repeat("0", len(digest))
	if err := verifyChecksum(path, wrong); err == nil {
		t.Error("verifyChecksum(wrong) = nil, want error")
	} else if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Errorf("error %q does not name the mismatch cause", err)
	}

	if err := verifyChecksum(filepath.Join(t.TempDir(), "missing"), digest); err == nil {
		t.Error("verifyChecksum(missing file) = nil, want error")
	}
}

// TestEnsureCacheHit pins the cache short-circuit and the URL -> raw-filename
// derivation, with no network. Note the derivation appends ".raw"
// unconditionally after stripping .qcow2/.img: debian's URL already ends in
// .raw, so its cached name is the double-".raw.raw" below — a quirk this test
// documents. ubuntu's .img URL derives cleanly to a single .raw.
func TestEnsureCacheHit(t *testing.T) {
	cases := []struct {
		alias       string
		wantRawName string
	}{
		{"debian-12", "debian-12-generic-arm64.raw.raw"},
		{"ubuntu-24.04", "ubuntu-24.04-server-cloudimg-arm64.raw"},
	}
	for _, tc := range cases {
		t.Run(tc.alias, func(t *testing.T) {
			cacheDir := t.TempDir()
			rawPath := filepath.Join(cacheDir, tc.wantRawName)
			if err := os.WriteFile(rawPath, []byte("cached raw disk"), 0o644); err != nil {
				t.Fatalf("seed cache: %v", err)
			}

			got, err := Ensure(cacheDir, tc.alias)
			if err != nil {
				t.Fatalf("Ensure: %v", err)
			}
			if got != rawPath {
				t.Errorf("Ensure = %q, want cached %q", got, rawPath)
			}
		})
	}
}
