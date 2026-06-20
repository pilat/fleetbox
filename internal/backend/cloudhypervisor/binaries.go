//go:build linux

package cloudhypervisor

import (
	"fmt"
	"os"
	"runtime"

	"github.com/pilat/fleetbox/internal/fetch"
)

// Pinned VMM version. Bumping it means recomputing the per-arch SHA256 below. The
// binary is checksum-verified on download (ADR-0011) — unlike cloud images, it is
// never left unpinned.
const chVersion = "v52.0" // cloud-hypervisor

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

// artifact is a pinned, checksum-verified downloadable: the VMM binary.
type artifact struct {
	name   string // cache filename under the bin dir (carries version + arch)
	url    string
	sha256 string
	exec   bool // chmod 0755 after caching
}

// ensureBinaries downloads (once) and returns the cached cloud-hypervisor binary
// path for the current architecture under binDir. Re-running it is a no-op once
// cached. The download is SHA256-pinned (ADR-0011). Direct kernel boot needs no
// separate boot artifact (ADR-0029).
func ensureBinaries(binDir string) (chPath string, err error) {
	chPath, err = ensureArtifact(binDir, chBinaries)
	if err != nil {
		return "", fmt.Errorf("ensure cloud-hypervisor: %w", err)
	}
	return chPath, nil
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
