# ADR: Helper as a Thin Backend-Server (Client-Side Orchestrator)

**Date:** 2026-06-10
**Status:** Accepted (inverts the orchestrator-in-helper half of ADR-0017; amends ADR-0006, ADR-0008, ADR-0009, ADR-0011, ADR-0013)

## Context

ADR-0017 severed the macOS client from Virtualization.framework by moving **the
whole orchestrator** into the signed `fleetbox-helper`: the helper resolved
images, built disks/seeds/fixtures, managed the store, AND ran the VMs. That made
the importable client pure Go, but it put the wrong things behind the signature.
The signed helper's only justification is the cgo + codesign that
Virtualization.framework demands — its job is to *run VMs*. Knowing about
operating systems, image catalogs, qcow2→raw conversion, and cloud-init is not a
VM-running concern, yet all of it lived inside the signed binary. The freshly
landed image-pinning catalog (ADR-0019) made the smell concrete: a catalog of
Debian snapshots was compiled into a binary whose reason to exist is "I hold the
virtualization entitlement."

Linux already showed the right shape. There the VMM (cloud-hypervisor) is a thin,
downloaded, checksum-pinned binary driven over a socket; the orchestrator runs
in-process and is just a client of that VMM. But the two platforms had drifted
into a split-brain: macOS ran the orchestrator inside the helper, Linux ran it
in-process, and the cross-process holder protocol was exercised only on darwin (by
the fake-backend coordination tests, ADR-0018). Two lifetime models, two code
paths, one protocol tested on one platform.

Two grounded facts made an inversion clean. (1) The orchestrator's cut line is
already sharp: everything up to and including `fixture.BuildImage` is pure Go, and
only `CreateNetwork`/`Create`/`Start`/`WaitForIP`/`Stop` touch a backend. (2) The
holder protocol is already member-oriented and reconnectable (per-member sockets
addressed by name), which is exactly what the CLI's detached reconnect needs.

## Decision

**The helper becomes a thin backend-server on BOTH platforms; the orchestrator
runs client-side.** The helper holds only the *live cluster* — one shared network
and its member VMs — and nothing else. Everything that is not a live VM/network
(image resolve, disk/seed/fixture build, store, SSH keys, orchestration, ssh/cp)
runs in a pure-Go client that links no hypervisor on either platform.

1. **A real helper process on both platforms.** macOS keeps the separately
   distributed **signed** `cmd/fleetbox-helper` (downloaded via `internal/helperdist`,
   bumped to `helper-v0.2.0` for the protocol change). Linux **self-reexecs** the
   single client binary — there is nothing to sign and cloud-hypervisor is the
   downloaded VMM, so the binary links the CH backend and becomes the holder after
   reexec. A package `init()` interceptor (`internal/holder/reexec_linux.go`, the
   docker/reexec pattern) catches `--fleetbox-runner`/`--fleetbox-reconcile` before
   Go's test framework or the CLI `main()` runs, so self-reexec works even for a
   library user's `go test` binary.

2. **The orchestrator drives the backend by RPC.** The orchestrator links the
   `internal/backend` interface plus a pure-Go **remote-proxy backend**
   (`internal/backend/remote`) that turns each backend call into a control-protocol
   RPC. The real vz/cloud-hypervisor backend lives only behind the helper
   (`internal/holder` + its `newRealBackend` selector); `internal/backend/fake`
   moves there too, under the `fleetbox_fake` tag.

3. **The helper owns the network AND IP allocation.** `CreateNetwork()` stays
   arg-less; a per-member `Network.Reserve(name, ipHint)` allocates the address
   helper-side (Linux static IP honoring the client's stored-IP hint, or just a
   deterministic MAC on the DHCP/vz path) and returns `{ip, mac}`. The client bakes
   the returned IP/MAC into the seed. The old client-side `allocateIP` is gone; the
   IP is always helper-side — allocated on Linux, discovered post-boot on macOS — and
   surfaced through the holder's `status`.

4. **One member-oriented JSON protocol.** The fixed-256-byte text protocol becomes
   newline-delimited JSON (`createnetwork`/`reserve`/`boot-member`/`status`/`stop`),
   `ProtocolVersion` "1"→"2". The spawn payload changes from an image alias to a
   **resolved member spec** (ready disk/seed/fixture paths, cpu/mem, serial-log
   path). Adding a member to a live cluster is just `reserve` + `boot-member` on the
   running helper (no dedicated command). Serial output crosses as a **path**, not an
   `io.Writer` (the helper opens it).

5. **Lifetime, reaping, and the gates are unchanged in spirit.** Bound (library,
   reaped with the test even on `kill -9`) vs detached (CLI, persistent, reconnect
   by name) are preserved, now uniform on both platforms. `NestedVirtSupported`/
   `SupportsClustering` stay pure-Go client-side host heuristics that never spawn
   the helper. The macOS <26 single-VM path and `ErrClustersUnsupported` gate are
   unchanged.

## Alternatives Considered

- **Keep the orchestrator in the helper (ADR-0017 status quo).** This is the smell
  the whole change removes — images/disks/catalog inside the signed binary.

- **Invert only macOS, keep Linux in-process.** Rejected: it leaves the split-brain
  (two lifetime models, the `vmState`/`clusterState` seam, no real-VM CI coverage of
  the protocol). One protocol exercised by Linux CI with real VMs is the payoff.

- **A helper per VM (literal cloud-hypervisor parity).** Rejected: it would require
  proving vmnet SharedMode interconnects VMs across separate processes — unproven and
  out of scope. The helper holds the live cluster, not a single VM.

- **A generic backend-object RPC (ship `backend.VM`/`Network` handles over the
  wire).** Rejected: in-memory handles don't survive the CLI's reconnect to a
  detached helper. The member-named protocol is forced by reconnect-by-name.

- **Client-side IP allocation (scan the store after fetching the subnet).** Rejected:
  the network is the helper's, so allocation is too. The client passes the stored IP
  as a hint; the helper honors it.

## Consequences

- **The catalog leaves the signed helper.** `internal/image` (and seed/fixture and
  the orchestrator) are now in the *client's* dependency graph and NOT the macOS
  helper's. The sever gate inverts: `go list -deps` of the darwin client now
  *includes* `internal/{image,seed,fixture,orchestrator}` and still *excludes*
  `internal/backend/vz` + `third_party/vz`; the helper is the mirror image.

- **The split-brain is gone.** `fleetbox.VM`/`Cluster` are one client implementation
  over `orchestrator.VM`/`Cluster`; the `darwinVM`/`darwinCluster` seam is deleted.
  The one protocol is exercised by the fake-backend coordination tests on **both**
  platforms (cross-process, via the FLEETBOX_FAKE_RECORD record file since the fake
  now runs in the helper's address space) and by a **real VM** on Linux CI
  (`vm-linux.yml`) — strictly more coverage than before.

- **Linux gains a helper subprocess it does not strictly need.** Per ADR-0011's own
  philosophy ("a library that owns a subprocess and talks to it over a socket is
  still a library"), this is acceptable; the cost is one extra process and the
  self-reexec init() hook. The Linux binary links the CH backend + orchestrator +
  image — there is no backend-free sever on Linux, and that is fine (CH is pure-Go,
  nothing is signed). The macOS sever is the only one; `depguard`'s `vz-isolation`
  is macOS-scoped and there is deliberately no CH-isolation rule.

- **A new signed helper release.** The protocol bump forces `helper-v0.2.0`; the
  published `helper-v0.1.0` is rejected at the version handshake by design.

- **Network ownership (ADR-0013) relocates into the helper process**, keyed on the
  helper PID (already `os.Getpid()` in the record), reconciling on helper start. A
  `kill -9` of the helper is reconciled on the next `up`; `Prune` spawns a
  short-lived reconcile helper (Linux) / is a no-op (macOS).

- **Supersedes the orchestrator-in-helper half of ADR-0017** and amends the network
  and process-model ADRs: ADR-0006 (CLI runner), ADR-0008 (vmnet one-process),
  ADR-0009 (one holder per up-group — now both platforms), ADR-0011 (Linux backend —
  now helper-hosted), ADR-0013 (network lifecycle — helper-owned). ADR-0017's sever
  (pure-Go client) is **kept and generalized**, not reversed.
