# ADR: Linux Requires Root — Smooth the Sudo Path Instead of Going Sudo-less

**Date:** 2026-06-11
**Status:** Accepted

## Context

On Linux the backend creates a shared bridge and per-VM taps, enables IPv4
forwarding, and installs a masquerade rule. It does this by shelling out to `ip`
and `iptables` (ADR-0011) and by writing `/proc/sys/net/ipv4/ip_forward`. None of
that works for an unprivileged process, and the way fleetbox *told* users to make it
work was actively wrong:

- The runtime error suggested granting the binary a `cap_net_admin+ep` file capability.
  That made the preflight pass — the preflight read the effective-capability set from
  `/proc/self/status`, and the file-capability grant sets exactly that bit — and then the
  real work failed anyway, because file capabilities are **not inherited across `exec`**
  into `ip`/`iptables`, and `/proc/sys/.../ip_forward` is gated by file permission (DAC),
  needing `CAP_DAC_OVERRIDE`, not `CAP_NET_ADMIN`. The check lied: it gated on something
  that did not predict success.

The only thing that actually works is running as root (sudo). But the sudo path was
itself booby-trapped:

- `sudo fleetbox` → `command not found`, because sudo's `secure_path` drops the
  Go/mise bin dir where the binary lives.
- `sudo fleetbox up` then a non-sudo `fleetbox ssh` → "VM does not exist", because the
  store resolves its base dir from `$HOME`: under sudo that is `/root/.fleetbox`, so
  state created by `up` landed somewhere the non-root `ssh` never looked.
- Even when state was found, the SSH private key (`0600`, root-owned) and the holder's
  per-member control socket (root umask `0755`, and a unix-socket `connect()` needs
  *write* permission) were unreadable/unconnectable by the invoking user, so read-only
  commands still needed sudo.

## Decision

**Linux requires root. We do not make it sudo-less; we make the sudo path smooth.**

- **Honest preflight.** The Linux preflight gates on `euid == 0`, not the effective
  capability set. With the shell-out architecture only root actually works, so root is the
  honest gate. The file-capability suggestion is gone. (`internal/orchestrator/preflight.go`
  holds the pure `requireRoot`; `preflight_linux.go` calls it after the `/dev/kvm` check.)
- **The CLI auto-elevates; the library never does.** `cmd/fleetbox`'s privileged
  commands (`up`, `down`, `rm`) call `ensurePrivileged()` first. Interactive (a
  controlling terminal exists) → re-exec via `sudo`, which prompts for the password.
  Non-interactive (CI, pipes, `setsid`, `go test`) → print the exact ready-to-paste
  command and exit non-zero. It must **never** hang waiting on a password. The library
  (`fleetbox.*`, `fleetboxtest.*`, `internal/orchestrator`) spawns no `sudo` — tests run
  under sudo already, and the library's contract stays "fail fast with a clear error".
- **One state location.** `store.New` resolves the base dir to the *invoking* user's
  home when root-via-sudo (`euid==0 && SUDO_USER != ""`), via a passwd lookup
  (`os/user.Lookup(SUDO_USER).HomeDir`), not `$HOME`. This is the single source of
  truth every process agrees on, so an auto-elevated `up` and a non-root `ls`/`ssh`
  read the same `~alice/.fleetbox`.
- **Two narrow ownership fixups, not a whole-tree chown.** The elevated client chowns
  the SSH key pair to `SUDO_UID:SUDO_GID` on every run (idempotent, so a stale
  root-owned key is repaired), keeping mode `0600` — `ssh` refuses a world-readable
  key, so ownership is the only fix. The root holder `chmod 0666`s each unix socket it
  creates so a non-root client can connect. Disks/logs stay root-owned; only the holder
  touches them.

Privilege classes: **privileged (auto-elevate)** = `up`, `down`, `rm`; **user-level
(never elevate)** = `ls`, `ssh`, `cp`, `ssh-config`, `version`, `completion`.

## Alternatives Considered

**Make the file-capability grant actually work (native netlink + native nftables +
ambient capabilities).** Rejected — it is a network-layer rewrite for marginal value:

- The library/`go test` path is the primary use case (testcontainers-for-VMs), and its
  binary is ephemeral: recompiled into a temp dir every `go test`, often on a `nosuid`
  mount, deleted after. "grant the capability once" is impossible there — you would have to
  re-grant it every run, i.e. sudo every run. No win for the use case that matters.
- Enabling IPv4 forwarding writes the root-owned, DAC-gated
  `/proc/sys/net/ipv4/ip_forward`, which needs `CAP_DAC_OVERRIDE` (≈ root-for-files)
  regardless of `CAP_NET_ADMIN`. Full sudo-less is partial at best.
- cloud-hypervisor is a cap-less child; attaching it to the tap would need owner-uid
  taps or fd-passing — extra machinery the "run everything as root" model hides for free.

**Forward `HOME` through sudo to fix the state path.** Rejected. A *manual* `sudo
fleetbox` sets `HOME=/root` while `SUDO_USER=alice`; trusting `$HOME` lands state in
`/root`. The store's `SUDO_USER` passwd home is authoritative in the root case, so the
elevation does not (and must not) depend on forwarding `HOME`.

**Whole-tree `chown` of `~/.fleetbox`.** Rejected as broader than needed: only the key
and the socket block read-only commands; disks/logs are root-only by design.

## Consequences

- The Linux story is honest end-to-end: a normal user runs `fleetbox up`, approves one
  sudo prompt, and then `ls`/`ssh`/`cp` work with no sudo and no split state; `rm` shows
  the sudo prompt first and the `[y/N]` confirmation after (authenticate, then confirm).
- `0666` on the local-only holder unix sockets is an accepted tradeoff on a dev tool.
- The passwd lookup uses `CGO_ENABLED=0` `os/user`, which parses `/etc/passwd`. Local
  users work; network-directory (LDAP/SSSD) users are an accepted edge case (fall back to
  `$HOME`, never error). A user who deliberately overrides `$HOME` to a non-passwd path
  for a non-root command is an accepted divergence; we add no machinery for it.
- Auto-elevation lives only in `cmd/fleetbox` (`elevate_linux.go` + a `!linux` no-op
  stub `elevate_other.go`; the pure decision in `elevate.go`). macOS uses the
  signed-helper model (ADR-0017/0020) and never sudo, so its stub returns nil.
- Behavioral coverage is mostly manual on a Linux host: the macOS CI runs the
  `fleetbox_fake` path (preflight is a no-op), and the Linux VM CI runs as root via
  sudo, so `euid==0` short-circuits the elevation and ownership paths. The automated
  coverage is the pure functions (`requireRoot`, `resolveBaseHome`, `decideElevation`),
  unit-tested off-root on darwin/arm64.
- References ADR-0011 (Linux backend's privileged shell-outs), ADR-0014 (store layout),
  ADR-0020 (self-reexec holder that inherits `SUDO_*`), ADR-0022 (the cobra CLI this
  wires into). Amends none of them; it adds the privilege model they assumed but left
  unspecified.
