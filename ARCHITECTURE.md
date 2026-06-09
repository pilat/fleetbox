# ARCHITECTURE.md — fleetbox

This file is **descriptive**: it records what the system looks like today.
`docs/coding-style.md` is **prescriptive** (how new code must be written), and
`docs/adr/` records **why** the significant decisions were made. When this file and the
code disagree, one of them is wrong — fix whichever one drifted (see §8).

Status: **v0** — the public API is not yet stable.

---

## §1. Overview

fleetbox provides Linux VMs as Go test fixtures. On macOS (Apple Silicon) it boots stock
Linux cloud images via Apple Virtualization.framework (VZ); on Linux (amd64/arm64) it
boots them via cloud-hypervisor (CH). Either way it configures the guest once with
cloud-init and hands it to tests as an SSH-reachable fixture, through one
backend-neutral Go API. Think "testcontainers-go, but for real VMs" — real kernel, real
systemd, real KVM via nested virtualization.

Module: `github.com/pilat/fleetbox`

The backend is chosen at compile time per platform (§4.5): VZ on `darwin/arm64`,
cloud-hypervisor on `linux/{amd64,arm64}`, a clear "unsupported platform" error
everywhere else. See ADR-0011 (Linux backend) and ADR-0012 (the macOS version matrix).

Two consumers, one API:

- **Library mode** — `fleetbox.Start()` / `fleetboxtest.Start(t, ...)` inside a Go test
  process. On Linux the orchestration runs in-process; on macOS it runs in a downloaded,
  signed `fleetbox-helper` subprocess bound to the test process — so the test binary stays
  pure Go (no cgo, no codesign) and the helper plus its VMs die when the test exits
  (ADR-0017). Either way, when the owning process goes, the VMs go.
- **CLI mode** — `fleetbox up/down/ls/ssh/cp/ssh-config/rm` for manual work. The CLI runs a
  detached, persistent *holder* per `up` group — a single VM or a whole cluster sharing one
  network (§4.4): on Linux it re-execs itself, on macOS it spawns the same downloaded
  helper. The holder's VMs outlive the command.

Everything the CLI does goes through the public Go API (or the same helper the library
drives). The library is the product; the CLI is a wrapper (see ADR-0001, ADR-0017).

## §2. Glossary

| Term | Meaning |
|------|---------|
| **VM** | One virtual machine: a member directory under `~/.fleetbox/clusters/<cluster>/<member>/` plus, when running, a backend VM object (a VZ machine, or a cloud-hypervisor child process) inside some process. |
| **Backend** | The hypervisor abstraction (`internal/backend.Backend`). Exactly one implementation per platform, selected at compile time: VZ on darwin/arm64, cloud-hypervisor on linux/{amd64,arm64}. |
| **Image** | A stock cloud distro image (raw or qcow2), downloaded once and cached in `~/.fleetbox/images/`. Never modified. |
| **Seed ISO** | A cloud-init NoCloud ISO generated per VM. The only thing fleetbox ever "puts inside" a guest, and it is read by the guest's own cloud-init. |
| **Holder** | The process that owns an `up`/`Start` group (one VM or a whole cluster) and exposes status/stop/addmember over per-member unix sockets. On Linux it is the re-exec'd `fleetbox` binary (CLI) or the in-process orchestrator (library); on macOS it is always the downloaded `fleetbox-helper`. Library mode spawns it *bound* (it dies with the test); CLI mode spawns it *detached* (it persists). |
| **Helper** | `cmd/fleetbox-helper`: the macOS-only, ad-hoc-signed binary that links Virtualization.framework and runs the holder server loop. Downloaded + cached on first use (`internal/helperdist`), so the user's test/CLI binary stays pure Go and unsigned (ADR-0017). On Linux there is no separate helper. |
| **Store** | The `~/.fleetbox/` directory layout and its `config.json` files. The only persistent state fleetbox has. |
| **Cluster** | VMs sharing one network, named by convention (`prefix-1`, `prefix-2`, ...). The `fleetbox.Cluster` type is an in-process runtime handle for membership/health. The cluster is *also* a storage grouping — members nest under `~/.fleetbox/clusters/<cluster>/` (the cluster name derived from the member name) — but there is no cluster *object* with persisted membership; disk grouping and runtime grouping need not be 1:1 (ADR-0014, see §4.2). |

## §3. Source-of-Truth Map

Where the canonical version of each thing lives. When two files disagree, the SoT wins.

| Concept | Source of truth | Notes |
|---------|----------------|-------|
| Public library API | `fleetbox.go` | Exported symbols + doc comments. §5.1 summarizes. |
| Test-fixture API | `fleetboxtest/fleetboxtest.go` | §5.2 summarizes. |
| CLI command surface | `cmd/fleetbox/main.go` (`usage()` + dispatch in `main()`) | §5.3 summarizes. |
| Backend contract | `internal/backend/backend.go` | `Backend`, `VM`, `Network`, `Config`, `State`. |
| On-disk state layout | `internal/store/store.go` path methods | §4.2 summarizes. |
| Image catalog | `internal/image/image.go` `Catalog` map | Alias → per-GOARCH URL + sha256. |
| Pinned VMM binaries | `internal/backend/cloudhypervisor/binaries.go` | cloud-hypervisor + firmware: version + per-arch URL + sha256. |
| macOS helper catalog | `internal/helperdist/helperdist.go` `catalog` map | fleetbox-helper: version + per-arch URL + sha256; `FLEETBOX_HELPER` override (ADR-0017). |
| Holder protocol | `internal/control/control.go` (client) + `internal/holder/holder.go` (server) | Wire commands + states + bound-mode bind/version handshake. |
| Network teardown records & reconcile | `internal/backend/cloudhypervisor/netstate.go` | Write-ahead bridge/tap records + `ip_forward` marker under `~/.fleetbox/networks/`; crash recovery (ADR-0013). |
| Guest provisioning contract | `internal/seed/seed.go` (user-data / meta-data) | One user, one SSH key, hostname, fixture mount lines. Nothing else. |
| Fixture payload format | `internal/fixture/fixture.go` | Host dir → read-only ext4 image (go-ext4fs), attached read-only, mounted by LABEL (ADR-0015). |
| Code style rules | `docs/coding-style.md` + `.golangci.yml` | Prescriptive. Lint enforces the machine-checkable subset. |
| Architecture (current state) | `ARCHITECTURE.md` (this file) | Descriptive. |
| Design decisions & rationale | `docs/adr/` | One file per decision, sequentially numbered. |
| Build & signing recipe | `Makefile` + `entitlements.plist` | `com.apple.security.virtualization` entitlement. |
| Vendored vz provenance & regen | `third_party/vz/NOTICE` + `hack/vendor-vz.sh` | Pinned upstream + vmnet-patch SHAs; `make vendor-vz` regenerates (ADR-0008, ADR-0016). |
| CI behavior | `.github/workflows/ci.yml` | Lint + build + unit tests only; no VM boots on CI. |
| Working specs (local only) | `ai/tasks/` | Gitignored. Durable decisions must graduate to ADRs. |

## §4. System Model

### §4.1 VM lifecycle

`fleetbox.Start(ctx, name, opts...)` is the single public entry point for creating/booting
a VM. The orchestration itself lives in `internal/orchestrator` (extracted from the old
root package, ADR-0017): on Linux the root package calls it in-process; on macOS the root
package is a thin client that hands the work to the helper, where the same orchestrator
runs. fleetboxtest and the CLI both reach this path. The sequence:

0. **Preflight** — `preflight()` (per-platform) fails fast with an actionable message if
   the host can't run a VM, before the (possibly multi-GB) image download. No-op on macOS;
   on Linux it checks `/dev/kvm` is openable and `CAP_NET_ADMIN` is held.
1. **Options** — apply functional options over defaults (image=debian-12, cpus=2,
   mem=4GB, disk=20GB).
2. **Store** — `store.New()` ensures `~/.fleetbox/{clusters,images}/` exist.
3. **SSH key** — `sshkey.EnsureKey()` generates the per-installation ed25519 keypair on
   first use (`~/.fleetbox/id_ed25519[.pub]`).
4. **Image** — `image.Ensure()` returns a cached raw image, downloading / verifying /
   converting qcow2→raw if needed.
5. **VM config** — if `store.Exists(name)`: load `config.json` and boot from it
   (**all options are ignored for an existing VM** — the stored config wins; the image
   option only affects the shared image cache). Otherwise: create the config (stable
   MAC derived from name via `backend.GenerateMAC`; any `WithFixture` payloads validated,
   absolutized, and labeled `FBFIX<i>` here), copy the cached image to `disk.raw` (sparse,
   truncated to requested size), and generate `seed.iso` via `seed.Create` (with the
   fixtures' `LABEL=` fstab entries when the VM has fixtures — ADR-0015).
   On a backend that assigns static addresses (Linux), a free IP is allocated from the
   network's subnet here (`allocateIP`, scanning persisted configs so members get
   distinct, stable addresses), persisted as `config.json`'s `ip`, and injected into
   the seed as a NoCloud `network-config`; the DHCP backend (vz) reports no subnet and
   skips this. On **every** boot, new or existing, each fixture's read-only ext4 image is
   rebuilt from its persisted host dir (`internal/fixture`, no cache) and re-attached as a
   read-only block device — the set is frozen at create, the content refreshed per boot
   (ADR-0015).
6. **Network & boot** — `newBackend()` (per-platform, §4.5) is called once in
   `orchestrator.resolveStartDeps`. `CreateNetwork()` makes the shared network (vmnet SharedMode on
   macOS 26+, a Linux bridge on Linux; a single `Start` gets a one-member network, a
   `StartN` cluster shares one — §4.3). `Create(backend.Config{...}, net)` builds the
   platform VM (VZ: EFI bootloader, vmnet/NAT NIC, virtio disk + seed ISO, serial →
   `serial.log`; CH: a tap on the bridge + the command line to launch) on that network,
   then `Start(ctx)` boots it and waits until the backend reports ready.
7. **IP discovery** — `backendVM.WaitForIP(ctx)` (2-min budget) blocks until the IP is
   known and TCP :22 is reachable, then returns it. vz parses
   `/var/db/dhcpd_leases` by hostname (ADR-0007); cloud-hypervisor returns the IP it
   statically assigned after the reachability probe. This is the one platform coupling
   that used to live in the root package, now behind the backend (ADR-0011).
8. **SSH readiness** — `sshkey.WaitForSSH()` until an authenticated SSH session
   succeeds (timeout 2 min).

`VM.Stop(ctx)` asks the backend for graceful (ACPI) shutdown; the disk persists and a
later `Start` boots it again. `VM.Destroy(ctx)` stops and then deletes the VM directory.
Destroy is the only destructive operation.

### §4.2 State & persistence

All persistent state lives under `~/.fleetbox/` and is owned by `internal/store`:

```
~/.fleetbox/
├── clusters/<cluster>/<member>/   # <cluster> = member name with a trailing -<N> stripped; solo VM = cluster of one
│   ├── config.json        # store.VM: name, MAC, cpus, memory, disk, image, created_at, fixtures[], ip (Linux)
│   ├── disk.raw           # the VM's root disk (sparse file)
│   ├── seed.iso           # cloud-init NoCloud seed (generated once at create)
│   ├── fixture-<i>.img    # read-only ext4 fixture payload, one per WithFixture, rebuilt each boot (ADR-0015)
│   ├── efi.nvram          # EFI variable store (VZ backend only)
│   ├── ch.sock            # cloud-hypervisor REST api socket (Linux, while running)
│   ├── serial.log         # serial console capture (debugging)
│   ├── pid                # holder pidfile, one per member (while a holder serves it)
│   └── .lock              # flock target for TryLock
├── run/                   # holder control sockets, hashed names so the path fits the
│   │                      #   104-byte unix sun_path limit (ADR-0017, amends ADR-0014):
│   ├── <hash>.sock        #   per-member control socket (status/stop/addmember)
│   └── <hash>.ctl         #   bound-mode holder-wide socket (bind handshake + EOF teardown)
├── images/                # downloaded + converted raw cloud images (cache)
├── bin/                   # downloaded checksum-pinned binaries: cloud-hypervisor + firmware (Linux), fleetbox-helper-<ver> (macOS)
├── networks/              # Linux: per-bridge write-ahead records (<bridge>.json) + ipforward.orig marker (ADR-0013)
├── id_ed25519, id_ed25519.pub   # per-installation SSH keypair
└── runner-<name>.log      # holder process output, named after the first member
```

The cluster segment is **derived** from the member name (strip a single trailing
`-<digits>`), never stored: `web-3 → web`, `dev → dev`, `node-2024 → node`. A solo VM is
a cluster of one (`clusters/dev/dev/`); a `-n N` cluster groups under one cluster dir
(`clusters/web/{web-1,web-2,web-3}/`). The derivation must be computable from the member
name alone because the member directory has to exist before `config.json` is written into
it (ADR-0014).

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

There is no database and no global state file. The cluster is a **storage grouping** —
members nest under `clusters/<cluster>/` so a cluster's state (and, later, a shared
cluster-level artifact) has one home — but it is **not a persisted entity**: there is no
cluster object with stored membership, the cluster name is *derived* from the member name
rather than recorded, and disk grouping need not match runtime grouping (a heterogeneous
`up a b c` makes three single-member cluster dirs yet one shared network — ADR-0014). The
in-process `fleetbox.Cluster` (and the CLI holder that shares one network across a
cluster's VMs) remains a runtime handle for membership/health only: it lives in memory,
dies with the process, and writes nothing about "the cluster" beyond the directory tree
(ADR-0009, ADR-0014). The harness's job is N named VMs sharing a network, nothing more —
membership beyond that is what the software under test does. `ls ~/.fleetbox/clusters/*/`
is the entire "database."

### §4.3 Networking model

The shared property across platforms: **directly routable IPs, no port forwarding**
(ADR-0004), and VMs on one network reach the host, the internet, and each other on a
single NIC. How that is realized differs by backend.

- **macOS (VZ).** On macOS 26+ VMs attach to a **vmnet SharedMode logical network**
  (`VZVmnetNetworkDeviceAttachment`); each gets a directly-routable IP from bootpd on
  the network's bridge, with no root and no `com.apple.vm.networking` entitlement.
  VM↔VM is the capability VZ NAT lacked (ADR-0008). On macOS < 26 there is no shared
  network: the backend falls back to a per-VM `VZNATNetworkDeviceAttachment` (a single,
  isolated VM, no clusters — ADR-0012).
- **Linux (cloud-hypervisor).** `CreateNetwork` makes one **Linux bridge per cluster**
  on a free `/24` (gateway `.1`), enables IPv4 forwarding (only if it was off, restoring
  it once nothing of ours remains — ADR-0013), and installs `iptables`
  MASQUERADE/FORWARD rules for egress. Each VM gets a **tap** enslaved to the bridge
  (`--net tap=…`); its static IP is allocated from the subnet and injected via the
  seed's NoCloud `network-config`. Members on one bridge reach the host, each other,
  and the internet — the SharedMode property reproduced on Linux (ADR-0011). The bridge
  is a real OS resource, so it is torn down explicitly (`Cluster.Close` /
  sole-owner `VM.Destroy`), not by GC.
- **One network per `up`/Start group.** A single `Start` creates a one-member network;
  a `StartN`/`StartCluster` cluster — and, in CLI mode, a holder process (§4.4) —
  shares one network across all members, so the cluster is interconnected. The network
  is a runtime object tied to VM lifetime — never persisted; the cluster's only on-disk
  trace is the `clusters/<cluster>/` storage grouping, not a network or membership record
  (§4.2). Concurrent networks get distinct `/24`s in
  `192.168.0.0/16` from a host-aware subnet detector (one per backend), and separate
  networks are isolated.
- **IP discovery is behind the backend** (`backend.VM.WaitForIP`). vz finds VMs by
  **hostname** in `/var/db/dhcpd_leases` (cloud-init sets the hostname; VZ writes
  DUID-based identifiers instead of plain MACs — ADR-0007); cloud-hypervisor already
  knows the IP it assigned and just probes TCP :22. Both return the IP once port 22 is
  reachable.
- SSH: library mode uses `golang.org/x/crypto/ssh` programmatically; CLI `ssh`/`cp`
  exec the system `ssh`/`scp` binaries for a proper interactive terminal.

### §4.4 Process model

A **holder** is the process that owns an `up`/`Start` group — a single VM or an
interconnected cluster — via one `orchestrator.Cluster` (one shared network: a vmnet
network on macOS, or the Linux bridge and the cloud-hypervisor child processes booted onto
it). Where the holder lives differs by platform and consumer (ADR-0017):

| | Library (test process) | CLI |
|---|---|---|
| **Linux** | in-process (no holder process) | re-exec'd `fleetbox` binary (detached) |
| **macOS** | downloaded `fleetbox-helper` (bound) | downloaded `fleetbox-helper` (detached) |

On **Linux** nothing needs signing and cloud-hypervisor is already the downloaded VMM, so
the library runs the orchestrator in-process and the CLI re-execs itself with a hidden
`--fleetbox-runner <name,name,...>` flag. On **macOS** all Virtualization.framework work is
severed into `cmd/fleetbox-helper` — the only binary that links vz and carries the
entitlement — so the test/CLI binary stays pure Go; the client downloads the helper
(`internal/helperdist`), spawns it, and drives it over the same socket protocol.

The holder server loop (`internal/holder`) is identical in both lifetime modes; the client
half (`internal/control`) chooses the mode:

- **Bound** (library): the helper is spawned *attached* (no `Setsid`) with the client PID
  in `FLEETBOX_PARENT_PID`, and the client holds one long-lived control connection (a
  `bind` handshake on the holder-wide `<hash>.ctl` socket, which also exchanges a protocol
  version so a stale `FLEETBOX_HELPER` is rejected). When the test process dies — even on
  `kill -9` — the helper reaps itself and its in-process VMs, via the control connection's
  EOF (fast path) and a reparent poll (`os.Getppid()` change, the backstop). On macOS the
  VM runs in-process in the helper, so helper exit ⇒ VM gone.
- **Detached** (CLI): the helper/holder is a session leader (`Setsid`), no parent watch —
  its VMs persist past the command (cattle-with-persistence).

Either way the holder, for each member, ensures its directory, writes its `pid`, and
listens on its per-member `<hash>.sock`; creates the cluster's shared network once and
boots each member onto it (`Cluster.Add`); answers `status` / `stop` / `addmember <name>`
(JSON-encoded `control.Status`); on `stop` gracefully shuts down that one member and
retires its socket+pidfile, exiting when its last member is gone or on SIGTERM; and on exit
releases the shared network via a deferred `Cluster.Close()` — a no-op for vmnet, the
explicit Linux bridge/egress teardown otherwise. Its *entire* job is holding VMs and
answering the socket: no forwarding, no guest protocol, no tunnels (ADR-0006). Options
cross to the holder via the `FLEETBOX_OPTS` env var (Option funcs → `opts.Options` → JSON).

SSH and `cp` never go through the holder: the client dials the VM's directly-routable IP
itself (the helper protocol only reports the IP). Because per-name sockets and pidfiles are
unchanged, every per-name command (`ls`/`ssh`/`down`/`rm`) addresses a member without
knowing whether it shares a process with siblings. `up` of a member whose siblings already
run sends `addmember` to a live sibling so the node re-joins the existing network instead of
getting an isolated one (ADR-0009). A holder crash takes its whole cluster down — the
accepted cost of in-process network sharing.

### §4.5 Platform & build constraints

The support matrix and how it is realized in build tags:

| Platform | Backend | Networking | Clusters |
|----------|---------|------------|----------|
| macOS Apple Silicon, ≥ 26 | VZ | vmnet SharedMode | yes |
| macOS Apple Silicon, < 26 | VZ | VZ NAT (single, isolated VM) | no — `ErrClustersUnsupported` |
| macOS Intel | — | — | unsupported (clear runtime error) |
| Linux amd64 / arm64 | cloud-hypervisor | shared bridge + tap | yes |

- **The module compiles on `darwin/arm64`, `linux/amd64`, and `linux/arm64`.** Backend
  selection is three build-tagged files in `internal/orchestrator`: `backend_darwin_arm64.go`
  (→ vz), `backend_linux.go` (→ cloud-hypervisor), `backend_unsupported.go` (everything
  else). `newBackend() (backend.Backend, error)`; the unsupported stub returns
  `"fleetbox: unsupported platform (<GOOS>/<GOARCH>)"`, so `darwin/amd64` et al. build and
  fail cleanly rather than as an opaque link error. The root package is build-tagged too
  (ADR-0017): `fleetbox_linux.go` wraps the orchestrator in-process, `fleetbox_darwin_arm64.go`
  is the pure-Go helper client, `fleetbox_unsupported.go` errors; `VM`/`Cluster` are defined
  once in the neutral `fleetbox.go` over a build-tagged impl. `internal/backend/vz` is tagged
  `darwin && arm64` (linked by `internal/orchestrator`/`internal/holder` there);
  `internal/backend/cloudhypervisor` is tagged `linux`; `internal/dhcp` is tagged `darwin`.
  `cmd/fleetbox-helper` is darwin/arm64 (a stub elsewhere). `cmd/fleetbox` and the
  backend-free packages (`internal/{opts,control,helperdist,store,…}`) carry no build tag
  and never link a hypervisor. `fleetboxtest` compiles everywhere but its fixtures skip on
  non-darwin/arm64 (Linux fixtures are a follow-up).
- **macOS networking floor is 26.0 for clusters.** vmnet SharedMode
  (`VZVmnetNetworkDeviceAttachment`) exists only on macOS 26+ (ADR-0008). The vz backend
  detects the major version (`syscall.Sysctl("kern.osproductversion")`) once and, below
  26, uses a per-VM `VZNATNetworkDeviceAttachment` for a single isolated VM;
  `SupportsClustering()` is false there, so a second cluster member errors up front
  (ADR-0012).
- **macOS signing is the helper's, not the user's (ADR-0017).** Only `cmd/fleetbox-helper`
  links vz, so only it carries `com.apple.security.virtualization` — ad-hoc codesign is
  enough, and sufficient for vmnet SharedMode (no `com.apple.vm.networking`). The user's
  test binary and the CLI link no vz and need neither cgo nor codesign. `make build`
  compiles the pure-Go CLI (no signing, any platform); `make helper` builds + ad-hoc-signs
  the helper; `make test-vm` builds+signs the helper and points the library at it via
  `FLEETBOX_HELPER`. The published helper auto-downloads, checksum-pinned, once the release
  pipeline fills the `internal/helperdist` catalog; until then `FLEETBOX_HELPER` is the path.
- **Linux host prerequisites** (not provisionable; probed with clear errors): `/dev/kvm`
  present and accessible (user in the `kvm` group) and `CAP_NET_ADMIN` (to make the
  bridge and taps). The cloud-hypervisor binary and firmware are downloaded and
  checksum-pinned to `~/.fleetbox/bin/` (ADR-0011); the Linux path is pure Go, no cgo.
- **Nested virtualization** (consumers running KVM inside guests) needs M3+ on macOS,
  or the host KVM `nested` parameter on Linux. `fleetbox.NestedVirtSupported()` reports
  availability; `fleetboxtest` skips when unsupported.
- **CI.** The `macos-26` runner cannot boot VZ VMs (no nested virtualization): it runs
  lint + build + unit tests; `make test` passes `-short` so it stays VM-free even on
  capable hardware. Unlike VZ, the cloud-hypervisor backend *is* CI-testable on a GitHub
  x86-64 Linux runner, which does expose `/dev/kvm` (after a one-time udev rule so the
  runner user can open it) — the "develop on a Mac, run in cheap hosted Linux CI" path
  (ADR-0017); arm64 hosted Linux runners have no KVM. Not yet wired. Do not switch the
  macOS CI to ubuntu.

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

- Purpose: the public library API — everything a consumer can do with a VM. The VM-owning
  orchestration moved to `internal/orchestrator` (ADR-0017); this package is now the
  build-tagged public surface over it — in-process on Linux, a pure-Go helper client on macOS.
- Owns: the public `VM`/`Cluster` types, defined once (neutral `fleetbox.go`) over a
  build-tagged unexported impl (`vmState`/`clusterState`); the clustering gate and
  `ErrClustersUnsupported`; on macOS the client handles (the helper's `control.Session`,
  member IPs) and the pure-Go capability probes. It owns no backend objects directly.
- Depends on:
  - neutral (`fleetbox.go`): `internal/opts` (re-exported option types).
  - linux (`fleetbox_linux.go`): `internal/orchestrator` (which links the backend).
  - darwin (`fleetbox_darwin_arm64.go`): `internal/control`, `internal/helperdist`,
    `internal/sshkey`, `internal/store`, `golang.org/x/sys/unix` — and NOT
    `internal/orchestrator` or `internal/backend/vz` (the sever, verified by `go list -deps`).
- Public API:
  - `Start(ctx, name, opts...) (*VM, error)`, `StartN(ctx, prefix, n, opts...) ([]*VM, error)`
  - `StartCluster(ctx, names, opts...) (*Cluster, error)`, `NewCluster(opts...) (*Cluster, error)`
  - `type Cluster`: `Add(ctx, name) (*VM, error)`, `VMs() []*VM`, `Close() error` — a set
    of VMs sharing one network; members can be added at runtime. `Close` releases the
    network on Linux; on macOS it stops every remaining member and waits for the helper to
    exit (ADR-0009, ADR-0011, ADR-0017)
  - `ErrClustersUnsupported` — returned when a 2nd member is requested on a non-clustering
    backend (macOS < 26, ADR-0012)
  - `NestedVirtSupported() bool`
  - `Prune() error` — reclaim the inert host resources (Linux bridges, taps, iptables
    rules) a crashed holder left, and restore `ip_forward`; no-op on macOS. Runs
    automatically on the CLI `down`, so cleanup is never the user's job (crashed VMs
    themselves die with their holder — `Pdeathsig` on Linux, the parent-pid poll on macOS,
    ADR-0013/ADR-0017); exported for library callers that want to sweep explicitly
  - `type VM`: `Name()`, `IP() net.IP`, `SSH(ctx, cmd) (string, error)`, `Stop(ctx)`,
    `Destroy(ctx)`, `State() string`
  - `type Options{Image, CPUs, MemGB, DiskGB, Fixtures}`, `type Option func(*Options)`,
    `WithImage`, `WithCPUs`, `WithMemoryGB`, `WithDiskGB`, `WithFixture(hostDir, guestPath)`
  - `type Fixture{HostPath, GuestPath}` — a read-only host directory packed into the guest
    at boot as an ext4 payload (ADR-0015)
  - image aliases: `Debian12`, `Ubuntu2404`
- Invariants:
  - No hypervisor (vz/CH) types in any exported signature — the API is backend-neutral
    (ADR-0002, enforced by depguard for vz). IP discovery is behind `backend.VM.WaitForIP`
    (ADR-0011), so nothing in the root package is platform-specific.
  - `StartN`/`StartCluster` boot an **interconnected cluster** where the backend supports
    it: members share one network and reach each other by IP (ADR-0008 macOS, ADR-0011
    Linux); a 2nd member on a non-clustering backend returns `ErrClustersUnsupported`
    before booting (ADR-0012). Shared per-call setup (store, SSH key, image, backend) runs
    once via `orchestrator.resolveStartDeps`; `orchestrator.startOnNetwork` does the per-VM
    work (including static-IP allocation on Linux); `StartN`/`StartCluster` and the gate are
    neutral in `fleetbox.go`. `Cluster` is a runtime handle — the shared network is never
    persisted, so "clusters are a naming convention, no state" still holds (ADR-0009).
  - Network ownership/teardown: a bare `Start` marks its VM `ownsNetwork`, so `Destroy`
    releases its one-member network; cluster members share a network and leave it to
    `Cluster.Close` (R3). A no-op for vmnet (GC), the explicit teardown for the Linux
    bridge.
  - **The fixture set is frozen at birth, the content refreshed each boot** (ADR-0015):
    `WithFixture` payloads are validated, absolutized, and labeled (`FBFIX<i>`) once at
    first create and persisted in `config.json`; the guest's `LABEL=` fstab line is written
    once by cloud-init. A different set passed to an existing VM is ignored (like
    cpu/mem/disk) — changing it means `rm` + recreate. But because there is no cache, every
    boot rebuilds each fixture's read-only ext4 image from its persisted host dir and
    re-attaches it, so the guest sees the host dir as of that boot (never live within a
    boot). Files arrive world-readable (`0444`/`0555`, uid 0); a VM with no fixtures is
    byte-for-byte unchanged.
  - `Start` on an existing (stopped) VM boots it from its stored config; options are
    ignored for existing VMs. Note: `Start` does **not** detect an already-running VM
    — that guard lives in the CLI's `upMembers` (`control.IsRunning`); concurrent `Start`
    on the same name in library mode is unguarded (known gap, see §5.8).
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

- Purpose: the CLI — `up`, `down`, `ls`, `ssh`, `cp`, `ssh-config`, `rm`, plus (on Linux)
  the hidden re-exec holder dispatch.
- Owns: flag parsing, terminal output, exec of system `ssh`/`scp`; the per-platform holder
  seam (`holder_{linux,darwin,other}.go`): how to spawn a holder, and whether to run as one.
- Depends on: `fleetbox` (public API), `internal/control`, `internal/store`; on Linux also
  `internal/holder` (the re-exec server loop); on darwin also `internal/helperdist` (to
  fetch the helper to spawn). It links no hypervisor on darwin (ADR-0017).
- Public API: none (package main).
- Invariants:
  - The CLI adds no capability of its own — every VM operation goes through the public API
    or the same holder protocol the library uses (ADR-0001).
  - VM lifecycle in CLI mode is always delegated to a detached holder; the CLI process
    itself never holds a VM. On macOS that holder is the downloaded `fleetbox-helper`; on
    Linux it is the re-exec'd CLI (`maybeRunHolder`/`holderExe` are the build-tagged seam —
    ADR-0017, R9).
  - `up` boots a single VM (`up name`) or an interconnected cluster (`up prefix -n N`,
    or `up a b c`) — the whole group runs in one holder sharing one network (ADR-0009).
    `up` partitions members into running/missing: none running → fresh holder; some
    running in one holder → `addmember` the rest so a re-upped node re-joins the live
    network; running members split across processes → rejected (their networks can't
    merge). Flags and positional names may be interspersed (`up test1 -n 2` works).
  - `up` accepts a repeatable `--fixture host:guest` flag (a custom `flag.Value`); host
    paths are resolved to absolute against the CLI cwd before they cross into the holder
    (split on the last colon), and flow to the library as `WithFixture` (ADR-0015). In a
    cluster every member gets the same fixtures.
  - Cleanup is automatic, never a user command: `down` (like `up`) runs the backend
    reconcile via `fleetbox.Prune()` to reclaim resources a crashed holder left, so on
    Linux it too needs root for the `ip`/`iptables` calls (ADR-0013).
  - No yaml, no config files — flags and defaults only.

### §5.4 `internal/backend`

- Purpose: the hypervisor-neutral contract every backend implements.
- Owns: stateless (interface + enum + MAC derivation).
- Depends on: stdlib only.
- Public API (internal): `Backend{CreateNetwork, Create, NestedVirtSupported,
  SupportsClustering, Reconcile}`, `Network{Close, Subnet}` (opaque handle — no hypervisor types;
  `Subnet` returns the CIDR for static-IP backends, "" for DHCP backends),
  `VM{Start, Stop, State, Wait, WaitForIP}`, `Config{Name, DiskPath, SeedPath, EFIPath,
  MAC, CPUs, MemoryBytes, SerialOut, FixturePaths, AssignedIP}` — `FixturePaths` are host
  paths of pre-built read-only ext4 fixture images to attach (backend-neutral, no SDK
  types, no guest path), `State` enum + `String()`,
  `GenerateMAC(name)`. `Create` takes the `Network` to attach the VM to.
- Invariants:
  - Imports no hypervisor SDK — pure contract. `Network` is opaque: no `vmnet`/`vz`/CH
    types appear on it (ADR-0002, ADR-0008, ADR-0011).
  - `WaitForIP` is where IP discovery lives, so the root package stays platform-neutral;
    `SupportsClustering` lets the public layer reject clusters before booting on a
    backend that can't interconnect VMs (ADR-0012).
  - `Reconcile` reclaims host resources orphaned by a crashed holder (Linux); it backs
    `fleetbox.Prune` and runs implicitly at each `CreateNetwork` (up) and on `down`. No-op
    where the backend owns no host network state (vz — vmnet manages its own) (ADR-0013).
  - `GenerateMAC` is deterministic: same name → same MAC (locally-administered,
    unicast).

### §5.5 `internal/backend/vz`

- Purpose: the VZ (Apple Virtualization.framework) implementation of `backend.Backend`.
- Owns: the `vz.VirtualMachine` object and the serial-console copy goroutine; the
  `vzNetwork` wrapper around a vmnet logical network; the process-wide reserved-subnet
  set used by the subnet detector.
- Depends on: `internal/backend` and the vendored vz fork
  `github.com/pilat/fleetbox/third_party/vz` + its `.../third_party/vz/vmnet`
  subpackage — the import-path-renamed Code-Hex/vz, vendored in-module under
  `third_party/vz` (no separate go.mod; `//go:build darwin` throughout, ADR-0008).
- Public API (internal): `New() *Backend`; `Backend` (`CreateNetwork`, `Create`,
  `NestedVirtSupported`, `SupportsClustering`) and `VM`/`vzNetwork` satisfy the backend
  interfaces (`var _` checks present). `VM.WaitForIP` discovers the IP via
  `dhcp.LookupByHostname` + a TCP:22 probe; `vzNetwork.Subnet` returns "" (DHCP).
- Invariants:
  - **The only package in the module that imports the vendored vz fork** and its
    `vmnet` subpackage (ADR-0002; enforced by the depguard rule in `.golangci.yml`).
  - All vz/vmnet types/states are translated to `backend` types at this boundary;
    nothing vz leaks upward. The `vmnet.Network` lives behind the opaque
    `backend.Network`.
  - The macOS major version is detected once in `New`
    (`syscall.Sysctl("kern.osproductversion")`) and cached. `CreateNetwork`/`Create`
    branch on it: **≥26** → vmnet SharedMode on a free `/24` (`vzNetwork.Close` is a
    no-op, GC reaps it — R3); **<26** → a no-op network holder plus a per-VM
    `VZNATNetworkDeviceAttachment`, a single isolated VM. `SupportsClustering` returns
    `major >= 26` (ADR-0008, ADR-0012).
  - Fixture payloads attach as one read-only `VZVirtioBlockDeviceConfiguration` per
    `cfg.FixturePaths` entry (a `NewDiskImageStorageDeviceAttachment(p, true)`), appended to
    the disk + seed storage devices; the loop is a no-op when there are no fixtures, so a
    fixtureless VM's device set is unchanged (ADR-0015). The guest mounts each by volume
    `LABEL=`, so attachment order is irrelevant. The images are built — and rebuilt on every
    boot — by `internal/fixture` before `Create` runs; VZ only attaches them.
  - EFI boot of stock images only — no kernel/initrd extraction (ADR-0003).

### §5.6 `internal/image`

- Purpose: cloud image catalog + qcow2→raw conversion + cache; the download/verify
  itself is delegated to `internal/fetch`.
- Owns: `~/.fleetbox/images/` contents (via paths given by store).
- Depends on: `go-qcow2reader`, `internal/fetch`, stdlib.
- Public API (internal): `Catalog` (alias → `ImageInfo{URLs map[GOARCH]string, SHA256}`),
  `Ensure(cacheDir, urlOrAlias) (string, error)`, `CopyDisk(src, dst, sizeBytes)`.
- Invariants:
  - One code path for all images — adding a distro is adding a `Catalog` entry, never
    new code (ADR-0003). The catalog resolves an alias to the URL for the current
    `runtime.GOARCH`, so the same alias works on macOS arm64 and Linux amd64/arm64.
  - Cached images are immutable; per-VM disks are copies. Image checksums may be empty
    ("latest"); `fetch` then skips verification.

### §5.7 `internal/seed`

- Purpose: cloud-init NoCloud seed ISO generation.
- Owns: stateless (writes one file per call).
- Depends on: `github.com/pilat/cloudiso`.
- Public API (internal): `Config{Hostname, User, SSHKey, Fixtures, Network}`,
  `Fixture{Label, GuestPath}`, `NetworkConfig{MAC, IP, Gateway, Netmask}`,
  `Create(path, cfg)`.
- Invariants:
  - The user-data stays minimal: one user, authorized key, passwordless sudo, hostname.
    Nothing else goes into the guest (ADR-0005).
  - No per-distro templates — the same seed works for every image.
  - A `network-config` (NoCloud netplan v2, static IPv4 matched by MAC) is emitted only
    when `Config.Network` is set (Linux); when nil the guest stays on DHCP and the macOS
    seed is byte-for-byte unchanged (ADR-0011). DNS uses fixed public resolvers
    (`1.1.1.1, 8.8.8.8`), not the gateway — the bridge gateway runs no resolver, so
    pointing the guest at it would break name resolution (ADR-0013).
  - User-data is built by string concatenation (`buildUserData`), no yaml/template
    library. With no fixtures the output is byte-identical to the original template; a
    `mounts:` block (one `[ LABEL=<label>, <guestPath>, ext4, "ro,nofail", "0", "0" ]` line
    per fixture) is emitted only when fixtures are set (ADR-0015). The `mounts:` directive
    writes `/etc/fstab`, so fixtures re-mount on every boot with no cloud-init re-run; the
    label is stable, so the per-boot rebuilt image is always found.

### §5.8 `internal/store`

- Purpose: the `~/.fleetbox/` directory layout, `config.json` persistence, flock-based
  VM locking.
- Owns: all on-disk state (§4.2).
- Depends on: stdlib only.
- Public API (internal): `Store` (`New`, `NewAt`, path methods incl. `BinDir`,
  `NetworkStateDir`, `FixturePath`, `PidfilePath`, `SocketPath`, `ControlSocketPath`,
  `EnsureDir`, `Exists/Create/Save/Load/Delete/List`, `TryLock`), `VM` config struct
  (incl. `Fixtures`, `IP`), `Fixture{HostPath, GuestPath, Label}`, `Lock.Unlock`.
- Invariants:
  - Every path under `~/.fleetbox/` is produced by a `Store` method — no other package
    builds those paths by hand. `BinDir` (`~/.fleetbox/bin`) and `NetworkStateDir`
    (`~/.fleetbox/networks`, the Linux backend's write-ahead records — ADR-0013) are
    created on first use, not by `New`, so macOS installs grow neither.
  - Holder control sockets live in `~/.fleetbox/run/` under a hash of the name
    (`SocketPath` → `<hash>.sock`, `ControlSocketPath` → `<hash>.ctl`), NOT in the member
    dir: a unix socket path must fit the 104-byte sun_path limit, which the nested member
    dir blows past for long names (ADR-0017, amends ADR-0014). `run/` is created by `New`;
    `Delete` sweeps both sockets. The pidfile stays in the member dir.
  - A VM's member directory is `clusters/<cluster>/<member>/`, with `<cluster>` derived
    from the member name (`VMDir` → `clusterName`, never a stored field — ADR-0014). All
    member-dir creation funnels through `EnsureDir` (called by both `Create` and the
    holder's `register`). `List` walks the two-level tree and returns member names; every
    name it returns round-trips back to the same dir through `VMDir`. `Delete` removes the
    member dir, then `os.Remove`s the parent cluster dir (which refuses, harmlessly, while
    siblings remain) — never `os.RemoveAll`.
  - `config.json` is human-readable (indented JSON).
  - `VM.Fixtures` is the persisted source of truth for a VM's read-only payloads; each
    carries its `FBFIX<i>` volume label, computed once at create and never re-derived — the
    label is shared byte-for-byte between the image and the guest's `LABEL=` mount line
    (ADR-0015). `FixturePath(name, i)` gives the per-member image path
    (`…/<member>/fixture-<i>.img`). `fixtures` is omitted from `config.json` for a VM with
    none.
  - `VM.IP` is the persisted static address for static-IP backends (Linux), assigned
    once at create so reboots and re-joining members keep it; omitted on macOS (DHCP).
- Notes: `TryLock` (flock-based per-VM locking) is implemented and tested but **not
  yet wired into `fleetbox.Start`** — it was designed to guard concurrent starts of
  the same VM. Wiring it in is an open task; until then the §5.1 known gap stands.

### §5.9 `internal/dhcp`

- Purpose: `/var/db/dhcpd_leases` parsing — hostname → IP.
- Owns: stateless. **Build-tagged `darwin`** — only the vz backend uses it, and it must
  not compile into Linux builds.
- Depends on: stdlib only.
- Public API (internal): `LookupByHostname`, `ParseLeases`,
  `ParseLeasesFile`, `ParseLeasesData`, `Lease` struct.
- Invariants:
  - Read-only consumer of a macOS system file; never writes anything.
  - Imported only by `internal/backend/vz` (the IP-discovery logic moved there with
    `WaitForIP`, ADR-0011).

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

### §5.11 `internal/control`

- Purpose: the CLIENT half of the holder protocol (ADR-0017) — spawn a holder, wait for
  its members, query/stop them over the per-member sockets, and in bound mode hold the
  control connection whose EOF reaps the helper when the test process dies.
- Owns: stateless except the spawned `*exec.Cmd` + bind connection wrapped in a `Session`.
- Depends on: `internal/opts`, `internal/store` only — NO backend, NO orchestrator (the
  sever's backend-free client; the `backend-free-client` depguard rule enforces it).
- Public API (internal): `SpawnConfig{Exe, Names, Options, Bound}`, `Spawn → *Session`,
  `Session.Close`, `Status` struct, `IsRunning`, `GetStatus`, `Stop`, `AddMember`,
  `WaitMembers`; constants `RunnerFlag`, `EnvOpts`, `EnvParentPID`, `ProtocolVersion`,
  `Cmd*`, `State*`.
- Invariants:
  - Backend-free — links no hypervisor and no orchestrator, so the darwin client and CLI
    that import it stay pure Go (ADR-0017).
  - Two spawn modes share one wire protocol: detached (CLI, `Setsid`, persistent) and bound
    (library, attached + `FLEETBOX_PARENT_PID` + a `bind`/version/ack handshake on the
    holder-wide control socket whose later EOF triggers teardown). The death-watch arms only
    after the client's ack, so a mid-handshake close never causes a spurious teardown.
  - The bind handshake exchanges `ProtocolVersion`; a mismatch (stale `FLEETBOX_HELPER`) is
    fatal and the incompatible helper is killed.
  - Socket protocol is host-only plain commands (`status`/`stop`/`addmember`, plus bound
    `bind`) answering JSON/`ok` — no forwarding, no guest protocol (ADR-0006).
  - The one-time image/VMM pull is a distinct `downloading` state: `WaitMembers` does not
    spend the per-boot deadline while a member downloads (it prints `Pulling …`), and
    `GetStatus` reads the live holder socket *before* on-disk `config.json`, so a
    still-downloading member reports correctly (ADR-0013).
  - Options cross to the holder via `FLEETBOX_OPTS` (`opts.Encode`/`Decode`); fixtures ride
    along as host+guest path pairs (labels assigned at first-create, not serialized — ADR-0015).

### §5.12 `internal/backend/cloudhypervisor`

- Purpose: the cloud-hypervisor implementation of `backend.Backend` on Linux. Boots a
  stock cloud image with the pinned `rust-hypervisor-firmware`, controlled over CH's
  REST API on a per-VM unix socket — pure Go, no cgo (ADR-0011).
- Owns: the CH child process per VM and its `exited` channel; the `chNetwork` (Linux
  bridge, subnet, taps, egress rules); the per-bridge write-ahead records and
  `ip_forward` marker under `~/.fleetbox/networks/` (`netstate.go`); the pinned
  binary/firmware table; the process-wide reserved-subnet set. **Build-tagged `linux`.**
- Depends on: `internal/backend`, `internal/fetch`, stdlib (`os/exec`, `net/http` over a
  unix socket). No cgo, no third-party module.
- Public API (internal): `New(binDir, netDir) *Backend`; `Backend` (incl. `Reconcile`),
  `VM`, `chNetwork` satisfy the backend interfaces (`var _` checks present).
- Invariants:
  - **The only package that knows cloud-hypervisor specifics** (the CH analogue of the
    vz-isolation rule — ADR-0002, ADR-0011).
  - `NestedVirtSupported` probes `/dev/kvm` + the KVM `nested` parameter; `Create` opens
    `/dev/kvm`. Fixture images (`cfg.FixturePaths`) are appended as extra
    `path=…,readonly=on` values on the single `--disk` flag, after the seed — the guest
    mounts each by `LABEL`, so order is irrelevant (ADR-0015). `CreateNetwork`'s first `ip`
    call doubles as the `CAP_NET_ADMIN` probe.
  - `CreateNetwork` makes one bridge per cluster on a free `/24` (gateway `.1`) and
    installs `iptables` MASQUERADE/FORWARD egress rules; `Create` adds a tap enslaved to
    the bridge. `Network.Close` removes taps, egress rules, and the bridge — real OS
    resources, so teardown is explicit, not GC.
  - **Crash-safe lifecycle (ADR-0013, `netstate.go`):** every bridge/tap is mirrored to a
    write-ahead record (`<bridge>.json{bridge,subnet,owner_pid,masquerade,taps}`) written
    *before* the `ip` command and deleted *after* teardown is verified (`linkExists`), so
    the record is always a superset of reality. `Reconcile` (run at each `CreateNetwork`
    and on `down`) tears down every record whose `owner_pid` is dead — taps, rules,
    bridge, and any orphaned CH process still naming those taps — then deletes the record;
    a live owner is never touched.
  - **`ip_forward` is flipped only if it was `0`**, with the original kept in an `O_EXCL`
    marker and restored once no record and no `fbx-*` bridge remain (cross-process
    "last one out"). A host that already forwarded is never touched.
  - The VM gets its whole config on the CH command line (boots on launch) and is started
    with `Pdeathsig: SIGKILL`, so a dying holder takes its VMs with it (the holder pins
    its boot thread via `LockOSThread` so the signal fires only on real holder exit); the
    REST API is used for readiness (`vm.info`) and graceful shutdown (`vm.shutdown`), then
    SIGTERM/SIGKILL. `WaitForIP` returns the statically-assigned IP after a TCP:22 probe.
  - All cloud-hypervisor specifics are translated to `backend` types at this boundary;
    nothing CH-specific leaks upward.

### §5.13 `internal/fetch`

- Purpose: the shared download primitive — download → optional SHA256 verify → atomic
  rename → cache — behind both `internal/image` and the cloud-hypervisor backend
  (ADR-0011).
- Owns: stateless (writes into a caller-given cache dir).
- Depends on: stdlib only (`net/http`, `crypto/sha256`).
- Public API (internal): `Ensure(cacheDir, name, url, sha256) (string, error)`.
- Invariants:
  - A cached file is always complete: the download goes to a `.download` temp that is
    verified (when a digest is given) and atomically renamed; a mismatch or HTTP error
    leaves nothing behind. An empty digest skips verification (image "latest" entries);
    pinned callers (the CH binaries) always pass one.
  - A low-level utility imported by two building blocks — the one sanctioned exception to
    "building-block packages don't import each other" (coding-style B.1.2; recorded in
    ADR-0011). It imports nothing of ours.

### §5.14 `internal/fixture`

- Purpose: pack a host directory into a read-only ext4 image for attaching to a VM as a
  block device — the host→guest fixture payload (ADR-0015).
- Owns: stateless (writes one `.img` file per call).
- Depends on: `github.com/pilat/go-ext4fs`, stdlib.
- Public API (internal): `BuildImage(imgPath, srcDir, label string) error`.
- Invariants:
  - **The only package that imports `go-ext4fs`** — the seed ISO stays on cloudiso, so each
    image library has exactly one import site (the fixture/seed split).
  - Builds on a 16 GiB sparse canvas, then `Resize(MinSize())` to fit, so the on-disk image
    is a few MiB for a typical fixture and the canvas never materializes; a payload > 16 GiB
    is unsupported. No content-hash cache — the image is rebuilt on every boot (ADR-0015).
  - Every entry is world-readable: files `0444`, dirs `0555`, uid/gid `0`; symlinks are
    copied as symlinks; host permission/exec bits are not preserved. Arbitrary filenames
    (255-byte, spaces, unicode) and nesting are supported — the reason ext4 was chosen over
    ISO9660 (ADR-0015).

### §5.15 `internal/opts`

- Purpose: backend-neutral VM creation options + their serialization across the helper
  boundary — the pure-data leaf both the client (`control`) and server (`orchestrator`)
  depend on (ADR-0017).
- Owns: stateless.
- Depends on: stdlib only (`encoding/json`).
- Public API (internal): `Options{Image, CPUs, MemGB, DiskGB, Fixtures}`,
  `Fixture{HostPath, GuestPath}`, `Option func(*Options)`, `With*`, `Encode`/`Decode`.
- Invariants:
  - No backend, store, or orchestrator imports (the `backend-free-client` depguard rule).
    The root `fleetbox` package re-exposes these verbatim (`type Options = opts.Options`,
    thin `With*` wrappers) so its public signatures are byte-identical.
  - `Encode`/`Decode` round-trip applied option *values* (not the funcs) through
    `FLEETBOX_OPTS`; defaults are filled later in the orchestrator, not here.

### §5.16 `internal/orchestrator`

- Purpose: the VM-owning lifecycle extracted from the old root package — resolve deps,
  create the network, boot/wait/teardown VMs; the one package that wires in a backend
  (ADR-0017).
- Owns: the `VM`/`Cluster` runtime objects, the per-VM serial-log handle, the shared
  `backend.Network` reference (kept so the network is not GC'd while a member lives),
  compile-time backend selection (`newBackend`), and the per-platform `preflight` /
  `nestedVirtSupported`.
- Depends on: `internal/backend` (+ `internal/backend/vz` on darwin/arm64,
  `internal/backend/cloudhypervisor` on linux), `internal/{opts,image,seed,sshkey,fixture,store}`.
- Public API (internal): `Start`, `NewCluster`, `Prune`, `NestedVirtSupported`; `VM`
  (Name/IP/SSH/Stop/Destroy/State), `Cluster` (Add/VMs/Close/SupportsClustering).
- Invariants:
  - The only backend-linking package besides the backend impls; on darwin it is compiled
    only into `cmd/fleetbox-helper` (via `internal/holder`), never the root client.
  - The clustering gate and `ErrClustersUnsupported` live in the root package, not here:
    `Cluster.Add` boots unconditionally (the caller is expected to have gated). `Destroy`
    is idempotent (skips delete when the store entry is already gone).

### §5.17 `internal/holder`

- Purpose: the SERVER half of the holder protocol — one process owns an `up`/`Start` group
  via one `orchestrator.Cluster`, serving per-member sockets, in two lifetime modes
  (ADR-0017, ADR-0009).
- Owns: the holder process lifecycle, the per-member listener+handler goroutines, the
  mutex-protected member registry, and (bound mode) the parent-pid poll + control-socket
  watcher.
- Depends on: `internal/control`, `internal/opts`, `internal/orchestrator`, `internal/store`.
- Public API (internal): `IsRunner`, `GetRunnerVMNames`, `Run`, `WritePidfile`, `RemovePidfile`.
- Invariants:
  - Boots VMs through the orchestrator only — no backdoor into backends. `Run` pins its
    boot thread (`LockOSThread`) so the Linux `Pdeathsig` fires only on real holder exit;
    on exit it releases the network via a deferred `Cluster.Close()` after `stopAll`.
  - Bound mode (`FLEETBOX_PARENT_PID` set) reaps itself and its in-process VMs when the
    parent dies — control-connection EOF (fast) + reparent poll (backstop), the macOS
    no-PDEATHSIG analogue. Detached mode persists. On darwin it is compiled into
    `cmd/fleetbox-helper`; on linux the CLI re-execs into `Run`.

### §5.18 `internal/helperdist`

- Purpose: resolve the signed `fleetbox-helper` the darwin client drives — a pre-staged
  `FLEETBOX_HELPER`, or a checksum-pinned download into `~/.fleetbox/bin` (ADR-0017).
- Owns: stateless (writes into `BinDir`); the per-arch helper catalog.
- Depends on: `internal/fetch`, `internal/store`, `golang.org/x/sys/unix` (darwin quarantine
  xattr). NO backend/orchestrator (the `backend-free-client` depguard rule).
- Public API (internal): `Ensure(st) (string, error)`, `EnvHelper`.
- Invariants:
  - Refuses to run an unverified helper: an empty catalog URL or sha256 errors and points at
    `FLEETBOX_HELPER` (the entitlement makes an unverified download unacceptable — R5). The
    download is version-stamped (`fleetbox-helper-<ver>`) so the client runs the exact match.
  - Quarantine strip is `unix.Removexattr` (signature-safe — xattrs/mode bits are outside the
    mach-o signature), never a shell-out; then `chmod 0755`; the downloaded bytes are not
    modified (R8). No-op off macOS.

### §5.19 `cmd/fleetbox-helper`

- Purpose: the macOS VM host — the only binary that links Virtualization.framework and
  carries the entitlement; runs the holder server loop (ADR-0017).
- Owns: nothing of its own; `main` calls `holder.Run()`.
- Depends on: `internal/holder` (→ `internal/orchestrator` → `internal/backend/vz`).
  darwin/arm64 only; a `main_other.go` stub elsewhere keeps `go build ./...` resolving.
- Public API: none (package main).
- Invariants:
  - Ad-hoc-signed with `entitlements.plist` (`make helper`), downloaded + cached by the
    client, launched with `--fleetbox-runner` + `FLEETBOX_OPTS` (+ `FLEETBOX_PARENT_PID` in
    bound mode). The user's test/CLI binary never links it (ADR-0017).

### Dependency graph

The sever (ADR-0017) splits the import graph into a backend-free **client side** and a
backend-bearing **holder side**. On darwin they are separate binaries (the helper is the
only one linking vz); on linux they coexist in one process.

```
CLIENT SIDE (pure Go, no hypervisor — builds with CGO_ENABLED=0):

  cmd/fleetbox ──► fleetbox (root) ──► internal/opts            fleetboxtest ──► fleetbox
       │                 │ (darwin) ──► control, helperdist, sshkey, store
       │                 │ (linux)  ──► internal/orchestrator  ────────────────┐
       ├──► internal/control                                                    │
       ├──► internal/helperdist (darwin)   control/helperdist/opts ──► only     │
       └──► internal/holder (linux re-exec) ◄── store / fetch, never a backend  │
                                                                                │
HOLDER SIDE (links a backend):                                                  │
  cmd/fleetbox-helper (darwin) ──► internal/holder ──► internal/orchestrator ◄──┘
                                                            │ (+ backend, image, seed,
                                                            │    sshkey, fixture, opts, store)
                                  internal/backend ◄────────┤
                                       ▲   ◄─ backend/vz ──► third_party/vz (darwin, only site)
                                       └─  ◄─ backend/cloudhypervisor ──► fetch (linux, stdlib)
                                  image, fixture, helperdist ──► fetch
```

Edges (verified by `go list -deps`):

- `cmd/fleetbox` → `fleetbox`, `internal/control`, `internal/store`; + `internal/holder`
  (linux only) / + `internal/helperdist` (darwin only)
- `fleetboxtest` → `fleetbox` (public API only)
- `fleetbox` (root): neutral → `internal/opts`; linux → `internal/orchestrator`; darwin →
  `internal/control`, `internal/helperdist`, `internal/sshkey`, `internal/store` (NOT
  `internal/orchestrator`, NOT `internal/backend/vz` — the sever)
- `cmd/fleetbox-helper` (darwin/arm64) → `internal/holder`
- `internal/holder` → `internal/control`, `internal/opts`, `internal/orchestrator`, `internal/store`
- `internal/control`, `internal/helperdist`, `internal/opts` → at most
  `internal/{opts,store,fetch}` + `x/sys/unix`; never a backend or the orchestrator
  (depguard `backend-free-client`)
- `internal/orchestrator` → `internal/backend` (+ `internal/backend/vz` on darwin/arm64,
  + `internal/backend/cloudhypervisor` on linux), `internal/{opts,image,seed,sshkey,fixture,store}`
- `internal/backend/vz` (darwin/arm64) → `internal/backend`, `internal/dhcp`, the vendored
  `third_party/vz` (+ `vmnet`) — the only vz import site (depguard `vz-isolation`, ADR-0008)
- `internal/backend/cloudhypervisor` (linux) → `internal/backend`, `internal/fetch`, stdlib
  only (the only CH import site; no third-party module, no cgo)
- `internal/image` → `internal/fetch`, `go-qcow2reader`; `internal/fixture` → `go-ext4fs`
  (its only import site)
- The building-block→`fetch` edges (`image`, `fixture`, `helperdist`,
  `backend/cloudhypervisor`) are the sanctioned exception to B.1.2 (ADR-0011); `fetch`
  imports nothing of ours

**DEP-GRAPH GATE (the sever):** `GOOS=darwin GOARCH=arm64 go list -deps` of `fleetbox`,
`fleetboxtest`, and `cmd/fleetbox` must exclude `internal/backend/vz` and `third_party/vz`,
and all three must build with `CGO_ENABLED=0`. Only `cmd/fleetbox-helper` links vz on darwin.

`internal/holder` is architecturally a *consumer* of the orchestrator (like the CLI it
serves), despite living under `internal/` — internal only because it is not part of the
public contract.

## §6. Architectural invariants

Violations of these are bugs, not style issues (they restate the core principles from
CLAUDE.md as checkable rules):

1. **Library-first.** Every capability exists in the Go API; the CLI only wraps it.
   Check: `cmd/fleetbox` imports `fleetbox`/`internal/control`, and the holder
   (`internal/holder`) drives `internal/orchestrator` — never the reverse.
2. **Backend-neutral public API.** No hypervisor type appears in an exported signature.
   the vendored vz fork (`third_party/vz`) is imported only by `internal/backend/vz`
   (depguard rule `vz-isolation` in `.golangci.yml`; `make lint` fails on violation);
   cloud-hypervisor specifics live only in `internal/backend/cloudhypervisor` (a
   convention, mirroring vz-isolation). The backend-free client leaves
   (`internal/{opts,control,helperdist}`) may import neither a backend nor the orchestrator
   (depguard rule `backend-free-client`).
3. **Nothing of ours inside the guest.** The only artifact fleetbox produces for a
   guest is a cloud-init NoCloud seed ISO. No agent, no helper binary, no host↔guest
   protocol. Check: `internal/seed` writes user-data/meta-data only; no other package
   writes into guest-visible storage.
4. **No port forwarding.** VMs are reached by their own IP. Check: no listener/proxy
   code outside the holder's control sockets (`internal/holder`, host-only, not
   guest-related); SSH/cp dial the VM IP directly.
5. **No yaml, no templates, no per-distro code paths.** Check: no yaml parser in
   go.mod; `internal/image.Catalog` is a dumb map; `internal/seed` has a single code
   path.
6. **Clusters are a naming convention.** Check: no cluster *state file* anywhere. The
   `fleetbox.Cluster` type is an in-process runtime handle only — `StartN`/`StartCluster`
   produce `prefix-i`/named members sharing one network, and the holder keeps that network
   in memory (ADR-0009); nothing about a cluster is persisted to `~/.fleetbox/`
   (§5.1, §5.16, §5.17).
7. **Cattle with persistence.** `Start`/`up` boot existing VMs instead of failing;
   `Destroy`/`rm` is the only destructive operation. Check: nothing else calls
   `store.Delete`.
8. **Platform gating is compile-time.** Backend selection happens via build-tagged files
   in `internal/orchestrator` (`backend_darwin_arm64.go`, `backend_linux.go`,
   `backend_unsupported.go`), never via runtime config. (The *macOS version* branch within
   the vz backend is a runtime detail of one compile-time-selected backend, not backend
   selection — ADR-0012.)
9. **macOS sever: the user's binary is pure Go (ADR-0017).** The importable `fleetbox`
   package, `fleetboxtest`, and `cmd/fleetbox` link no hypervisor on darwin — only the
   downloaded `cmd/fleetbox-helper` does. Check: `GOOS=darwin GOARCH=arm64 go list -deps`
   of those three excludes `internal/backend/vz` and `third_party/vz`, and they build with
   `CGO_ENABLED=0`.

## §7. Known limitations (accepted for v0)

- **A holder crash takes its whole cluster down (but no longer leaks).** A CLI cluster's
  VMs share one holder process so they can share one network (ADR-0009). That trades the
  per-VM crash isolation ADR-0006 originally had: lose the holder, lose every member of
  that cluster. Single-VM `up` is unaffected (a cluster of one). We deliberately do not
  try to keep a cluster alive across a holder crash — but the crash is now clean: each VM
  carries a parent-death `SIGKILL`, so a SIGKILL'd/OOM'd holder takes its VMs with it
  (no zombie processes), and the inert leftovers (bridge, taps, iptables rules,
  `ip_forward`) are reclaimed automatically on the next `up` or `down` (ADR-0013). The
  deferred `Cluster.Close` still does the clean-exit/SIGTERM/panic teardown inline.
- **Linux reboot of a stopped VM depends on subnet stability.** The static IP and gateway
  are baked into the seed at first create; on a later `up`, `CreateNetwork` re-picks a
  free `/24` from scratch (the subnet is not persisted). On an idle host it picks the same
  `/24` and the VM is reachable; on a contended host (another cluster, Docker, a VPN
  holding that `/24`) it can shift, and the guest's baked gateway no longer matches the
  bridge — `WaitForIP` then times out. The primary fixture path (create + use + destroy
  within one process) is unaffected. Persisting/reusing the subnet is a follow-up.
- **CLI members of one cluster can't span separate processes.** If members were started
  by separate `up` commands (separate holders, separate networks), `up`-ing them
  together can't merge their networks; it reports this instead of silently producing a
  disconnected node (ADR-0009). Bring a cluster up together, or `rm` and retry.
- **IP discovery on macOS is hostname-based**, which assumes cloud-init sets the
  hostname and it is unique per VM. Both hold for fleetbox-created VMs (ADR-0007). On
  Linux the IP is allocated by fleetbox itself, so there is no discovery dance.
- **Platform matrix.** Apple Silicon macOS 26+ (clusters, vmnet SharedMode), Apple
  Silicon macOS < 26 (single VM, VZ NAT), Linux amd64/arm64 (clusters, cloud-hypervisor);
  Intel macOS unsupported. Nested virt needs M3+ on macOS or the host KVM `nested`
  parameter on Linux (ADR-0008, ADR-0011, ADR-0012).
- **Fixtures are read-only, not a live share.** `WithFixture` copies a host dir into the
  guest read-only (an ext4 payload, both platforms — ADR-0015); there is no live read-write
  folder share on either backend (cloud-hypervisor has no daemon-free one, so it was dropped
  on macOS too). The output direction is `fleetbox cp` / scp. Host permission and exec bits
  are not preserved — everything arrives world-readable, uid 0.
- **`fleetboxtest` fixtures skip on Linux.** They target darwin/arm64 for now; the
  package compiles everywhere but `skipIfUnsupported` skips non-darwin/arm64. Wiring
  Linux fixtures (and Linux KVM CI) is a follow-up.
- **macOS VM tests can't run on the macOS CI runner** (no nested virtualization). But the
  cloud-hypervisor backend *is* CI-testable on a GitHub x86-64 Linux runner, which exposes
  `/dev/kvm` after a one-time udev rule (arm64 hosted runners have no KVM) — the "develop on
  a Mac, run in cheap hosted Linux CI" path (ADR-0017). Not yet wired.
- **First run downloads, then caches.** The cloud image (both platforms) and, on macOS, the
  signed `fleetbox-helper` are fetched once into `~/.fleetbox` and reused. Until the release
  pipeline publishes the helper, build it locally (`make helper`) and set `FLEETBOX_HELPER`
  (also the offline override, ADR-0017). In CI, cache `~/.fleetbox/{bin,images}`.

## §8. Keeping This Document Accurate

After implementation changes, verify:

- **Module list**: packages in §5 == `go list ./...`. A new/removed/renamed package
  requires a §5 section update.
- **Public API**: exported symbols in `fleetbox.go` and `fleetboxtest/` match §5.1 /
  §5.2. Quick check: `go doc github.com/pilat/fleetbox | grep '^func\|^type\|^const'`.
- **CLI surface**: commands in `cmd/fleetbox/main.go`'s dispatch switch match §5.3 and
  the `usage()` text.
- **Backend contract**: `internal/backend/backend.go` interfaces match §5.4.
- **State layout**: path methods in `internal/store/store.go` match the §4.2 tree.
- **Sever gate (macOS)**: `GOOS=darwin GOARCH=arm64 go list -deps` of `fleetbox`,
  `fleetboxtest`, and `cmd/fleetbox` excludes `internal/backend/vz` and `third_party/vz`,
  and the three build with `CGO_ENABLED=0` (§6.9, ADR-0017).
- **Dependencies**: direct requires in `go.mod` match the deps named in §5 module
  sections (currently: pilat/cloudiso, pilat/go-ext4fs, go-qcow2reader, x/crypto, and
  x/sys — now a direct dep, used by `internal/helperdist` (quarantine xattr) and the darwin
  capability probes, as well as by the in-module vendored vz; plus go-infinity-channel +
  x/mod pulled in by vz). The vz fork itself is not a require — it is vendored in-module (see below).
  The vz fork is the import-path-renamed Code-Hex/vz + norio-nomura's unreleased
  vmnet patch (PR #205), regenerated from pinned sources by `make vendor-vz`. It is
  vendored in-module under `third_party/vz` — no separate go.mod, so a downstream
  `go get` builds it like our own package — with every file constrained to
  `//go:build darwin`, so non-darwin `go build ./...` skips it (ADR-0008).
  `third_party/` is excluded from lint; provenance and the regen recipe are in
  `third_party/vz/NOTICE` and `hack/vendor-vz.sh`.
- **Invariants**: `make lint` passes (depguard enforces invariant #2); the rest of §6
  is spot-checked by reading the named files.
- **New design decisions**: anything decided in a spec under `ai/tasks/` that changed
  the architecture must land here (what) and in `docs/adr/` (why) in the same PR —
  `ai/tasks/` is gitignored and does not travel with the repo.

Run `/pilat:arch-sync` to check automatically.
