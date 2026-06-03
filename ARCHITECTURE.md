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
| **Runner / Holder** | A re-exec'd `fleetbox` process that holds an `up` group (one VM or a whole cluster) alive in CLI mode, exposing status/stop/addmember over per-member unix sockets. Does not exist in library mode. |
| **Store** | The `~/.fleetbox/` directory layout and its `config.json` files. The only persistent state fleetbox has. |
| **Cluster** | VMs sharing one vmnet network, named by convention (`prefix-1`, `prefix-2`, ...). The `fleetbox.Cluster` type is an in-process runtime handle; no cluster state is *persisted* anywhere (see §4.2). |

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
6. **Network & boot** — `newBackend().CreateNetwork()` makes a vmnet SharedMode
   logical network (a single `Start` gets a one-member network; a `StartN` cluster
   shares one network across all nodes — §4.3). `Create(backend.Config{...}, net)`
   builds the platform VM (EFI bootloader, vmnet SharedMode NIC, virtio disk + seed
   ISO, serial console → `serial.log`) on that network, then `Start(ctx)` boots it
   and polls until the backend reports running.
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
├── pid-<name>             # holder pidfile, one per member (CLI mode only)
├── sock-<name>            # holder unix socket, one per member (CLI mode only)
└── runner-<name>.log      # holder process output, named after the first member (CLI mode only)
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

There is no database, no global state file, and **no persisted cluster entity** —
clusters are a naming convention (`prefix-N`). The in-process `fleetbox.Cluster` (and
the CLI holder that shares one network across a cluster's VMs) is a runtime handle only:
it lives in memory, dies with the process, and writes nothing about "the cluster" to
disk (ADR-0009). The harness's job is N named VMs sharing a network, nothing more —
membership beyond that is what the software under test does. `ls ~/.fleetbox/vms/` is
the entire "database."

### §4.3 Networking model

- VMs attach to a **vmnet SharedMode logical network**
  (`VZVmnetNetworkDeviceAttachment`, macOS 26+). Each VM gets a **directly routable
  IP** from bootpd on the network's bridge. There is no port forwarding of any kind,
  by design (ADR-0004).
- Host→VM, VM→internet (NAT44), **and VM↔VM** all work on a single NIC, with no root
  and no `com.apple.vm.networking` entitlement. VM↔VM is the capability VZ NAT lacked
  (NAT's bridge members carried a `PRIVATE` flag; SharedMode's do not). See ADR-0008.
- **One network per `up`/Start group.** A single `Start` creates a one-member network;
  a `StartN`/`StartCluster` cluster — and, in CLI mode, a holder process (§4.4) —
  shares one network across all members, so the cluster is interconnected. The network
  is an in-process object tied to VM lifetime — never persisted, so clusters remain a
  naming convention with no state (§4.2). Concurrent networks get distinct `/24`s in
  `192.168.0.0/16` from a host-aware subnet detector, and separate networks are
  isolated from each other.
- IP discovery: VMs are found by **hostname** in `/var/db/dhcpd_leases` (cloud-init
  sets the hostname; VZ writes DUID-based identifiers instead of plain MACs). Unchanged
  by the move to SharedMode — it rides the same bootpd/bridge machinery as NAT. See
  ADR-0007.
- SSH: library mode uses `golang.org/x/crypto/ssh` programmatically; CLI `ssh`/`cp`
  exec the system `ssh`/`scp` binaries for a proper interactive terminal.

### §4.4 Process model

**Library mode** — the test process calls `fleetbox.Start()`; the `*VM` value holds the
backend VM object. VMs die when the process exits. `fleetboxtest` registers
`t.Cleanup(Destroy)` so test VMs never outlive their test.

**CLI mode** — `fleetbox up` calls `runner.Spawn()`, which re-execs the `fleetbox`
binary with a hidden `--fleetbox-runner <name,name,...>` flag. The re-exec'd process is
the *holder*. One holder owns a whole `up` group — a single VM or an interconnected
cluster — via one `fleetbox.Cluster` (one shared vmnet network, ADR-0008, ADR-0009). It:

1. for each member, writes `pid-<name>` and listens on `sock-<name>`;
2. creates the cluster's shared network once, then boots each member onto it
   (`Cluster.Add`);
3. answers `status` / `stop` / `addmember <name>` per member socket (JSON-encoded
   `runner.Status`);
4. on `stop`: graceful `VM.Stop()` of that one member, retiring its socket+pidfile; the
   holder exits when its last member is gone, or on SIGTERM (stops all, 30s budget).

The holder's *entire* job is holding VMs and answering the socket. No forwarding, no
guest protocol, no tunnels (ADR-0006). CLI options are serialized to the holder via the
`FLEETBOX_OPTS` env var (Option funcs → `Options` values → JSON).

Because per-name sockets and pidfiles are unchanged, every per-name command
(`ls`/`ssh`/`down`/`rm`) addresses a member without knowing whether it shares a process
with siblings. `up` of a member whose siblings already run sends `addmember` to a live
sibling so the node re-joins the existing network instead of getting an isolated one
(ADR-0009). A holder crash takes its whole cluster down — the accepted cost of
in-process network sharing.

### §4.5 Platform & build constraints

- The whole module is build-tagged `darwin && arm64` (every non-test `.go` file in the
  root, cmd, fleetboxtest, and backend/vz packages). It does not compile elsewhere.
- **macOS 26.0 is the floor.** Networking uses vmnet SharedMode
  (`VZVmnetNetworkDeviceAttachment`), which exists only on macOS 26+; the requirement
  surfaces as an error from the first network creation, wrapped once in the vz backend
  (ADR-0008). Earlier releases (13–15) are no longer supported.
- Any binary that creates VZ VMs (the CLI, VM test binaries) must carry the
  `com.apple.security.virtualization` entitlement — ad-hoc codesign is enough for dev,
  and it is sufficient for vmnet SharedMode too (no `com.apple.vm.networking`).
  `make build` compiles and signs the CLI; `make test-vm` compiles, signs, and runs the
  `fleetboxtest` binary. There is no generic sign target — signing a VM test binary for
  any other package is a manual `go test -c` + `codesign --entitlements entitlements.plist`.
- Nested virtualization (required by consumers that run KVM inside guests) needs M3+.
  `fleetbox.NestedVirtSupported()` reports availability; `fleetboxtest` skips tests when
  unsupported.
- CI runs on a `macos-26` GitHub-hosted runner and cannot boot VZ VMs. CI = lint +
  build + unit tests only; VM-boot tests are skipped there (no nested virtualization).
  `make test` passes `-short` so it stays VM-free even on capable hardware; VM tests run
  locally via `make test-vm`.

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
- Owns: the VM lifecycle orchestration (§4.1); per-VM serial log file handle; the
  reference to the shared `backend.Network` held on each `VM` so the network is not
  GC'd while the VM (or a cluster sibling) lives (§4.3, ADR-0008).
- Depends on: `internal/backend` (+ compile-time `internal/backend/vz` via
  `backend_darwin_arm64.go`), `internal/dhcp`, `internal/image`, `internal/seed`,
  `internal/sshkey`, `internal/store`.
- Public API:
  - `Start(ctx, name, opts...) (*VM, error)`, `StartN(ctx, prefix, n, opts...) ([]*VM, error)`
  - `StartCluster(ctx, names, opts...) (*Cluster, error)`, `NewCluster(opts...) (*Cluster, error)`
  - `type Cluster`: `Add(ctx, name) (*VM, error)`, `VMs() []*VM` — a set of VMs sharing
    one in-process vmnet network; members can be added at runtime (ADR-0009)
  - `NestedVirtSupported() bool`
  - `type VM`: `Name()`, `IP() net.IP`, `SSH(ctx, cmd) (string, error)`, `Stop(ctx)`,
    `Destroy(ctx)`, `State() string`
  - `type Options{Image, CPUs, MemGB, DiskGB}`, `type Option func(*Options)`,
    `WithImage`, `WithCPUs`, `WithMemoryGB`, `WithDiskGB`
  - image aliases: `Debian12`, `Ubuntu2404`
- Invariants:
  - No backend (vz) types in any exported signature — the API is backend-neutral
    (ADR-0002, enforced by depguard).
  - `StartN`/`StartCluster` boot an **interconnected cluster**: all members share one
    vmnet network and reach each other by IP (ADR-0008). Shared per-call setup (store,
    SSH key, image, backend) runs once via `resolveStartDeps`; `startOnNetwork` does the
    per-VM work; `StartN` is a thin wrapper over `StartCluster`. `Cluster` is an
    in-process runtime handle — the shared network is never persisted, so "clusters are
    a naming convention, no state" still holds (ADR-0009).
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
  - VM lifecycle in CLI mode is always delegated to the holder; the CLI process itself
    never holds a VM.
  - `up` boots a single VM (`up name`) or an interconnected cluster (`up prefix -n N`,
    or `up a b c`) — the whole group runs in one holder sharing one network (ADR-0009).
    `up` partitions members into running/missing: none running → fresh holder; some
    running in one holder → `addmember` the rest so a re-upped node re-joins the live
    network; running members split across processes → rejected (their networks can't
    merge). Flags and positional names may be interspersed (`up test1 -n 2` works).
  - No yaml, no config files — flags and defaults only.

### §5.4 `internal/backend`

- Purpose: the hypervisor-neutral contract every backend implements.
- Owns: stateless (interface + enum + MAC derivation).
- Depends on: stdlib only.
- Public API (internal): `Backend{CreateNetwork, Create, NestedVirtSupported}`,
  `Network{Close}` (opaque network handle — no hypervisor types on it),
  `VM{Start, Stop, State, Wait}`, `Config{Name, DiskPath, SeedPath, EFIPath, MAC, CPUs,
  MemoryBytes, SerialOut}`, `State` enum + `String()`, `GenerateMAC(name)`. `Create`
  takes the `Network` to attach the VM to.
- Invariants:
  - Imports no hypervisor SDK — pure contract. `Network` is opaque: no `vmnet`/`vz`
    types appear on it (ADR-0002, ADR-0008).
  - `GenerateMAC` is deterministic: same name → same MAC (locally-administered,
    unicast).

### §5.5 `internal/backend/vz`

- Purpose: the VZ (Apple Virtualization.framework) implementation of `backend.Backend`.
- Owns: the `vz.VirtualMachine` object and the serial-console copy goroutine; the
  `vzNetwork` wrapper around a vmnet logical network; the process-wide reserved-subnet
  set used by the subnet detector.
- Depends on: `internal/backend`, `github.com/Code-Hex/vz/v3` and its
  `.../vz/v3/vmnet` subpackage (both vendored under `third_party/vz`, ADR-0008).
- Public API (internal): `New() *Backend`; `Backend` (`CreateNetwork`, `Create`,
  `NestedVirtSupported`) and `VM`/`vzNetwork` satisfy the backend interfaces (`var _`
  checks present).
- Invariants:
  - **The only package in the module that imports `Code-Hex/vz`** and its `vmnet`
    subpackage (ADR-0002; enforced by the depguard rule in `.golangci.yml`).
  - All vz/vmnet types/states are translated to `backend` types at this boundary;
    nothing vz leaks upward. The `vmnet.Network` lives behind the opaque
    `backend.Network`.
  - `CreateNetwork` makes a vmnet SharedMode network on a free `/24`; the macOS-26
    requirement is the single canonical error here (ADR-0008). `vzNetwork.Close` is a
    no-op — the network is released by GC once unreferenced (R3).
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

- Purpose: the CLI-mode holder process — one process owns an `up` group (single VM or
  cluster) via one `fleetbox.Cluster`; re-exec, per-member pidfile, per-member
  unix-socket control (ADR-0006, ADR-0009).
- Owns: holder process lifecycle, per-member `pid-<name>` / `sock-<name>` files and the
  shared `runner-<first>.log`, the per-member listener + handler goroutines, the
  mutex-protected member registry (`holder`/`member`).
- Depends on: `fleetbox` (public API), `internal/store`.
- Public API (internal): `IsRunner`, `GetRunnerVMNames`, `Spawn` (takes a name list),
  `AddMember`, `Run`, `IsRunning`, `GetStatus`, `Stop`, `WritePidfile`,
  `RemovePidfile`, `Status` struct.
- Invariants:
  - The holder boots VMs through the public `fleetbox` API (`NewCluster`/`Cluster.Add`)
    — no backdoor into internals (ADR-0006).
  - Socket protocol is three plain commands (`status`, `stop`, `addmember <name>`)
    answering JSON/`ok`; it stays host-only — no forwarding, no guest protocol.
  - `stop` shuts down one member; the holder process survives until its last member is
    gone (or SIGTERM stops all). An initial multi-member boot is all-or-nothing.
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
- `internal/backend/vz` → `internal/backend` + `Code-Hex/vz` and its `vmnet`
  subpackage (the only SDK imports; the vz module is vendored under `third_party/vz`
  and wired via a relative `replace` in `go.mod`, ADR-0008 — a temporary bridge until
  PR #205 releases upstream)
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
6. **Clusters are a naming convention.** Check: no cluster *state file* anywhere. The
   `fleetbox.Cluster` type is an in-process runtime handle only — `StartN`/`StartCluster`
   produce `prefix-i`/named members sharing one in-process network, and the CLI holder
   keeps that network in memory (ADR-0009); nothing about a cluster is persisted to
   `~/.fleetbox/` (§5.1, §5.11).
7. **Cattle with persistence.** `Start`/`up` boot existing VMs instead of failing;
   `Destroy`/`rm` is the only destructive operation. Check: nothing else calls
   `store.Delete`.
8. **Platform gating is compile-time.** Backend selection happens via build-tagged
   files (`backend_darwin_arm64.go`), never via runtime config.

## §7. Known limitations (accepted for v0)

- **A holder crash takes its whole cluster down.** A CLI cluster's VMs share one holder
  process so they can share one in-process vmnet network (ADR-0009). That trades the
  per-VM crash isolation ADR-0006 originally had: lose the holder, lose every member of
  that cluster. Single-VM `up` is unaffected (a cluster of one). Accepted for a
  test-fixture tool; per-VM isolation would mean cross-process network sharing (the XPC
  path ADR-0009 rejected).
- **CLI members of one cluster can't span separate processes.** If members were started
  by separate `up` commands (separate holders, separate networks), `up`-ing them
  together can't merge their networks; it reports this instead of silently producing a
  disconnected node (ADR-0009). Bring a cluster up together, or `rm` and retry.
- **IP discovery is hostname-based**, which assumes cloud-init sets the hostname and
  the hostname is unique per VM. Both hold for fleetbox-created VMs (ADR-0007).
- **macOS 26+ / Apple Silicon / M3+ only.** Networking requires macOS 26 (vmnet
  SharedMode, ADR-0008); nested virt requires M3+. Not limitations to fix but scope
  decisions; a linux/KVM backend is possible behind the same Backend interface
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
  Code-Hex/vz is vendored under `third_party/vz` and resolved via a relative `replace`
  pending upstream release of PR #205 (ADR-0008); `third_party/` is excluded from lint
  and is its own module (skipped by `go ... ./...`).
- **Invariants**: `make lint` passes (depguard enforces invariant #2); the rest of §6
  is spot-checked by reading the named files.
- **New design decisions**: anything decided in a spec under `ai/tasks/` that changed
  the architecture must land here (what) and in `docs/adr/` (why) in the same PR —
  `ai/tasks/` is gitignored and does not travel with the repo.

Run `/pilat:arch-sync` to check automatically.
