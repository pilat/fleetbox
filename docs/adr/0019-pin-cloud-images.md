# ADR: Pin Cloud Images to Dated Snapshots via an Embedded JSON Catalog

**Date:** 2026-06-10
**Status:** Accepted

## Context

fleetbox sells itself as a *test fixture* tool, yet its base cloud images were the
one artifact class it did not pin. The catalog pointed `debian-12` / `ubuntu-24.04`
at `/latest/` URLs with an empty `SHA256`, and `internal/fetch` skips verification
when the digest is empty. The fixture was therefore both non-deterministic and
unverified.

The root cause is in the cache, not the catalog. `internal/fetch` keys the cache on
**filename only** and returns on the first `os.Stat` hit — no HEAD, no revalidation.
The cache filename is derived from the URL's last path segment, and `/latest/` never
appears in it. So:

- `latest` resolves **once per machine**, at cache-warm time. Forever after the
  cached file is served — unverified and unrecorded.
- Two CI runners warmed a month apart silently run **different** base images, and
  nothing records which snapshot ran.
- The "freshness" that justified the empty `SHA256` is illusory the moment the cache
  is warm (≈always). The old design delivered **neither** reproducibility **nor**
  freshness.

The VMM binary and firmware (ADR-0011) do not suffer this: their cache filename
carries the version (`cloud-hypervisor-v52.0-amd64`), so a version bump changes the
filename, guarantees a cache miss, and forces a re-download that `fetch` verifies.
The fix is to restore that same idiom for images.

## Decision

1. **Cloud images are first-class pinned artifacts.** Every alias pins a **dated
   upstream snapshot URL + per-arch SHA256**. Every `/latest/` URL and every empty
   `SHA256` is dropped.

2. **The snapshot serial goes into the cache filename — for both the converted raw
   and the downloaded source.** The converted raw is `<alias>-<snapshot>-<arch>.raw`;
   the source download is `<alias>-<snapshot>-<arch><ext>` (`.img`/`.raw`, the URL's
   real extension). This is the load-bearing change: a snapshot bump changes the
   filename, so a warm cache can never silently serve a stale image. Stamping the
   **source** name too closes an Ubuntu-specific hole — the Ubuntu basename
   (`ubuntu-24.04-server-cloudimg-amd64.img`) is identical across snapshots, so
   without the stamp a leftover old-snapshot source would be served by `fetch`'s
   name-cache and converted under the new snapshot's raw name without re-verifying.
   This applies to the **alias branch only**; a literal `WithImage(url)` keeps its
   unchanged basename-derived, unverified path (the BYO escape hatch).

3. **`fetch` stays sha256-only and behaviorally unchanged.** It still verifies before
   rename and (for images) before conversion: `image.Ensure` fetches the `.img`
   source and `fetch` verifies *that* against the pinned sha256 before
   `convertQcow2ToRaw` runs. No sidecar source-hash file; the converted `.raw` is not
   re-verified. The convert-error path now removes **both** the raw and the source so
   a failed conversion never leaves a stale verified source behind.

4. **The catalog is an embedded JSON data file, not a Go literal.** `//go:embed
   catalog.json` in `internal/image`, parsed once via `sync.Once` into an unexported
   cache and exposed through `loadCatalog() (map[string]ImageInfo, error)` — a
   wrapped error on malformed JSON, never a panic in package init (this is a
   library). The old exported `Catalog` var is removed. The cost — a malformed
   catalog now fails at runtime parse instead of compile time — is bought back by
   `TestCatalogValid`, which gates `make test` / CI.

5. **The "no yaml, no templates" principle is not violated — it is carved.** That
   principle forbids **user-side** configuration. catalog.json is **internal data
   compiled into the binary** that the user never sees or edits — the same role the
   Go map played, in a form a bot can rewrite safely (marshal a struct) instead of
   editing Go source in place. JSON, not YAML, because `encoding/json` is stdlib and
   this library deliberately keeps a pure-Go, minimal-dependency footprint. No inline
   comments; provenance lives in a `bumped_at` data field.

6. **A `contrib/catalog` Go tool refreshes the values; the human keys decide which
   OSes exist.** The human authors `distro` / `version` / `codename`; the tool
   refreshes `snapshot` / `bumped_at` / `arch.<a>.url` / `arch.<a>.sha256`. It
   imports the `internal/image` `ImageInfo`/`ArchImage` types (single source of truth
   for the shape) and holds the per-distro resolvers — allowed because it is
   `contrib/`, not the runtime library. `make catalog` runs it; a monthly scheduled
   GitHub Action runs it and opens a PR when upstream moved.

7. **The Debian/Ubuntu hash asymmetry is handled entirely in `contrib/`.** `fetch`
   verifies sha256. Ubuntu publishes `SHA256SUMS` (binary-mode, `*`-prefixed
   filenames), so the tool parses the sha256 directly — no image download. Debian
   publishes **only** `SHA512SUMS`, so the tool **stream-downloads each `.raw`,
   computing sha256 and sha512 in one pass**, cross-checks the sha512 against
   `SHA512SUMS` (fails loudly on mismatch), and records the computed sha256. The
   multi-GB file is streamed through the hashers, never persisted. `fetch` is **not**
   taught sha512 — the shared primitive stays sha256-only for the binaries' sake.

8. **Renovate does not do these bumps.** Renovate here only manages `go.mod` +
   GitHub Actions; it has no datasource for distro snapshot dirs and does not
   recompute asset checksums. The bump mechanism is the `contrib/catalog` tool driven
   by the scheduled Action.

9. **v0 OS set:** Debian 11/12/13 (bullseye/bookworm/trixie), Ubuntu 22.04/24.04/26.04.
   More OSes are added by adding a key. No new user-facing knob: `WithImage(url)`
   already passes a literal URL through unverified for BYO/bleeding-edge.

## Alternatives Considered

**"Real latest" via per-`Ensure` manifest revalidation** (fetch `SHA*SUMS` every
boot, sidecar source-hash, TTL, stale-while-revalidate). Rejected: a round-trip on
every boot's hot path, and *still* less reproducible than pinning.

**ETag / conditional GET.** Rejected: Debian `/latest/` is a 302 to a rotating
mirror (mirror-specific ETag); only Ubuntu has a usable ETag. Not uniform.

**Keep `latest` and only document it.** Rejected: does not fix per-machine
divergence — the whole point.

**Codegen the whole Go catalog file** instead of embedding JSON. Rejected: emitting
data (marshal a struct) is a far simpler and safer generator than emitting Go source.

**YAML instead of JSON.** Rejected: YAML would add a dependency to a library that
keeps a minimal pure-Go footprint; `encoding/json` is stdlib.

**Add an RPM family now.** Deferred to the first real request.

## Consequences

- **The fixture is reproducible and verified.** Every boot of an alias pulls a
  named, SHA256-checked snapshot; a snapshot bump is a guaranteed cache miss and a
  re-download that `fetch` verifies. Which snapshot ran is recorded in the catalog.
- **A malformed catalog fails at runtime, not compile time.** `TestCatalogValid`
  buys back the lost compile-time check: it asserts every entry has both arches,
  64-hex lowercase sha256s, https dated-snapshot URLs (no `/latest/`, no bare
  `/release/`), a non-empty snapshot, and a debian codename — and that the cache name
  is well-formed and exactly the six expected aliases are present.
- **Refreshing the catalog downloads the Debian images** (their sha256 must be
  computed). Acceptable for a monthly maintenance run; the tool streams through the
  hashers so disk stays bounded.
- **New surface:** `internal/image/catalog.json` (embedded data), the
  `contrib/catalog` tool, a `make catalog` target, and a `catalog-refresh.yml`
  scheduled workflow that opens a PR with a PAT/App token (so the PR triggers CI).
- **Supersedes in part ADR-0003 and ADR-0011.** Both carried "latest URLs can't have
  stable checksums" language that no longer holds: images are now pinned and verified
  exactly like the binaries. The rest of those ADRs (stock images, EFI boot, the dumb
  alias→data map, download-and-cache) stands.
