//go:build linux

package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/pilat/fleetbox/internal/store"
)

// TestElevatedArgvForwardsHome pins the FLEETBOX_HOME forwarding through sudo
// (ADR-0028, ties to ADR-0023): when the branded root is set, the elevated argv
// must carry a FLEETBOX_HOME=<value> token positioned before the self path, so root
// and the invoking user resolve the same storage tree; when unset, no such token
// appears and the default SUDO_USER resolution is untouched. Linux-tagged because
// elevatedArgv is //go:build linux — it runs on Linux CI, not the macOS dev box.
func TestElevatedArgvForwardsHome(t *testing.T) {
	t.Run("set: token appears before self", func(t *testing.T) {
		const root = "/some/abs/.aaa"
		t.Setenv(store.EnvHome, root)

		argv, err := elevatedArgv()
		if err != nil {
			t.Fatalf("elevatedArgv: %v", err)
		}

		token := store.EnvHome + "=" + root
		ti := slices.Index(argv, token)
		if ti < 0 {
			t.Fatalf("argv missing %q: %v", token, argv)
		}
		// self is the first token after the all-KEY=value env prefix; original args
		// are appended after it, so the first non-KEY=value token is self.
		si := indexOfSelf(argv)
		if ti >= si {
			t.Errorf("FLEETBOX_HOME token at %d must come before self at %d: %v", ti, si, argv)
		}
	})

	t.Run("unset: no token", func(t *testing.T) {
		t.Setenv(store.EnvHome, "")

		argv, err := elevatedArgv()
		if err != nil {
			t.Fatalf("elevatedArgv: %v", err)
		}
		if i := slices.IndexFunc(argv, func(a string) bool {
			return strings.HasPrefix(a, store.EnvHome+"=")
		}); i >= 0 {
			t.Errorf("argv must not carry a FLEETBOX_HOME token when unset: %q in %v", argv[i], argv)
		}
	})
}

// indexOfSelf returns the position of the embedded executable path: the first token
// past "sudo"/"env" that is not a KEY=value env assignment.
func indexOfSelf(argv []string) int {
	for i := 2; i < len(argv); i++ {
		if !strings.Contains(argv[i], "=") {
			return i
		}
	}
	return len(argv)
}
