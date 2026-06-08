#!/usr/bin/env bash
#
# vendor-vz.sh — regenerate third_party/vz from pinned sources.
#
# third_party/vz is NOT a separate module: it is part of the fleetbox module, a
# vendored + patched + renamed copy of Code-Hex/vz that ships and builds like our
# own package. Recipe (every input pinned by an immutable SHA):
#   1. clone stock Code-Hex/vz at VZ_BASE_SHA          (the PR's branch point)
#   2. apply norio-nomura's vmnet patch (PR #205) at VZ_PATCH_SHA
#   3. rename the import path Code-Hex/vz/v3 -> this module's third_party/vz
#   4. constrain every .go to `//go:build darwin` (it is darwin-only cgo) so the
#      package is cleanly skipped by `go build ./...` on non-darwin platforms
#   5. drop the upstream go.mod, tests, dev tools and scaffolding; keep the build
#      payload + LICENSE; write a NOTICE recording what we did and why
#
# Rerun only to re-sync, then commit the result. License: MIT (see LICENSE / NOTICE).
# Exit bridge: when PR #205 lands upstream, delete third_party/vz and this script
# and depend on the released Code-Hex/vz directly (docs/adr/0008).
set -euo pipefail

# --- pinned sources -----------------------------------------------------------
VZ_UPSTREAM="https://github.com/Code-Hex/vz"
VZ_BASE_SHA="0d35cf3a3a8b834ee3b5bf61e4946971b2c0d61a"   # Code-Hex/vz main, PR #205 branch point
VZ_PATCH_REPO="https://github.com/norio-nomura/vz"
VZ_PATCH_SHA="e27a5fb55e5936b69a62590f4ce326d8772641a9"  # PR #205 head: VZVmnetNetworkDeviceAttachment (macOS 26)
OLD_PATH="github.com/Code-Hex/vz/v3"
NEW_PATH="github.com/pilat/fleetbox/third_party/vz"

# --- locations ----------------------------------------------------------------
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
dest="${root}/third_party/vz"
work="$(mktemp -d)"
patch="$(mktemp)"
trap 'rm -rf "${work}" "${patch}"' EXIT

echo ">> 1/5 clone stock vz @ ${VZ_BASE_SHA}"
git clone --quiet "${VZ_UPSTREAM}" "${work}"
git -C "${work}" -c advice.detachedHead=false checkout --quiet "${VZ_BASE_SHA}"

echo ">> 2/5 fetch + apply vmnet patch @ ${VZ_PATCH_SHA}"
git -C "${work}" remote add patch "${VZ_PATCH_REPO}"
git -C "${work}" fetch --quiet patch "${VZ_PATCH_SHA}"
git -C "${work}" diff "${VZ_BASE_SHA}" "${VZ_PATCH_SHA}" >"${patch}"
git -C "${work}" apply --whitespace=nowarn "${patch}"

echo ">> 3/5 rename import path ${OLD_PATH} -> ${NEW_PATH}"
find "${work}" -name '*.go' -not -path '*/.git/*' \
	-exec perl -pi -e "s,\\Q${OLD_PATH}\\E,${NEW_PATH},g" {} +

echo ">> 4/5 constrain every .go to //go:build darwin (darwin-only cgo)"
while IFS= read -r f; do
	if grep -qE '^//go:build' "${f}"; then
		grep -qE '^//go:build.*darwin' "${f}" && continue   # already darwin-constrained
		# AND darwin into the existing (non-darwin) constraint, first line only
		perl -i -pe 's{^//go:build (.+)$}{//go:build darwin && ($1)} unless $seen; $seen=1 if m{^//go:build}' "${f}"
	else
		case "$(basename "${f}")" in
			*_darwin.go | *_darwin_*.go) continue ;;   # darwin via filename already
		esac
		tmp="$(mktemp)"
		{ printf '//go:build darwin\n\n'; cat "${f}"; } >"${tmp}" && mv "${tmp}" "${f}"
	fi
done < <(find "${work}" -name '*.go' -not -path '*/.git/*')

# Normalize build constraints: gofmt canonicalizes the edited //go:build lines and
# syncs the legacy `// +build` lines to match (a mismatch makes `go vet`/`go test`
# fail even though `go build` tolerates it).
gofmt -w "${work}"

echo ">> 5/5 stage vendored package into ${dest}"
rm -rf "${dest}"
mkdir -p "${dest}"
# Keep only the buildable package payload + license; drop the upstream module
# boundary, tests, dev tools, examples, CI and other repo scaffolding.
rsync -a \
	--exclude='.git' --exclude='.github' --exclude='example' --exclude='cmd' \
	--exclude='testdata' --exclude='*_test.go' --exclude='internal/testhelper' \
	--exclude='go.mod' --exclude='go.sum' \
	--exclude='Makefile' --exclude='README.md' --exclude='CONTRIBUTING.md' \
	--exclude='.clang-format' --exclude='.gitignore' \
	"${work}/" "${dest}/"

cat >"${dest}/NOTICE" <<EOF
fleetbox vendors a patched copy of Code-Hex/vz in this directory.

WHAT
  third_party/vz is Code-Hex/vz (Go bindings for Apple Virtualization.framework)
  plus norio-nomura's not-yet-released "VZVmnetNetworkDeviceAttachment" change
  (PR #205), which fleetbox needs for macOS 26 vmnet SharedMode VM-to-VM networking.

WHY IT LIVES HERE
  The vmnet patch is unreleased upstream. A separate module + replace directive
  would not resolve for a downstream \`go get\`, so the code lives here as part of
  the fleetbox module and builds like our own package.

WHAT WE CHANGED (mechanically — see hack/vendor-vz.sh; nothing edited by hand)
  - import path renamed: ${OLD_PATH} -> ${NEW_PATH}
  - every .go constrained to //go:build darwin (darwin-only cgo) so the package is
    skipped by 'go build ./...' on non-darwin platforms
  - upstream go.mod/go.sum, tests, dev tools, examples and CI scaffolding removed
  Regenerate with: make vendor-vz

SOURCES (pinned)
  base:  ${VZ_UPSTREAM} @ ${VZ_BASE_SHA}
  patch: ${VZ_PATCH_REPO} @ ${VZ_PATCH_SHA} (PR #205)

LICENSE
  MIT, (c) 2025 codehex — retained verbatim in LICENSE. The vendored patch is a
  contribution to that MIT-licensed project, under the same license.

EXIT
  When PR #205 (or a successor) is released upstream, delete third_party/vz and
  hack/vendor-vz.sh and depend on the released Code-Hex/vz directly. See docs/adr/0008.
EOF

echo ">> done. review:  git -C \"${root}\" status third_party/vz"
