//go:build linux

package cloudhypervisor

import (
	"fmt"
	"os"
	"runtime"

	"github.com/pilat/fleetbox/internal/fetch"
)

// Pinned VMM + firmware versions. Bumping a version means recomputing the
// per-arch SHA256 below. Both artifacts are checksum-verified on download
// (ADR-0011) — unlike cloud images, they are never left unpinned.
const (
	chVersion = "v52.0" // cloud-hypervisor
	fwVersion = "0.5.0"  // rust-hypervisor-firmware
)

// chBinaries maps GOARCH to the static cloud-hypervisor binary for it.
var chBinaries = map[string]artifact{
	"amd64": {
		name:   "cloud-hypervisor-" + chVersion + "-amd64",
		url:    "https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/" + chVersion + "/cloud-hypervisor-static",
		sha256: "829af01ff075bb96c4f183905134c453a88d68cbabdc6b87df21098842581ee9",
		exec:   true,
	},
	"arm64": {
		name:   "cloud-hypervisor-" + chVersion + "-arm64",
		url:    "https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/" + chVersion + "/cloud-hypervisor-static-aarch64",
		sha256: "bf004ddc1a148f47caa87ac49a783b8dbd6bf9bc27abe522ed197df7b982d3b1",
		exec:   true,
	},
}

// fwBinaries maps GOARCH to the rust-hypervisor-firmware image for it (PVH on
// x86_64, the aarch64 build on arm64).
var fwBinaries = map[string]artifact{
	"amd64": {
		name:   "hypervisor-fw-" + fwVersion + "-amd64",
		url:    "https://github.com/cloud-hypervisor/rust-hypervisor-firmware/releases/download/" + fwVersion + "/hypervisor-fw",
		sha256: "4a0a1e977368f6b15d2198a216bdedf9a350bf5e5ae07e29e695373ec16ad958",
	},
	"arm64": {
		name:   "hypervisor-fw-" + fwVersion + "-arm64",
		url:    "https://github.com/cloud-hypervisor/rust-hypervisor-firmware/releases/download/" + fwVersion + "/hypervisor-fw-aarch64",
		sha256: "2a22aed888572ae319e231b85a7b4de951c7eca8857730300653512d064c8102",
	},
}

// artifact is a pinned, checksum-verified downloadable: a VMM binary or its
// firmware.
type artifact struct {
	name   string // cache filename under the bin dir (carries version + arch)
	url    string
	sha256 string
	exec   bool // chmod 0755 after caching (the VMM binary; firmware is data)
}

// ensureBinaries downloads (once) and returns the cached cloud-hypervisor binary
// and firmware paths for the current architecture, both under binDir. Re-running
// it is a no-op once cached. Both downloads are SHA256-pinned (ADR-0011).
func ensureBinaries(binDir string) (chPath, fwPath string, err error) {
	chPath, err = ensureArtifact(binDir, chBinaries)
	if err != nil {
		return "", "", fmt.Errorf("ensure cloud-hypervisor: %w", err)
	}
	fwPath, err = ensureArtifact(binDir, fwBinaries)
	if err != nil {
		return "", "", fmt.Errorf("ensure firmware: %w", err)
	}
	return chPath, fwPath, nil
}

// ensureArtifact fetches the architecture-appropriate entry from table into
// binDir, chmod'ing it executable when the entry is a binary.
func ensureArtifact(binDir string, table map[string]artifact) (string, error) {
	a, ok := table[runtime.GOARCH]
	if !ok {
		return "", fmt.Errorf("no pinned binary for %s/%s", runtime.GOOS, runtime.GOARCH)
	}

	path, err := fetch.Ensure(binDir, a.name, a.url, a.sha256)
	if err != nil {
		return "", fmt.Errorf("fetch %s: %w", a.name, err)
	}

	if a.exec {
		if err := os.Chmod(path, 0o755); err != nil {
			return "", fmt.Errorf("chmod %s: %w", path, err)
		}
	}

	return path, nil
}
