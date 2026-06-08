# ADR: Vendor the vz Fork In-Module Instead of a `replace` Directive

**Date:** 2026-06-08
**Status:** Accepted

## Context

ADR-0008 needed norio-nomura's unreleased `VZVmnetNetworkDeviceAttachment` change
(Code-Hex/vz PR #205) for macOS 26 vmnet SharedMode networking. It vendored that fork
under `third_party/vz` as a **separate module** — its own `go.mod`, declaring the
upstream path `github.com/Code-Hex/vz/v3` — wired in with
`replace github.com/Code-Hex/vz/v3 => ./third_party/vz`.

That builds fine when fleetbox is the main module, i.e. for us and in CI. But Go applies
`replace` directives **only from the main module's `go.mod`**. For anyone who runs
`go get github.com/pilat/fleetbox` and imports the package, fleetbox is a dependency, so
its `replace` is ignored and Go resolves the genuine upstream `Code-Hex/vz v3.7.1` —
which has no `vmnet` subpackage and no `NewVmnetNetworkDeviceAttachment`. The darwin
build of `internal/backend/vz` then fails to compile. In effect the library was not
installable on macOS by anyone but us — fatal for a library-first project whose pitch is
"go get and use." (Linux consumers were unaffected: the cloud-hypervisor backend never
imports vz.)

Depending on norio-nomura's fork directly does not help: his branch keeps the
`github.com/Code-Hex/vz/v3` module path (so the PR can merge), so it cannot be `require`d
(path mismatch) and a `replace` onto it is still ignored downstream. The patch is also
unreleased, so there is nothing tagged to depend on.

## Decision

Vendor the fork as **part of the fleetbox module** — an ordinary in-repo package at
`third_party/vz`, with **no separate `go.mod` and no `replace`**. It is generated, not
hand-edited, by `make vendor-vz` (`hack/vendor-vz.sh`) from inputs pinned by immutable
SHA:

1. clone stock `Code-Hex/vz` at the PR's branch point (`0d35cf3…`);
2. apply norio-nomura's vmnet patch, PR #205 (`e27a5fb…`);
3. rename the import path `github.com/Code-Hex/vz/v3` → `github.com/pilat/fleetbox/third_party/vz`;
4. constrain **every** `.go` file to `//go:build darwin` (the package is darwin-only cgo),
   then `gofmt` to canonicalize the constraints and keep the legacy `// +build` lines in
   sync;
5. drop the upstream `go.mod`/`go.sum`, tests, dev tools (`cmd/`), examples and CI
   scaffolding; keep the build payload, the MIT `LICENSE` (© codehex, retained verbatim),
   and a generated `NOTICE` recording provenance.

The vendored package's transitive dependencies (`go-infinity-channel`, `golang.org/x/mod`,
`golang.org/x/sys`) become direct requires of fleetbox. The whole-file `//go:build darwin`
constraint is what stops a non-darwin `go build/test/vet ./...` from trying to compile the
darwin cgo sources — it replaces the isolation the separate-module boundary used to give.

## Alternatives Considered

**Separate module + relative `replace` (ADR-0008's mechanism).** The `replace` is ignored
for downstream consumers, so `go get` on macOS resolves the real upstream and fails to
compile. This is the breakage that forced this ADR.

**Depend on norio-nomura's fork directly.** His fork keeps the `Code-Hex/vz/v3` module
path, so `require github.com/norio-nomura/vz/v3` is a path mismatch and a `replace` onto it
is again ignored downstream. No published, renamed module exists.

**Nested module under our own path + a published tag** (e.g.
`github.com/pilat/fleetbox/third_party/vz` tagged `third_party/vz/vX.Y.Z`). This *does*
resolve for consumers and keeps the module boundary that shields non-darwin `go ... ./...`
from the cgo files. Rejected: it adds submodule tag/`require` ceremony and a release step
that is easy to forget, for code that is effectively ours. Folding it in with build tags
achieves the same isolation without a second module.

**Republish the fork as a standalone module we own** (`github.com/pilat/vz`). Same
import-path rewrite as folding in, but adds a separate repo to host, tag and maintain — not
worth it for a temporary bridge.

## Consequences

- `go get github.com/pilat/fleetbox` builds on macOS for any consumer: the vendored code
  ships inside the main module's zip, there is no `replace` to be ignored, and no extra tag
  to publish.
- Non-darwin is unaffected: the per-file `//go:build darwin` tags make
  `go build/test/vet ./...` skip the vz packages on Linux, exactly as the old
  separate-module boundary did; the cloud-hypervisor backend never imports vz.
- The fork is maintained by re-running `make vendor-vz` against new pinned SHAs — a
  deliberate, auditable regeneration rather than hand-patched files. `hack/vendor-vz.sh`
  and `third_party/vz/NOTICE` carry the provenance and the MIT attribution.
- `go.mod` gains `go-infinity-channel`, `x/mod` and `x/sys` as direct requires.
- `third_party/vz` is importable by external code (it is not under `internal/`); accepted,
  since it exposes no API fleetbox advertises and the `vz-isolation` depguard rule keeps our
  own code from importing it outside `internal/backend/vz`.
- **Supersedes the dependency-wiring mechanism of ADR-0008** — its `replace` directive and
  its "drop the `replace`" exit criterion. ADR-0008's networking decision (vmnet SharedMode
  as the sole macOS path) is unchanged. The exit bridge is restated: when PR #205 (or a
  successor) is released upstream, delete `third_party/vz` and `hack/vendor-vz.sh` and
  depend on the released `Code-Hex/vz` directly.
