# ARCHITECTURE.md — fleetbox

This file is **descriptive**: it records what the system looks like today.
`docs/coding-style.md` is **prescriptive** (how new code must be written), and
`docs/adr/` records **why** the significant decisions were made. When this file and the
code disagree, one of them is wrong — fix whichever one drifted (see §8).

Status: **v0** — the public API is not yet stable.

---

## §1. Overview

fleetbox provides Linux VMs as Go test fixtures on macOS (Apple Silicon). It boots stock
Linux cloud images via Apple Virtualization.framework (VZ), configures them once with
cloud-init, and hands them to tests as SSH-reachable fixtures. Think "testcontainers-go,
but for real VMs" — real kernel, real systemd, real KVM via nested virtualization.

Module: `github.com/pilat/fleetbox`

Two consumers, one API:

- **Library mode** — `fleetbox.Start()` / `fleetboxtest.Start(t, ...)` inside a Go test
  process. The test process owns the VMs; when it exits, the VMs die.
- **CLI mode** — `fleetbox up/down/ls/ssh/cp/ssh-config/rm` for manual work. Because a VZ
  VM lives only as long as its owning process, the CLI re-execs itself as a background
  *runner* process per VM (§4.4).

Everything the CLI does goes through the public Go API. The library is the product; the
CLI is a wrapper (see ADR-0001).

## §2. Glossary

| Term | Meaning |
|------|---------|
| **VM** | One virtual machine: a directory under `~/.fleetbox/vms/<name>/` plus, when running, a VZ virtual machine object inside some process. |
| **Backend** | The hypervisor abstraction (`internal/backend.Backend`). Exactly one implementation per platform, selected at compile time. v0: VZ on darwin/arm64. |
| **Image** | A stock cloud distro image (raw or qcow2), downloaded once and cached in `~/.fleetbox/images/`. Never modified. |
| **Seed ISO** | A cloud-init NoCloud ISO generated per VM. The only thing fleetbox ever "puts inside" a guest, and it is read by the guest's own cloud-init. |
| **Runner** | A re-exec'd `fleetbox` process that holds one VM alive in CLI mode, exposing status/stop over a unix socket. Does not exist in library mode. |
| **Store** | The `~/.fleetbox/` directory layout and its `config.json` files. The only persistent state fleetbox has. |
| **Cluster** | A naming convention (`prefix-1`, `prefix-2`, ...), not an entity. No cluster state exists anywhere (see §4.2). |

## §3. Source-of-Truth Map

Where the canonical version of each thing lives. When two files disagree, the SoT wins.

| Concept | Source of truth | Notes |
|---------|----------------|-------|
| Public library API | `fleetbox.go` | Exported symbols + doc comments. §5.1 summarizes. |
| Test-fixture API | `fleetboxtest/fleetboxtest.go` | §5.2 summarizes. |
| CLI command surface | `cmd/fleetbox/main.go` (`usage()` + dispatch in `main()`) | §5.3 summarizes. |
| Backend contract | `internal/backend/backend.go` | `Backend`, `VM`, `Config`, `State`. |
| On-disk state layout | `internal/store/store.go` path methods | §4.2 summarizes. |
| Image catalog | `internal/image/image.go` `Catalog` map | Alias → URL + sha256. |
| Guest provisioning contract | `internal/seed/seed.go` (user-data / meta-data) | One user, one SSH key, hostname. Nothing else. |
| Code style rules | `docs/coding-style.md` + `.golangci.yml` | Prescriptive. Lint enforces the machine-checkable subset. |
| Architecture (current state) | `ARCHITECTURE.md` (this file) | Descriptive. |
| Design decisions & rationale | `docs/adr/` | One file per decision, sequentially numbered. |
| Build & signing recipe | `Makefile` + `entitlements.plist` | `com.apple.security.virtualization` entitlement. |
| CI behavior | `.github/workflows/ci.yml` | Lint + build + unit tests only; no VM boots on CI. |
| Working specs (local only) | `ai/tasks/` | Gitignored. Durable decisions must graduate to ADRs. |

## §4. System Model

### §4.1 VM lifecycle

`fleetbox.Start(ctx, name, opts...)` in `fleetbox.go` is the single entry point for
creating/booting a VM. Both the CLI runner and fleetboxtest go through it. The sequence:

1. **Options** — apply functional options over defaults (image=debian-12, cpus=2,
   mem=4GB, disk=20GB).
2. **Store** — `store.New()` ensures `~/.fleetbox/{vms,images}/` exist.
3. **SSH key** — `sshkey.EnsureKey()` generates the per-installation ed25519 keypair on
   first use (`~/.fleetbox/id_ed25519[.pub]`).
4. **Image** — `image.Ensure()` returns a cached raw image, downloading / verifying /
   converting qcow2→raw if needed.
5. **VM config** — if `store.Exists(name)`: load `config.json` and boot from it
   (**all options are ignored for an existing VM** — the stored config wins; the image
   option only affects the shared image cache). Otherwise: create the config (stable
   MAC derived from name via `backend.GenerateMAC`), copy the cached image to
   `disk.raw` (sparse, truncated to requested size), and generate `seed.iso` via
   `seed.Create`.
6. **Boot** — `newBackend().Create(backend.Config{...})` builds the platform VM
   (EFI bootloader, NAT NIC, virtio disk + seed ISO, serial console → `serial.log`),
   then `Start(ctx)` boots it and polls until the backend reports running.
7. **IP discovery** — poll `dhcp.LookupByHostname(name)` (parses
   `/var/db/dhcpd_leases`) until the VM's hostname appears and TCP :22 is reachable
   (timeout 2 min). See ADR-0007 for why hostname, not MAC.
8. **SSH readiness** — `sshkey.WaitForSSH()` until an authenticated SSH session
   succeeds (timeout 2 min).

`VM.Stop(ctx)` asks the backend for graceful (ACPI) shutdown; the disk persists and a
later `Start` boots it again. `VM.Destroy(ctx)` stops and then deletes the VM directory.
Destroy is the only destructive operation.

### §4.2 State & persistence

All persistent state lives under `~/.fleetbox/` and is owned by `internal/store`:

```
~/.fleetbox/
├── vms/<name>/
│   ├── config.json        # store.VM: name, MAC, cpus, memory, disk, image, created_at
│   ├── disk.raw           # the VM's root disk (sparse file)
│   ├── seed.iso           # cloud-init NoCloud seed (generated once at create)
│   ├── efi.nvram          # EFI variable store (created by the VZ backend)
│   ├── serial.log         # serial console capture (debugging)
│   └── .lock              # flock target for TryLock
├── images/                # downloaded + converted raw cloud images (cache)
├── id_ed25519, id_ed25519.pub   # per-installation SSH keypair
├── pid-<name>             # runner pidfile (CLI mode only)
├── sock-<name>            # runner unix socket (CLI mode only)
└── runner-<name>.log      # runner process output (CLI mode only)
```

The VM model is **cattle with persistence**, a deliberate midpoint between two
opposing styles:

- **Not pets** (named, snapshotted, backed-up, registry-managed): a test fixture that
  grows snapshot/backup management has lost the plot.
- **Not pure cattle** (destroyed the instant the owning process exits, the
  testcontainers model): real VMs boot in tens of seconds, not milliseconds. Throwing
  away a booted, software-provisioned VM because a test process exited makes manual and
  iterative workflows miserable.

So: `up`/`Start` is idempotent (missing → create+boot; stopped → boot; running →
return it), `down` exists for graceful shutdown that preserves the disk, and the disk
survives host reboots and runner death. `rm`/`Destroy` is the only thing that deletes
data — there is no GC, no TTL; explicit destruction is a feature, not an omission.

There is no database, no global state file, and **no cluster entity** — clusters are a
naming convention (`prefix-N`). Cluster features (membership, add/remove node) would
duplicate what the software under test already does; the harness's job is N named VMs,
nothing more. `ls ~/.fleetbox/vms/` is the entire "database."

### §4.3 Networking model

- Every VM gets a **directly routable IP** from macOS's VZ NAT (`bridge100`, DHCP from
  bootpd). There is no port forwarding of any kind, by design (ADR-0004).
- Host→VM and VM→internet work. **VM→VM does not work** — VZ NAT isolates VMs from each
  other. This is a known limitation discovered after the spec was written; multi-node
  testing is deferred to v1 (bridged networking or socket_vmnet are the candidate
  fixes). See ADR-0004 consequences.
- IP discovery: VMs are found by **hostname** in `/var/db/dhcpd_leases` (cloud-init
  sets the hostname; VZ writes DUID-based identifiers instead of plain MACs). See
  ADR-0007.
- SSH: library mode uses `golang.org/x/crypto/ssh` programmatically; CLI `ssh`/`cp`
  exec the system `ssh`/`scp` binaries for a proper interactive terminal.

### §4.4 Process model

**Library mode** — the test process calls `fleetbox.Start()`; the `*VM` value holds the
backend VM object. VMs die when the process exits. `fleetboxtest` registers
`t.Cleanup(Destroy)` so test VMs never outlive their test.

**CLI mode** — `fleetbox up` calls `runner.Spawn()`, which re-execs the `fleetbox`
binary with a hidden `--fleetbox-runner <name>` flag. The runner process:

1. writes `pid-<name>`, listens on `sock-<name>`;
2. boots the VM via the same public `fleetbox.Start()`;
3. answers `status` / `stop` commands on the socket (JSON-encoded `runner.Status`);
4. on stop/SIGTERM: graceful `VM.Stop()` with a 30s budget, then exits.

The runner's *entire* job is holding the VM and answering the socket. No forwarding, no
guest protocol, no tunnels (ADR-0006). CLI options are serialized to the runner via the
`FLEETBOX_OPTS` env var (Option funcs → `Options` values → JSON).

### §4.5 Platform & build constraints

- The whole module is build-tagged `darwin && arm64` (every non-test `.go` file in the
  root, cmd, fleetboxtest, and backend/vz packages). It does not compile elsewhere.
- Any binary that creates VZ VMs (the CLI, VM test binaries) must carry the
  `com.apple.security.virtualization` entitlement — ad-hoc codesign is enough for dev.
  `make build` compiles and signs the CLI; `make test-vm` compiles, signs, and runs the
  `fleetboxtest` binary. There is no generic sign target — signing a VM test binary for
  any other package is a manual `go test -c` + `codesign --entitlements entitlements.plist`.
- Nested virtualization (required by consumers that run KVM inside guests)
  needs M3+ and macOS 15+. `fleetbox.NestedVirtSupported()` reports availability;
  `fleetboxtest` skips tests when unsupported.
- CI (GitHub-hosted macOS runners) cannot boot VZ VMs. CI = lint + build + unit tests
  only. VM tests run locally via `make test-vm`.

## §5. Modules

Each module section follows this template:

```
### §5.<n> <package path>

- Purpose: <one line>
- Owns: <files, state, goroutines — or "stateless">
- Depends on: <internal packages + key external deps>
- Public API: <exported symbols>
- Invariants: <things that must always hold>
```

When a PR changes any of these fields for a package, update its section.

### §5.1 `fleetbox` (root package)

- Purpose: the public library API — everything a consumer can do with a VM.
- Owns: the VM lifecycle orchestration (§4.1); per-VM serial log file handle.
- Depends on: `internal/backend` (+ compile-time `internal/backend/vz` via
  `backend_darwin_arm64.go`), `internal/dhcp`, `internal/image`, `internal/seed`,
  `internal/sshkey`, `internal/store`.
- Public API:
  - `Start(ctx, name, opts...) (*VM, error)`, `StartN(ctx, prefix, n, opts...) ([]*VM, error)`
  - `NestedVirtSupported() bool`
  - `type VM`: `Name()`, `IP() net.IP`, `SSH(ctx, cmd) (string, error)`, `Stop(ctx)`,
    `Destroy(ctx)`, `State() string`
  - `type Options{Image, CPUs, MemGB, DiskGB}`, `type Option func(*Options)`,
    `WithImage`, `WithCPUs`, `WithMemoryGB`, `WithDiskGB`
  - image aliases: `Debian12`, `Ubuntu2404`
- Invariants:
  - No backend (vz) types in any exported signature — the API is backend-neutral
    (ADR-0002, enforced by depguard).
  - `Start` on an existing (stopped) VM boots it from its stored config; options are
    ignored for existing VMs. Note: `Start` does **not** detect an already-running VM
    — that guard currently lives in the CLI runner (`runner.Spawn` checks
    `IsRunning`); concurrent `Start` on the same name in library mode is unguarded
    (known gap, see §5.8).
  - Every exported symbol has a doc comment.

### §5.2 `fleetboxtest`

- Purpose: `testing.TB` fixtures with automatic cleanup.
- Owns: stateless (cleanup registration only).
- Depends on: `fleetbox` (public API only — no internal imports).
- Public API: `Start(t, image, opts...) *fleetbox.VM`,
  `StartN(t, prefix, n, opts...) []*fleetbox.VM`, `SkipIfShort(t, reason)`.
- Invariants:
  - Uses only the public `fleetbox` API.
  - Every VM it creates is destroyed via `t.Cleanup` — test VMs never outlive tests.
  - Skips (not fails) on unsupported platforms (`skipIfUnsupported`).
  - VM names are derived from test names (`safeName`) so parallel tests don't collide.

### §5.3 `cmd/fleetbox`

- Purpose: the CLI — `up`, `down`, `ls`, `ssh`, `cp`, `ssh-config`, `rm`, plus the
  hidden runner mode dispatch.
- Owns: flag parsing, terminal output, exec of system `ssh`/`scp`.
- Depends on: `fleetbox` (public API), `internal/runner`, `internal/store`.
- Public API: none (package main).
- Invariants:
  - The CLI adds no capability of its own — every VM operation goes through
    `fleetbox` / `runner` (ADR-0001).
  - VM lifecycle in CLI mode is always delegated to the runner; the CLI process itself
    never holds a VM.
  - No yaml, no config files — flags and defaults only.

### §5.4 `internal/backend`

- Purpose: the hypervisor-neutral contract every backend implements.
- Owns: stateless (interface + enum + MAC derivation).
- Depends on: stdlib only.
- Public API (internal): `Backend{Create, NestedVirtSupported}`,
  `VM{Start, Stop, State, Wait}`, `Config{Name, DiskPath, SeedPath, EFIPath, MAC, CPUs,
  MemoryBytes, SerialOut}`, `State` enum + `String()`, `GenerateMAC(name)`.
- Invariants:
  - Imports no hypervisor SDK — pure contract.
  - `GenerateMAC` is deterministic: same name → same MAC (locally-administered,
    unicast).

### §5.5 `internal/backend/vz`

- Purpose: the VZ (Apple Virtualization.framework) implementation of `backend.Backend`.
- Owns: the `vz.VirtualMachine` object and the serial-console copy goroutine.
- Depends on: `internal/backend`, `github.com/Code-Hex/vz/v3`.
- Public API (internal): `New() *Backend`; `Backend` and `VM` satisfy the backend
  interfaces (`var _` checks present).
- Invariants:
  - **The only package in the module that imports `Code-Hex/vz`** (ADR-0002; enforced
    by the depguard rule in `.golangci.yml`).
  - All vz types/states are translated to `backend` types at this boundary; nothing vz
    leaks upward.
  - EFI boot of stock images only — no kernel/initrd extraction (ADR-0003).

### §5.6 `internal/image`

- Purpose: cloud image download, checksum verification, qcow2→raw conversion, cache.
- Owns: `~/.fleetbox/images/` contents (via paths given by store).
- Depends on: `go-qcow2reader`, stdlib (`net/http`, `crypto/sha256`).
- Public API (internal): `Catalog` (alias → `ImageInfo{URL, SHA256}`),
  `Ensure(cacheDir, urlOrAlias) (string, error)`, `CopyDisk(src, dst, sizeBytes)`.
- Invariants:
  - One code path for all images — adding a distro is adding a `Catalog` entry, never
    new code (ADR-0003).
  - Cached images are immutable; per-VM disks are copies.

### §5.7 `internal/seed`

- Purpose: cloud-init NoCloud seed ISO generation.
- Owns: stateless (writes one file per call).
- Depends on: `github.com/pilat/cloudiso`.
- Public API (internal): `Config{Hostname, User, SSHKey}`, `Create(path, cfg)`.
- Invariants:
  - The user-data stays minimal: one user, authorized key, passwordless sudo, hostname.
    Nothing else goes into the guest (ADR-0005).
  - No per-distro templates — the same seed works for every image.

### §5.8 `internal/store`

- Purpose: the `~/.fleetbox/` directory layout, `config.json` persistence, flock-based
  VM locking.
- Owns: all on-disk state (§4.2).
- Depends on: stdlib only.
- Public API (internal): `Store` (`New`, `NewAt`, path methods, `Exists/Create/Save/
  Load/Delete/List`, `TryLock`), `VM` config struct, `Lock.Unlock`.
- Invariants:
  - Every path under `~/.fleetbox/` is produced by a `Store` method — no other package
    builds those paths by hand.
  - `config.json` is human-readable (indented JSON).
- Notes: `TryLock` (flock-based per-VM locking) is implemented and tested but **not
  yet wired into `fleetbox.Start`** — it was designed to guard concurrent starts of
  the same VM. Wiring it in is an open task; until then the §5.1 known gap stands.

### §5.9 `internal/dhcp`

- Purpose: `/var/db/dhcpd_leases` parsing — hostname/MAC → IP.
- Owns: stateless.
- Depends on: stdlib only.
- Public API (internal): `LookupByHostname`, `LookupByMAC`, `ParseLeases`,
  `ParseLeasesFile`, `ParseLeasesData`, `Lease` struct.
- Invariants:
  - Read-only consumer of a macOS system file; never writes anything.

### §5.10 `internal/sshkey`

- Purpose: per-installation ed25519 keypair + programmatic SSH client.
- Owns: `~/.fleetbox/id_ed25519[.pub]` (via path given by store).
- Depends on: `golang.org/x/crypto/ssh`.
- Public API (internal): `Manager` (`NewManager`, `EnsureKey`, `PrivateKey`, `Path`,
  `Dial`, `DialIP`, `WaitForSSH`), `Client` (`Run`, `Close`).
- Invariants:
  - One keypair per installation, generated lazily, injected into guests via cloud-init
    only.
  - Host key checking is intentionally disabled (ephemeral test VMs).

### §5.11 `internal/runner`

- Purpose: the CLI-mode VM holder process — re-exec, pidfile, unix-socket control.
- Owns: runner process lifecycle, `pid-<name>` / `sock-<name>` / `runner-<name>.log`
  files, the socket-listener goroutine and per-connection handler goroutines,
  mutex-protected `runnerState`.
- Depends on: `fleetbox` (public API), `internal/store`.
- Public API (internal): `IsRunner`, `GetRunnerVMName`, `Spawn`, `Run`, `IsRunning`,
  `GetStatus`, `Stop`, `WritePidfile`, `RemovePidfile`, `Status` struct.
- Invariants:
  - The runner boots VMs through the public `fleetbox.Start()` — it has no backdoor
    into internals (ADR-0006).
  - Socket protocol is two plain commands (`status`, `stop`) answering JSON; nothing
    else will be added to it (no forwarding, no guest protocol).
  - CLI options survive the re-exec: `Spawn` serializes applied `Options` values into
    `FLEETBOX_OPTS`; `Run` deserializes them.

### Dependency graph

```
consumers      cmd/fleetbox ──► internal/runner      fleetboxtest
                    │   │            │    │              │
                    │   └────────────┼────┼──────────────┤
                    ▼                ▼    │              ▼
public API               fleetbox (root) ◄──────────────┘
                              │  (backend_darwin_arm64.go also
                              │   imports internal/backend/vz)
               ┌──────────────┼──────────┬──────────┬─────────┬──────────┐
               ▼              ▼          ▼          ▼         ▼          ▼
internal   backend ◄── backend/vz     image       seed     sshkey      dhcp
               ▲            │            │          │         │
               │       Code-Hex/vz   go-qcow2-  cloudiso  x/crypto/ssh
               │                      reader
               └── (contract; no SDK imports)

           store ◄── (root, runner, and cmd/fleetbox all use it for paths)
```

Edges that exist (verified by `go list -f '{{.Imports}}'`):

- `cmd/fleetbox` → `fleetbox`, `internal/runner`, `internal/store`
- `fleetboxtest` → `fleetbox` (nothing else)
- `internal/runner` → `fleetbox`, `internal/store`
- `fleetbox` (root) → all internal packages except `runner`; the
  `internal/backend/vz` import lives only in the build-tagged
  `backend_darwin_arm64.go`
- `internal/backend/vz` → `internal/backend` + `Code-Hex/vz` (the only SDK import)
- All other internal packages import stdlib / their one external dep only

`internal/runner` is architecturally a *consumer* of the public API (like the CLI it
serves), despite living under `internal/` — it is internal only because it is not part
of the public contract.

## §6. Architectural invariants

Violations of these are bugs, not style issues (they restate the core principles from
CLAUDE.md as checkable rules):

1. **Library-first.** Every capability exists in the Go API; the CLI only wraps it.
   Check: `cmd/fleetbox` and `internal/runner` import `fleetbox`, never the reverse.
2. **Backend-neutral public API.** `Code-Hex/vz` appears in exactly one import site:
   `internal/backend/vz`. Check: depguard rule `vz-isolation` in `.golangci.yml`
   (`make lint` fails on violation).
3. **Nothing of ours inside the guest.** The only artifact fleetbox produces for a
   guest is a cloud-init NoCloud seed ISO. No agent, no helper binary, no host↔guest
   protocol. Check: `internal/seed` writes user-data/meta-data only; no other package
   writes into guest-visible storage.
4. **No port forwarding.** VMs are reached by their own IP. Check: no listener/proxy
   code outside `internal/runner`'s control socket (which is host-only, not
   guest-related).
5. **No yaml, no templates, no per-distro code paths.** Check: no yaml parser in
   go.mod; `internal/image.Catalog` is a dumb map; `internal/seed` has a single code
   path.
6. **Clusters are a naming convention.** Check: no cluster type, no cluster state file
   anywhere; `StartN`/`up -n` just loop over `prefix-i` names.
7. **Cattle with persistence.** `Start`/`up` boot existing VMs instead of failing;
   `Destroy`/`rm` is the only destructive operation. Check: nothing else calls
   `store.Delete`.
8. **Platform gating is compile-time.** Backend selection happens via build-tagged
   files (`backend_darwin_arm64.go`), never via runtime config.

## §7. Known limitations (accepted for v0)

- **VM→VM networking does not work** over VZ NAT (VMs are isolated from each other).
  Single-VM testing is the v0 target; multi-node is deferred (ADR-0004).
- **IP discovery is hostname-based**, which assumes cloud-init sets the hostname and
  the hostname is unique per VM. Both hold for fleetbox-created VMs (ADR-0007).
- **macOS / Apple Silicon / M3+ only.** Not a limitation to fix but a v0 scope
  decision; a linux/KVM backend is possible behind the same Backend interface
  (ADR-0002).
- **VM tests can't run on CI.** GitHub-hosted runners have no nested virtualization.

## §8. Keeping This Document Accurate

After implementation changes, verify:

- **Module list**: packages in §5 == `go list ./...` (minus `spike/`, which is a
  separate throwaway module). A new/removed/renamed package requires a §5 section
  update.
- **Public API**: exported symbols in `fleetbox.go` and `fleetboxtest/` match §5.1 /
  §5.2. Quick check: `go doc github.com/pilat/fleetbox | grep '^func\|^type\|^const'`.
- **CLI surface**: commands in `cmd/fleetbox/main.go`'s dispatch switch match §5.3 and
  the `usage()` text.
- **Backend contract**: `internal/backend/backend.go` interfaces match §5.4.
- **State layout**: path methods in `internal/store/store.go` match the §4.2 tree.
- **Dependencies**: direct requires in `go.mod` match the deps named in §5 module
  sections (currently: Code-Hex/vz, pilat/cloudiso, go-qcow2reader, x/crypto).
- **Invariants**: `make lint` passes (depguard enforces invariant #2); the rest of §6
  is spot-checked by reading the named files.
- **New design decisions**: anything decided in a spec under `ai/tasks/` that changed
  the architecture must land here (what) and in `docs/adr/` (why) in the same PR —
  `ai/tasks/` is gitignored and does not travel with the repo.

Run `/pilat:arch-sync` to check automatically.
