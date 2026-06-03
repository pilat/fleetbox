# ADR: Library-First Design in a Standalone Repo

**Date:** 2026-06-03
**Status:** Accepted

## Context

fleetbox exists to give Go tests real Linux VMs as fixtures on macOS (Apple Silicon).
Existing VM tooling in this space leans on a host agent, SSH port forwarding, and
userspace networking that are fragile and opaque, plus yaml templates and patched cloud
images — moving parts this use case doesn't need.

The immediate consumer is a Go test suite, but the tool is potentially useful beyond it.
The question: a package embedded inside its first consumer, or a standalone project?
And: CLI tool with a library bolted on, or library with a CLI bolted on?

## Decision

1. **Standalone repo**: `github.com/pilat/fleetbox`. Not embedded inside a consumer.
   Not marketed as a product in v0 — v0.x tags, no semver promises, honest minimal
   README. Product framing comes after the API has been shaped by real usage.
2. **Library-first**: the Go package is the product. Every capability exists in the
   public Go API (`fleetbox.Start`, `VM.SSH`, ...); the CLI (`cmd/fleetbox`) is a thin
   wrapper over that same API. Two consumers from day one (go test + CLI) keep the API
   boundary honest.

## Alternatives Considered

**Embed it inside its first consumer.** Rejected: ties the tool's lifecycle to one
consumer, prevents reuse, and makes it too easy to leak that consumer's assumptions into
the VM harness.

**CLI-first.** Rejected: the primary use case is `go test` fixtures. A CLI-first design
ends up with the library shelling out to its own binary, or with a "library" that is
just an exec wrapper — both add process-management complexity to the hot path (test
code).

## Consequences

- The public Go API is the contract; CLI features that can't be expressed through it
  don't get built.
- The CLI needs a holder process for VMs (ADR-0006), because a library VM dies with its
  owning process — this is the cost of not making the CLI the primary.
- Consumers take a dependency on an external module they must version-bump, instead of
  an internal package they edit freely. Accepted: API friction discovered by the first
  real consumer feeds back into fleetbox before anything is published to others.
