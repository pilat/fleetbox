// Command catalog refreshes the pinned cloud-image catalog
// (internal/image/catalog.json). The human-authored keys decide which OSes exist;
// this tool only refreshes the values — the dated upstream snapshot, the
// snapshot-stamped per-arch download URLs, and the SHA256 the runtime verifies.
//
// It imports the internal/image types so the JSON shape has a single source of
// truth. Run it via `make catalog`. A scheduled GitHub Action runs it monthly and
// opens a PR when upstream has moved (ADR-0019).
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/pilat/fleetbox/internal/image"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "catalog:", err)
		os.Exit(1)
	}
}

// run reads the catalog, resolves every entry against its upstream, and writes
// the refreshed JSON back. It resolves all entries before writing anything, so a
// single failure aborts the run with no partial write.
func run() error {
	catFile, err := catalogPath()
	if err != nil {
		return err
	}
	data, err := os.ReadFile(catFile)
	if err != nil {
		return fmt.Errorf("read catalog: %w", err)
	}
	var catalog map[string]image.ImageInfo
	if err := json.Unmarshal(data, &catalog); err != nil {
		return fmt.Errorf("parse catalog: %w", err)
	}

	// No client timeout: a Debian refresh streams multi-GB images through the
	// hashers, far longer than any default deadline would allow.
	client := &http.Client{}
	today := time.Now().UTC().Format("2006-01-02")

	resolved := make(map[string]resolution, len(catalog))
	var failures []string
	for _, alias := range sortedKeys(catalog) {
		info := catalog[alias]
		res, err := resolve(client, info)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", alias, err))
			continue
		}
		resolved[alias] = res
		fmt.Fprintf(os.Stderr, "resolved %s -> %s\n", alias, res.snapshot)
	}
	if len(failures) > 0 {
		return fmt.Errorf("failed to resolve %d entries:\n  %s", len(failures), strings.Join(failures, "\n  "))
	}

	for alias, res := range resolved {
		info := catalog[alias]
		// bumped_at advances only when the snapshot or a checksum actually moved,
		// so a no-op refresh leaves the file byte-identical (idempotent).
		if changed(info, res) {
			info.BumpedAt = today
		}
		info.Snapshot = res.snapshot
		info.Arch = res.arch
		catalog[alias] = info
	}

	out, err := json.MarshalIndent(catalog, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal catalog: %w", err)
	}
	out = append(out, '\n')
	if err := os.WriteFile(catFile, out, 0o644); err != nil {
		return fmt.Errorf("write catalog: %w", err)
	}
	return nil
}

// changed reports whether a freshly resolved entry differs from the one on disk
// in its snapshot or any per-arch checksum — the trigger for bumping bumped_at.
func changed(old image.ImageInfo, res resolution) bool {
	if old.Snapshot != res.snapshot {
		return true
	}
	if len(old.Arch) != len(res.arch) {
		return true
	}
	for arch, img := range res.arch {
		prev, ok := old.Arch[arch]
		if !ok || prev.SHA256 != img.SHA256 || prev.URL != img.URL {
			return true
		}
	}
	return false
}

// catalogPath locates internal/image/catalog.json by walking up from the working
// directory to the module root (the directory holding go.mod).
func catalogPath() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("getwd: %w", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return filepath.Join(dir, "internal", "image", "catalog.json"), nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("could not locate module root (no go.mod found)")
		}
		dir = parent
	}
}

// sortedKeys returns the catalog aliases in deterministic order, so progress
// output and resolution order do not depend on map iteration.
func sortedKeys(m map[string]image.ImageInfo) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
