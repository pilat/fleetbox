package fleetbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pilat/fleetbox/internal/store"
)

// TestSetStorageRoot pins the SetStorageRoot contract (ADR-0028): a ~ prefix
// expands to the user's home, the result is absolute, a relative path resolves
// against cwd, and an empty path is an error — all surfacing through FLEETBOX_HOME.
// t.Setenv resets the env after the test so the override never leaks.
func TestSetStorageRoot(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}

	t.Run("tilde expands to home", func(t *testing.T) {
		t.Setenv(store.EnvHome, "")
		if err := SetStorageRoot("~/.aaa"); err != nil {
			t.Fatalf("SetStorageRoot: %v", err)
		}
		want := filepath.Join(home, ".aaa")
		if got := os.Getenv(store.EnvHome); got != want {
			t.Errorf("FLEETBOX_HOME = %q, want %q", got, want)
		}
	})

	t.Run("bare tilde expands to home", func(t *testing.T) {
		t.Setenv(store.EnvHome, "")
		if err := SetStorageRoot("~"); err != nil {
			t.Fatalf("SetStorageRoot: %v", err)
		}
		if got := os.Getenv(store.EnvHome); got != home {
			t.Errorf("FLEETBOX_HOME = %q, want %q", got, home)
		}
	})

	t.Run("relative path becomes absolute", func(t *testing.T) {
		t.Setenv(store.EnvHome, "")
		if err := SetStorageRoot("rel/root"); err != nil {
			t.Fatalf("SetStorageRoot: %v", err)
		}
		got := os.Getenv(store.EnvHome)
		if !filepath.IsAbs(got) {
			t.Errorf("FLEETBOX_HOME = %q, want absolute", got)
		}
		if !strings.HasSuffix(got, filepath.Join("rel", "root")) {
			t.Errorf("FLEETBOX_HOME = %q, want suffix rel/root", got)
		}
	})

	t.Run("empty path errors", func(t *testing.T) {
		t.Setenv(store.EnvHome, "")
		if err := SetStorageRoot(""); err == nil {
			t.Error("SetStorageRoot(\"\") = nil, want error")
		}
	})
}
