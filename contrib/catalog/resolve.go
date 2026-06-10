package main

import (
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/pilat/fleetbox/internal/image"
)

// arches is the set every catalog entry must publish, keyed by GOARCH.
var arches = []string{"amd64", "arm64"}

var (
	// debianSnapshotRE matches a Debian dated snapshot dir (YYYYMMDD-NNNN/),
	// excluding the latest/, daily/, … pointers in the same listing.
	debianSnapshotRE = regexp.MustCompile(`^\d{8}-\d+/$`)
	// ubuntuSnapshotRE matches an Ubuntu dated release dir
	// (release-YYYYMMDD[.N]/), excluding the bare rolling release/ pointer.
	ubuntuSnapshotRE = regexp.MustCompile(`^release-\d{8}(\.\d+)?/$`)
	// hrefRE captures the target of each <a href="…"> in an autoindex page.
	hrefRE = regexp.MustCompile(`href="([^"]+)"`)
)

// resolution is the refreshed value set for one alias: the selected snapshot and
// the per-arch URL + SHA256 derived from it.
type resolution struct {
	snapshot string
	arch     map[string]image.ArchImage
}

// resolve dispatches on the entry's distro to the matching upstream resolver.
func resolve(client *http.Client, info image.ImageInfo) (resolution, error) {
	switch info.Distro {
	case "debian":
		return resolveDebian(client, info)
	case "ubuntu":
		return resolveUbuntu(client, info)
	default:
		return resolution{}, fmt.Errorf("unknown distro %q", info.Distro)
	}
}

// resolveDebian selects the newest dated snapshot under the codename dir and, for
// each arch, builds the serial-bearing .raw URL and computes its SHA256. Debian
// publishes only SHA512SUMS, so each image is stream-downloaded through both a
// sha256 and a sha512 hasher; the sha512 is cross-checked against SHA512SUMS and
// the computed sha256 is recorded. The multi-GB file is never persisted.
func resolveDebian(client *http.Client, info image.ImageInfo) (resolution, error) {
	base := "https://cloud.debian.org/images/cloud/" + info.Codename + "/"
	listing, _, err := httpGet(client, base)
	if err != nil {
		return resolution{}, fmt.Errorf("list %s: %w", base, err)
	}
	snapshot, err := newestDir(listing, debianSnapshotRE, debianSnapshotKey)
	if err != nil {
		return resolution{}, fmt.Errorf("select snapshot in %s: %w", base, err)
	}

	snapDir := base + snapshot + "/"
	sums, _, err := httpGet(client, snapDir+"SHA512SUMS")
	if err != nil {
		return resolution{}, fmt.Errorf("fetch SHA512SUMS: %w", err)
	}

	arch := make(map[string]image.ArchImage, len(arches))
	for _, a := range arches {
		fname := fmt.Sprintf("debian-%s-generic-%s-%s.raw", info.Version, a, snapshot)
		want512, err := sumFor(sums, fname)
		if err != nil {
			return resolution{}, fmt.Errorf("SHA512SUMS: %w", err)
		}
		url := snapDir + fname
		got256, got512, err := hashImage(client, url)
		if err != nil {
			return resolution{}, fmt.Errorf("hash %s: %w", url, err)
		}
		if !strings.EqualFold(got512, want512) {
			return resolution{}, fmt.Errorf(
				"sha512 mismatch for %s: computed %s, SHA512SUMS %s",
				fname,
				got512,
				want512,
			)
		}
		arch[a] = image.ArchImage{URL: url, SHA256: got256}
	}
	return resolution{snapshot: snapshot, arch: arch}, nil
}

// resolveUbuntu selects the newest dated release dir and reads SHA256SUMS for the
// per-arch .img source. The version path 302-redirects to the codename path; the
// resolved (codename) URL is pinned so boot-time downloads do not re-redirect. No
// image is downloaded — Ubuntu publishes the source sha256 directly.
func resolveUbuntu(client *http.Client, info image.ImageInfo) (resolution, error) {
	base := "https://cloud-images.ubuntu.com/releases/" + info.Version + "/"
	listing, effective, err := httpGet(client, base)
	if err != nil {
		return resolution{}, fmt.Errorf("list %s: %w", base, err)
	}
	snapshot, err := newestDir(listing, ubuntuSnapshotRE, ubuntuSnapshotKey)
	if err != nil {
		return resolution{}, fmt.Errorf("select snapshot in %s: %w", base, err)
	}

	// effective is the codename-resolved listing URL after the 302; build the
	// snapshot dir off it so the pinned URLs point straight at the codename path.
	snapDir := effective + snapshot + "/"
	sums, _, err := httpGet(client, snapDir+"SHA256SUMS")
	if err != nil {
		return resolution{}, fmt.Errorf("fetch SHA256SUMS: %w", err)
	}

	arch := make(map[string]image.ArchImage, len(arches))
	for _, a := range arches {
		fname := fmt.Sprintf("ubuntu-%s-server-cloudimg-%s.img", info.Version, a)
		sum256, err := sumFor(sums, fname)
		if err != nil {
			return resolution{}, fmt.Errorf("SHA256SUMS: %w", err)
		}
		arch[a] = image.ArchImage{URL: snapDir + fname, SHA256: sum256}
	}
	return resolution{snapshot: snapshot, arch: arch}, nil
}

// newestDir returns the matching directory name (trailing slash stripped) with the
// highest (primary, secondary) key in an autoindex listing, ranked numerically.
func newestDir(listing string, re *regexp.Regexp, key func(string) (int, int, error)) (string, error) {
	best := ""
	bestA, bestB := -1, -1
	for _, name := range dirNames(listing) {
		if !re.MatchString(name) {
			continue
		}
		bare := strings.TrimSuffix(name, "/")
		a, b, err := key(bare)
		if err != nil {
			return "", fmt.Errorf("parse %q: %w", bare, err)
		}
		if a > bestA || (a == bestA && b > bestB) {
			bestA, bestB, best = a, b, bare
		}
	}
	if best == "" {
		return "", errors.New("no matching snapshot directory in listing")
	}
	return best, nil
}

// debianSnapshotKey splits a Debian snapshot (YYYYMMDD-NNNN) into its date and
// build serials for numeric ordering.
func debianSnapshotKey(name string) (date, build int, err error) {
	datePart, buildPart, ok := strings.Cut(name, "-")
	if !ok {
		return 0, 0, fmt.Errorf("malformed debian snapshot %q", name)
	}
	date, err = strconv.Atoi(datePart)
	if err != nil {
		return 0, 0, fmt.Errorf("date: %w", err)
	}
	build, err = strconv.Atoi(buildPart)
	if err != nil {
		return 0, 0, fmt.Errorf("build: %w", err)
	}
	return date, build, nil
}

// ubuntuSnapshotKey splits an Ubuntu release dir (release-YYYYMMDD[.N]) into its
// date and optional point-release serials for numeric ordering.
func ubuntuSnapshotKey(name string) (date, point int, err error) {
	rest := strings.TrimPrefix(name, "release-")
	datePart, pointPart, hasPoint := strings.Cut(rest, ".")
	date, err = strconv.Atoi(datePart)
	if err != nil {
		return 0, 0, fmt.Errorf("date: %w", err)
	}
	if hasPoint {
		point, err = strconv.Atoi(pointPart)
		if err != nil {
			return 0, 0, fmt.Errorf("point: %w", err)
		}
	}
	return date, point, nil
}

// dirNames extracts the directory names (trailing slash kept) from an
// Apache/nginx autoindex page, normalized to their final path segment.
func dirNames(html string) []string {
	var names []string
	for _, m := range hrefRE.FindAllStringSubmatch(html, -1) {
		href := m[1]
		if !strings.HasSuffix(href, "/") {
			continue
		}
		name := strings.TrimSuffix(href, "/")
		name = name[strings.LastIndex(name, "/")+1:]
		if name == "" || name == ".." {
			continue
		}
		names = append(names, name+"/")
	}
	return names
}

// sumFor returns the checksum whose line names filename. It handles both the
// Debian SHA512SUMS form (<hash>␠␠<name>) and the Ubuntu SHA256SUMS binary form
// (<hash>␠*<name>) by splitting on whitespace and stripping a leading '*'.
func sumFor(sums, filename string) (string, error) {
	for line := range strings.SplitSeq(sums, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		if strings.TrimPrefix(fields[1], "*") == filename {
			return fields[0], nil
		}
	}
	return "", fmt.Errorf("no checksum line for %q", filename)
}

// hashImage streams url through a sha256 and a sha512 hasher in one pass and
// returns both digests as lowercase hex, without persisting the file.
func hashImage(client *http.Client, url string) (sha256hex, sha512hex string, err error) {
	resp, err := client.Get(url)
	if err != nil {
		return "", "", fmt.Errorf("http get: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("http %s", resp.Status)
	}

	h256 := sha256.New()
	h512 := sha512.New()
	if _, err := io.Copy(io.MultiWriter(h256, h512), resp.Body); err != nil {
		return "", "", fmt.Errorf("stream: %w", err)
	}
	return hex.EncodeToString(h256.Sum(nil)), hex.EncodeToString(h512.Sum(nil)), nil
}

// httpGet fetches url and returns the body and the effective URL after any
// redirects (used to pin Ubuntu's codename-resolved path). Non-200 responses are
// errors.
func httpGet(client *http.Client, url string) (body, effectiveURL string, err error) {
	resp, err := client.Get(url)
	if err != nil {
		return "", "", fmt.Errorf("http get: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("http %s", resp.Status)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", fmt.Errorf("read body: %w", err)
	}

	eff := resp.Request.URL.String()
	if strings.HasSuffix(url, "/") && !strings.HasSuffix(eff, "/") {
		eff += "/"
	}
	return string(b), eff, nil
}
