//go:build darwin && arm64

package fleetboxtest_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/pilat/fleetbox"
	"github.com/pilat/fleetbox/fleetboxtest"
)

// TestVMFixtureReadOnly is the end-to-end proof of ADR-0015: a host directory
// packed via WithFixture is built into an ext4 image, attached read-only, and
// mounted by the stock guest at the requested path. It asserts the content is
// readable — including a long unicode, spaced filename that ISO9660's 31-byte
// charset-restricted names could not carry, proving the ext4 advantage — and
// that the mount is read-only (a write fails).
//
// Named with the TestVM prefix so `make test-vm` (-test.run TestVM) runs it.
// Boots a real VM: skipped on unsupported platforms (via the fixture) and -short.
func TestVMFixtureReadOnly(t *testing.T) {
	fleetboxtest.SkipIfShort(t, "boots real VMs")

	hostDir := t.TempDir()
	const fname = "a long file name with spaces and ünïcödé.txt"
	const content = "fixture-payload\n"
	if err := os.WriteFile(filepath.Join(hostDir, fname), []byte(content), 0o644); err != nil {
		t.Fatalf("write host fixture file: %v", err)
	}

	vm := fleetboxtest.Start(t, fleetbox.Debian12, fleetbox.WithFixture(hostDir, "/work"))
	ctx := context.Background()

	// The fixture content is present at the guest path with matching bytes. The
	// single-quoted path carries the spaces and unicode through the shell as-is.
	out, err := vm.SSH(ctx, "cat '/work/"+fname+"'")
	if err != nil {
		t.Fatalf("cat fixture file: %v\n%s", err, out)
	}
	if strings.TrimSpace(out) != strings.TrimSpace(content) {
		t.Errorf("guest read %q, want %q", out, content)
	}

	// The mount is read-only: a write into /work must fail (non-zero exit).
	if out, err := vm.SSH(ctx, "touch /work/should-fail"); err == nil {
		t.Errorf("write to /work unexpectedly succeeded (mount not read-only):\n%s", out)
	}
}

// TestVMFixturesPersistAcrossReboot proves a fixture is frozen at birth and
// survives a Stop/Start with NO options re-passed: the seed's LABEL= fstab entry
// persists on the guest disk, and the per-boot rebuilt ext4 image keeps the same
// stable label, so the guest re-mounts it without a cloud-init re-run (ADR-0015).
//
// It cannot use the fleetboxtest fixture (which owns naming + cleanup and will not
// let the same name be stopped and re-started), so it drives the raw
// fleetbox.Start / vm.Stop / fleetbox.Start API and manages the name + teardown.
func TestVMFixturesPersistAcrossReboot(t *testing.T) {
	fleetboxtest.SkipIfShort(t, "boots real VMs")
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" || !fleetbox.NestedVirtSupported() {
		t.Skip("requires darwin/arm64 with nested virtualization")
	}

	// A fixed name (unique to this test) so the second Start re-targets the same VM.
	// Cleanup is via defer Destroy; a leftover from a crashed prior run is the same
	// accepted limitation every fixed-name VM test has.
	const name = "vmfixture-persist"

	hostDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(hostDir, "marker.txt"), []byte("persisted\n"), 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	ctx := context.Background()

	vm, err := fleetbox.Start(ctx, name,
		fleetbox.WithImage(fleetbox.Debian12),
		fleetbox.WithFixture(hostDir, "/work"),
	)
	if err != nil {
		t.Fatalf("first Start: %v", err)
	}
	// Destroy whichever VM handle is current when the test ends.
	defer func() {
		dctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := vm.Destroy(dctx); err != nil {
			t.Logf("warning: destroy %s: %v", name, err)
		}
	}()

	if out, err := vm.SSH(ctx, "cat /work/marker.txt"); err != nil || strings.TrimSpace(out) != "persisted" {
		t.Fatalf("first boot: /work not mounted: out=%q err=%v", out, err)
	}

	if err := vm.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// Re-Start the same name with NO fixture option. If /work is still mounted, it
	// came from the persisted store + fstab and the per-boot rebuilt same-label
	// image, not from options — exactly the frozen-at-birth guarantee.
	vm, err = fleetbox.Start(ctx, name)
	if err != nil {
		t.Fatalf("second Start: %v", err)
	}
	if out, err := vm.SSH(ctx, "cat /work/marker.txt"); err != nil || strings.TrimSpace(out) != "persisted" {
		t.Fatalf("second boot: fixture did not persist: out=%q err=%v", out, err)
	}
}
