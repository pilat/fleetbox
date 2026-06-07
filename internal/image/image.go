// Package image handles cloud image download, verification, and conversion.
package image

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/lima-vm/go-qcow2reader"

	"github.com/pilat/fleetbox/internal/fetch"
)

// Catalog maps image aliases to a per-GOARCH URL and an optional SHA256. The
// arch tokens in the URLs (amd64/arm64) match Go's GOARCH, so the same alias
// resolves to the right image on macOS Apple Silicon and on Linux amd64/arm64.
var Catalog = map[string]ImageInfo{
	"debian-12": {
		URLs: map[string]string{
			"amd64": "https://cloud.debian.org/images/cloud/bookworm/latest/debian-12-generic-amd64.raw",
			"arm64": "https://cloud.debian.org/images/cloud/bookworm/latest/debian-12-generic-arm64.raw",
		},
		SHA256: "", // Skip verification for latest
	},
	"ubuntu-24.04": {
		URLs: map[string]string{
			"amd64": "https://cloud-images.ubuntu.com/releases/24.04/release/ubuntu-24.04-server-cloudimg-amd64.img",
			"arm64": "https://cloud-images.ubuntu.com/releases/24.04/release/ubuntu-24.04-server-cloudimg-arm64.img",
		},
		SHA256: "",
	},
}

// ImageInfo describes a cloud image: a URL per GOARCH and an optional SHA256
// (empty means "latest, unverified").
type ImageInfo struct {
	URLs   map[string]string
	SHA256 string
}

// Ensure downloads and prepares an image, returning the path to the raw disk.
// If the image is already cached and verified, returns immediately.
// If url is a known alias (e.g., "debian-12"), uses the catalog.
func Ensure(cacheDir, urlOrAlias string) (string, error) {
	// Resolve an alias to the URL for the current architecture; a non-alias is
	// treated as a literal URL (unverified).
	url := urlOrAlias
	sha := ""
	if info, ok := Catalog[urlOrAlias]; ok {
		archURL, ok := info.URLs[runtime.GOARCH]
		if !ok {
			return "", fmt.Errorf("image %q has no URL for %s", urlOrAlias, runtime.GOARCH)
		}
		url = archURL
		sha = info.SHA256
	}

	// Determine filename and format from URL
	urlParts := strings.Split(url, "/")
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

	// A raw source needs no conversion: fetch it (verified) straight to its raw
	// cache name and we are done.
	if !isQcow2 {
		path, err := fetch.Ensure(cacheDir, rawFilename, url, sha)
		if err != nil {
			return "", fmt.Errorf("fetch image: %w", err)
		}
		return path, nil
	}

	// A qcow2/img source is fetched (verified) under its own name, converted to
	// raw, then removed so only the raw image stays cached.
	srcPath, err := fetch.Ensure(cacheDir, filename, url, sha)
	if err != nil {
		return "", fmt.Errorf("fetch image: %w", err)
	}
	if err := convertQcow2ToRaw(srcPath, rawPath); err != nil {
		_ = os.Remove(rawPath)
		return "", fmt.Errorf("convert qcow2: %w", err)
	}
	_ = os.Remove(srcPath)

	return rawPath, nil
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
