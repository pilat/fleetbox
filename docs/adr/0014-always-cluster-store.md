# ADR: Cluster-Rooted Store ("Always a Cluster")

**Date:** 2026-06-07
**Status:** Accepted

## Context

The runtime already groups VMs per holder: one CLI holder process owns one
`fleetbox.Cluster` — a single shared network and every VM on it (ADR-0009). But the disk
did not reflect that grouping. Every VM was a flat directory `~/.fleetbox/vms/<name>/`,
and the holder's per-member control socket and pidfile sat loose in the base directory as
`~/.fleetbox/sock-<name>` / `pid-<name>`. The base dir collected one stray `sock-*` and
`pid-*` per member, `rm` had to sweep those files separately from the VM directory, and
there was nowhere on disk for a cluster-level artifact to live.

ARCHITECTURE.md stated the invariant "clusters are a naming convention (`prefix-N`),
never an entity with state." That remains true for *membership and health* — there is no
cluster object you create and add VMs to — but it left the on-disk layout flatter than
the model the runtime actually lives by.

## Decision

Make the disk reflect the runtime model: **everything is a cluster, and a single VM is
just a cluster of one.** The layout becomes:

```
~/.fleetbox/clusters/<cluster>/<member>/{config.json,disk.raw,seed.iso,efi.nvram,serial.log,sock,pid,.lock}
```

- **The cluster name is derived from the member name, not stored.** Strip a single
  trailing `-<digits>` group: `web-3 → web`, `dev → dev`, `web-1-2 → web-1`,
  `node-2024 → node`. The directory path must be computable from the member name *alone*,
  because the member directory has to exist before `config.json` is written into it during
  boot — a persisted cluster field could not be read to *locate* the dir
  (chicken-and-egg). This means no `store.VM` schema change and no public API change.
- **Sockets and pidfiles are per-member, inside the member dir** (`<member>/sock`,
  `<member>/pid`). There is no holder-level socket; each member of a holder writes the
  same PID into its own pidfile. Per-member addressing for `ls`/`ssh`/`down`/`rm` is
  preserved exactly. The filenames are kept short (`sock`, not `control.socket`) to stay
  under the macOS 104-byte `sun_path` limit.
- **A solo VM is a cluster of one** with a bare member name (`clusters/dev/dev/`), so
  `ssh dev`, `rm dev`, etc. keep working with the name the user typed.
- **A heterogeneous `up a b c` / `StartCluster(["a","b","c"])` becomes one single-member
  cluster dir per name** (`clusters/a/a/`, `clusters/b/b/`, …), while at runtime the holder
  still wires them onto one shared network. On-disk grouping and runtime grouping
  deliberately diverge here, because those names share no prefix and there is no
  non-arbitrary cluster name to invent.
- **Breaking change, no migration.** The project is pre-release; a developer with old
  state deletes `~/.fleetbox` (or its `vms/`, `sock-*`, `pid-*`) by hand. No compat shims.

This **supersedes the "clusters are a naming convention, never an entity with state"
invariant in the *storage* sense**: the cluster is now a first-class storage grouping. It
remains a runtime-only object for membership and health — disk grouping and runtime
grouping are not even 1:1 (the heterogeneous case above), so this is an evolution of
ADR-0009, not a contradiction of it.

## Alternatives Considered

**Persist a `cluster` field in `config.json`.** Rejected: the member directory must exist
before its config is written, so a stored cluster name could not be used to locate the
directory in the first place. It would also be redundant with the derivable name.

**A shared cluster dir for heterogeneous `up a b c`.** Rejected: there is no
non-arbitrary shared name to invent, inventing one would need either a persisted cluster
field (above) or a `StartCluster` signature change (the public API stays fixed — ADR-0001
library-first). Per-member dirs are the only resolution consistent with the other
decisions.

**Keep the flat `vms/<name>/` layout and just move the loose `sock-*`/`pid-*` files into
each VM dir.** This solves the litter but not the larger goal — giving the cluster a
durable on-disk home for the upcoming read-only payload-ISO fixtures work, which needs a
cluster-root location. The cluster-rooted layout is the foundation for that.

**Write a migration that detects and moves an existing flat tree.** Rejected: pre-release,
no users to migrate; a compat shim is pure cost.

## Consequences

- `rm` is a single `RemoveAll` of the member dir (the socket/pid ride inside it), and the
  base directory stops collecting loose `sock-*`/`pid-*` files. `Delete` then `os.Remove`s
  the now-maybe-empty parent cluster dir — `os.Remove` (never `RemoveAll`) refuses a
  non-empty dir, which is exactly the "siblings still present, keep it" case.
- The change is contained to `internal/store` (path-method bodies, no signature changes)
  plus one ordering fix in `internal/runner` (`register` calls `EnsureDir` before opening
  the socket, since the dir now must exist before boot). `fleetbox.go` and both backends
  are untouched — the cloud-hypervisor API socket (`ch.sock`) and VZ's disk path follow
  `DiskPath` into the member dir automatically.
- **Known lossy-derivation collision (benign for this task, flagged for the next):**
  because the derivation drops information, two independent solo VMs `node` and `node-2024`
  both resolve to cluster `node` and become siblings under `clusters/node/`. For
  storage-only this is harmless — paths round-trip, and `Delete`'s `os.Remove` refuses the
  shared cluster dir while either lives. It stops being harmless once a shared
  cluster-root artifact (the payload ISO) lands under `clusters/<cluster>/`; resolving that
  is the fixtures task's problem (keep the payload per-member, or make cluster identity
  explicit there).
- A solo VM doubles its name in the path (`clusters/dev/dev/sock`), so a pathologically
  long CLI name under a long `$HOME` could overflow `sun_path`. The fallback, if it ever
  bites, is one segment shallower (`clusters/<cluster>/sock-<member>`); the member-dir form
  is the default for cleaner `rm`.
- ARCHITECTURE.md's §3 model table, §4.2 layout tree and invariant prose, and the §4.4 /
  §5 holder text are updated in the same change.
