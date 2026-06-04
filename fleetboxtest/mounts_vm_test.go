//go:build darwin && arm64

package fleetboxtest_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pilat/fleetbox"
	"github.com/pilat/fleetbox/fleetboxtest"
)

// TestVMMountsLive is the real proof of ADR-0010: a host directory shared via
// WithMount appears live and read-write inside the guest, host-owned files line
// up with the guest login user (uid alignment), and git accepts the tree.
//
// Named with the TestVM prefix so `make test-vm` (-test.run TestVM) runs it.
// Boots a real VM: skipped on unsupported platforms (via the fixture) and -short.
func TestVMMountsLive(t *testing.T) {
	fleetboxtest.SkipIfShort(t, "boots real VMs")

	hostDir := t.TempDir()
	vm := fleetboxtest.Start(t, fleetbox.Debian12, fleetbox.WithMount(hostDir, "/work"))
	ctx := context.Background()

	// Host -> guest: a file written on the host is visible inside the guest.
	if err := os.WriteFile(filepath.Join(hostDir, "hello.txt"), []byte("from-host\n"), 0o644); err != nil {
		t.Fatalf("write host file: %v", err)
	}
	out, err := vm.SSH(ctx, "cat /work/hello.txt")
	if err != nil {
		t.Fatalf("cat /work/hello.txt: %v\n%s", err, out)
	}
	if strings.TrimSpace(out) != "from-host" {
		t.Errorf("guest read %q, want from-host", out)
	}

	// Guest -> host: a file written in the guest is visible on the host.
	out, err = vm.SSH(ctx, "echo from-guest > /work/out.txt")
	if err != nil {
		t.Fatalf("write guest file: %v\n%s", err, out)
	}
	data, err := os.ReadFile(filepath.Join(hostDir, "out.txt"))
	if err != nil {
		t.Fatalf("read host out.txt: %v", err)
	}
	if strings.TrimSpace(string(data)) != "from-guest" {
		t.Errorf("host read %q, want from-guest", data)
	}

	// uid alignment: host-owned files show up under the guest login user's uid,
	// which is pinned to the host uid (virtiofs identity pass-through, ADR-0010).
	out, err = vm.SSH(ctx, "stat -c %u /work/hello.txt")
	if err != nil {
		t.Fatalf("stat: %v\n%s", err, out)
	}
	if got, want := strings.TrimSpace(out), strconv.Itoa(os.Getuid()); got != want {
		t.Errorf("guest uid of /work/hello.txt = %q, want %q (host uid)", got, want)
	}

	// git is happy — no "dubious ownership" — because the tree is owned by the
	// guest's own uid. Cloud images may not ship git, so install it on demand.
	const ensureGit = "if ! command -v git >/dev/null; then " +
		"sudo DEBIAN_FRONTEND=noninteractive apt-get update -qq && " +
		"sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq git; fi"
	out, err = vm.SSH(ctx, ensureGit)
	if err != nil {
		t.Fatalf("ensure git installed: %v\n%s", err, out)
	}
	out, err = vm.SSH(ctx, "cd /work && git init -q && git status")
	if err != nil {
		t.Fatalf("git in /work: %v\n%s", err, out)
	}
	if strings.Contains(out, "dubious ownership") {
		t.Errorf("git reported dubious ownership despite uid alignment:\n%s", out)
	}
}

// TestVMMountsPersistAcrossReboot proves mounts are frozen at birth and survive
// a Stop/Start with NO options re-passed — re-attached from the persisted store
// (host device) and /etc/fstab (guest), with no cloud-init re-run.
//
// It cannot use the fleetboxtest fixture (which owns naming + cleanup and will
// not let the same name be stopped and re-started), so it drives the raw
// fleetbox.Start / vm.Stop / fleetbox.Start API and manages the name + teardown.
func TestVMMountsPersistAcrossReboot(t *testing.T) {
	fleetboxtest.SkipIfShort(t, "boots real VMs")
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" || !fleetbox.NestedVirtSupported() {
		t.Skip("requires darwin/arm64 with nested virtualization")
	}

	// A fixed name (unique to this test) so the second Start re-targets the same
	// VM. Like the rest of the VM suite, cleanup is via defer Destroy; a leftover
	// from a crashed prior run is the same accepted limitation every fixed-name VM
	// test has, and is not worth reaching past the public API to pre-clean.
	const name = "vmmount-persist"

	hostDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(hostDir, "marker.txt"), []byte("persisted\n"), 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	ctx := context.Background()

	vm, err := fleetbox.Start(ctx, name,
		fleetbox.WithImage(fleetbox.Debian12),
		fleetbox.WithMount(hostDir, "/work"),
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

	// Re-Start the same name with NO mount option. If /work is still mounted, it
	// came from the store + fstab, not from options — exactly the frozen-at-birth
	// guarantee.
	vm, err = fleetbox.Start(ctx, name)
	if err != nil {
		t.Fatalf("second Start: %v", err)
	}
	if out, err := vm.SSH(ctx, "cat /work/marker.txt"); err != nil || strings.TrimSpace(out) != "persisted" {
		t.Fatalf("second boot: mount did not persist: out=%q err=%v", out, err)
	}
}
