# ADR: CLI Clusters Run in One Holder Process (Shared In-Process Network)

**Date:** 2026-06-04
**Status:** Accepted (superseded on macOS by ADR-0017; amended by [ADR-0020](0020-helper-thin-backend-server.md) — one holder per up-group is now the model on both platforms, with the orchestrator client-side)

## Context

ADR-0008 made VM↔VM connectivity work by attaching every VM in a cluster to one
`vmnet.Network` object, and shipped it in the library (`StartN`) where it is
trivial: one process, one in-process network, N VMs. It deferred the CLI side to
"Phase 2" because ADR-0006 gives the CLI **one runner process per VM**, and two
runner processes cannot share an in-process `vmnet.Network`. ADR-0008 sketched
the Phase-2 fix as `Network.CopySerialization()` plus an XPC/Mach transport
between runners.

That XPC path is heavy: a network-owner process with its own lifecycle, a Mach
service registered via a launchd plist, and a serialized network handed between
processes. It also turns a cluster into a stateful, process-spanning entity —
in direct tension with the core principle "clusters are a naming convention,
never an entity with state." The only thing the CLI actually needs is for a
cluster's VMs to share one network. Sharing an *in-process* object is already
solved (that is exactly what `StartN` does); the friction was self-imposed by
"one runner per VM."

## Decision

1. **One holder process owns a whole CLI cluster.** `fleetbox up <prefix> -n N`
   (or `up a b c`) re-execs a single runner that creates one
   `fleetbox.Cluster` — one shared `vmnet.Network` — and boots every member onto
   it, exactly like the library `StartN`. No XPC, no Mach service, no launchd, no
   serialized network.
2. **The holder serves one control socket and one pidfile per member name.** A
   process can listen on N unix sockets trivially, so every per-name CLI command
   (`ls`, `ssh`, `down`, `rm`, `status`) keeps working unchanged — each addresses
   a member by talking to `sock-<name>`, unaware that several members share one
   process.
3. **Members are independently stoppable; the process outlives individual
   members.** `down <member>` / `rm <member>` stops just that VM and retires its
   socket+pidfile; the holder exits only when its last member is gone. SIGTERM
   stops them all.
4. **A stopped node re-joins the live cluster's network.** `up` partitions the
   requested members into running and missing. None running → spawn a fresh
   holder for the set. Some running in one holder → send it `addmember <name>`
   over a live sibling's socket, and it boots the missing member onto its
   existing network (so the re-upped node reaches its peers, not an isolated
   network). Running members split across separate processes cannot be merged —
   `up` reports that instead of silently creating a disconnected node.
5. **A new library primitive, `Cluster`, carries this.** `NewCluster(opts)`
   creates the shared network with no VMs; `(*Cluster).Add(ctx, name)` boots a
   VM onto it; `StartCluster(ctx, names, opts)` boots a named set;
   `StartN` becomes a thin wrapper over `StartCluster`. `Cluster` is an
   in-process runtime handle, never persisted — so principle 6 ("naming
   convention, no state") still holds.

## Alternatives Considered

**XPC serialization between per-VM runners (ADR-0008's sketched Phase 2).**
Rejected for the CLI: it needs a network-owner process with a lifecycle, a
launchd-registered Mach service, and a serialized network crossing processes —
substantially more code and moving parts, and it makes a cluster a stateful
cross-process entity. Worth revisiting only if CLI clusters ever need members in
genuinely separate processes (e.g. per-VM crash isolation); the in-process
holder trades that isolation for simplicity.

**Keep one runner per VM and accept disconnected CLI "clusters."** Rejected:
isolated networks defeat the entire point of ADR-0008 (VM↔VM).

**Make the library expose the raw `vmnet.Network` so runners share it.** Rejected:
violates the backend-neutral API (ADR-0002); `vmnet`/`vz` types must never leave
`internal/backend/vz`. The `Cluster` handle keeps the network opaque.

## Consequences

- **CLI clusters work**, with VM↔VM connectivity, addressed by the existing
  per-name commands. The Phase-2 limitation in ADR-0008 and ARCHITECTURE §7 is
  resolved.
- **Amends ADR-0006.** "One runner per VM" becomes "one holder per `up` group
  (1..N VMs)." Everything else in ADR-0006 stands: the holder is still the same
  re-exec'd binary, still does nothing but hold VMs and answer a tiny socket
  protocol (now `status` / `stop` / `addmember`), still no forwarding or guest
  protocol. A crash now loses a whole cluster rather than one VM — the accepted
  cost of in-process sharing.
- **The socket protocol gains one verb, `addmember <name>`**, for live re-join.
  It stays host-only and VM-unrelated (principle 4 intact).
- **Crash blast radius grows**: all members of a cluster share one process, so a
  holder crash takes the cluster down. Single-VM `up` is unaffected (a cluster of
  one). Acceptable for a test-fixture tool; revisit if isolation is ever needed.
- **`Cluster` is new public API.** It is a runtime handle only — no on-disk
  cluster state is introduced, so "clusters are a naming convention" still holds.
