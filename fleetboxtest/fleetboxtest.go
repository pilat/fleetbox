// Package fleetboxtest provides testing.T integration for fleetbox VMs.
//
// VMs created with Start or StartN are automatically destroyed when the test completes.
//
// Example:
//
//	func TestMyApp(t *testing.T) {
//		vm := fleetboxtest.Start(t, "debian-12")
//		out, err := vm.SSH(context.Background(), "uname -a")
//		if err != nil {
//			t.Fatal(err)
//		}
//		t.Log(out)
//	}
package fleetboxtest

import (
	"context"
	"errors"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/pilat/fleetbox"
)

const (
	// fallbackName is the VM name used when a test name sanitizes to an empty string.
	fallbackName = "test"

	// archAMD64 and archARM64 are runtime.GOARCH values, hoisted to constants so the
	// capability checks reference them rather than repeating the string literals.
	archAMD64 = "amd64"
	archARM64 = "arm64"
)

// Start creates a VM and registers cleanup to destroy it when the test completes.
// The VM name is derived from the test name to ensure uniqueness in parallel tests.
func Start(t testing.TB, image string, opts ...fleetbox.Option) *fleetbox.VM {
	t.Helper()
	skipIfUnsupported(t)

	name := safeName(t.Name())
	opts = append([]fleetbox.Option{fleetbox.WithImage(image)}, opts...)

	ctx, cancel := context.WithTimeout(context.Background(), BootTimeout(1))
	defer cancel()

	vm, err := fleetbox.Start(ctx, name, opts...)
	if err != nil {
		t.Fatalf("fleetbox.Start: %v", err)
	}

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := vm.Destroy(ctx); err != nil {
			t.Logf("warning: failed to destroy VM %s: %v", name, err)
		}
	})

	return vm
}

// StartN creates N VMs with names prefix-1, prefix-2, etc.
// All VMs are destroyed when the test completes.
//
// On a backend without clustering (macOS < 26) StartN(n≥2) returns the public
// fleetbox.ErrClustersUnsupported; the fixture turns that into t.Skip rather than a
// failure, so the cluster test self-skips where clustering is not available.
func StartN(t testing.TB, prefix string, n int, opts ...fleetbox.Option) []*fleetbox.VM {
	t.Helper()
	skipIfUnsupported(t)

	// Include test name in prefix for parallel safety
	fullPrefix := safeName(t.Name() + "-" + prefix)

	ctx, cancel := context.WithTimeout(context.Background(), BootTimeout(n))
	defer cancel()

	vms, err := fleetbox.StartN(ctx, fullPrefix, n, opts...)
	if errors.Is(err, fleetbox.ErrClustersUnsupported) {
		t.Skip("clustering not supported on this backend (macOS < 26)")
	}
	if err != nil {
		t.Fatalf("fleetbox.StartN: %v", err)
	}

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		for _, vm := range vms {
			if err := vm.Destroy(ctx); err != nil {
				t.Logf("warning: failed to destroy VM %s: %v", vm.Name(), err)
			}
		}
	})

	return vms
}

// SkipIfCannotBootVM skips the test unless the host can boot a leaf VM through the
// active backend. The gate is boot-capability, NOT the ability to offer nested virt
// to a guest: linux/{amd64,arm64} only needs an openable /dev/kvm (true even inside a
// nested arm64 guest, where NestedVirtSupported reports false); darwin/arm64 keeps the
// fleetbox.NestedVirtSupported gate (Apple Silicon M3+, macOS 26+ — the project's
// stated minimum for VM tests, which also makes the GitHub macOS runner skip). On any
// other platform the test is skipped, never failed.
func SkipIfCannotBootVM(t testing.TB) {
	t.Helper()

	switch {
	case runtime.GOOS == "darwin" && runtime.GOARCH == archARM64:
		if !fleetbox.NestedVirtSupported() {
			t.Skip("host cannot boot a VM: need M3+/macOS 26 vz (darwin)")
		}
	case runtime.GOOS == "linux" && (runtime.GOARCH == archAMD64 || runtime.GOARCH == archARM64):
		f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
		if err != nil {
			t.Skipf("host cannot boot a VM: need /dev/kvm (linux): %v", err)
		}
		_ = f.Close()
	default:
		t.Skipf("fleetbox supports darwin/arm64 and linux/{amd64,arm64}, not %s/%s", runtime.GOOS, runtime.GOARCH)
	}
}

// BootTimeout returns the per-call boot budget for a fixture that boots n VMs. It
// honors FLEETBOX_IP_WAIT_TIMEOUT (a time.ParseDuration string) when set and valid —
// the same knob the holder uses for its IP-wait — so one env widens both the holder's
// wait and the test's context (e.g. inside a slow nested guest). Unset or unparseable
// falls back silently to the default of 5 minutes per VM (n is treated as at least 1).
func BootTimeout(n int) time.Duration {
	// A zero or negative override (e.g. "0s") parses cleanly but yields an
	// immediately-expired context, so treat it like unset and fall back.
	if v := os.Getenv("FLEETBOX_IP_WAIT_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	if n < 1 {
		n = 1
	}
	return time.Duration(n) * 5 * time.Minute
}

// SkipIfShort skips the test if -short is set.
func SkipIfShort(t testing.TB, reason string) {
	t.Helper()
	if testing.Short() {
		t.Skipf("skipping in short mode: %s", reason)
	}
}

// skipIfUnsupported skips the test unless the host can boot a VM. It delegates to
// SkipIfCannotBootVM, which both rejects unsupported platforms (its default case
// carries the supported-platform message) and probes boot capability — darwin/arm64
// vz, linux/{amd64,arm64} `/dev/kvm`.
func skipIfUnsupported(t testing.TB) {
	t.Helper()
	SkipIfCannotBootVM(t)
}

// safeName converts a test name to a valid VM name.
// VM names should be valid hostnames: lowercase alphanumeric and hyphens.
func safeName(testName string) string {
	name := strings.ToLower(testName)
	name = strings.ReplaceAll(name, "/", "-")
	name = strings.ReplaceAll(name, "_", "-")

	// Remove invalid characters
	var result strings.Builder
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			result.WriteRune(r)
		}
	}

	name = result.String()

	// Truncate if too long (hostnames max 63 chars, leave room for -N suffix)
	if len(name) > 50 {
		name = name[:50]
	}

	// Remove leading/trailing hyphens
	name = strings.Trim(name, "-")

	if name == "" {
		name = fallbackName
	}

	return name
}
