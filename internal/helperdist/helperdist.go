// Package helperdist resolves the signed fleetbox-helper binary the darwin
// client drives: it returns a locally pre-staged helper named by the
// FLEETBOX_HELPER environment variable, or downloads the checksum-pinned helper
// for this platform into ~/.fleetbox/bin, strips its Gatekeeper quarantine, and
// makes it executable.
//
// It mirrors how the Linux backend already fetches cloud-hypervisor (ADR-0011):
// the helper is delivered at first use, cached and verified, not embedded in the
// module. The download is version-stamped so a client always runs the exact
// helper its protocol matches, and an empty catalog checksum is rejected — a
// binary that runs with the virtualization entitlement must never be used
// unverified (ADR-0017, R5). The FLEETBOX_HELPER override serves both the
// offline/air-gapped escape hatch and the dev bootstrap (the Makefile points it
// at the locally built, ad-hoc-signed helper).
package helperdist

import (
	"fmt"
	"os"
	"runtime"

	"github.com/pilat/fleetbox/internal/fetch"
	"github.com/pilat/fleetbox/internal/store"
)

// EnvHelper names a pre-staged helper binary to use instead of downloading. It
// bypasses the catalog and the checksum pin, so the bind handshake's protocol
// check is what guards against pointing it at a stale build (ADR-0017, R5).
const EnvHelper = "FLEETBOX_HELPER"

// catalogEntry pins one platform's published helper.
type catalogEntry struct {
	version string
	url     string
	sha256  string
}

// catalog maps GOOS/GOARCH to the published helper. Only darwin/arm64 needs a
// downloaded helper (linux self-reexecs the client binary into the holder —
// ADR-0020). The pin tracks both the wire protocol and the helper's own behavior:
// the helper-v0.2.x line speaks protocol "2" (the ADR-0020 inversion; the older
// helper-v0.1.0 speaks protocol "1" and is rejected at the bind handshake), and
// within that line helper-v0.2.1 is the first whose store.New honors FLEETBOX_HOME,
// so a client-set storage root actually reaches the separate helper process
// (ADR-0028) — the 0.2.0 helper ignored the env and split state across two roots on
// macOS; helper-v0.2.2 makes a guest hang recoverable (force-stop escalation + vmnet
// subnet rotation, ADR-0031). The url/sha256 are the published, ad-hoc-signed darwin/arm64 release asset
// (release-helper.yml), checksum-pinned so an unverified entitlement-bearing binary
// is never run (ADR-0017, R5).
var catalog = map[string]catalogEntry{
	"darwin/arm64": {
		version: "0.2.2",
		url:     "https://github.com/pilat/fleetbox/releases/download/helper-v0.2.2/fleetbox-helper-darwin-arm64",
		sha256:  "7dbfed23deb5d0db2e77eb3db46c31571fb4cfc038ec35bdef2d0a7000cf699e",
	},
}

// Ensure returns the path to a runnable fleetbox-helper. It prefers the
// FLEETBOX_HELPER override, otherwise downloads, verifies, de-quarantines, and
// chmods the catalog helper for this platform into the store's bin directory.
func Ensure(st *store.Store) (string, error) {
	if override := os.Getenv(EnvHelper); override != "" {
		info, err := os.Stat(override)
		if err != nil {
			return "", fmt.Errorf("%s=%q: %w", EnvHelper, override, err)
		}
		if info.IsDir() {
			return "", fmt.Errorf("%s=%q is a directory, not a helper binary", EnvHelper, override)
		}
		return override, nil
	}

	key := runtime.GOOS + "/" + runtime.GOARCH
	entry, ok := catalog[key]
	if !ok {
		return "", fmt.Errorf("no fleetbox helper for %s; set %s to a locally built helper", key, EnvHelper)
	}
	if entry.url == "" || entry.sha256 == "" {
		// The virtualization entitlement makes an unverified download unacceptable
		// (R5): refuse unless BOTH the URL and checksum are pinned. Until the signed
		// artifact is published, the FLEETBOX_HELPER override is the path.
		return "", fmt.Errorf(
			"no published fleetbox helper yet for %s; set %s to a locally built, signed helper",
			key, EnvHelper)
	}

	name := "fleetbox-helper-" + entry.version
	dest := st.BinDir() + "/" + name
	if _, err := os.Stat(dest); err != nil {
		// Not cached: a multi-MB download is about to happen, so announce it
		// rather than let a first run look like a hung test (R11).
		fmt.Fprintf(
			os.Stderr,
			"Downloading fleetbox-helper %s (first run, cached under ~/.fleetbox/bin)...\n",
			entry.version,
		)
	}

	path, err := fetch.Ensure(st.BinDir(), name, entry.url, entry.sha256)
	if err != nil {
		return "", fmt.Errorf("download helper: %w", err)
	}

	// Strip Gatekeeper quarantine and make it executable. Both touch only xattrs
	// and mode bits, which are outside the mach-o signature, so the signed bytes
	// are preserved (ADR-0017, R8).
	if err := stripQuarantine(path); err != nil {
		return "", fmt.Errorf("strip quarantine: %w", err)
	}
	if err := os.Chmod(path, 0o755); err != nil {
		return "", fmt.Errorf("chmod helper: %w", err)
	}
	return path, nil
}
