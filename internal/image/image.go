// Package image handles cloud image download, verification, and conversion.
//
// The catalog is an embedded JSON file (catalog.json) keyed by alias; each alias
// pins a dated upstream snapshot and, per GOARCH, a download URL plus the SHA256
// fetch verifies before use. The snapshot is stamped into the cache filename (for
// both the source and the converted raw) so an upstream bump is a cache miss, not
// a stale hit — images are pinned the way the VMM binary and firmware are
// (ADR-0011, ADR-0019).
package image

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/lima-vm/go-qcow2reader"

	"github.com/pilat/fleetbox/internal/fetch"
)

//go:embed catalog.json
var catalogJSON []byte

var (
	catalogOnce sync.Once
	catalogData map[string]ImageInfo
	catalogErr  error
)

// ImageInfo describes a pinned cloud image: a dated upstream snapshot and, per
// GOARCH, the snapshot-stamped download URL plus the SHA256 fetch verifies before
// use. The arch keys (amd64/arm64) match Go's GOARCH, so one alias resolves to
// the right image on macOS Apple Silicon and on Linux amd64/arm64.
//
// Distro/Version/Codename/BumpedAt are inputs for the contrib/catalog refresher
// (the only writer of Snapshot/Arch values) and are ignored at runtime: the
// library reads only Snapshot (for the cache filename) and Arch (URL + SHA256).
type ImageInfo struct {
	Distro   string               `json:"distro"`
	Version  string               `json:"version"`
	Codename string               `json:"codename,omitempty"`
	Snapshot string               `json:"snapshot"`
	BumpedAt string               `json:"bumped_at,omitempty"`
	Arch     map[string]ArchImage `json:"arch"`
}

// ArchImage is the per-architecture pinned download: the snapshot-stamped URL and
// the SHA256 fetch verifies the downloaded source against before it is cached.
type ArchImage struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

// Ensure downloads and prepares an image, returning the path to the raw disk.
// If the image is already cached it returns immediately. If urlOrAlias is a
// known catalog alias (e.g. "debian-12") the per-GOARCH entry is used: the source
// is fetched under a snapshot-stamped name, SHA256-verified, and converted (when
// it is a qcow2/.img) to the snapshot-stamped raw name. A non-alias is treated as
// a literal URL — unverified, with a basename-derived cache name (the BYO escape
// hatch, unchanged).
func Ensure(cacheDir, urlOrAlias string) (string, error) {
	catalog, err := loadCatalog()
	if err != nil {
		return "", fmt.Errorf("load catalog: %w", err)
	}

	// Resolve the source URL, the verifying SHA256, and the cache filenames. An
	// alias pins all three from the catalog and snapshot-stamps the names; a
	// literal URL derives them from its basename, unverified (unchanged).
	var url, sha, srcFilename, rawFilename string
	if info, ok := catalog[urlOrAlias]; ok {
		arch, ok := info.Arch[runtime.GOARCH]
		if !ok {
			return "", fmt.Errorf("image %q has no URL for %s", urlOrAlias, runtime.GOARCH)
		}
		url = arch.URL
		sha = arch.SHA256
		rawFilename = cacheName(urlOrAlias, info.Snapshot, runtime.GOARCH)
		// The source name carries the snapshot too, with the URL's real extension,
		// so a leftover old-snapshot source is never served stale by fetch's
		// name-keyed cache and the .img conversion below still triggers. For a raw
		// source (Debian) the source and raw names coincide → one verified fetch.
		srcFilename = strings.TrimSuffix(rawFilename, ".raw") + path.Ext(url)
	} else {
		url = urlOrAlias
		urlParts := strings.Split(url, "/")
		srcFilename = urlParts[len(urlParts)-1]
		rawFilename = strings.TrimSuffix(srcFilename, ".qcow2")
		rawFilename = strings.TrimSuffix(rawFilename, ".img")
		rawFilename += ".raw"
	}

	isQcow2 := strings.HasSuffix(srcFilename, ".qcow2") || strings.HasSuffix(srcFilename, ".img")
	rawPath := filepath.Join(cacheDir, rawFilename)

	// If raw already exists, return it
	if _, err := os.Stat(rawPath); err == nil {
		return rawPath, nil
	}

	// Not cached: a multi-hundred-MB download is about to start, so announce it
	// rather than let a first run look like a hung test (ADR-0017, R11). In the
	// macOS helper and the Linux CLI holder this line goes to the holder log (the
	// client surfaces its own "pulling…" line); on the in-process Linux library
	// path it reaches the user directly.
	fmt.Fprintf(os.Stderr, "Pulling cloud image %q (first run, this can take a few minutes)...\n", urlOrAlias)

	// A raw source needs no conversion: fetch it (verified) straight to its raw
	// cache name and we are done.
	if !isQcow2 {
		dest, err := fetch.Ensure(cacheDir, rawFilename, url, sha)
		if err != nil {
			return "", fmt.Errorf("fetch image: %w", err)
		}
		return dest, nil
	}

	// A qcow2/img source is fetched (verified) under its own name, converted to
	// raw, then removed so only the raw image stays cached. On a convert failure
	// remove the source too, so it can't be reused unverified next run.
	srcPath, err := fetch.Ensure(cacheDir, srcFilename, url, sha)
	if err != nil {
		return "", fmt.Errorf("fetch image: %w", err)
	}
	if err := convertQcow2ToRaw(srcPath, rawPath); err != nil {
		_ = os.Remove(rawPath)
		_ = os.Remove(srcPath)
		return "", fmt.Errorf("convert qcow2: %w", err)
	}
	_ = os.Remove(srcPath)

	return rawPath, nil
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

// loadCatalog parses the embedded catalog.json once and caches the result. It
// returns a wrapped error on malformed JSON rather than panicking in package
// init — this is a library, so a bad catalog must surface as an ordinary error to
// the caller of Ensure.
func loadCatalog() (map[string]ImageInfo, error) {
	catalogOnce.Do(func() {
		catalogErr = json.Unmarshal(catalogJSON, &catalogData)
	})
	if catalogErr != nil {
		return nil, fmt.Errorf("parse embedded catalog: %w", catalogErr)
	}
	return catalogData, nil
}

// cacheName returns the snapshot-stamped raw cache filename for a catalog alias:
// <alias>-<snapshot>-<goarch>.raw. The snapshot in the name makes an upstream bump
// a cache miss, the same trick the version-stamped VMM binary names use.
func cacheName(alias, snapshot, goarch string) string {
	return fmt.Sprintf("%s-%s-%s.raw", alias, snapshot, goarch)
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
