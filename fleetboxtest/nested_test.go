// This is the dogfood gate: use fleetbox to test fleetbox. On an Apple-Silicon mac
// (M3+, macOS 26+) fleetbox boots a Linux guest with a working nested /dev/kvm, then
// the SAME unified fleetboxtest suite — cross-built for linux/arm64 — runs INSIDE that
// guest on the cloud-hypervisor backend via the arm64 direct-kernel path (ADR-0024).
// Inside, the conformance test (single-VM lifecycle + egress through the nft
// masquerade, ADR-0025) — matrixed over a debian baseline plus ubuntu-26.04, the
// first real arm64-ubuntu boot (ADR-0030) — and the cluster test (VM↔VM over the
// shared bridge + subnet isolation, ADR-0011) run against a live kernel; the
// orchestrator below self-skips there (it is darwin-only), so there is no recursion.
// fleetbox testing fleetbox, exercising the real netlink/nftables code end to end.
//
// There is no separate in-guest test: the dogfood is the real suite. The inner run's
// PASS/FAIL is the inner process's exit code (which VM.SSH surfaces as a non-nil error
// on non-zero exit) — the output is logged for diagnosis, not string-matched.
//
// No build tag: TestNestedLinuxBoot is gated at runtime instead (darwin/arm64 +
// NestedVirtSupported + !-short + FLEETBOX_HELPER set). It folds into the full
// `make test-vm` run; it is NOT auto-downloaded — nested is deliberate, heavy setup,
// so it skips unless FLEETBOX_HELPER points at a signed helper (`make helper` does this
// for `make test-vm`).
//
// LOCAL-ONLY: no CI lane, because the runner must be an M3+ macOS host.
//
// Note: a nested boot is slow; FLEETBOX_IP_WAIT_TIMEOUT (honored by both the holder's
// IP-wait and the fixtures' BootTimeout) is widened to 20m inside the guest. If the
// inner cluster still trips it, the serial log under the inner ~/.fleetbox shows the
// boot progressing — nested timing, not the direct-kernel path, is the cause.
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

// elevateInGuest runs a privileged fleetbox binary inside the guest as root. The
// cloud-init user has passwordless sudo; the PATH keeps /sbin reachable for the
// elevated environment (the backend itself no longer shells out — host networking
// is netlink/nftables, ADR-0025), and FLEETBOX_IP_WAIT_TIMEOUT widens both the
// holder's IP-wait and the fixtures' BootTimeout because a doubly-nested boot is slow.
// FLEETBOX_TEST_IMAGES matrixes the inner conformance run over a debian baseline plus
// one ubuntu — the first real arm64-ubuntu boot, exercising ubuntu's separate-/boot
// layout through the shared arm64 direct-kernel path (ADR-0029/0030).
const elevateInGuest = "sudo -n env FLEETBOX_IP_WAIT_TIMEOUT=20m " +
	"FLEETBOX_TEST_IMAGES=debian-12,ubuntu-26.04 " +
	"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// TestNestedLinuxBoot is the Mac-side orchestrator (see the file comment).
func TestNestedLinuxBoot(t *testing.T) {
	SkipIfShort(t, "nested dogfood boots an outer guest and runs the full suite inside it")
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Skip("nested boot needs an Apple-Silicon mac (M3+, macOS 26+)")
	}
	if !fleetbox.NestedVirtSupported() {
		t.Skip("host lacks nested virtualization (Apple Silicon M3+, macOS 26+)")
	}
	if os.Getenv("FLEETBOX_HELPER") == "" {
		t.Skip("set FLEETBOX_HELPER to the signed helper (see `make helper` / make test-vm)")
	}

	// Generous: cross-build + outer boot + two inner conformance boots (debian +
	// ubuntu, 20m IP-wait each) + the inner cluster. A Go -timeout kills without
	// running t.Cleanup, leaking VMs, so this must clear the real worst case.
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Minute)
	defer cancel()

	// 1. Cross-build the linux/arm64 fleetboxtest *test binary* that will run inside
	//    the guest. It is the SAME unified suite (no special tag) and is its own helper
	//    there (self-reexec on --fleetbox-runner), exactly like vm-linux.yml's binary.
	bin := filepath.Join(t.TempDir(), "fleetboxtest-linux-arm64")
	build := exec.CommandContext(ctx, "go", "test", "-c", "-o", bin, ".")
	build.Env = append(os.Environ(), "GOOS=linux", "GOARCH=arm64", "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("cross-build linux/arm64 fleetboxtest binary: %v\n%s", err, out)
	}

	// 2. Boot the outer Linux guest; on M3+ it gets a working /dev/kvm (nested virt).
	//    Sized with headroom for the inner cluster (peak 8 GB across 4 small members)
	//    and the two inner cloud images + their disks cached inside the guest.
	outer := Start(t, "debian-12",
		fleetbox.WithCPUs(8), fleetbox.WithMemoryGB(16), fleetbox.WithDiskGB(100))

	if out, err := outer.SSH(ctx, "test -e /dev/kvm && echo kvm-ok"); err != nil ||
		!strings.Contains(out, "kvm-ok") {
		t.Fatalf("guest has no /dev/kvm (nested virt unavailable?): %v\n%s", err, out)
	}

	// 3. Push the test binary in via the public copy API (it now exposes CopyTo, so
	//    no scp shell-out and no hand-reconstructed key path). The cross-built binary
	//    is 0755 and CopyTo preserves the mode, so it lands executable in the guest.
	if err := outer.CopyTo(ctx, bin, "/tmp/fleetboxtest"); err != nil {
		t.Fatalf("copy test binary into guest: %v", err)
	}

	// 4. Run the FULL suite inside the guest: no -run selector, no -short, so capability
	//    skip runs everything the guest supports — conformance + cluster on the
	//    cloud-hypervisor backend; the orchestrator self-skips (not darwin). A nested
	//    boot is slow, hence the generous inner -test.timeout and the widened
	//    FLEETBOX_IP_WAIT_TIMEOUT in elevateInGuest. Pass/fail is the binary's exit code
	//    (surfaced as the SSH error), NOT a string match on the output. The binary
	//    arrived executable via CopyTo's preserved mode, so no in-guest chmod.
	cmd := elevateInGuest + " /tmp/fleetboxtest -test.v -test.timeout 60m"
	out, err := outer.SSH(ctx, cmd)
	t.Logf("in-guest unified suite output:\n%s", out)
	if err != nil {
		t.Fatalf("in-guest unified suite failed (inner exit non-zero): %v", err)
	}
}
