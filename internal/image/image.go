// Package image handles cloud image download, verification, and conversion.
package image

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/lima-vm/go-qcow2reader"
)

// Catalog maps image aliases to their URLs and optional SHA256 checksums.
var Catalog = map[string]ImageInfo{
	"debian-12": {
		URL:    "https://cloud.debian.org/images/cloud/bookworm/latest/debian-12-generic-arm64.raw",
		SHA256: "", // Skip verification for latest
	},
	"ubuntu-24.04": {
		URL:    "https://cloud-images.ubuntu.com/releases/24.04/release/ubuntu-24.04-server-cloudimg-arm64.img",
		SHA256: "",
	},
}

// ImageInfo describes a cloud image.
type ImageInfo struct {
	URL    string
	SHA256 string
}

// Ensure downloads and prepares an image, returning the path to the raw disk.
// If the image is already cached and verified, returns immediately.
// If url is a known alias (e.g., "debian-12"), uses the catalog.
func Ensure(cacheDir, urlOrAlias string) (string, error) {
	info, ok := Catalog[urlOrAlias]
	if !ok {
		info = ImageInfo{URL: urlOrAlias}
	}

	// Determine filename and format from URL
	urlParts := strings.Split(info.URL, "/")
	filename := urlParts[len(urlParts)-1]
	isQcow2 := strings.HasSuffix(filename, ".qcow2") || strings.HasSuffix(filename, ".img")

	// Raw filename (what we'll use)
	rawFilename := strings.TrimSuffix(filename, ".qcow2")
	rawFilename = strings.TrimSuffix(rawFilename, ".img")
	rawFilename += ".raw"
	rawPath := filepath.Join(cacheDir, rawFilename)

	// If raw already exists, return it
	if _, err := os.Stat(rawPath); err == nil {
		return rawPath, nil
	}

	// Download to temp file
	downloadPath := rawPath + ".download"
	if err := download(info.URL, downloadPath); err != nil {
		return "", fmt.Errorf("download: %w", err)
	}

	// Verify checksum if provided
	if info.SHA256 != "" {
		if err := verifyChecksum(downloadPath, info.SHA256); err != nil {
			_ = os.Remove(downloadPath)
			return "", fmt.Errorf("verify checksum: %w", err)
		}
	}

	// Convert if needed
	if isQcow2 {
		if err := convertQcow2ToRaw(downloadPath, rawPath); err != nil {
			_ = os.Remove(downloadPath)
			return "", fmt.Errorf("convert qcow2: %w", err)
		}
		_ = os.Remove(downloadPath)
	} else {
		if err := os.Rename(downloadPath, rawPath); err != nil {
			_ = os.Remove(downloadPath)
			return "", fmt.Errorf("rename: %w", err)
		}
	}

	return rawPath, nil
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

func convertQcow2ToRaw(qcow2Path, rawPath string) error {
	// Open qcow2 file
	f, err := os.Open(qcow2Path)
	if err != nil {
		return fmt.Errorf("open qcow2: %w", err)
	}
	defer func() { _ = f.Close() }()

	// Read qcow2 image
	img, err := qcow2reader.Open(f)
	if err != nil {
		return fmt.Errorf("read qcow2: %w", err)
	}

	// Create output file
	out, err := os.Create(rawPath)
	if err != nil {
		return fmt.Errorf("create raw: %w", err)
	}
	defer func() { _ = out.Close() }()

	// Create section reader and copy
	reader := io.NewSectionReader(img, 0, img.Size())
	if _, err := io.Copy(out, reader); err != nil {
		_ = os.Remove(rawPath)
		return fmt.Errorf("convert: %w", err)
	}

	return nil
}

// CopyDisk copies the source disk image to the destination with the given size.
// If size is larger than the source, the file is extended (sparse).
func CopyDisk(src, dst string, sizeBytes int64) error {
	srcF, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open source: %w", err)
	}
	defer func() { _ = srcF.Close() }()

	// Truncate may only ever extend: a requested size smaller than the base
	// image would shrink (corrupt) the copy. Fail fast before creating the
	// destination so no partial/corrupt dst is left behind.
	srcInfo, err := srcF.Stat()
	if err != nil {
		return fmt.Errorf("stat source: %w", err)
	}
	if sizeBytes > 0 && sizeBytes < srcInfo.Size() {
		return fmt.Errorf("requested disk size %d is smaller than base image %d", sizeBytes, srcInfo.Size())
	}

	dstF, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("create destination: %w", err)
	}
	defer func() { _ = dstF.Close() }()

	if _, err := io.Copy(dstF, srcF); err != nil {
		_ = os.Remove(dst)
		return fmt.Errorf("copy: %w", err)
	}

	// Extend to desired size (sparse file)
	if sizeBytes > 0 {
		if err := dstF.Truncate(sizeBytes); err != nil {
			_ = os.Remove(dst)
			return fmt.Errorf("truncate: %w", err)
		}
	}

	return nil
}
