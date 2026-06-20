// Package fetch downloads and caches remote files (cloud images, VMM binaries),
// verifying an optional SHA256 and writing atomically so a cached file is always
// complete. It is the shared download primitive behind internal/image and the
// cloud-hypervisor backend (ADR-0011).
package fetch

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// Ensure downloads url into cacheDir/name and returns the path. If the file is
// already cached it returns immediately. The download goes to a .download temp
// file that is verified (when sha256hex is non-empty) and atomically renamed, so
// a partial or mismatched download never leaves a usable file behind. An empty
// sha256hex skips verification (used by the image catalog's "latest" entries);
// callers that pin versions pass the digest to make verification mandatory.
func Ensure(cacheDir, name, url, sha256hex string) (string, error) {
	dest := filepath.Join(cacheDir, name)
	if _, err := os.Stat(dest); err == nil {
		return dest, nil
	}

	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", fmt.Errorf("create cache dir %s: %w", cacheDir, err)
	}

	tmp := dest + ".download"
	if err := download(url, tmp); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("download %s: %w", url, err)
	}

	if sha256hex != "" {
		if err := verifyChecksum(tmp, sha256hex); err != nil {
			_ = os.Remove(tmp)
			return "", fmt.Errorf("verify %s: %w", name, err)
		}
	}

	if err := os.Rename(tmp, dest); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("rename %s: %w", dest, err)
	}

	return dest, nil
}

func download(url, destPath string) error {
	resp, err := http.Get(url)
	if err != nil {
		return fmt.Errorf("http get: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %s", resp.Status)
	}

	f, err := os.Create(destPath)
	if err != nil {
		return fmt.Errorf("create %s: %w", destPath, err)
	}
	defer func() { _ = f.Close() }()

	if _, err := io.Copy(f, resp.Body); err != nil {
		return fmt.Errorf("write %s: %w", destPath, err)
	}

	return nil
}

func verifyChecksum(path, expected string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("hash %s: %w", path, err)
	}

	actual := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(actual, expected) {
		return fmt.Errorf("checksum mismatch: got %s, want %s", actual, expected)
	}

	return nil
}
