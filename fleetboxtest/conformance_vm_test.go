package fleetboxtest_test

import (
	"context"
	"os"
	"path/filepath"
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

	// Programmatic copy in/out over the same SSH connection (ADR-0026). This is the
	// ONLY coverage of the real guest `tar` commands: the hermetic unit tests
	// substitute Go's archive/tar for GNU tar and so cannot validate mode
	// preservation across the hop, --no-same-owner, or the guest-side mkdir -p of a
	// missing parent. A wrong tar flag / -C / basename fails here, not in unit tests.
	hostDir := t.TempDir()

	// CopyTo a 0755 file to a guest path whose parent dir does NOT pre-exist: proves
	// mkdir -p (parent created), -p (mode restored, so it stays executable), and
	// --no-same-owner (extraction as the connecting user succeeds).
	execSrc := filepath.Join(hostDir, "app")
	if err := os.WriteFile(execSrc, []byte("#!/bin/sh\necho hi\n"), 0o755); err != nil {
		t.Fatalf("write exec source: %v", err)
	}
	if err := os.Chmod(execSrc, 0o755); err != nil { // defeat umask so the mode is exactly 0755
		t.Fatalf("chmod exec source: %v", err)
	}
	if err := vm.CopyTo(ctx, execSrc, "/tmp/fbcopy/sub/app"); err != nil {
		t.Fatalf("CopyTo file: %v", err)
	}
	out, err = vm.SSH(ctx, "cat /tmp/fbcopy/sub/app")
	if err != nil || !strings.Contains(out, "echo hi") {
		t.Fatalf("copied file content wrong: %v\n%s", err, out)
	}
	out, err = vm.SSH(ctx, "test -x /tmp/fbcopy/sub/app && echo exec-ok")
	if err != nil || !strings.Contains(out, "exec-ok") {
		t.Fatalf("copied file is not executable (mode not preserved): %v\n%s", err, out)
	}

	// CopyTo a small directory tree → assert structure and contents over SSH.
	treeSrc := filepath.Join(hostDir, "tree")
	if err := os.MkdirAll(filepath.Join(treeSrc, "sub"), 0o755); err != nil {
		t.Fatalf("mkdir tree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(treeSrc, "a.txt"), []byte("alpha"), 0o644); err != nil {
		t.Fatalf("write a.txt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(treeSrc, "sub", "b.txt"), []byte("beta"), 0o644); err != nil {
		t.Fatalf("write b.txt: %v", err)
	}
	if err := vm.CopyTo(ctx, treeSrc, "/tmp/fbtree"); err != nil {
		t.Fatalf("CopyTo dir: %v", err)
	}
	out, err = vm.SSH(ctx, "cat /tmp/fbtree/a.txt /tmp/fbtree/sub/b.txt")
	if err != nil || !strings.Contains(out, "alpha") || !strings.Contains(out, "beta") {
		t.Fatalf("copied tree wrong: %v\n%s", err, out)
	}

	// CopyFrom the whole directory back → assert structure and contents on the host,
	// exercising the guest `tar -c` of a directory and the in-process extractor's
	// top-component rename (/tmp/fbtree → back) over real GNU tar.
	back := filepath.Join(t.TempDir(), "back")
	if err := vm.CopyFrom(ctx, "/tmp/fbtree", back); err != nil {
		t.Fatalf("CopyFrom dir: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(back, "a.txt")); err != nil || string(got) != "alpha" {
		t.Fatalf("CopyFrom a.txt = %q (err %v), want alpha", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(back, "sub", "b.txt")); err != nil || string(got) != "beta" {
		t.Fatalf("CopyFrom sub/b.txt = %q (err %v), want beta", got, err)
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
