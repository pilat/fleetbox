# ADR: vmnet SharedMode Networking (macOS 26+), Replacing VZ NAT

**Date:** 2026-06-04
**Status:** Accepted

## Context

ADR-0004 chose `VZNATNetworkDeviceAttachment` for VM networking. Implementation
then discovered that VZ NAT **isolates VMs from each other**: every VM lands on
`bridge100`, but the framework marks each member port with a `PRIVATE` flag
(`20803<...,PRIVATE,...>`), so VMs reach the host and the internet but not their
neighbours. That killed multi-node cluster testing for v0, which ADR-0004 and the
v0 spec explicitly deferred.

macOS 26 shipped `VZVmnetNetworkDeviceAttachment`, backed by vmnet "logical
networks" (`vmnet_network_create`, `SharedMode`). A throwaway spike
(`spike/main.go`), ad-hoc codesigned with **only** `com.apple.security.virtualization`,
proved on macOS 26.4 that SharedMode delivers everything NAT did **plus** VM↔VM —
on a single NIC, with no root and no `com.apple.vm.networking` entitlement:

- Two VMs on one shared `vmnet.Network` booted, got IPs, and the host reached both.
- VM→VM ping worked (3/3 packets, ~0.34 ms).
- On the host, the bridge members carried `20003<LEARNING,DISCOVER,VIRTIO>` — **no
  `PRIVATE` flag**. That missing flag is the structural reason VM↔VM works.
- IP discovery via the existing `/var/db/dhcpd_leases` hostname lookup (ADR-0007)
  worked unchanged — SharedMode rides the same `bootpd`/`bridge` machinery as NAT.
- Nested virtualization still worked inside the guests (`/dev/kvm` present).

The one negative result: `AddDhcpReservation` did not pin IPs on reused disks
(bootpd ignored the reservation), so DHCP reservations are not a viable IP
assignment mechanism — hostname discovery remains the path.

## Decision

1. **vmnet SharedMode is the sole network path; VZ NAT is removed.** Both a single
   `Start` and an N-node `StartN` run on a `vmnet.Network` created in
   `SharedMode`. SharedMode is a strict superset of NAT (host reachability,
   internet via NAT44, plus VM↔VM), so nothing is lost on macOS 26+.
2. **The platform floor moves to macOS 26.0.** `vmnet.NewNetworkConfiguration`
   errors on older releases; that error is the single source of the "fleetbox
   networking requires macOS 26+" message, wrapped once in `internal/backend/vz`
   and propagated. Single-VM use on macOS 13–15 (previously supported) is traded
   away. A NAT fallback for <26 would be a separate code path, out of scope here.
3. **No new entitlement.** Codesigning keeps only
   `com.apple.security.virtualization` (proven sufficient). `com.apple.vm.networking`
   is not added anywhere.
4. **One network per Start/StartN group, held in process.** `StartN` creates one
   `vmnet.Network` and attaches all N VMs to it, so the cluster is interconnected.
   The network is an **in-process object tied to VM lifetime** — never persisted
   to `~/.fleetbox/`, so "clusters are a naming convention, never an entity with
   state" still holds. It is released by GC once every VM referencing it is
   unreferenced; teardown never releases it per-VM (a sibling would lose its
   network).
5. **Distinct subnets are chosen by a host-aware detector.** Each network gets a
   free `/24` in `192.168.0.0/16`, picked by scanning host interfaces and an
   in-process reservation set so concurrent/sequential networks never collide.
6. **The unreleased dependency is vendored, not forked.** norio-nomura's
   `Code-Hex/vz` PR #205 (commit `e27a5fb…`) is copied into `third_party/vz/`
   (plain files, MIT `LICENSE` kept, no `.git`), wired via a relative
   `replace github.com/Code-Hex/vz/v3 => ./third_party/vz`. **Exit criterion:**
   when PR #205 or its successor merges and is released upstream, delete
   `third_party/vz/`, drop the `replace`, and bump to the release. This vendor is
   a temporary bridge.

## Alternatives Considered

**Stay on VZ NAT.** Cannot do VM↔VM — the exact thing being fixed. Rejected.

**Bridged networking (`com.apple.vm.networking`).** Makes VMs reachable from the
LAN and each other, but the entitlement needs a paid Developer ID and
notarization — unacceptable friction for an ad-hoc-codesigned dev tool. Rejected
again (same as ADR-0004).

**socket_vmnet / `FileHandleNetworkDeviceAttachment`.** Adds an external daemon
dependency. Rejected now that SharedMode gives VM↔VM natively with no daemon.

**DHCP reservations to pin IPs.** Proven not to work (bootpd ignores them on reused
disks). Hostname discovery (ADR-0007) is retained.

**Fork / remote-module replace / git submodule for the dependency.** A
remote-fork replace hits Go module path-rename pain; a submodule complicates
checkout and CI. Vendoring with a relative replace travels with the repo and
builds for any dev and in CI.

## Consequences

- **VM↔VM works; clusters are testable.** `StartN` yields an interconnected
  cluster — the multi-node limitation of ADR-0004 is resolved.
- **macOS 26+ is required for networking** (i.e. for any VM). CI moves to a
  `macos-26` runner.
- **Partially supersedes ADR-0004.** Its NAT-attachment choice and its "VM↔VM is
  impossible" consequence are overturned; its **no-port-forwarding** decision
  still stands. ADR-0004 stays Accepted with a pointer here.
- **CLI multi-node was deferred to Phase 2 — now resolved by ADR-0009.** This ADR
  shipped clusters in the library first (`StartN`) and left the CLI rejecting
  multi-VM `up`, expecting the cross-process fix to need
  `Network.CopySerialization()` + an XPC/Mach transport. ADR-0009 instead runs a
  whole CLI cluster in **one holder process** sharing one in-process network — no
  XPC needed — so `up <prefix> -n N` now boots an interconnected cluster.
- **A temporary vendored dependency** lives in `third_party/vz/` with the exit
  criterion above; transitive deps bumped (`golang.org/x/crypto`, `golang.org/x/sys`).
- Hostname IP discovery (ADR-0007) and the backend-neutral API (ADR-0002) are both
  upheld unchanged — `vmnet`/`vz` types never appear in exported signatures or in
  the neutral `backend` package; only `internal/backend/vz` imports the vz module.
