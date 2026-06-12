//go:build linux && amd64

// The --disk assembly under test is arch-independent; this runs on amd64, where
// bootArgs is the firmware no-op and needs no real disk. The arm64 direct-kernel
// boot (which extracts a kernel from a real image) is exercised by the nested
// integration test instead (ADR-0024).

package cloudhypervisor

import (
	"strings"
	"testing"
)

// TestBuildArgsFixtures confirms fixture images become additional read-only
// values on the SAME --disk flag, after the seed (ADR-0015): one --disk flag,
// disk → seed → fixtures in order, every non-disk volume readonly=on.
func TestBuildArgsFixtures(t *testing.T) {
	v := &VM{
		apiSocket:    "/run/ch.sock",
		fwPath:       "/bin/fw",
		diskPath:     "/vm/disk.raw",
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
	want := "--disk path=/vm/disk.raw path=/vm/seed.iso,readonly=on " +
		"path=/vm/fixture-0.img,readonly=on path=/vm/fixture-1.img,readonly=on"
	if !strings.Contains(joined, want) {
		t.Errorf("disk args = %q\nwant to contain %q", joined, want)
	}
}

// TestBuildArgsNoFixtures confirms a fixtureless VM still emits exactly the disk
// and seed on one --disk flag — no stray readonly fixture values.
func TestBuildArgsNoFixtures(t *testing.T) {
	v := &VM{
		apiSocket: "/run/ch.sock",
		fwPath:    "/bin/fw",
		diskPath:  "/vm/disk.raw",
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
	want := "--disk path=/vm/disk.raw path=/vm/seed.iso,readonly=on --cpus"
	if !strings.Contains(joined, want) {
		t.Errorf("disk args = %q\nwant to contain %q", joined, want)
	}
}
