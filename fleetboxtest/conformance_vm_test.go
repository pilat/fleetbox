package fleetboxtest_test

import (
	"context"
	"strings"
	"testing"

	"github.com/pilat/fleetbox"
	"github.com/pilat/fleetbox/fleetboxtest"
	"github.com/pilat/fleetbox/internal/store"
)

// TestVMConformance is the cross-platform behavioral guard that the public VM
// lifecycle behaves identically whether the orchestration runs in-process (Linux)
// or in the downloaded, signed helper (macOS) — ADR-0017 R2. It boots one real VM
// through the public API and exercises Name/IP/State/SSH, then verifies Destroy
// tears it down, is idempotent on a second call, and removes the VM's store files.
//
// It carries no build tag, so it runs on both backends; it boots a real VM, so it
// is skipped in -short and where the host cannot boot one (SkipIfCannotBootVM). Named
// with the TestVM prefix so `make test-vm` runs it.
func TestVMConformance(t *testing.T) {
	fleetboxtest.SkipIfShort(t, "boots a real VM")
	fleetboxtest.SkipIfCannotBootVM(t)

	ctx, cancel := context.WithTimeout(context.Background(), fleetboxtest.BootTimeout(1))
	defer cancel()

	const name = "fbconformance"
	vm, err := fleetbox.Start(ctx, name, fleetbox.WithImage("debian-12"))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Safety net: tear down even if an assertion below fails early. Destroy is
	// idempotent, so the explicit Destroy later is harmless.
	defer func() {
		_ = vm.Destroy(context.Background())
	}()

	if vm.Name() != name {
		t.Errorf("Name() = %q, want %q", vm.Name(), name)
	}
	if vm.IP() == nil {
		t.Error("IP() = nil, want an address")
	}
	if vm.State() == "" {
		t.Error("State() = empty, want a state")
	}

	out, err := vm.SSH(ctx, "echo conformance-ok")
	if err != nil {
		t.Fatalf("SSH: %v\n%s", err, out)
	}
	if !strings.Contains(out, "conformance-ok") {
		t.Errorf("SSH output = %q, want it to contain conformance-ok", out)
	}

	// Egress: the guest must reach the public internet. On Linux this drives the nft
	// masquerade + per-interface forwarding end to end (ADR-0025) — a missing or
	// silently-dropped masq rule fails here, which a plain echo-over-SSH would never
	// catch. Ping by IP so the check does not depend on guest DNS.
	out, err = vm.SSH(ctx, "ping -c1 -W5 1.1.1.1")
	if err != nil {
		t.Fatalf("egress ping failed (guest cannot reach the internet): %v\n%s", err, out)
	}
	if !strings.Contains(out, "0% packet loss") {
		t.Fatalf("egress ping got no reply (no internet egress):\n%s", out)
	}

	// Stop gracefully (disk preserved), then Destroy removes everything — the full
	// lifecycle the public API promises on both backends (R2).
	if err := vm.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// Destroy tears the VM down and removes its files.
	if err := vm.Destroy(ctx); err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	// Idempotent: a second Destroy finds the VM already gone and returns nil.
	if err := vm.Destroy(ctx); err != nil {
		t.Errorf("second Destroy() = %v, want nil (idempotent)", err)
	}

	// Destroy must have removed the member's store files.
	st, err := store.New()
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	if st.Exists(name) {
		t.Errorf("Destroy did not remove store files for %q", name)
	}
}
