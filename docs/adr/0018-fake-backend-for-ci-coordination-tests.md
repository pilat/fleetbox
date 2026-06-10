# ADR: Fake Backend for CI Coordination Tests

**Date:** 2026-06-10
**Status:** Accepted

## Context

The most stateful, highest-risk code on the macOS path had zero automated
coverage on pull requests. Two things in particular:

- **Bound-helper teardown** — when the test process dies (including `kill -9`),
  the helper must reap itself and its in-process VMs, via the control-connection
  EOF and the reparent poll (ADR-0017, R4).
- **The holder protocol** — the `internal/control` client and `internal/holder`
  server agreeing on status framing, the bind/version handshake, and the EOF
  death-watch.

Both were exercised only by booting a real VM (`make test-vm`, hand-run on a
maintainer's Mac). PR CI cannot reach them: the `macos-26` runner has no nested
virtualization so it cannot boot a VZ VM, and `vm-linux.yml` (a real
cloud-hypervisor boot) triggers only on `push:[main]` + `workflow_dispatch`, not
`pull_request`. So a regression in teardown — a helper that leaks VMs when its
test process dies — passed PR CI green. For a "VMs as test fixtures" library, a
harness that leaks VMs is the cardinal sin.

Two facts made a fix tractable. The backend is already a clean interface
(`backend.Backend`/`VM`/`Network`) that `internal/orchestrator` operates against;
the only thing hardwiring a real hypervisor is the compile-time `newBackend()`
selector. And the ADR-0017 sever already made the importable package and the
helper-driving client pure Go — so a fake helper can be built with one `go build`
on a stock runner, no entitlement, no codesign.

## Decision

Introduce a **build-tagged fake backend** (`fleetbox_fake`) at the existing
`backend.Backend` seam, so the whole cross-process coordination path —
`control.Spawn` → exec `fleetbox-helper` → `holder.Run` → orchestrator → backend
— runs fast, deterministic, and green on a stock macOS CI runner with no VM boot
and no codesign.

- `internal/backend/fake` is a dumb, instant, pure-Go implementation. It records
  every `backend.Config` and exposes mutex-guarded package globals for tests to
  inspect (`Created`, `Stopped`, `NetworksClosed`) plus a `FailCreate` fault hook
  (also readable cross-process via `FLEETBOX_FAKE_FAIL_CREATE`). `WaitForIP`
  returns an unroutable TEST-NET-3 address; `Start`/`Stop` are no-ops.
- Selection is a **build tag, not an env var**. `internal/orchestrator/backend_fake.go`
  (`//go:build fleetbox_fake`) provides `newBackend`/`nestedVirtSupported`/`preflight`;
  the three platform files gain `!fleetbox_fake`. The fake is therefore physically
  absent from a normal `go build ./...`; only `-tags fleetbox_fake` links it.
- A companion `skipSSHWait()` build-tag seam (`sshwait.go` / `sshwait_fake.go`)
  short-circuits the real `ssh.Dial` in `startOnNetwork`, which would otherwise
  block for the full timeout against the fake's unroutable IP. The production
  build compiles `skipSSHWait() == false` literally.
- Tests, all under `-race`:
  - in-process orchestrator tests (`-tags fleetbox_fake`) covering the shared
    Linux+darwin lifecycle (create/cluster/teardown/create-failure);
  - white-box `internal/holder` and `internal/control` protocol tests (untagged,
    so they ride `make test`) for status framing, the EOF death-watch, and the
    version-mismatch rejection;
  - a darwin cross-process teardown test driving a **separately built** fake
    helper via `FLEETBOX_HELPER`, gated at runtime on `FLEETBOX_FAKE_HELPER`. It
    calls the public `fleetbox.Start` directly (NOT `fleetboxtest.Start`, which
    skips when nested virt is absent — on a hosted `macos-26` runner that would
    silently turn the gate into a no-op).
- `-race` is wired into `make test` AND the fake-helper build (`-race` on the
  parent test process does not instrument the spawned subprocess). New
  `make test-fake` / `make lint-fake`; `ci.yml` gains the matching steps.

## Alternatives Considered

**Env-var backend selection (`FLEETBOX_FAKE_BACKEND=1`).** Rejected: it would ship
a synthetic boot path inside the released, signed helper, reachable by an
environment variable. The build tag makes the fake code physically absent from the
production artifact.

**In-process-only test (no fake helper), or a socketpair protocol test as the
terminus.** Both miss the cross-process spawn/reap/EOF teardown — the exact
regression feared. The fake helper is the only thing that crosses a real process
boundary, so it is the only thing that can test `watchParent` and the EOF reap.

**Asserting values the fake defines (e.g. the fake IP).** Rejected as tautology.
Assertions target observable effects of the real coordination code: recorded
`backend.Config` fields, on-disk artifacts the real orchestrator wrote
(`disk.raw`, `seed.iso`, `fixture-<i>.img`), retired sockets/pidfiles, and the
helper process actually exiting.

## Consequences

- Teardown and the holder protocol now gate **pre-merge** on a stock macOS runner,
  fast and deterministic, with the race detector running the holder's goroutines
  for the first time. The kill-9 test fails loudly if the helper leaks — verified
  by temporarily no-op'ing `triggerBoundShutdown`, which turns
  `TestCoordReapOnKillNine` into `--- FAIL` (not PASS, not SKIP).
- **False-confidence boundary (the fake can NEVER catch this — keep it honest):**
  the fake proves *coordination*, not that a VM boots. It cannot exercise
  `vz.Backend.Create`'s real wiring (EFI, NIC attachment, device config, serial
  goroutine), whether a real hypervisor process dies on Stop, real IP discovery
  (`dhcpd_leases`, the `:22` probe), SSH/cloud-init success, fixture arrival
  in-guest, VM↔VM connectivity, or image download/qcow2 conversion. Those stay
  covered only by `make test-vm` (macOS, M3+/26+) and `vm-linux.yml` (real CH
  boot). Discipline: keep the fake trivial — if it grows latency simulation or a
  fault matrix, that is re-implementing the hypervisor, and real-hardware tests
  cover it better.
- The fake package compiles under `go list ./...` but must never be **linked**
  into a no-tag binary; this is checked with `go list -deps`.
- A forgotten `!fleetbox_fake` on a platform file self-catches: under the tag it
  produces a duplicate `newBackend` redeclaration compile error.
- `-race` requires cgo; the new steps set `CGO_ENABLED=1` explicitly so a stray
  `CGO_ENABLED=0` cannot silently drop instrumentation. The fake helper built
  `-tags fleetbox_fake -race` links the race runtime (cgo) but not vz, so it still
  needs no entitlement or codesign.
- **Out of scope, not lost:** `vm-linux.yml` still triggers only on
  `push:[main]` — a real cloud-hypervisor boot does not gate PRs. Adding a
  `pull_request` trigger (cost/secrets permitting) is a follow-up, as are image
  pinning (the `latest`-URL catalog entries skip checksum verification) and the
  boot-path shutdown context / timeout cleanup.
