//go:build darwin && arm64

// Package fleetboxtest provides testing.T integration for fleetbox VMs.
//
// VMs created with Start or StartN are automatically destroyed when the test completes.
//
// Example:
//
//	func TestMyApp(t *testing.T) {
//		vm := fleetboxtest.Start(t, fleetbox.Debian12)
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

// skipIfUnsupported skips the test if running on unsupported platform.
func skipIfUnsupported(t testing.TB) {
	t.Helper()

	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Skip("fleetbox requires darwin/arm64")
	}

	if !fleetbox.NestedVirtSupported() {
		t.Skip("nested virtualization not supported (requires M3+ and macOS 15+)")
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
		name = "test"
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
