# ADR: Backend-Neutral Public API with Compile-Time Backend Selection

**Date:** 2026-06-03
**Status:** Accepted

## Context

fleetbox v0 targets exactly one hypervisor: Apple Virtualization.framework (VZ) via
`Code-Hex/vz`, on darwin/arm64. But the project should be able to exist on its own,
independent of any single platform — a linux/KVM backend is plausible someday.

Some VM tools support multiple hypervisors (QEMU, VZ) selectable per VM at runtime,
which produces a matrix of code paths, configs, and bugs. We explicitly do not want
that.

## Decision

1. **No `Code-Hex/vz` types in any exported signature.** The public API speaks in
   fleetbox types only. The only package allowed to import vz is
   `internal/backend/vz`. This is enforced mechanically by a depguard rule in
   `.golangci.yml`.
2. **The backend is a function of platform, selected at compile time.** Build-tagged
   files (`backend_darwin_arm64.go`) bind the platform to its one backend. No runtime
   hypervisor selection, no config option, ever.
3. **The backend contract is minimal**: `Backend{Create, NestedVirtSupported}` and
   `VM{Start, Stop, State, Wait}` in `internal/backend`. Everything that can be built
   on top (IP discovery, SSH, persistence) lives outside the backend.
4. **v0 ships VZ + arm64 only.** No QEMU, no Intel Macs, no x86 emulation.

## Alternatives Considered

**Expose vz types directly (no abstraction).** Rejected: locks every consumer to one
hypervisor and makes the library's API hostage to a third-party SDK's churn.

**Runtime-selectable backends.** Rejected: "one backend per platform, chosen by the
compiler, not by config" is the whole point. Multi-hypervisor support multiplies test
surface and produces per-hypervisor behavior differences that leak to users.

**Skip the interface until a second backend exists (pure YAGNI).** Rejected, narrowly:
the interface costs ~80 lines and buys a mechanically enforceable isolation boundary
(depguard). Without it, vz types creep into business logic and the eventual extraction
becomes a rewrite.

## Consequences

- A future linux/KVM backend is an additive change: new `internal/backend/kvm` package
  + new `backend_linux_amd64.go` file. The public API doesn't change.
- The module only compiles on darwin/arm64 for now (every root-package file is
  build-tagged). CI must run on macOS runners.
- All vz state/error translation happens at one boundary (`internal/backend/vz`),
  which adds a small amount of mapping code.
