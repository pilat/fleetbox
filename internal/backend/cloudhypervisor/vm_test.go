//go:build linux

// The --disk assembly under test is arch-independent. bootArgs now direct-boots on
// both arches (ADR-0029), extracting the guest kernel from the image on a cache
// miss; these tests pre-stage the cached vmlinux+initrd.img next to disk.raw so
// bootArgs short-circuits to the cached paths and needs no real image. The real
// extraction is exercised by the VM-boot conformance matrix (ADR-0030).

package cloudhypervisor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stagedDisk returns a disk.raw path inside a fresh temp dir with the cached
// vmlinux and initrd.img pre-created beside it, so VM.bootArgs short-circuits to the
// cached paths instead of extracting from a (nonexistent) real image.
func stagedDisk(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"vmlinux", "initrd.img"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatalf("stage %s: %v", name, err)
		}
	}
	return filepath.Join(dir, "disk.raw")
}

// TestBuildArgsFixtures confirms fixture images become additional read-only
// values on the SAME --disk flag, after the seed (ADR-0015): one --disk flag,
// disk → seed → fixtures in order, every non-disk volume readonly=on.
func TestBuildArgsFixtures(t *testing.T) {
	diskPath := stagedDisk(t)
	v := &VM{
		apiSocket:    "/run/ch.sock",
		diskPath:     diskPath,
		seedPath:     "/vm/seed.iso",
		fixturePaths: []string{"/vm/fixture-0.img", "/vm/fixture-1.img"},
		cpus:         2,
		memBytes:     4 * 1024 * 1024 * 1024,
		mac:          "52:54:00:12:34:56",
		tap:          "fbtap0",
	}

	args, err := v.buildArgs()
	if err != nil {
		t.Fatalf("buildArgs: %v", err)
	}

	diskFlags := 0
	for _, a := range args {
		if a == "--disk" {
			diskFlags++
		}
	}
	if diskFlags != 1 {
		t.Errorf("got %d --disk flags, want exactly 1: %v", diskFlags, args)
	}

	joined := strings.Join(args, " ")
	want := "--disk path=" + diskPath + " path=/vm/seed.iso,readonly=on " +
		"path=/vm/fixture-0.img,readonly=on path=/vm/fixture-1.img,readonly=on"
	if !strings.Contains(joined, want) {
		t.Errorf("disk args = %q\nwant to contain %q", joined, want)
	}
}

// TestBuildArgsNoFixtures confirms a fixtureless VM still emits exactly the disk
// and seed on one --disk flag — no stray readonly fixture values.
func TestBuildArgsNoFixtures(t *testing.T) {
	diskPath := stagedDisk(t)
	v := &VM{
		apiSocket: "/run/ch.sock",
		diskPath:  diskPath,
		seedPath:  "/vm/seed.iso",
		cpus:      2,
		memBytes:  2 * 1024 * 1024 * 1024,
		mac:       "52:54:00:12:34:56",
		tap:       "fbtap0",
	}

	args, err := v.buildArgs()
	if err != nil {
		t.Fatalf("buildArgs: %v", err)
	}
	joined := strings.Join(args, " ")
	want := "--disk path=" + diskPath + " path=/vm/seed.iso,readonly=on --cpus"
	if !strings.Contains(joined, want) {
		t.Errorf("disk args = %q\nwant to contain %q", joined, want)
	}
}
