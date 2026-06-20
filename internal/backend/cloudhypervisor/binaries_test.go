//go:build linux

package cloudhypervisor

import (
	"net/http"
	"os"
	"runtime"
	"testing"
	"time"
)

// TestEnsureBinaries fetches the pinned cloud-hypervisor binary, verifies its
// SHA256, and confirms the second call is a cache no-op. It is network-gated: if
// the release host is unreachable it skips rather than fails, so it is safe
// offline. It runs only on linux (the backend's platform).
func TestEnsureBinaries(t *testing.T) {
	if _, ok := chBinaries[runtime.GOARCH]; !ok {
		t.Skipf("no pinned cloud-hypervisor binary for %s", runtime.GOARCH)
	}
	if !reachable(t, chBinaries[runtime.GOARCH].url) {
		t.Skip("release host unreachable; skipping network-gated fetch test")
	}

	binDir := t.TempDir()

	chPath, err := ensureBinaries(binDir)
	if err != nil {
		t.Fatalf("ensureBinaries: %v", err)
	}

	info, err := os.Stat(chPath)
	if err != nil {
		t.Fatalf("stat cloud-hypervisor: %v", err)
	}
	if info.Mode().Perm()&0o100 == 0 {
		t.Errorf("cloud-hypervisor binary %s is not executable (mode %v)", chPath, info.Mode())
	}

	// Second call is a no-op: the verified binary is already cached.
	chPath2, err := ensureBinaries(binDir)
	if err != nil {
		t.Fatalf("ensureBinaries (cached): %v", err)
	}
	if chPath2 != chPath {
		t.Errorf("cached path differs: got %q, want %q", chPath2, chPath)
	}
}

func reachable(t *testing.T, url string) bool {
	t.Helper()
	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest(http.MethodHead, url, nil)
	if err != nil {
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}
