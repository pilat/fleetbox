package helperdist

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/pilat/fleetbox/internal/store"
)

func newStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.NewAt(t.TempDir())
	if err != nil {
		t.Fatalf("store.NewAt: %v", err)
	}
	return st
}

func TestEnsureOverrideReturnsLocalHelper(t *testing.T) {
	helper := filepath.Join(t.TempDir(), "fleetbox-helper")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write helper: %v", err)
	}
	t.Setenv(EnvHelper, helper)

	got, err := Ensure(newStore(t))
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if got != helper {
		t.Errorf("Ensure = %q, want %q", got, helper)
	}
}

func TestEnsureOverrideMissingFails(t *testing.T) {
	t.Setenv(EnvHelper, filepath.Join(t.TempDir(), "nope"))
	if _, err := Ensure(newStore(t)); err == nil {
		t.Fatal("Ensure with missing FLEETBOX_HELPER = nil error, want error")
	}
}

func TestEnsureOverrideDirFails(t *testing.T) {
	t.Setenv(EnvHelper, t.TempDir())
	if _, err := Ensure(newStore(t)); err == nil {
		t.Fatal("Ensure with directory FLEETBOX_HELPER = nil error, want error")
	}
}

// TestEnsureWithoutOverrideRefusesUnverified pins R5: with no override and no
// pinned checksum, the client must not fetch and run an unverified helper —
// Ensure errors and points at the override instead.
func TestEnsureWithoutOverrideRefusesUnverified(t *testing.T) {
	t.Setenv(EnvHelper, "")
	// Blank this platform's catalog entry to simulate the unverifiable state (no
	// pinned sha256), independent of whether a real helper is published, so Ensure
	// must refuse rather than fetch an unchecked binary.
	key := runtime.GOOS + "/" + runtime.GOARCH
	saved, had := catalog[key]
	catalog[key] = catalogEntry{version: saved.version}
	t.Cleanup(func() {
		if had {
			catalog[key] = saved
		} else {
			delete(catalog, key)
		}
	})

	_, err := Ensure(newStore(t))
	if err == nil {
		t.Fatal("Ensure without override = nil error, want refusal to run unverified helper")
	}
	if !strings.Contains(err.Error(), EnvHelper) {
		t.Errorf("error %q does not mention the %s override", err, EnvHelper)
	}
}
