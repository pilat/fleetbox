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
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/pilat/fleetbox"
)

// fallbackName is the VM name used when a test name sanitizes to an empty string.
const fallbackName = "test"

// Start creates a VM and registers cleanup to destroy it when the test completes.
// The VM name is derived from the test name to ensure uniqueness in parallel tests.
func Start(t testing.TB, image string, opts ...fleetbox.Option) *fleetbox.VM {
	t.Helper()
	skipIfUnsupported(t)

	name := safeName(t.Name())
	opts = append([]fleetbox.Option{fleetbox.WithImage(image)}, opts...)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
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
func StartN(t testing.TB, prefix string, n int, opts ...fleetbox.Option) []*fleetbox.VM {
	t.Helper()
	skipIfUnsupported(t)

	// Include test name in prefix for parallel safety
	fullPrefix := safeName(t.Name() + "-" + prefix)

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(n)*5*time.Minute)
	defer cancel()

	vms, err := fleetbox.StartN(ctx, fullPrefix, n, opts...)
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

// skipIfUnsupported skips the test on platforms fleetbox does not support, or
// when the host cannot boot a VM. Supported platforms are darwin/arm64 (Apple
// Virtualization) and linux/{amd64,arm64} (cloud-hypervisor).
func skipIfUnsupported(t testing.TB) {
	t.Helper()

	darwinARM := runtime.GOOS == "darwin" && runtime.GOARCH == "arm64"
	linux := runtime.GOOS == "linux" && (runtime.GOARCH == "amd64" || runtime.GOARCH == "arm64")
	if !darwinARM && !linux {
		t.Skipf("fleetbox supports darwin/arm64 and linux/{amd64,arm64}, not %s/%s", runtime.GOOS, runtime.GOARCH)
	}

	if !fleetbox.NestedVirtSupported() {
		t.Skip("host lacks nested virtualization (macOS: Apple Silicon M3+; Linux: /dev/kvm access)")
	}
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

// SkipIfShort skips the test if -short is set.
func SkipIfShort(t testing.TB, reason string) {
	t.Helper()
	if testing.Short() {
		t.Skipf("skipping in short mode: %s", reason)
	}
}
