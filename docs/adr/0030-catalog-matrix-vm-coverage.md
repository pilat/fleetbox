# ADR: Catalog-Matrix VM Coverage

**Date:** 2026-06-20
**Status:** Accepted

## Context

fleetbox's VM-boot tests and every CI lane booted exactly one image: `debian-12`. The
conformance test (boot → SSH → egress → copy → teardown) is the per-image *boot* guard, and
it only ever guarded one image. That is how the x86_64 firmware boot bug (ADR-0029) shipped
green: `debian-12` (kernel 6.1, small initrd) booted fine through the firmware, so a catalog
image that the firmware *could not* boot (`ubuntu-26.04`, kernel 7.0, 69 MB initrd) was never
exercised by any test. The catalog is the contract — the set of OSes fleetbox claims to boot —
but the test suite only proved one entry of it.

## Decision

**Matrix the conformance test over the whole catalog, enumerated programmatically.**

- `image.Aliases()` returns the catalog's aliases (sorted) — the programmatic source of truth,
  so a new catalog entry is covered with no hand-maintained list to update.
- `fleetboxtest.MatrixImages(tb)` resolves the set from `FLEETBOX_TEST_IMAGES` (comma-separated):
  **unset or empty → the full catalog** (load-bearing: CI's nightly lane sets the var empty to
  mean "everything", so empty must NOT collapse to "none"); a non-empty value is the explicit
  subset, each alias validated against the catalog (an unknown alias is a fatal test error, not
  a silent literal-URL fetch). `TestVMConformance` loops over it with one `t.Run` subtest per
  image, **serially** (each VM is heavy), each subtest getting its own boot budget and a
  store-safe VM name.
- **Only conformance is matrixed.** It is the per-image boot guard. The cluster and fixtures
  tests exercise VM↔VM / fixture mechanics that are orthogonal to per-image boot, so they stay
  single-image (`debian-12`).
- **Lanes:** PR/push boot a fixed 3-image subset (`debian-12` baseline + the two newest kernels,
  `debian-13`,`ubuntu-26.04`) to bound CI wait; a nightly `schedule` and manual `workflow_dispatch`
  boot the full catalog; local `make test-vm` boots the full catalog. Matrixing the *suite* (not
  just the amd64 lane) means macOS VZ (local `make test-vm`) and arm64 (the nested dogfood's inner
  run, now `debian-12,ubuntu-26.04`) get multi-image coverage for free; only the amd64
  `vm-linux.yml` lane is real CI.

## Alternatives Considered

**A hand-maintained image list in the test.** Rejected: it drifts from the catalog — the exact
failure this ADR fixes (a catalog entry no test covered). Enumerating `image.Aliases()` keeps the
guard honest by construction.

**Matrix every VM test (cluster, fixtures) too.** Rejected: those test mechanics that do not vary
by image, so N× the boots buys no coverage. One boot per image in conformance is the per-image
guard; the others stay single-image.

**Full catalog on every PR.** Rejected on cost: six serial boots (plus first-run downloads +
qcow2→raw converts of the Ubuntu images) make PRs wait too long. The 3-image subset covers the
baseline + the newest kernels (where boot regressions actually appear); the nightly full run
catches the rest.

## Consequences

- **A non-bootable catalog entry can no longer ship green.** The boot guard now covers every OS
  fleetbox claims to support, on the lanes that can boot it.
- **The matrix may surface backend-specific boot bugs.** Running the full set on macOS VZ (local
  `make test-vm`) may expose a *separate* VZ boot bug on a newer image — that is the matrix doing
  its job; triage and fix as a follow-up, do **not** drop the image from the catalog. arm64 +
  ubuntu is booted for real for the first time by the nested subset; a root/console failure there
  is an arm64 finding in the shared extraction/cmdline.
- **CI cost rises:** PRs now wait on three real VM boots; the nightly lane boots six. Bounded
  deliberately by the subset and a `-test.timeout 60m` (amd64 CI) / `120m` (local `make test-vm`,
  which also clears the nested dogfood).
- **Deferred (unchanged):** cluster/fixtures stay single-image; deriving `root=` for a non-catalog
  BYO image stays out of scope (ADR-0029).
