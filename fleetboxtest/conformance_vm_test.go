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

	// Egress: the guest must reach the public internet. This drives the nft masquerade
	// + per-interface forwarding end to end (ADR-0025) — a missing or silently-dropped
	// masq rule fails here, which a plain echo-over-SSH would never catch. Use a TCP
	// connect, NOT ICMP ping: some CI networks (notably GitHub-hosted runners) drop
	// outbound ICMP while allowing TCP, so a ping would be a false negative even when
	// egress works. 1.1.1.1:443 is a stable, DNS-independent target; bash's /dev/tcp
	// needs no extra package in the stock guest.
	out, err = vm.SSH(ctx, "timeout 8 bash -c 'exec 3<>/dev/tcp/1.1.1.1/443 && echo egress-ok'")
	if err != nil || !strings.Contains(out, "egress-ok") {
		t.Fatalf("egress failed (guest cannot open TCP to the internet): %v\n%s", err, out)
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
