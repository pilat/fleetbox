package main

import (
	"slices"
	"testing"

	"github.com/pilat/fleetbox/internal/store"
)

func newTestStore(t *testing.T, names ...string) *store.Store {
	t.Helper()
	st, err := store.NewAt(t.TempDir())
	if err != nil {
		t.Fatalf("store.NewAt: %v", err)
	}
	for _, n := range names {
		if err := st.Create(&store.VM{Name: n}); err != nil {
			t.Fatalf("create %s: %v", n, err)
		}
	}
	return st
}

func TestResolveTargets(t *testing.T) {
	st := newTestStore(t, "web-1", "web-2", "web-prod", "dev")

	cases := []struct {
		name        string
		patterns    []string
		wantTargets []string
		wantUnknown []string
	}{
		{name: "exact solo", patterns: []string{"dev"}, wantTargets: []string{"dev"}},
		{name: "exact member", patterns: []string{"web-1"}, wantTargets: []string{"web-1"}},
		// "web" expands to web-1/web-2 (the -<digits> rule) but NOT the unrelated
		// solo "web-prod" — the old HasPrefix over-matched it.
		{name: "cluster prefix expands", patterns: []string{"web"}, wantTargets: []string{"web-1", "web-2"}},
		{name: "unknown is not an error", patterns: []string{"nope"}, wantUnknown: []string{"nope"}},
		{
			name:        "mixed known and unknown",
			patterns:    []string{"dev", "nope"},
			wantTargets: []string{"dev"},
			wantUnknown: []string{"nope"},
		},
		{
			name:        "dedup overlapping patterns",
			patterns:    []string{"web", "web-1"},
			wantTargets: []string{"web-1", "web-2"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			targets, unknown, err := resolveTargets(st, tc.patterns)
			if err != nil {
				t.Fatalf("resolveTargets(%v): %v", tc.patterns, err)
			}
			if !slices.Equal(targets, tc.wantTargets) {
				t.Errorf("targets = %v, want %v", targets, tc.wantTargets)
			}
			if !slices.Equal(unknown, tc.wantUnknown) {
				t.Errorf("unknown = %v, want %v", unknown, tc.wantUnknown)
			}
		})
	}
}

// TestResolveTargetsExactWins pins the documented edge: when a solo "web" and a
// cluster "web-N" both exist, an exact "web" targets only the solo.
func TestResolveTargetsExactWins(t *testing.T) {
	st := newTestStore(t, "web", "web-1", "web-2")

	targets, unknown, err := resolveTargets(st, []string{"web"})
	if err != nil {
		t.Fatalf("resolveTargets: %v", err)
	}
	if len(unknown) != 0 {
		t.Errorf("unknown = %v, want none", unknown)
	}
	if !slices.Equal(targets, []string{"web"}) {
		t.Errorf("targets = %v, want [web] (exact match wins)", targets)
	}
}
