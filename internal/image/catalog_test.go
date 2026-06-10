package image

import (
	"net/url"
	"regexp"
	"strings"
	"testing"
)

// sha256RE matches a lowercase 64-hex SHA256 digest.
var sha256RE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// wantAliases is the exact set of catalog aliases v0 ships (ADR-0019). Adding an
// OS means adding a key here and in catalog.json.
var wantAliases = []string{
	"debian-11", "debian-12", "debian-13",
	"ubuntu-22.04", "ubuntu-24.04", "ubuntu-26.04",
}

// TestCatalogValid is the safety net that buys back the compile-time check the Go
// map gave us before the catalog became embedded JSON (ADR-0019): it parses
// catalog.json and asserts every entry is a fully pinned, verifiable image. A
// hand-broken entry (empty sha, a /latest/ URL, a missing arch, an empty snapshot)
// fails it. Pure parse/string checks — platform-independent, runs in `make test`.
func TestCatalogValid(t *testing.T) {
	catalog, err := loadCatalog()
	if err != nil {
		t.Fatalf("loadCatalog: %v", err)
	}

	if len(catalog) != len(wantAliases) {
		t.Errorf("catalog has %d entries, want %d", len(catalog), len(wantAliases))
	}
	for _, alias := range wantAliases {
		if _, ok := catalog[alias]; !ok {
			t.Errorf("catalog missing expected alias %q", alias)
		}
	}

	for alias, info := range catalog {
		if info.Distro != "debian" && info.Distro != "ubuntu" {
			t.Errorf("%s: distro %q not in {debian, ubuntu}", alias, info.Distro)
		}
		if info.Distro == "debian" && info.Codename == "" {
			t.Errorf("%s: debian entry has empty codename", alias)
		}
		if info.Snapshot == "" {
			t.Errorf("%s: empty snapshot", alias)
		}

		for _, arch := range []string{"amd64", "arm64"} {
			img, ok := info.Arch[arch]
			if !ok {
				t.Errorf("%s: missing arch %q", alias, arch)
				continue
			}
			if !sha256RE.MatchString(img.SHA256) {
				t.Errorf("%s/%s: sha256 %q is not 64 lowercase hex chars", alias, arch, img.SHA256)
			}
			u, err := url.Parse(img.URL)
			if err != nil {
				t.Errorf("%s/%s: url %q: %v", alias, arch, img.URL, err)
				continue
			}
			if u.Scheme != "https" {
				t.Errorf("%s/%s: url scheme %q, want https (%s)", alias, arch, u.Scheme, img.URL)
			}
			if !strings.Contains(img.URL, info.Snapshot) {
				t.Errorf("%s/%s: url %q does not contain snapshot %q", alias, arch, img.URL, info.Snapshot)
			}
			if strings.Contains(img.URL, "/latest/") {
				t.Errorf("%s/%s: url %q still points at /latest/", alias, arch, img.URL)
			}
			if strings.Contains(u.Path, "/release/") {
				t.Errorf("%s/%s: url %q points at the bare rolling /release/ pointer", alias, arch, img.URL)
			}
		}

		// cacheName must be well-formed; a "--" means a field (most likely the
		// snapshot) is empty, e.g. debian-12--amd64.raw.
		name := cacheName(alias, info.Snapshot, "amd64")
		if want := alias + "-" + info.Snapshot + "-amd64.raw"; name != want {
			t.Errorf("%s: cacheName = %q, want %q", alias, name, want)
		}
		if strings.Contains(name, "--") {
			t.Errorf("%s: cacheName %q has an empty field (likely empty snapshot)", alias, name)
		}
	}
}
