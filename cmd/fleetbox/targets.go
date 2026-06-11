package main

import (
	"fmt"

	"github.com/pilat/fleetbox/internal/store"
)

// resolveTargets expands user-supplied patterns into concrete VM member names,
// shared by down and rm so both treat a name the same way. For each pattern:
//
//   - an exact existing VM name resolves to just that VM (exact match wins, so a
//     solo "web" is targeted alone even if a cluster "web-N" also exists);
//   - otherwise the pattern is treated as a cluster prefix and expands to every
//     member m where store.ClusterName(m) == pattern (the "-<digits>" rule), so
//     "web" hits web-1/web-2/... but never an unrelated solo "web-prod";
//   - a pattern matching neither is collected into unknown rather than hard-failing
//     mid-resolution, so the caller's best-effort loop can report it and still act
//     on the patterns that did resolve.
//
// err is reserved for a store.List failure. Targets are de-duplicated, preserving
// first-seen order, so overlapping patterns do not act on a member twice.
func resolveTargets(st *store.Store, patterns []string) (targets, unknown []string, err error) {
	all, err := st.List()
	if err != nil {
		return nil, nil, fmt.Errorf("list vms: %w", err)
	}

	exists := make(map[string]bool, len(all))
	for _, n := range all {
		exists[n] = true
	}

	seen := make(map[string]bool, len(all))
	add := func(name string) {
		if !seen[name] {
			seen[name] = true
			targets = append(targets, name)
		}
	}

	for _, pattern := range patterns {
		if exists[pattern] {
			add(pattern)
			continue
		}
		matched := false
		for _, n := range all {
			if store.ClusterName(n) == pattern {
				add(n)
				matched = true
			}
		}
		if !matched {
			unknown = append(unknown, pattern)
		}
	}
	return targets, unknown, nil
}
