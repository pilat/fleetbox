//go:build fleetbox_nested

// This is the dogfood gate: use fleetbox to test fleetbox. On an Apple-Silicon mac
// (M3+, macOS 26+) fleetbox boots a Linux guest with a working nested /dev/kvm,
// then a freshly built linux/arm64 fleetbox boots a NESTED VM inside it via the
// arm64 direct-kernel path (ADR-0024) — the path that does NOT run under the
// rust-hypervisor-firmware the firmware boot used.
//
// It is gated behind the `fleetbox_nested` build tag and is LOCAL-ONLY for now:
// there is no CI lane because the runner must be an M3+ macOS host. The vector is
// left for a future CI job. Run it locally with the signed helper:
//
//	FLEETBOX_HELPER=$PWD/bin/fleetbox-helper \
//	  go test -tags fleetbox_nested -run TestNestedLinuxBoot -timeout 30m -v ./fleetboxtest
//
// Note: a nested boot is slow; if the inner `up` trips the holder's IP-wait, the
// serial log under the inner ~/.fleetbox shows the boot progressing — nested timing,
// not the direct-kernel path, is the cause.
package fleetboxtest

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/pilat/fleetbox"
)

// elevateInGuest runs a privileged fleetbox command inside the guest as root. The
// cloud-init user has passwordless sudo; FLEETBOX_ELEVATED short-circuits the CLI's
// own auto-elevation, the PATH carries sbin so the holder finds ip/iptables, and
// FLEETBOX_IP_WAIT_TIMEOUT widens the IP-wait because a doubly-nested boot is slow.
const elevateInGuest = "sudo -n env FLEETBOX_ELEVATED=1 FLEETBOX_IP_WAIT_TIMEOUT=15m " +
	"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin /tmp/fleetbox"

func TestNestedLinuxBoot(t *testing.T) {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Skip("nested boot needs an Apple-Silicon mac (M3+, macOS 26+)")
	}
	if os.Getenv("FLEETBOX_HELPER") == "" {
		t.Skip("set FLEETBOX_HELPER to the signed helper (see `make helper` / make test-vm)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()

	// 1. Build the linux/arm64 fleetbox that will run inside the guest.
	bin := filepath.Join(t.TempDir(), "fleetbox-linux-arm64")
	build := exec.CommandContext(ctx, "go", "build", "-o", bin, "../cmd/fleetbox")
	build.Env = append(os.Environ(), "GOOS=linux", "GOARCH=arm64", "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build linux/arm64 fleetbox: %v\n%s", err, out)
	}

	// 2. Boot the outer Linux guest; on M3+ it gets a working /dev/kvm (nested virt).
	outer := Start(t, "debian-12",
		fleetbox.WithCPUs(4), fleetbox.WithMemoryGB(8), fleetbox.WithDiskGB(40))

	if out, err := outer.SSH(ctx, "test -e /dev/kvm && echo kvm-ok"); err != nil ||
		!strings.Contains(out, "kvm-ok") {
		t.Fatalf("guest has no /dev/kvm (nested virt unavailable?): %v\n%s", err, out)
	}

	// 3. Push the linux fleetbox in (the public VM API exposes SSH but not copy, so
	//    scp directly against the VM IP with fleetbox's per-installation key).
	scp := exec.CommandContext(ctx, "scp",
		"-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null",
		"-i", sshKeyPath(t), bin, "fleetbox@"+outer.IP().String()+":/tmp/fleetbox")
	if out, err := scp.CombinedOutput(); err != nil {
		t.Fatalf("scp fleetbox into guest: %v\n%s", err, out)
	}

	// 4. Boot a NESTED VM inside the guest with the new arm64 direct-kernel path.
	if out, err := outer.SSH(ctx, "chmod +x /tmp/fleetbox && "+elevateInGuest+" up innervm"); err != nil {
		t.Fatalf("nested `fleetbox up` failed: %v\n%s", err, out)
	} else {
		t.Logf("nested up:\n%s", out)
	}

	// 5. The nested VM must be running with an IP (root `ls` — the holder is root).
	out, err := outer.SSH(ctx, elevateInGuest+" ls")
	if err != nil {
		t.Fatalf("nested `ls`: %v\n%s", err, out)
	}
	if !strings.Contains(out, "innervm") || !strings.Contains(out, "running") {
		t.Fatalf("nested VM is not running:\n%s", out)
	}
	t.Logf("nested VM running:\n%s", out)
}

// sshKeyPath returns fleetbox's per-installation SSH private key, used to scp into
// the guest. The library boots in bound mode against the real ~/.fleetbox, where
// Start has already generated the key.
func sshKeyPath(t testing.TB) string {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("home dir: %v", err)
	}
	return filepath.Join(home, ".fleetbox", "id_ed25519")
}
