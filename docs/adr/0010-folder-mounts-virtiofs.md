# ADR: Folder Mounts via VZ virtiofs

**Date:** 2026-06-04
**Status:** Accepted

## Context

fleetbox VMs could exchange files with the host only by copying — `fleetbox cp` /
`scp` over SSH. That is one-shot and one-directional: there was no way to give a VM a
live, shared working tree the way a testcontainers bind mount does. The v0 spec
(decision 17) and ADR-0005 anticipated this and pre-blessed Apple
Virtualization.framework's native virtiofs as the mechanism, deferring the work to v1.
This ADR is that v1.

The constraints carried in from earlier decisions: the public API must stay
backend-neutral (no `Code-Hex/vz` types in exported signatures — ADR-0002); nothing of
ours may run inside the guest (ADR-0005); and as of ADR-0008 the platform floor is
already macOS 26, well above virtiofs's macOS 12 minimum.

## Decision

Add a live, read-write shared-folder capability built on VZ-native virtiofs.

- **Mechanism.** One `VZVirtioFileSystemDeviceConfiguration` per mount, each backed by
  a read-write `VZSharedDirectory` + `VZSingleDirectoryShare` on the host, mounted in
  the guest by stock cloud-init (`mounts:` directive → `/etc/fstab` →
  `mount -t virtiofs`). No agent, no helper binary, no host↔guest protocol — the guest
  kernel's own virtiofs driver does the work.

- **API.** A public `WithMount(hostPath, guestPath string) Option` and a plain
  `Mount{HostPath, GuestPath}` struct (no vz types). Read-write only for v1 — the vz
  layer hardcodes `readOnly=false`; a read-only variant can be added later without
  breaking callers. In `StartN`/`StartCluster`, mounts apply uniformly to every member.

- **Lifecycle: frozen at birth.** Mounts are a property the VM is created with. They are
  validated, absolutized, and tagged once at first create, persisted in `config.json`,
  and re-applied identically on every later boot — the host re-attaches the virtiofs
  device from the stored config, the guest re-mounts from fstab (no cloud-init re-run).
  Changing a VM's mounts means `rm` + recreate. This matches how cpu/mem/disk options
  are already ignored for an existing VM, and is forced by reality: VZ attaches devices
  at config-build time, with no hot-attach to a running VM.

- **Tag scheme.** Each mount gets a short stable tag `fbm<i>` where `i` is its position
  in the mount list, assigned at first create and persisted. The tag is the single
  source of truth shared between the host device and the guest fstab entry — computed
  once, never re-derived in two places (drift would mean a broken fstab). VZ caps tags
  at 35 bytes, so the path cannot be the tag. Tags are per-VM; different VMs reusing
  `fbm0` is fine.

- **uid alignment.** VZ virtiofs is identity pass-through with no uid/gid mapping. Host
  files (owned by the macOS uid, ~501) would otherwise appear in the guest as bare uid
  501 rather than the guest login user (uid 1000), breaking `chown`-to-self and making
  git refuse the tree ("dubious ownership"). fleetbox controls both sides, so **a VM
  with ≥1 mount has its guest user created with `uid: <host uid>`** (`os.Getuid()`),
  making the numbers line up. This is conditional on mounts: a mountless VM's cloud-init
  user-data is byte-for-byte unchanged. gid is not aligned (macOS gid 20 `staff`
  collides semantically with Linux gid 20 `dialout`, and uid alignment alone fixes the
  real pain). Guest→host writes already land owned by the host user, because Apple's
  virtiofs server runs inside the host process identity.

- **Data flow.** The mount threads through four backend-neutral structs, each layer
  owning its own type to avoid import cycles and keep vz types out of the public API:
  `fleetbox.Mount` → `store.Mount` (persisted, +tag) → `backend.Mount` (host path +
  tag) / `seed.Mount` (tag + guest path). Tag assignment lives in exactly one place.

## Alternatives Considered

- **9p** — VZ does not expose it, and it is slower than virtiofs. Rejected.
- **A payload ext4 disk** (spec decision 18) — a different feature: copy-in /
  preinstall, not a live bidirectional share. Deferred separately, not a substitute.
- **scp/cp** — already exists; one-shot and not live. The whole point here is "live."
- **`MultipleDirectoryShare`** — forces all shares under one parent directory, wrong for
  arbitrary `host:guest` pairs at independent guest paths. One device per mount instead.
- **A one-shot `runcmd`/`bootcmd` mount** instead of `mounts:` — would not survive a
  reboot. fstab on the persistent disk is what guarantees re-mount; `mounts:` writes it.
- **Remapping on uid collision** — if the host uid is already taken in the guest image,
  cloud-init fails loudly rather than silently picking a different uid (which would
  re-break the alignment the feature depends on). Rare; accepted for v1.

## Consequences

- Test authors get `fleetboxtest.Start(t, image, fleetbox.WithMount(dir, "/work"))` and
  CLI users get `fleetbox up dev --mount ./src:/work` — a live, read-write,
  bidirectional shared folder, with host-owned files lining up under the guest user so
  git and `chown` behave.
- No new platform floor — virtiofs is macOS 12+, far below the macOS 26 floor ADR-0008
  already set. No version gating around the virtiofs calls.
- Mounts cannot be changed on a live or existing VM; reconfiguring means `rm` + recreate.
  Supplying different mounts to an existing VM is silently ignored (as cpu/mem/disk are).
- `nofail` keeps a *guest-side* mount failure from blocking boot, but it does not make a
  VM with a **deleted host directory** bootable: `VZSharedDirectory` stats the host path
  when the device is attached, so if a mount's host dir was removed, the next boot fails
  at host-side device attachment, not silently inside the guest. fleetbox does not
  re-validate the host path on reboot (validation is a create-time concern) — this is an
  inherent VZ constraint, not a fleetbox check.
- On uid collision in the guest image there is no fleetbox-side surfacing: cloud-init
  user creation fails and manifests only as the SSH-readiness wait timing out. Accepted
  for v1.
- The mountless path is unchanged: no directory-sharing device, no `mounts:` block, no
  `uid:` line — existing VMs and their seed output are byte-for-byte identical.
- Read-only mounts, per-VM (non-uniform) cluster mounts, and gid alignment are
  deliberately out of scope and can be added later without breaking this API.

Implements the mount deferral recorded in ADR-0005; relies on the macOS-26 floor from
ADR-0008.
