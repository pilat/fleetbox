# ADR: Overridable Storage Root (`SetStorageRoot` / `FLEETBOX_HOME`)

**Date:** 2026-06-20
**Status:** Accepted

## Context

fleetbox keeps all of its on-disk state under `~/.fleetbox/` — per-cluster member
dirs, the SSH keypair, the holder run sockets, and two expensive caches: the multi-GB
converted image cache (`images/`) and the checksum-pinned downloaded binaries (`bin/`:
the signed macOS helper, cloud-hypervisor + firmware on Linux). The base dir is
resolved once in `store.New` (ADR-0023).

That hard-coded brand leaks. A project `aaa` that embeds fleetbox purely as a test
fixture, or to spin VMs for its own purposes, makes `~/.fleetbox` appear in its end
user's home directory — a user who is not a fleetbox user and has no idea what it is.
The author of `aaa` has no way to say "store this under *my* brand." The state is
fleetbox's, but the surprise is the user's.

## Decision

**One overridable storage root, set from code, carried by an env var, moving the
entire tree.**

- **Public surface — `fleetbox.SetStorageRoot(path string) error`.** A package-level
  function in the root package. It expands a leading `~` or `~/` via
  `os.UserHomeDir`, makes the result absolute (`filepath.Abs`, so a relative input
  resolves against cwd), and sets `FLEETBOX_HOME` to it. Empty path or a failed `~`
  expansion returns an error; `~user` is not supported (treated as a literal segment).
  It must be called **before** `Start`/`StartN`/`StartCluster` — the root is read once
  at store creation — so the intended home is an `init()` or `TestMain`.

- **Transport — the `FLEETBOX_HOME` env var.** This is not sugar over the Go call; it
  is the load-bearing transport across the process boundary. The helper runs in a
  separate process (macOS detached/bound subprocess, Linux self-reexec child) and
  inherits the parent env through the existing `cmd.Env = os.Environ()`. The Linux
  reexec'd holder short-circuits in `init()` *before* any consumer code runs, so it
  cannot call `SetStorageRoot` and **must** read the root from env — which it does, via
  `store.New`. That is why the env is correct, not optional.

- **Precedence in `store.New`.** When `FLEETBOX_HOME` is set (non-empty), `New` uses
  it verbatim as the base dir and skips the ADR-0023 `SUDO_USER` resolution entirely.
  The value is an explicit absolute path, so the non-root client and a sudo-elevated
  child read the same env and agree — strictly more robust than re-deriving the
  invoking user's home. `New` does not normalize the env value (no `Abs`, no `~`, no
  relative-path rejection); `SetStorageRoot` already normalized it, and a hand-set env
  is the human's responsibility, documented as "must be an absolute path." Keeping
  `New` dumb removes divergence points and leaves one behavior. With the env unset and
  `SetStorageRoot` never called, every path resolves exactly as before.

- **Everything moves — no cache/state split.** When the root is overridden, *all* of
  `clusters/ images/ bin/ run/ networks/` relocate under it, including the expensive
  caches. The whole point is that embedding `aaa` must not produce a surprising
  `~/.fleetbox`; leaving `images/`/`bin/` behind in `~/.fleetbox` would defeat it.

- **Sudo forwarding (Linux).** `cmd/fleetbox`'s `elevatedArgv` forwards
  `FLEETBOX_HOME` into the `env` prefix of the `sudo` re-exec **only when it is set**,
  alongside the existing `FLEETBOX_ELEVATED`/`PATH` tokens. Without this, a branded
  non-root client and its sudo-elevated `up` would split-brain — the client at
  `~/.aaa`, the elevated child falling back to `~/.fleetbox`. An explicit shared root
  forwarded verbatim is better than SUDO_USER re-derivation precisely because it is
  unambiguous; this is why ADR-0023's "do not forward `HOME`" does not extend to
  `FLEETBOX_HOME`.

## Alternatives Considered

**Split the shared expensive cache from per-project state** (keep `images/`/`bin/` in
`~/.fleetbox`, relocate only `clusters/`). Rejected: it leaves the exact surprising
`~/.fleetbox` the feature exists to remove. Trade-off accepted instead: each branded
root re-downloads images and the helper/CH binaries. A future *additive*
`FLEETBOX_IMAGE_CACHE`-style override could re-share the cache without breaking the
single-root knob — out of scope here, built nothing.

**An `Option` passed to `Start`/`StartN`.** Rejected: the store is created in several
places with no options (holder, orchestrator), and the root is process-global, not
per-VM. A per-call Option does not fit and would still need an env hop to the helper.

**Env var only, no Go API.** Rejected: the author of `aaa` should own the brand from
code; a typed function is discoverable and normalizes the path. The env stays as the
manual escape hatch and the CLI debugging story.

**`SetAppName(name)` → `<home>/.<name>`.** Rejected as too presumptuous — it forces a
dotdir in home, with no `/tmp`, XDG, or arbitrary-path option. The path primitive is
the flexible choice.

## Consequences

- A library author writes `fleetbox.SetStorageRoot("~/.aaa")` in `init()`/`TestMain`
  and the entire tree lands under `~/.aaa`; the end user never sees `~/.fleetbox`.
  Default behavior is unchanged when the knob is unused.
- **Each branded root re-downloads** the image cache and the helper/cloud-hypervisor
  binaries — the accepted cost of "everything moves." Switching roots within one
  project pays it again.
- **The stock CLI is blind to a relocated root** unless the env is set:
  `FLEETBOX_HOME=/abs/path fleetbox ls` lets a human inspect/ssh/rm a branded
  project's VMs for debugging (no `--home`/`--root` flag — env inheritance covers it).
  Note the shell does not expand `~` inside `FLEETBOX_HOME=~/.aaa` the way
  `SetStorageRoot` does; the manual/CLI case wants an absolute path.
- **Switching roots orphans old state by design.** VMs created under a prior root
  become invisible after a switch; there is no migration. This is documented, not a
  bug — `rm` the old ones under the old root, or point the env back.
- `store.New` now has one conditional in its base-dir rule (env wins, else
  `resolveBaseHome`), still the single source of truth every process agrees on.
- References ADR-0023 (the SUDO_USER base-dir rule this overrides when the env is set,
  and whose sudo-forwarding seam it extends), ADR-0020 (the helper processes that
  inherit the env), ADR-0014 (the store layout that relocates wholesale), ADR-0017/0019
  (the caches — helper, image catalog — that move with it). Amends none; it adds an
  override the base-dir rule left unspecified.
