# ADR: Surviving a Guest Hang on the VZ Backend

**Date:** 2026-07-10
**Status:** Accepted (references [ADR-0008](0008-vmnet-sharedmode-networking.md) for the
vmnet network, [ADR-0012](0012-macos-version-matrix-vz-nat-fallback.md) for the `<26` NAT
path, and [ADR-0020](0020-helper-thin-backend-server.md) for the holder that owns it.)

## Context

A single guest hang on the macOS (VZ) backend cascaded into an unrecoverable host
networking state. Reproduced live on an M4 Pro / macOS 26.5.1:

1. `VM.Stop` only ever issued an ACPI `RequestStop`. A hung guest ignores ACPI, the loop
   expired, and `Stop` returned an error **without escalating** — so the holder stayed
   alive holding `disk.raw`, and the next `up` failed with `VZErrorDomain Code=2 "The
   storage device attachment is invalid."`.
2. With the disk wedged, the operator's only recourse was `kill -9` on the holder.
3. A `vmnet_network_ref` is reference-counted and holds a SharedMode subnet reservation
   whose lifetime equals the ref's; release runs only via a `runtime.AddCleanup`
   finalizer. `kill -9` runs no finalizers, so the reservation leaked at the framework
   level — and there is **no** enumerate/destroy/reclaim API, only sleep/wake or reboot.
   `detectFreeIPv4Subnet` then re-picked the same leaked `/24` (its bridge is gone from
   `ifconfig`, its in-process dedup died with the holder) → `VMNET_FAILURE`, forever.

The key finding: in this repro the holder never died on its own — it was `kill -9`ed by
the operator, and **only because Stop would not force-stop**. The vmnet leak is downstream
of that. Fix the escalation and the regular trigger for `kill -9` disappears. The live
repro also confirmed the failure is a subnet-reservation collision, not a global vmnet
daemon wedge: after leaking `192.168.1.0/24`, a fresh `up` that picked a different `/24`
booted fine while the leak was still outstanding.

## Decision

Two changes in `internal/backend/vz`, no vendored-fork edits:

- **Force-stop escalation (the root fix).** `VM.Stop` asks the guest to power off via
  ACPI, and if it has not stopped within `acpiStopGrace` (15s) it escalates to VZ's
  forceful `VirtualMachine.Stop()`, then confirms within `forceStopGrace` (10s). Total
  stays under the client's 30s stop deadline. A hung guest can no longer keep `disk.raw`
  open, so the operator never needs `kill -9` — removing the only regular path by which
  the holder died abnormally.
- **Random subnet start (cheap insurance).** `detectFreeIPv4Subnet` starts its circular
  scan at a random octet, so if the holder *does* die abnormally (panic in a boot
  goroutine, OOM, a cgo crash — rare), a fresh `up` dodges the leaked `/24` instead of
  re-picking it. Turns a would-be wedge into a soft leak that drains at reboot.

## Alternatives Considered

- **Explicit vmnet release (`Close` calls `CFRelease`).** Would need a hand-carried patch
  to the vendored vmnet fork. Rejected: a clean holder exit already has the OS reap the
  reservation (confirmed in the repro — graceful `down` never leaked), and force-stop
  removes the `kill -9` that was the only non-clean exit. It bought nothing over the OS
  reap for real cost in the vendor pipeline.
- **A supervised long-lived vmnet owner** (the lima `socket_vmnet` / Apple
  Containerization pattern): a daemon owns the networks and holders lease them, so a
  crashing holder never leaks. It is the textbook fix for an *unstable* network owner — but
  our owner is not unstable. The holder died only from an operator `kill -9` that
  force-stop now eliminates; the residual crash paths are rare. A whole new process layer
  (daemon lifecycle, launchd/supervision, a lease protocol over vmnet serialization,
  signing/entitlement, distribution) for a near-never frequency is overkill. Revisit only
  if real data shows the owner itself crashing often.

## Consequences

- A guest hang is recoverable by fleetbox alone: `down`/`rm` force-stop and release the
  disk; a subnet leaked by a rare abnormal exit is dodged on the next `up`. No `kill -9`,
  no host reboot.
- Subnet selection is now non-deterministic. Correctness is unchanged (overlap + dedup
  checks still hold); only the chosen `/24` varies.
- rotation is a bridge, not a cure: the `/24` pool is finite, so a host that accumulates
  many abnormal-exit leaks without a reboot can still exhaust it. Acceptable given
  force-stop makes such leaks rare; the supervised-owner option above is the escalation if
  that assumption ever breaks.
- This is VZ-only. The Linux/cloud-hypervisor backend already had the equivalent
  protections: `VM.Stop` escalates REST-shutdown → SIGTERM → SIGKILL and removes the tap
  (`cloudhypervisor/vm.go`); the VMM is tied to the holder via `Pdeathsig: SIGKILL`, so no
  orphan survives; and `Reconcile` (ADR-0013) reclaims stale bridges/taps on startup,
  possible because netlink objects are enumerable, unlike a vmnet network. VZ now reaches
  that parity.
- Not addressed here: guest **serial capture** on the VZ/EFI path (`serial.log` is empty
  because the guest logs to a console VZ does not expose; the CH path sets `console=` in
  its cmdline and works). Diagnosis-only; its fix changes the boot path. Tracked
  separately.
