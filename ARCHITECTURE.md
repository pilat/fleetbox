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
  process. The orchestration runs client-side and drives a VM helper that holds only the
  live VMs/network (ADR-0020); the helper is bound to the test process. On macOS the helper
  is a downloaded, signed `fleetbox-helper` subprocess, so the test binary stays pure Go (no
  cgo, no codesign) (ADR-0017); on Linux the test binary self-reexecs into the helper. Either
  way the helper plus its VMs die when the test exits.
- **CLI mode** — `fleetbox up/down/ls/ssh/cp/ssh-config/rm/version` for manual work. The CLI runs a
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
| **Holder** | The backend-server process that owns an `up`/`Start` group (one VM or a whole cluster): it holds the live network + VMs and answers `createnetwork`/`reserve`/`boot-member`/`status`/`stop` over per-member unix sockets (NDJSON). On macOS it is the downloaded `fleetbox-helper`; on Linux it is the `fleetbox`/test binary self-reexec'd via an `init()` interceptor (ADR-0020). Library mode spawns it *bound* (it dies with the test); CLI mode spawns it *detached* (it persists). |
| **Helper** | The holder process (the terms are used interchangeably). On macOS it is `cmd/fleetbox-helper`: the ad-hoc-signed binary that links Virtualization.framework, downloaded + cached on first use (`internal/helperdist`), so the user's test/CLI binary stays pure Go and unsigned (ADR-0017). On Linux there is no separate binary — the client self-reexecs into the holder, linking cloud-hypervisor (ADR-0020). Either way the helper holds only the live VMs/network; image/disk/seed/fixture/orchestration run client-side. |
| **Store** | The `~/.fleetbox/` directory layout and its `config.json` files. The only persistent state fleetbox has. |
| **Cluster** | VMs sharing one network, named by convention (`prefix-1`, `prefix-2`, ...). The `fleetbox.Cluster` type is an in-process runtime handle for membership/health. The cluster is *also* a storage grouping — members nest under `~/.fleetbox/clusters/<cluster>/` (the cluster name derived from the member name) — but there is no cluster *object* with persisted membership; disk grouping and runtime grouping need not be 1:1 (ADR-0014, see §4.2). |

## §3. Source-of-Truth Map

Where the canonical version of each thing lives. When two files disagree, the SoT wins.

| Concept | Source of truth | Notes |
|---------|----------------|-------|
| Public library API | `fleetbox.go` | Exported symbols + doc comments. §5.1 summarizes. |
| Test-fixture API | `fleetboxtest/fleetboxtest.go` | §5.2 summarizes. |
| CLI command surface | `cmd/fleetbox/` cobra tree (`newRootCmd` in `root.go` + one `newXxxCmd` per command file) | §5.3 summarizes (ADR-0022). |
| Backend contract | `internal/backend/backend.go` | `Backend`, `VM`, `Network`, `Config`, `State`. |
| On-disk state layout | `internal/store/store.go` path methods | §4.2 summarizes. |
| Image catalog | `internal/image/catalog.json` (embedded) + `internal/image/image.go` | Alias → dated snapshot + per-GOARCH URL + sha256 (pinned, verified); refreshed by `contrib/catalog`. |
| Pinned VMM binaries | `internal/backend/cloudhypervisor/binaries.go` | cloud-hypervisor + firmware: version + per-arch URL + sha256. |
| macOS helper catalog | `internal/helperdist/helperdist.go` `catalog` map | fleetbox-helper: version + per-arch URL + sha256; `FLEETBOX_HELPER` override (ADR-0017). |
| Holder protocol | `internal/control/control.go` (client) + `internal/holder/holder.go` (server) | Wire commands + states + bound-mode bind/version handshake. |
| Coordination test seam | `internal/backend/fake` + the `fleetbox_fake` build tag | Fake backend so CI gates teardown + the holder protocol with no VM boot; `make test-fake`/`make lint-fake` (ADR-0018). |
| Network teardown records & reconcile | `internal/backend/cloudhypervisor/netstate.go` | Write-ahead bridge/tap/uplink records + per-uplink forwarding marker under `~/.fleetbox/networks/`; crash recovery (ADR-0013, ADR-0025). |
| Guest provisioning contract | `internal/seed/seed.go` (user-data / meta-data) | One user, one SSH key, hostname, fixture mount lines. Nothing else. |
| Fixture payload format | `internal/fixture/fixture.go` | Host dir → read-only ext4 image (go-ext4fs), attached read-only, mounted by LABEL (ADR-0015). |
| Code style rules | `docs/coding-style.md` + `.golangci.yml` | Prescriptive. Lint enforces the machine-checkable subset. |
| Architecture (current state) | `ARCHITECTURE.md` (this file) | Descriptive. |
| Design decisions & rationale | `docs/adr/` | One file per decision, sequentially numbered. |
| Build & signing recipe | `Makefile` + `entitlements.plist` | `com.apple.security.virtualization` entitlement. |
| Vendored vz provenance & regen | `third_party/vz/NOTICE` + `hack/vendor-vz.sh` | Pinned upstream + vmnet-patch SHAs; `make vendor-vz` regenerates (ADR-0008, ADR-0016). |
| CI behavior | `.github/workflows/ci.yml` | macOS job: lint + linux/darwin build + unit + fake coordination (no VZ boot). Linux job: build + unit + fake coordination via self-reexec. |
| Linux VM-boot CI | `.github/workflows/vm-linux.yml` | Runs the full capability-driven suite (conformance + cluster) on an x86-64 KVM runner. |
| Release pipelines | `.github/workflows/{release-helper,release}.yml` + `.goreleaser.yaml` | Two channels: helper (`helper-v*`, macOS, codesign) and CLI (`v*`, goreleaser; also pushes a macOS Homebrew cask to the `pilat/homebrew-fleetbox` tap, ADR-0021). |
| Working specs (local only) | `ai/tasks/` | Gitignored. Durable decisions must graduate to ADRs. |

## §4. System Model

### §4.1 VM lifecycle

`fleetbox.Start(ctx, name, opts...)` is the single public entry point for creating/booting
a VM. The orchestration lives in `internal/orchestrator` and runs **client-side on both
platforms** (ADR-0020): the root package delegates to it; it does the pure-Go prep (image,
disk, seed, fixture, store) and drives the helper over the control protocol via the
remote-proxy backend (`internal/backend/remote`). fleetboxtest and the CLI both reach this
path. The sequence:

0. **Preflight** — `preflight()` (per-platform) fails fast with an actionable message if
   the host can't run a VM, before the (possibly multi-GB) image download. No-op on macOS;
   on Linux it checks `/dev/kvm` is openable and the process is root (`euid==0`). Root, not
   `CAP_NET_ADMIN`, is the honest gate — the backend programs netlink + nf_tables (which
   need `CAP_NET_ADMIN`) and writes DAC-gated per-interface forwarding under `/proc`, so
   only root reliably works; the CLI auto-elevates before this runs (ADR-0023, ADR-0025).
1. **Options** — apply functional options over defaults (image=debian-12, cpus=2,
   mem=4GB, disk=20GB).
2. **Store** — `store.New()` ensures `~/.fleetbox/{clusters,images}/` exist.
3. **SSH key** — `sshkey.EnsureKey()` generates the per-installation ed25519 keypair on
   first use (`~/.fleetbox/id_ed25519[.pub]`).
4. **Image** — `image.Ensure()` returns a cached raw image, downloading / verifying /
   converting qcow2→raw if needed.
5. **Spawn helper + create network** — `spawnHelper()` (per-platform helper acquisition,
   §4.5) launches the holder and returns a remote-proxy backend. `CreateNetwork()` (an RPC)
   makes the shared network in the helper (vmnet SharedMode on macOS 26+, a Linux bridge on
   Linux; a single `Start` gets a one-member network, a `StartN` cluster shares one — §4.3)
   and returns its subnet.
6. **Reserve + VM config** — per member, `Network.Reserve(name, ipHint)` (an RPC) allocates
   the address **helper-side** and returns `{ip, mac}` (the helper-side successor to the old
   client `allocateIP` — ADR-0020). If `store.Exists(name)`: load `config.json` and boot from
   it (**all options are ignored for an existing VM**; the stored IP rides along as the
   `ipHint` so the member keeps its address). Otherwise: create the config (MAC from the
   reservation = `backend.GenerateMAC(name)`; `WithFixture` payloads validated, absolutized,
   labeled `FBFIX<i>`), copy the cached image to `disk.raw`, and generate `seed.iso` via
   `seed.Create` — on Linux baking the reserved IP + derived gateway/netmask into a NoCloud
   `network-config`; the DHCP backend (vz) returns an empty subnet and the guest stays on
   DHCP. On **every** boot each fixture's read-only ext4 image is rebuilt from its persisted
   host dir (`internal/fixture`, no cache) and re-attached read-only (ADR-0015).
7. **Boot** — `boot-member` (an RPC carrying the resolved spec — ready disk/seed/fixture
   paths, cpu/mem, serial-log path) tells the helper to `backend.Create` + `Start` the VM on
   the shared network using the address it reserved (VZ: EFI bootloader, vmnet/NAT NIC,
   virtio disk + seed ISO, serial → `serial.log` opened by the helper; CH: a tap on the
   bridge + the launch command line). It is synchronous: the helper boots the VM and waits
   for its IP (`WaitForIP` — vz parses `/var/db/dhcpd_leases` by hostname per ADR-0007; CH
   returns its static IP after a TCP :22 probe) before replying. The client reads the IP via
   `status`.
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
│   ├── <hash>.sock        #   per-member control socket (status/stop; the primary also carries createnetwork/reserve/boot-member)
│   └── <hash>.ctl         #   bound-mode holder-wide socket (bind handshake + EOF teardown)
├── images/                # downloaded + converted raw cloud images (cache)
├── bin/                   # downloaded checksum-pinned binaries: cloud-hypervisor + firmware (Linux), fleetbox-helper-<ver> (macOS)
├── networks/              # Linux: per-bridge write-ahead records (<bridge>.json) + per-uplink forwarding markers (fwd-<iface>.orig) (ADR-0013, ADR-0025)
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
  on a free `/24` (gateway `.1`) via netlink, enables **per-interface** IPv4 forwarding on
  the bridge and the discovered uplink (never the global switch; restoring the uplink's
  flag once nothing of ours remains — ADR-0025), and installs an `nf_tables` table with a
  masquerade rule plus a self-protecting forward-filter drop for egress. Each VM gets a
  **tap** enslaved to the bridge (`--net tap=…`); its static IP is allocated **helper-side**
  by `Network.Reserve` (which
  honors a stopped member's stored IP as a hint) and the client injects the returned IP +
  derived gateway/netmask via the seed's NoCloud `network-config` (ADR-0020). Members on one
  bridge reach the host, each other, and the internet — the SharedMode property reproduced on
  Linux (ADR-0011). The bridge is a real OS resource owned by the helper, torn down when the
  helper exits (its deferred network close; or reconciled on the next `up` after a crash —
  ADR-0013), not by GC.
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
- SSH: library mode uses `golang.org/x/crypto/ssh` programmatically; the CLI `ssh`
  execs the system `ssh` binary for a proper interactive terminal. File copy (library
  `VM.CopyTo`/`CopyFrom` and the CLI `cp`) is tar over the same `golang.org/x/crypto/ssh`
  connection — no `scp` shell-out (ADR-0026).

### §4.4 Process model

A **holder** (a.k.a. **helper**) is the backend-server process that owns an `up`/`Start`
group — a single VM or an interconnected cluster. It holds the shared network (a vmnet
network on macOS, or the Linux bridge + cloud-hypervisor children) and its member VMs, and
**nothing else**: image resolve, disk/seed/fixture build, the store, and orchestration all
run **client-side**, driving the holder over the control protocol (ADR-0020). Where the
holder binary comes from differs by platform (ADR-0017/0020):

| | Library (test process) | CLI |
|---|---|---|
| **Linux** | self-reexec of the test binary (bound) | self-reexec of the `fleetbox` binary (detached) |
| **macOS** | downloaded `fleetbox-helper` (bound) | downloaded `fleetbox-helper` (detached) |

On **Linux** nothing needs signing and cloud-hypervisor is the downloaded VMM, so the client
self-reexecs `os.Executable()` with a hidden `--fleetbox-runner <name,...>` flag; a package
`init()` interceptor in `internal/holder` catches that flag (and `--fleetbox-reconcile`)
before the test framework or CLI `main()` runs, and runs the holder. On **macOS** all
Virtualization.framework work is severed into `cmd/fleetbox-helper` — the only binary that
links vz and carries the entitlement — so the test/CLI binary stays pure Go; the client
downloads the helper (`internal/helperdist`), spawns it, and drives it over the socket
protocol.

The holder server loop (`internal/holder`) is identical in both lifetime modes; the client
half (`internal/control`) chooses the mode:

- **Bound** (library): the helper is spawned *attached* (no `Setsid`) with the client PID
  in `FLEETBOX_PARENT_PID`, and the client holds one long-lived control connection (a
  `bind` handshake on the holder-wide `<hash>.ctl` socket, which also exchanges a protocol
  version so a stale `FLEETBOX_HELPER` is rejected). When the test process dies — even on
  `kill -9` — the helper reaps itself and its VMs, via the control connection's EOF (fast
  path) and a reparent poll (`os.Getppid()` change, the backstop). Helper exit ⇒ VMs gone.
- **Detached** (CLI): the helper is a session leader (`Setsid`), no parent watch — its VMs
  persist past the command (cattle-with-persistence).

The holder registers each spawn-name member (ensures its dir, writes its `pid`, listens on
its `<hash>.sock`), arms the death-watch, then **serves** — it boots nothing on its own. The
client drives it over NDJSON: `createnetwork` (make the shared network, return the subnet),
`reserve` (allocate a member's IP/MAC on it), `boot-member` (a resolved spec →
`backend.Create`+`Start`, held on the shared network), `status`, `stop`. On `stop` it
gracefully shuts down that one member and retires its socket+pidfile, exiting when its last
member is gone or on SIGTERM; on exit it releases the shared network (a no-op for vmnet, the
explicit Linux bridge/egress teardown otherwise; on Linux it also reconciles crash orphans
on start). Its *entire* job is holding VMs/network and answering the socket: no images, no
disk/seed building, no forwarding, no guest protocol (ADR-0006/0020).

SSH and `cp` never go through the holder: the client dials the VM's directly-routable IP
itself (the protocol only reports the IP). Because per-name sockets and pidfiles are
unchanged, every per-name command (`ls`/`ssh`/`down`/`rm`) addresses a member without
knowing whether it shares a process with siblings. `up` of a member whose siblings already
run reserves + boot-members it on a live sibling's holder, so the node re-joins the existing
network instead of getting an isolated one (ADR-0009/0020). A holder crash takes its whole
cluster down — the accepted cost of one-process network sharing.

### §4.5 Platform & build constraints

The support matrix and how it is realized in build tags:

| Platform | Backend | Networking | Clusters |
|----------|---------|------------|----------|
| macOS Apple Silicon, ≥ 26 | VZ | vmnet SharedMode | yes |
| macOS Apple Silicon, < 26 | VZ | VZ NAT (single, isolated VM) | no — `ErrClustersUnsupported` |
| macOS Intel | — | — | unsupported (clear runtime error) |
| Linux amd64 / arm64 | cloud-hypervisor | shared bridge + tap | yes |

- **The module compiles on `darwin/arm64`, `linux/amd64`, and `linux/arm64`.** The
  **concrete backend** is selected at compile time in the **helper** (ADR-0020): four
  build-tagged files in `internal/holder` — `backend_darwin_arm64.go` (→ vz),
  `backend_linux.go` (→ cloud-hypervisor), `backend_fake.go` (→ the fake under
  `-tags fleetbox_fake`), `backend_unsupported.go` (a clear error) — each defining
  `newRealBackend(*store.Store) (backend.Backend, error)`. The **client** orchestrator does
  NOT select a backend; its `internal/orchestrator` build-tagged files provide only
  `helperExe` (helper acquisition: `helperdist` on darwin, `os.Executable` self-reexec on
  linux, error otherwise) and `preflight` (`preflight_linux.go` checks /dev/kvm +
  `euid==0`/root, ADR-0023; `preflight_default.go`/`preflight_fake.go` are no-ops). The root package is
  build-tagged too (ADR-0020): `fleetbox_supported.go` (`darwin||linux`) holds the one
  client impl that delegates to the orchestrator; `fleetbox_{darwin_arm64,linux,unsupported}.go`
  hold only per-platform host probes (`fleetbox_linux.go` also blank-imports `internal/holder`
  to link its self-reexec `init()`); `VM`/`Cluster` are defined once in the neutral
  `fleetbox.go` over a build-tagged impl. `internal/backend/vz` is tagged `darwin && arm64`
  (linked only by `internal/holder` there); `internal/backend/cloudhypervisor` is tagged
  `linux`; `internal/dhcp` is tagged `darwin`. `cmd/fleetbox-helper` is darwin/arm64 (a stub
  elsewhere). `cmd/fleetbox` links the orchestrator (client) and, on linux, the holder (via
  the root's blank import) — but never a hypervisor on darwin. `fleetboxtest` compiles
  everywhere; its VM-boot tests run wherever the host can boot a VM (Linux via `/dev/kvm`,
  darwin/arm64 via vz — `SkipIfCannotBootVM`) and skip otherwise.
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
  `FLEETBOX_HELPER`. The published helper is `helper-v0.2.0` (the ADR-0020 protocol bump;
  the old `helper-v0.1.0` speaks the v1 protocol and is rejected at the handshake),
  auto-downloaded and checksum-pinned via the `internal/helperdist` catalog;
  `FLEETBOX_HELPER` overrides it for dev/offline.
- **Linux host prerequisites** (not provisionable; probed with clear errors): `/dev/kvm`
  present and accessible (user in the `kvm` group) and **root** — the bridge/taps and the
  nft firewall need `CAP_NET_ADMIN` (root has it), plus `CAP_DAC_OVERRIDE` to write the
  per-interface forwarding sysctls under `/proc`, so the preflight gates on `euid==0`, not
  on the capability. The CLI auto-elevates via sudo;
  library/tests run under sudo (ADR-0023). The cloud-hypervisor binary and firmware are
  downloaded and checksum-pinned to `~/.fleetbox/bin/` (ADR-0011); the Linux path is pure
  Go, no cgo.
- **Nested virtualization** (consumers running KVM inside guests) needs M3+ on macOS,
  or the host KVM `nested` parameter on Linux. `fleetbox.NestedVirtSupported()` reports
  availability. The fixtures gate VM-boot tests on boot-capability, NOT nested virt
  (`fleetboxtest.SkipIfCannotBootVM`: `/dev/kvm` openable on Linux, `NestedVirtSupported()`
  on darwin) — so a leaf VM boots on an arm64 Linux guest where nested virt is unavailable.
- **CI.** `ci.yml` has two jobs. The **macOS** (`macos-26`) job cannot boot VZ VMs (no
  nested virtualization): it runs lint (darwin/linux/fake) + darwin build + linux cross-build
  + unit + the **fake-backend coordination tests** (`make test-fake`). The **Linux**
  (`ubuntu`) job runs build + unit + the fake coordination via self-reexec
  (`make test-fake-linux`, no KVM needed). The fake backend lets the whole cross-process
  client↔helper path — teardown and the holder protocol — gate pre-merge with no VM boot and
  no codesign, the one VM-free check of the bound-helper reap (ADR-0018/0020); everything runs
  under `-race`. Unlike VZ, the cloud-hypervisor backend *is* CI-testable on a GitHub x86-64
  Linux runner, which exposes `/dev/kvm` (after a one-time udev rule): `vm-linux.yml` boots
  real VMs there — the full capability-driven suite (conformance + a multi-node cluster, so
  amd64 gets VM↔VM coverage; the nested dogfood self-skips, needing an M3+ macOS host) over
  the full client→RPC→helper path (the test binary is its own helper via self-reexec). arm64
  hosted Linux runners have no KVM. Do not switch the macOS CI to ubuntu.

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

- Purpose: the public library API — everything a consumer can do with a VM. The
  orchestration lives in `internal/orchestrator` and runs client-side on both platforms
  (ADR-0020); this package is a thin public surface over it.
- Owns: the public `VM`/`Cluster` types, defined once (neutral `fleetbox.go`) over a
  build-tagged unexported impl (`vmState`/`clusterState`, satisfied by `orchestrator.VM` and
  a `clientCluster` wrapper); the clustering gate and `ErrClustersUnsupported`; the per-platform
  pure-Go capability probes (`nestedVirtSupported`/`supportsClusteringHost`/`prune`). It owns
  no backend objects directly.
- Depends on:
  - neutral (`fleetbox.go`): `internal/opts` (re-exported option types).
  - supported (`fleetbox_supported.go`, `darwin||linux`): `internal/orchestrator`.
  - darwin probes (`fleetbox_darwin_arm64.go`): `golang.org/x/sys/unix` only.
  - linux probes (`fleetbox_linux.go`): `internal/orchestrator` + a blank import of
    `internal/holder` (to link the self-reexec `init()`).
  - NOT `internal/backend/vz` or `third_party/vz` on darwin — the sever, verified by
    `go list -deps` (now image/seed/fixture/orchestrator ARE in the darwin client; vz is not).
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
  - `Prune() error` — reclaim the inert host resources (Linux bridges, taps, nft firewall
    tables) a crashed holder left, and restore the uplink forwarding flag; no-op on macOS.
    Runs
    automatically on the CLI `down`, so cleanup is never the user's job (crashed VMs
    themselves die with their holder — `Pdeathsig` on Linux, the parent-pid poll on macOS,
    ADR-0013/ADR-0017); exported for library callers that want to sweep explicitly
  - `type VM`: `Name()`, `IP() net.IP`, `SSH(ctx, cmd) (string, error)`,
    `CopyTo(ctx, hostPath, guestPath) error`, `CopyFrom(ctx, guestPath, hostPath) error`,
    `Stop(ctx)`, `Destroy(ctx)`, `State() string` — copy is universal (file or directory,
    both directions), exact-destination, modes-preserved/ownership-not, tar over the SSH
    connection (ADR-0026)
  - `type Options{Image, CPUs, MemGB, DiskGB, Fixtures}`, `type Option func(*Options)`,
    `WithImage`, `WithCPUs`, `WithMemoryGB`, `WithDiskGB`, `WithFixture(hostDir, guestPath)`
  - `type Fixture{HostPath, GuestPath}` — a read-only host directory packed into the guest
    at boot as an ext4 payload (ADR-0015)
  - image aliases are plain strings resolved against the embedded catalog (e.g.
    `"debian-12"`); there are no exported alias constants — `catalog.json` is the sole
    list of which aliases exist (ADR-0019)
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
  - Helper ownership/teardown: a bare `Start` marks its VM `ownsSession`, so `Stop`/`Destroy`
    reap the helper (stopping the sole member makes it exit); cluster members share the
    session and leave reaping to `Cluster.Close` (or GC of the last reference). The helper, on
    exit, releases the shared network — a no-op for vmnet (GC), the explicit teardown for the
    Linux bridge (ADR-0020).
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
  `StartN(t, prefix, n, opts...) []*fleetbox.VM`, `SkipIfShort(t, reason)`,
  `SkipIfCannotBootVM(t)`, `BootTimeout(n int) time.Duration`.
- Invariants:
  - Uses only the public `fleetbox` API.
  - Every VM it creates is destroyed via `t.Cleanup` — test VMs never outlive tests.
  - **Capability-driven, no selectors.** Tests skip (never fail) on a host that cannot
    boot a VM via `SkipIfCannotBootVM` (`/dev/kvm` openable on Linux; vz M3+/macOS 26 via
    `NestedVirtSupported()` on darwin) and on `-short` via `SkipIfShort`. `StartN` turns the
    public `ErrClustersUnsupported` sentinel into `t.Skip`, so the cluster test self-skips on
    macOS < 26. No `-run` filters or build tags select tests.
  - Boot budgets come from `BootTimeout(n)`, which honors `FLEETBOX_IP_WAIT_TIMEOUT`
    (else `n*5min`) — one knob widens the holder's IP-wait and the fixture context together.
  - VM names are derived from test names (`safeName`) so parallel tests don't collide.
  - **Nested dogfood (`nested_test.go`, `TestNestedLinuxBoot`).** The fleetbox-tests-fleetbox
    loop: on M3+ macOS it boots an outer Linux guest with nested `/dev/kvm`, cross-builds the
    linux/arm64 `fleetboxtest` binary, and runs this SAME unified suite inside the guest on
    cloud-hypervisor via the arm64 direct-kernel path (ADR-0024); inner pass/fail is the inner
    binary's exit code. Runtime-gated (darwin/arm64 + `NestedVirtSupported()` + `!-short` +
    `FLEETBOX_HELPER` set) — no build tag. LOCAL-ONLY: no CI lane (the runner needs an M3+
    macOS host). Reached via `make test-vm`.

### §5.3 `cmd/fleetbox`

- Purpose: the CLI — `up`, `down`, `ls`, `ssh`, `cp`, `ssh-config`, `rm`, `version`, plus
  cobra's auto-generated `completion` and `help`. Aliases: `start`→`up`, `stop`/`halt`→`down`,
  `list`→`ls`, `shell`→`ssh`, `remove`/`destroy`/`delete`→`rm`.
- Owns: a cobra command tree, terminal output, exec of system `ssh` (for a proper
  interactive terminal). `cp` no longer execs `scp` — it uses the library copy primitive
  (`sshkey.Client.CopyTo`/`CopyFrom`, tar over SSH — ADR-0026). It is built on
  **cobra** (ADR-0022): `newRootCmd` in `root.go` assembles one `newXxxCmd` per command file
  (`up.go`, `down.go`, …); there is no `init()` and no package-level command globals (only the
  ldflags-set build-metadata `version/commit/date`). Each command's `RunE` calls a `runX`
  helper that opens its own `store.New()`, so store-free commands (`version`, `completion`,
  `--help`) never fail on a store error. A single `cliExit{code, silent}` error type drives the
  process exit code from `main` (used by ssh/cp exit-code propagation and the best-effort bulk
  loops). It no longer carries a holder seam: on Linux the re-exec into the holder is handled by
  `internal/holder`'s `init()` interceptor (linked via the root package), not a `maybeRunHolder`
  dispatch here (ADR-0020).
- Depends on: `fleetbox` (public API), `internal/orchestrator` (to drive a detached helper
  for `up`), `internal/control` (status/stop for the per-name commands), `internal/store`
  (incl. `ClusterName` for `down`/`rm` target resolution), `internal/sshkey` (the copy
  primitive for `cp`), and `github.com/spf13/cobra`
  (+ `pflag`). It links no hypervisor on darwin (ADR-0017); on Linux it links the holder +
  cloud-hypervisor via the root's blank import (the accepted non-sever).
- Public API: none (package main).
- Invariants:
  - The CLI adds no capability of its own — every VM operation goes through the public API
    or the same holder protocol the library uses (ADR-0001).
  - VM lifecycle in CLI mode is always delegated to a detached helper
    (`orchestrator.StartClusterDetached`); the CLI process itself never holds a VM. On macOS
    that helper is the downloaded `fleetbox-helper`; on Linux it is the re-exec'd CLI (via the
    `internal/holder` `init()` interceptor — ADR-0020).
  - `up` boots a single VM (`up name`) or an interconnected cluster (`up prefix -n N`,
    or `up a b c`) — the whole group runs in one helper sharing one network (ADR-0009).
    `up` partitions members into running/missing: none running → fresh detached helper; some
    running in one helper → `orchestrator.AddMember` (reserve + boot-member on a live
    sibling's holder) so a re-upped node re-joins the live network; running members split
    across processes → rejected (their networks can't merge). cobra parses flags and positional
    names interspersed (`up test1 -n 2` works). `up` warns (stderr) when a create-only flag is
    set on an already-existing member (options are frozen at create — ADR-0015).
  - `down` and `rm` resolve targets through one shared resolver: an exact VM name, else a
    cluster prefix expanded via `store.ClusterName` (the `-<digits>` rule — so `web` hits
    `web-1`/`web-2` but never an unrelated solo `web-prod`). Both are best-effort across
    multiple targets (per-target result lines, non-zero exit if any failed/unknown). `rm` is
    the only destructive command and confirms any non-empty removal unless `-f`/`--force`.
  - `ssh` requires `--` before a remote command (`ssh web -- uname -a`); trailing args without
    it are rejected, not silently dropped. `ssh` propagates the child `ssh` exit code
    verbatim. `cp` rejects VM↔VM copies (exactly one side is `name:/path`); it dials the VM
    via the copy primitive (no child process) and keeps scp's "copy into a directory"
    convenience for a local destination above the exact-path library method (ADR-0026).
  - `ls` renders a human table (default), bare names (`-q`), or a JSON array (`-o json`, the
    pinned snake_case machine contract — `internal/store`-consistent keys, no `age` field).
    `ssh`/`cp`/`down` dynamically complete running VM names, `rm` all VM names
    (`ValidArgsFunction`); the `completion` subcommand's per-shell scripts ship via the
    Homebrew cask (ADR-0021/0022).
  - `up` accepts a repeatable `--fixture host:guest` flag (`StringArrayVar`); host paths are
    resolved to absolute against the CLI cwd before they cross into the holder (split on the
    last colon; the guest path must be absolute), and flow to the library as `WithFixture`
    (ADR-0015). In a cluster every member gets the same fixtures.
  - Cleanup is automatic, never a user command: `down` (like `up`) runs the backend
    reconcile via `fleetbox.Prune()` to reclaim resources a crashed holder left, so on
    Linux it too needs root for the netlink/nf_tables calls (ADR-0013, ADR-0025).
  - **Linux auto-elevation (CLI-only — ADR-0023).** The privileged commands `up`/`down`/`rm`
    call `ensurePrivileged()` first (an allowlist in each `RunE`, never a root
    `PersistentPreRunE` — that would fire on `__complete`/`help`). Interactive (a `/dev/tty`
    opens) → re-exec `sudo env FLEETBOX_ELEVATED=1 PATH=… <abs self> <args>` via
    `syscall.Exec` (absolute self-path fixes `sudo: command not found`; the `env` prefix sets
    the loop-guard flag and PATH without relying on `sudo -E`). Non-interactive → print the
    exact command and exit non-zero (silent `cliExit`), never hang. The user-level commands
    (`ls`/`ssh`/`cp`/`ssh-config`/`version`/`completion`) never elevate. The pure decision is
    `decideElevation` in `elevate.go` (table-tested); the shell is `elevate_linux.go`, a
    `!linux` no-op stub is `elevate_other.go` (macOS uses the signed helper, never sudo). The
    **library never elevates** — this lives only here.
  - No yaml, no config files — flags and defaults only.

### §5.4 `internal/backend`

- Purpose: the hypervisor-neutral contract every backend implements.
- Owns: stateless (interface + enum + MAC derivation).
- Depends on: stdlib only.
- Public API (internal): `Backend{CreateNetwork, Create, NestedVirtSupported,
  SupportsClustering, Reconcile}`, `Network{Close, Subnet, Reserve}` (opaque handle — no
  hypervisor types; `Subnet` returns the CIDR for static-IP backends, "" for DHCP backends;
  `Reserve(name, ipHint) (ip, mac, err)` allocates a member's address helper-side, the
  successor to the orchestrator's old `allocateIP` — ADR-0020), `VM{Start, Stop, State, Wait,
  WaitForIP}`, `Config{Name, DiskPath, SeedPath, EFIPath, MAC, CPUs, MemoryBytes,
  SerialLogPath, FixturePaths, AssignedIP}` — `SerialLogPath` is a host file the backend opens
  itself (a path, not an `io.Writer`, so it crosses the helper boundary — ADR-0020);
  `FixturePaths` are host paths of pre-built read-only ext4 fixture images to attach
  (backend-neutral, no SDK types, no guest path), `State` enum + `String()`,
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
- Depends on: `go-qcow2reader`, `internal/fetch`, stdlib (incl. `embed`).
- Public API (internal): the `ImageInfo`/`ArchImage` types (also imported by
  `contrib/catalog`), `Ensure(cacheDir, urlOrAlias) (string, error)`,
  `CopyDisk(src, dst, sizeBytes)`. The catalog itself is an embedded JSON data file
  (`catalog.json`, `//go:embed`), parsed once via `loadCatalog()` (sync.Once →
  wrapped error on malformed JSON); there is no exported `Catalog` var (ADR-0019).
- Invariants:
  - One code path for all images — adding a distro is adding a `catalog.json` key,
    never new code (ADR-0003). The catalog resolves an alias to the per-`runtime.GOARCH`
    entry, so the same alias works on macOS arm64 and Linux amd64/arm64.
  - Catalog images are **pinned and verified**: each alias pins a dated upstream
    snapshot + per-arch SHA256, and the snapshot is stamped into the cache filename
    for both the downloaded source and the converted raw (`<alias>-<snapshot>-<arch>.raw`)
    — a snapshot bump is a guaranteed cache miss + re-download, never a stale warm hit
    (ADR-0019). Only a literal `WithImage(url)` stays unverified (basename-derived
    cache name). The `contrib/catalog` tool refreshes the snapshot/URL/sha values; the
    human keys decide which OSes exist.
  - Cached images are immutable; per-VM disks are copies.

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
  `EnsureDir`, `Exists/Create/Save/Load/Delete/List`, `TryLock`), `ClusterName` (the
  documented `-<digits>` member→cluster rule, exported so the CLI's `down`/`rm` resolution
  reuses it), `VM` config struct (incl. `Fixtures`, `IP`),
  `Fixture{HostPath, GuestPath, Label}`, `Lock.Unlock`.
- Invariants:
  - **One base dir, the invoking user's home (ADR-0023).** `New` resolves the base via the
    pure `resolveBaseHome`: when root-via-sudo (`euid==0 && SUDO_USER != ""`) it uses
    `SUDO_USER`'s passwd home (`os/user.Lookup`), NOT `$HOME` — a manual `sudo fleetbox`
    leaves `HOME=/root` while `SUDO_USER=alice`, so trusting `$HOME` would split state into
    `/root/.fleetbox`. Every other case (non-root, no `SUDO_USER`, or a failed/empty lookup)
    falls back to `os.UserHomeDir()` and never errors. This is the single place the rule
    lives, so CLI, orchestrator, and holder all agree on `~alice/.fleetbox`.
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

- Purpose: per-installation ed25519 keypair + programmatic SSH client (command run +
  file copy).
- Owns: `~/.fleetbox/id_ed25519[.pub]` (via path given by store).
- Depends on: `golang.org/x/crypto/ssh` + stdlib (`archive/tar` for copy). No new module.
- Public API (internal): `Manager` (`NewManager`, `EnsureKey`, `PrivateKey`, `Path`,
  `Dial`, `DialIP`, `WaitForSSH`), `Client` (`Run`, `CopyTo`, `CopyFrom`, `Close`).
- Invariants:
  - One keypair per installation, generated lazily, injected into guests via cloud-init
    only.
  - Host key checking is intentionally disabled (ephemeral test VMs).
  - **Copy is tar over the SSH session (ADR-0026).** `CopyTo`/`CopyFrom` build/extract a
    tar in-process (stdlib `archive/tar`) and pipe it through a guest `tar -x`/`tar -c`,
    so copy is universal (file or directory), exact-destination, modes-preserved but
    ownership-not (`-p --no-same-owner`), streamed via `io.Pipe` (no full-payload buffer),
    and adds zero dependencies. The in-process extractor refuses entries that escape the
    destination (absolute names, `..`, escaping symlink targets, and entries routed
    through a symlinked parent component planted earlier in the archive). The only guest
    requirement is `tar`.
  - **Ownership fixup (ADR-0023).** `EnsureKey` chowns the key pair to `SUDO_UID:SUDO_GID`
    on every run (idempotent — `chownToInvoker`) when root-via-sudo, so a non-root `ssh -i`
    can read the `0600` private key a root `up` created. Mode stays `0600` (`ssh` refuses a
    world-readable key, so ownership is the only fix); only the two key files are touched,
    never the base dir. No-op off-root and on macOS.

### §5.11 `internal/control`

- Purpose: the CLIENT half of the holder protocol (ADR-0017) — spawn a holder, wait for
  its members, query/stop them over the per-member sockets, and in bound mode hold the
  control connection whose EOF reaps the helper when the test process dies.
- Owns: stateless except the spawned `*exec.Cmd` + bind connection wrapped in a `Session`.
- Depends on: `internal/store` only — NO backend, NO orchestrator, NO opts (the sever's
  backend-free client; the `backend-free-client` depguard rule enforces it).
- Public API (internal): `SpawnConfig{Exe, Names, Bound}`, `Spawn → *Session`,
  `Session.Close`, `Status` struct, `IsRunning`, `GetStatus`, `Stop`, `SendCommand`,
  `WaitMembers`; the NDJSON wire types `Request`, `Response`, `MemberSpec`, `Reservation` +
  `WriteMessage`/`ReadMessage` (`wire.go`); constants `RunnerFlag`, `ReconcileFlag`,
  `EnvParentPID`, `ProtocolVersion` (= "2"), `Cmd*`, `State*`.
- Invariants:
  - Backend-free — links no hypervisor and no orchestrator, so the client and CLI that
    import it stay pure Go (ADR-0017).
  - Two spawn modes share one wire protocol: detached (CLI, `Setsid`, persistent) and bound
    (library, attached + `FLEETBOX_PARENT_PID` + a `bind`/version/ack handshake on the
    holder-wide control socket whose later EOF triggers teardown). The death-watch arms only
    after the client's ack, so a mid-handshake close never causes a spurious teardown.
  - The bind handshake exchanges `ProtocolVersion`; a mismatch (stale `FLEETBOX_HELPER`, or
    the old helper-v0.1.0 speaking protocol "1") is fatal and the incompatible helper is killed.
  - The command socket is **newline-delimited JSON** (ADR-0020): `Request{cmd,...}` →
    `Response{...}`, one per connection. Commands: `status`/`stop` (per-member socket) and
    `createnetwork`/`reserve`/`boot-member` (cluster-level, routed through the primary
    member's socket). `bind`/`ack` stay a raw-text handshake on the `.ctl` socket. No
    forwarding, no guest protocol (ADR-0006). `SendCommand` retries the dial briefly so the
    first RPC does not race a detached helper's socket setup.
  - `GetStatus` reads the live holder socket *before* on-disk `config.json`, so a
    still-starting member reports correctly. The image/VMM pull is the client's job before
    spawn now; `StateDownloading` covers only the helper's VMM-binary fetch (ADR-0020).
  - **Cross-uid liveness (ADR-0023).** `IsRunning` probes the holder with `kill(pid, 0)`
    and treats `EPERM` as ALIVE, not dead (`signalMeansAlive`): on Linux the holder an
    elevated `up` spawned is root-owned, so a non-root `ls`/`ssh` cannot signal it and gets
    `EPERM` though the process exists. Only `ESRCH` (absent) is "not running". Without this,
    `GetStatus` short-circuited to `StateStopped` before ever dialing the (0666) socket, so
    non-root read-only commands reported a running VM as stopped.

### §5.12 `internal/backend/cloudhypervisor`

- Purpose: the cloud-hypervisor implementation of `backend.Backend` on Linux. Boots a
  stock cloud image — via the pinned `rust-hypervisor-firmware` on x86_64, or a direct
  kernel boot on arm64 (ADR-0024) — controlled over CH's REST API on a per-VM unix socket
  — pure Go, no cgo (ADR-0011).
- Owns: the CH child process per VM and its `exited` channel; the `chNetwork` (Linux
  bridge, subnet, taps, uplink, nft firewall); the per-bridge write-ahead records and
  per-uplink forwarding marker under `~/.fleetbox/networks/` (`netstate.go`); the pinned
  binary/firmware table; the process-wide reserved-subnet set. **Build-tagged `linux`**
  (except the portable `purehelpers.go` — table-name mapping, nf_tables errno
  classification, uplink-name selection — which is untagged so it is unit-testable on the
  darwin dev box).
- Depends on: `internal/backend`, `internal/fetch`, `github.com/vishvananda/netlink` +
  `github.com/google/nftables` (host networking — netlink and nf_tables over netlink),
  `github.com/diskfs/go-diskfs` (arm64 only — the in-process kernel/initrd read for direct
  boot, surgical `backend`+`backend/file`+`partition/gpt`+`filesystem/ext4`; ADR-0027),
  `golang.org/x/sys/unix`, stdlib (`net/http` over a unix socket). Pure Go, no cgo, no
  host binary (ADR-0025, ADR-0027).
- Public API (internal): `New(binDir, netDir) *Backend`; `Backend` (incl. `Reconcile`),
  `VM`, `chNetwork` satisfy the backend interfaces (`var _` checks present).
- Invariants:
  - **The only package that knows cloud-hypervisor specifics** (the CH analogue of the
    vz-isolation rule — ADR-0002, ADR-0011).
  - `NestedVirtSupported` probes `/dev/kvm` + the KVM `nested` parameter; `Create` opens
    `/dev/kvm`. Fixture images (`cfg.FixturePaths`) are appended as extra
    `path=…,readonly=on` values on the single `--disk` flag, after the seed — the guest
    mounts each by `LABEL`, so order is irrelevant (ADR-0015). `CreateNetwork`'s first
    netlink write (the bridge `LinkAdd`) is the backstop permission check
    (`errors.Is(err, EPERM)` → `create bridge (needs root)` if a non-root holder ever
    reaches it; the preflight already gated on root — ADR-0023), and an nf_tables probe
    runs right after, discriminating "needs root" from "kernel lacks nf_tables" (ADR-0025).
  - **Arch-specific boot (ADR-0024, `boot_{amd64,arm64}.go`).** x86_64 boots via the PVH
    `rust-hypervisor-firmware` (`--kernel <fw>`), which chain-loads the guest kernel from
    the disk. arm64 boots the kernel **directly** — the aarch64 firmware does not execute
    the guest under Apple-Silicon nested virt and is untested on bare metal — extracting
    the image's own `vmlinuz`/`initrd` from `disk.raw` once (an **in-process pure-Go read**
    of the raw image via go-diskfs — `backend/file` → `partition/gpt` → `filesystem/ext4`
    (+ `backend` for the storage type), read-only, no loopback mount, no shell-out, no root;
    gunzip if needed; cached next to the disk) and passing `--kernel`/`--initramfs`/`--cmdline
    "console=ttyAMA0 root=/dev/vda1 rw"`. The extracted kernel is the image's at first
    boot (a later in-guest kernel update needs `rm`+`up`). The search/copy seam is the
    untagged `bootextract.go` (unit-tested on darwin, like `purehelpers.go`); the go-diskfs
    wiring is in `boot_arm64.go` (ADR-0024, ADR-0027).
  - `CreateNetwork` makes one bridge per cluster on a free `/24` (gateway `.1`) via netlink
    and installs an `ip`-family nft table (`nftTableName(bridge)`) holding a NAT-postrouting
    masquerade rule plus a filter-forward chain (policy accept) carrying one subnet-scoped
    drop of unsolicited inbound — the self-protecting filter (ADR-0025);
    `Network.Reserve(name, ipHint)` allocates a member's static IP on that `/24` (lowest
    free, or the hint if free) — the helper-side successor to the orchestrator's old client
    `allocateIP` (ADR-0020); `Create` adds a tap enslaved to the bridge. `Network.Close`
    removes taps, the nft table, and the bridge — real OS resources owned by the helper, so
    teardown is explicit, not GC.
  - **Crash-safe lifecycle (ADR-0013, ADR-0025, `netstate.go`):** every bridge/tap/uplink is
    mirrored to a write-ahead record
    (`<bridge>.json{bridge,subnet,owner_pid,uplink,uplink_fwd_orig,taps}`) written *before*
    the netlink/nft call and deleted *after* teardown is verified (`linkExists`), so the
    record is always a superset of reality. `Reconcile` (run at each `CreateNetwork` and on
    `down`) tears down every record whose `owner_pid` is dead — taps, the nft table (deleted
    whole by name), bridge, and any orphaned CH process still naming those taps — then
    deletes the record; a live owner is never touched.
  - **Per-interface forwarding, never the global switch (ADR-0025).** Forwarding is enabled
    on the bridge and the discovered uplink (`conf.<iface>.forwarding` under `/proc`); the
    global `ip_forward` is never written. On a host already forwarding globally nothing is
    flipped. The uplink's original is kept in a first-writer-wins per-uplink marker
    (`fwd-<iface>.orig`) and restored once no record and no `fbx-*` bridge remain
    (cross-process "last one out"); the bridge's flag is ephemeral (dies with the bridge).
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
    leaves nothing behind. An empty digest skips verification — now only literal
    `WithImage(url)` images pass one (the CH binaries and catalog images are all pinned;
    ADR-0019). `fetch` stays sha256-only; the Debian-image sha512 cross-check lives in
    `contrib/catalog`, not here.
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

- Purpose: backend-neutral VM creation options — the pure-data leaf the client packages
  depend on. Since ADR-0020 the orchestrator consumes `Options` directly client-side; they no
  longer cross a process boundary as JSON, so the old `Encode`/`Decode` codec is gone.
- Owns: stateless.
- Depends on: stdlib only.
- Public API (internal): `Options{Image, CPUs, MemGB, DiskGB, Fixtures}`,
  `Fixture{HostPath, GuestPath}`, `Option func(*Options)`, `With*`.
- Invariants:
  - No backend, store, or orchestrator imports (the `backend-free-client` depguard rule).
    The root `fleetbox` package re-exposes these verbatim (`type Options = opts.Options`,
    thin `With*` wrappers) so its public signatures are byte-identical.

### §5.16 `internal/orchestrator`

- Purpose: the **client-side** VM lifecycle sequencer (ADR-0020) — resolve deps, spawn the
  helper, then create the network and boot/wait/teardown members by RPC. It links NO concrete
  backend; its backend is the pure-Go remote proxy.
- Owns: the `VM`/`Cluster` runtime objects, the owning `control.Session` (held for bound
  reaping), helper acquisition + preflight (`helperExe` / `preflight`, per platform/tag), and
  the `skipSSHWait` build-tag seam (`sshwait.go` / `sshwait_fake.go`).
- Depends on: `internal/backend` + `internal/backend/remote` (the proxy), `internal/control`,
  `internal/{opts,image,seed,sshkey,fixture,store}`, and `internal/helperdist` (darwin
  `helperExe`). NEVER `internal/backend/vz` or `/cloudhypervisor` (the
  `orchestration-no-concrete-backend` depguard rule).
- Public API (internal): `Start`, `NewCluster`, `NewClusterDetached`, `StartClusterDetached`,
  `AddMember`, `Prune`; `VM` (Name/IP/SSH/Stop/Destroy/State), `Cluster` (Add/VMs/Close).
- Invariants:
  - Links no concrete backend on either platform — it drives the helper via the remote proxy
    (the macOS sever, now uniform; ADR-0020). `NestedVirtSupported`/`SupportsClustering` are
    NOT here — the root package answers host capability without spawning the helper (R7).
  - The clustering gate and `ErrClustersUnsupported` live in the root package, not here:
    `Cluster.Add` boots unconditionally (the caller is expected to have gated). `Destroy`
    is idempotent (skips delete when the store entry is already gone).
  - `StartClusterDetached`'s rollback is disk-safe: each VM carries `createdThisCall`
    (`= !st.Exists(name)` at build time), and on a partial-up failure `Cluster.rollback`
    Destroys only members this call created while merely Stopping pre-existing ones — so a
    re-up of stopped members never deletes a persisted disk. The library's bound
    `StartCluster` (root) keeps its destroy-all rollback on purpose: its fixtures are
    ephemeral within a test.
  - Reserve precedes the seed build: `Network.Reserve(name, ipHint)` (helper-side) returns the
    IP/MAC the client bakes into the seed; the previously-stored IP rides along as the hint so
    a stopped member keeps its address (ADR-0020).
  - Under `-tags fleetbox_fake`, `preflight_fake.go` skips the /dev/kvm+root check
    and `skipSSHWait` → true (the fake's IP is unroutable). The concrete backend selection is
    the helper's job (`internal/holder`), not here, so the orchestrator has no `newBackend`.

### §5.17 `internal/holder`

- Purpose: the SERVER half — a thin **backend-server** process that owns an `up`/`Start`
  group, holding the live network + VMs and serving createnetwork/reserve/boot-member/
  status/stop, in two lifetime modes. The only place a concrete backend is linked (ADR-0020,
  ADR-0009).
- Owns: the holder process lifecycle, the real backend (`newRealBackend`, per platform/tag),
  the shared network + per-member reservations, the per-member listener/handler goroutines,
  the mutex-protected member registry, (bound) the parent-pid poll + control-socket watcher,
  and the linux self-reexec `init()` interceptor (`reexec_linux.go`).
- Depends on: `internal/backend` + a concrete backend via `newRealBackend`
  (`internal/backend/{vz,cloudhypervisor,fake}`), `internal/control`, `internal/store`. NOT
  `internal/orchestrator` (ADR-0020 inverted that dependency).
- Public API (internal): `IsRunner`, `GetRunnerVMNames`, `IsReconcile`, `RunReconcile`, `Run`,
  `WritePidfile`, `RemovePidfile`.
- Invariants:
  - Calls the backend directly — no images/disks/seed/fixtures, no orchestrator. `Run` pins
    its thread (`LockOSThread`) so the Linux `Pdeathsig` fires only on real holder exit;
    reconciles host orphans on start; on exit releases the network after `stopAll`.
  - Boot is **client-driven**: it registers spawn-name members + arms the death-watch, then
    serves; `boot-member` does `backend.Create`+`Start` synchronously (waiting for the IP).
    A duplicate boot-member for an already-running member is refused (no counter underflow);
    a member stopped during boot has its just-started VM reaped (no orphan).
  - Bound mode (`FLEETBOX_PARENT_PID` set) reaps itself and its VMs when the parent dies —
    control-connection EOF (fast) + reparent poll (backstop). Detached mode persists. On
    darwin it is `cmd/fleetbox-helper`; on linux the client/test binary self-reexecs into it
    via the `init()` interceptor.
  - **Socket fixup (ADR-0023).** Each unix socket the holder creates (per-member and the
    bound control socket) is `chmod 0666`ed when running as root, so a non-root client can
    `connect()` (a unix-socket connect needs *write* permission; a root umask leaves it
    `0755` → EACCES). `0666` on a local-only dev socket is an accepted tradeoff. The holder
    inherits `SUDO_*` because `control.Spawn` passes `os.Environ()`, so the store resolves
    the same `~user/.fleetbox` the elevated client did.

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
  carries the entitlement; runs the holder backend-server (ADR-0017/0020).
- Owns: nothing of its own; `main` calls `holder.Run()` (or `holder.RunReconcile()` when
  launched with `--fleetbox-reconcile`).
- Depends on: `internal/holder` (→ `internal/backend/vz`). darwin/arm64 only; a
  `main_other.go` stub elsewhere keeps `go build ./...` resolving. It does NOT link the
  orchestrator/image/seed/fixture — those are client-side (the ADR-0020 catalog-out-of-helper
  payoff).
- Public API: none (package main).
- Invariants:
  - Ad-hoc-signed with `entitlements.plist` (`make helper`), downloaded + cached by the
    client (`helper-v0.2.0`), launched with `--fleetbox-runner` (or `--fleetbox-reconcile`)
    + `FLEETBOX_PARENT_PID` in bound mode. The user's test/CLI binary never links it (ADR-0017).

### §5.20 `internal/backend/fake`

- Purpose: a dumb, instant, pure-Go implementation of the backend interfaces used to
  exercise the cross-process client↔helper coordination layer in CI with no VM boot and no
  codesign (ADR-0018/0020). **Test-only:** linked only by the **helper's**
  `internal/holder/backend_fake.go` selector under the `fleetbox_fake` build tag, never by a
  normal `go build ./...`. The fake now runs in the helper's address space (ADR-0020).
- Owns: a per-VM IP counter (mutex-guarded) and the record file. Because the fake runs in the
  helper (a separate process from the test), it exposes observable effects via the protocol,
  on-disk store artifacts, and an append-only JSON **record file** named by
  `FLEETBOX_FAKE_RECORD` (one line per reserve/create/stop/close) — not in-process globals.
- Depends on: `internal/backend`, stdlib only.
- Public API (internal): `New() *Backend` (satisfies `backend.Backend`; its `VM`/`Network`
  satisfy the rest); constants `EnvFailCreate` (`FLEETBOX_FAKE_FAIL_CREATE`, cross-process
  fault injection) and `EnvFakeRecord` (`FLEETBOX_FAKE_RECORD`, the cross-process observation
  channel).
- Invariants:
  - Never linked into a no-tag binary (the carrier `backend_fake.go` is `//go:build
    fleetbox_fake`); a `go list -deps` of the real artifacts without the tag must not contain it.
  - Trivial by design — `Start`/`Stop` are no-ops, `WaitForIP` returns an unroutable
    TEST-NET-3 address, `Subnet` is "" (the DHCP path, so the Linux static-IP allocation is
    NOT exercised by the fake — that stays covered by real cloud-hypervisor in `vm-linux.yml`).
    It proves *coordination*, not that a VM boots; tests assert via the protocol, on-disk
    artifacts, and the record file, never a value the fake itself invents (ADR-0018/0020).

### §5.21 `contrib/catalog`

- Purpose: the build-time refresher for the pinned image catalog
  (`internal/image/catalog.json`) — re-resolves each alias to the newest upstream snapshot
  and its per-arch URL + SHA256 (ADR-0019). **Not linked into any runtime binary;** run via
  `make catalog` and the monthly `catalog-refresh.yml`.
- Owns: nothing of its own; rewrites `catalog.json` in place after resolving every entry.
- Depends on: `internal/image` (the `ImageInfo`/`ArchImage` types — single source of truth
  for the JSON shape), stdlib only (`net/http`, `crypto/sha256`+`sha512`, `encoding/json`).
- Public API: none (package main).
- Invariants:
  - The human-authored keys (`distro`/`version`/`codename`) decide which OSes exist; the tool
    only refreshes the values (snapshot, URLs, sha256, `bumped_at`). Per-distro resolvers live
    here — allowed because this is `contrib/`, not the runtime library.
  - Resolves all entries before writing anything (a single failure aborts with no partial
    write); `bumped_at` advances only on a real change, so a no-op run is byte-identical
    (idempotent). Debian publishes only SHA512SUMS, so each image is stream-hashed (sha256 +
    sha512 cross-check) without being persisted; Ubuntu's sha256 is read from SHA256SUMS.

### §5.22 `internal/backend/remote`

- Purpose: a pure-Go `backend.Backend` that drives a spawned helper over the control
  protocol instead of touching a hypervisor — the client half of the ADR-0020 inversion. The
  orchestrator links THIS, so its import graph carries no vz/cloud-hypervisor.
- Owns: stateless (a store + the primary member name per cluster).
- Depends on: `internal/backend`, `internal/control`, `internal/store`. NO hypervisor.
- Public API (internal): `New(st, primary) *Backend`; `Backend`/`Network`/`VM` satisfy the
  backend interfaces by RPC — `CreateNetwork`→createnetwork, `Network.Reserve`→reserve,
  `VM.Start`→boot-member, `VM.WaitForIP`/`State`/`Wait`→status polling, `VM.Stop`→stop.
- Invariants:
  - Links no hypervisor (the sever, now uniform across platforms — ADR-0020).
  - Cluster-level RPCs (createnetwork/reserve/boot-member) go over the primary member's
    socket; per-member status/stop over the target's own socket. `NestedVirtSupported`/
    `SupportsClustering` are not routed here — the client answers host capability without the
    helper (R7), so they return safe defaults that the orchestrator never consults.

### Dependency graph

The sever (ADR-0017, generalized by ADR-0020) splits the import graph into a backend-free
**client side** (orchestrator + remote proxy + image/seed/fixture) and a backend-bearing
**holder side** (the real vz/CH/fake backend). On darwin they are separate binaries (the
helper is the only one linking vz); on linux they coexist in one self-reexec'd binary.

```
CLIENT SIDE (no hypervisor; on darwin pure Go, builds with CGO_ENABLED=0):

  cmd/fleetbox ─┐                                   fleetboxtest ──► fleetbox
  fleetboxtest ─┼─► fleetbox (root) ──► internal/orchestrator (client sequencer)
                │        │                    │ ──► internal/backend/remote ──► control ──► (RPC)
                │        │ (probes) ──► x/sys/unix (darwin) / kvm probe (linux)
                │        └ (linux) ──► internal/holder (blank import: self-reexec init())
                │                            └────────────────────────────────────┐
        orchestrator ──► image, seed, fixture, sshkey, store, opts, helperdist     │
                                                                                   │
HOLDER SIDE (links a backend):                                                     │
  cmd/fleetbox-helper (darwin) ─┐                                                  │
  self-reexec'd binary (linux) ─┴─► internal/holder ──► internal/backend ◄─ newRealBackend
                                          │   ◄─ backend/vz ──► third_party/vz (darwin, only site)
                                          │   ◄─ backend/cloudhypervisor ──► fetch (linux)
                                          └   ◄─ backend/fake (─tags fleetbox_fake)
```

Edges (verified by `go list -deps`):

- `cmd/fleetbox` → `fleetbox`, `internal/orchestrator`, `internal/control`, `internal/store`,
  `github.com/spf13/cobra` (+ `pflag`); on linux it transitively links `internal/holder` +
  cloud-hypervisor (via the root's blank import); on darwin it links NO hypervisor (the sever)
- `fleetboxtest` → `fleetbox` (public API only)
- `fleetbox` (root): neutral → `internal/opts`; supported (`darwin||linux`) →
  `internal/orchestrator`; linux probes → blank `internal/holder`; darwin probes →
  `x/sys/unix`. NOT `internal/backend/vz` / `third_party/vz` on darwin — the sever (now WITH
  image/seed/fixture/orchestrator in the darwin client; ADR-0020)
- `cmd/fleetbox-helper` (darwin/arm64) → `internal/holder` (→ `internal/backend/vz`), NOT the
  orchestrator/image/seed/fixture
- `internal/holder` → `internal/control`, `internal/store`, `internal/backend` + a concrete
  backend via `newRealBackend` (vz/cloudhypervisor/fake). NOT `internal/orchestrator`
- `internal/orchestrator`, `internal/backend/remote` → `internal/backend` + (orchestrator
  only) `internal/{control,opts,image,seed,sshkey,fixture,store,helperdist}`; NEVER a concrete
  backend (depguard `orchestration-no-concrete-backend`)
- `internal/control`, `internal/helperdist`, `internal/opts` → at most
  `internal/{store,fetch}` + `x/sys/unix`; never a backend or the orchestrator
  (depguard `backend-free-client`)
- `internal/backend/vz` (darwin/arm64) → `internal/backend`, `internal/dhcp`, the vendored
  `third_party/vz` (+ `vmnet`) — the only vz import site (depguard `vz-isolation`, ADR-0008)
- `internal/backend/cloudhypervisor` (linux) → `internal/backend`, `internal/fetch`,
  `github.com/vishvananda/netlink` + `github.com/google/nftables` (host networking),
  `github.com/diskfs/go-diskfs` (arm64 only — the in-process kernel/initrd read, the only
  go-diskfs import site), `golang.org/x/sys/unix`, stdlib (the only CH import site; pure Go,
  no cgo — ADR-0025, ADR-0027)
- `internal/image` → `internal/fetch`, `go-qcow2reader`, stdlib `embed` (the catalog);
  `internal/fixture` → `go-ext4fs` (its only import site — the fixture **writer**; reads use
  go-diskfs in the CH backend, ADR-0027)
- `contrib/catalog` (build-time tool, not in any runtime binary) → `internal/image`
  (for the `ImageInfo`/`ArchImage` types), stdlib only; run via `make catalog` and the
  monthly `catalog-refresh.yml` (ADR-0019)
- The building-block→`fetch` edges (`image`, `fixture`, `helperdist`,
  `backend/cloudhypervisor`) are the sanctioned exception to B.1.2 (ADR-0011); `fetch`
  imports nothing of ours

**DEP-GRAPH GATE (the sever):** `GOOS=darwin GOARCH=arm64 go list -deps` of `fleetbox`,
`fleetboxtest`, and `cmd/fleetbox` must exclude `internal/backend/vz` and `third_party/vz`
(and now INCLUDE `internal/{image,seed,fixture,orchestrator}`), and all three must build with
`CGO_ENABLED=0`. Only `cmd/fleetbox-helper` links vz on darwin, and it must EXCLUDE
`internal/{image,seed,fixture,orchestrator}` (the catalog-out-of-helper payoff, ADR-0020).

`internal/holder` is architecturally the *server* the client orchestrator drives by RPC
(ADR-0020 inverted the old "holder consumes orchestrator" relationship); it lives under
`internal/` because it is not part of the public contract.

## §6. Architectural invariants

Violations of these are bugs, not style issues (they restate the core principles from
CLAUDE.md as checkable rules):

1. **Library-first.** Every capability exists in the Go API; the CLI only wraps it.
   Check: `cmd/fleetbox` imports `fleetbox`/`internal/orchestrator`; the orchestrator (the
   client) drives the holder by RPC via `internal/backend/remote`, and the holder links no
   orchestrator — the dependency runs client → holder, never the reverse (ADR-0020).
2. **Backend-neutral public API.** No hypervisor type appears in an exported signature.
   The vendored vz fork (`third_party/vz`) is imported only by `internal/backend/vz`
   (depguard rule `vz-isolation`; `make lint` fails on violation); cloud-hypervisor specifics
   live only in `internal/backend/cloudhypervisor`. The backend-free client leaves
   (`internal/{opts,control,helperdist}`) import neither a backend nor the orchestrator
   (depguard `backend-free-client`); the client orchestrator + remote proxy import the
   backend INTERFACE but never a concrete vz/cloud-hypervisor backend (depguard
   `orchestration-no-concrete-backend`, ADR-0020).
3. **Nothing of ours inside the guest.** The only artifact fleetbox produces for a
   guest is a cloud-init NoCloud seed ISO. No agent, no helper binary, no host↔guest
   protocol. Check: `internal/seed` writes user-data/meta-data only; no other package
   writes into guest-visible storage.
4. **No port forwarding.** VMs are reached by their own IP. Check: no listener/proxy
   code outside the holder's control sockets (`internal/holder`, host-only, not
   guest-related); SSH/cp dial the VM IP directly.
5. **No yaml, no templates, no per-distro code paths.** Check: no yaml parser in
   go.mod; the image catalog is a dumb alias→data map (`internal/image/catalog.json`,
   embedded JSON); `internal/seed` has a single code path. *Carve (ADR-0019):* the
   principle forbids **user-side** config. `catalog.json` is internal data compiled into
   the binary that the user never edits — the same role the Go map played, in a form a
   bot (`contrib/catalog`) can rewrite safely. Per-distro logic exists only in
   `contrib/catalog` (a build-time tool), never in the runtime library.
6. **Clusters are a naming convention.** Check: no cluster *state file* anywhere. The
   `fleetbox.Cluster` type is an in-process runtime handle only — `StartN`/`StartCluster`
   produce `prefix-i`/named members sharing one network, and the holder keeps that network
   in memory (ADR-0009); nothing about a cluster is persisted to `~/.fleetbox/`
   (§5.1, §5.16, §5.17).
7. **Cattle with persistence.** `Start`/`up` boot existing VMs instead of failing;
   `Destroy`/`rm` is the only destructive operation. Check: nothing else calls
   `store.Delete`.
8. **Platform gating is compile-time.** The concrete backend is selected via build-tagged
   files in `internal/holder` (`backend_{darwin_arm64,linux,unsupported}.go` + a test-only
   `backend_fake.go` under the `fleetbox_fake` tag — still compile-time, never via runtime
   config or an env-selected backend, ADR-0018/0020). The client orchestrator selects only
   the remote proxy + per-platform `helperExe`/`preflight`; it links no concrete backend.
   (The *macOS version* branch within the vz backend is a runtime detail of one
   compile-time-selected backend, not backend selection — ADR-0012.)
9. **macOS sever: the user's binary is pure Go (ADR-0017, generalized by ADR-0020).** The
   importable `fleetbox` package, `fleetboxtest`, and `cmd/fleetbox` link no hypervisor on
   darwin — only the downloaded `cmd/fleetbox-helper` does. Check: `GOOS=darwin GOARCH=arm64
   go list -deps` of those three excludes `internal/backend/vz` and `third_party/vz`, and they
   build with `CGO_ENABLED=0`. After ADR-0020 they now also INCLUDE
   `internal/{image,seed,fixture,orchestrator}` (the orchestration moved client-side) and the
   helper EXCLUDES them — the inverse of the pre-0020 split.

## §7. Known limitations (accepted for v0)

- **A holder crash takes its whole cluster down (but no longer leaks).** A CLI cluster's
  VMs share one holder process so they can share one network (ADR-0009). That trades the
  per-VM crash isolation ADR-0006 originally had: lose the holder, lose every member of
  that cluster. Single-VM `up` is unaffected (a cluster of one). We deliberately do not
  try to keep a cluster alive across a holder crash — but the crash is now clean: each VM
  carries a parent-death `SIGKILL`, so a SIGKILL'd/OOM'd holder takes its VMs with it
  (no zombie processes), and the inert leftovers (bridge, taps, nft firewall table,
  uplink forwarding flag) are reclaimed automatically on the next `up` or `down`
  (ADR-0013, ADR-0025). The
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
- **Programmatic copy is tar-over-SSH, quiescent-file only.** `VM.CopyTo`/`CopyFrom` (and
  the CLI `cp`, now built on them rather than shelling out to `scp`) move a file or
  directory in/out over the SSH connection via the guest's `tar`, preserving modes but not
  ownership (ADR-0026). v1 does not honor `ctx` cancellation mid-transfer (it matches
  `VM.SSH`), treats any non-zero guest `tar` exit as an error (so copying an
  actively-written file can fail), and offers no `io.Reader`/`io.Writer` variants — all
  deferred. This is distinct from fixtures (read-only host→guest at boot, above), which are
  unchanged.
- **`fleetboxtest` VM-boot tests are capability-driven on both backends.** They run wherever
  the host can boot a VM — Linux (`/dev/kvm`) and darwin/arm64 (vz M3+/macOS 26) — skipping
  (never failing) otherwise via `SkipIfCannotBootVM`, and skipping on `-short`. The VM-boot
  suite (conformance, cluster, fixtures, nested) uses no `-run` selectors or build-tag gates:
  `make test` (`-short`) is VM-free, `make test-vm` runs the full suite the host supports.
  (`TestVMFixturesPersistAcrossReboot` keeps a runtime darwin/arm64 guard — it is a
  backend-specific reboot test, not a build-tag selector.)
- **macOS VM tests can't run on the macOS CI runner** (no nested virtualization). But the
  cloud-hypervisor backend *is* CI-testable on a GitHub x86-64 Linux runner, which exposes
  `/dev/kvm` after a one-time udev rule (arm64 hosted runners have no KVM) — the "develop on
  a Mac, run in cheap hosted Linux CI" path (ADR-0017). Wired as `vm-linux.yml`, which now
  runs the full capability-driven suite (conformance + cluster).
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
- **CLI surface**: the cobra commands (`newXxxCmd` constructors wired in `root.go`'s
  `newRootCmd`) — their `Use`/`Aliases`/flags — match §5.3.
- **Backend contract**: `internal/backend/backend.go` interfaces match §5.4.
- **State layout**: path methods in `internal/store/store.go` match the §4.2 tree.
- **Sever gate (macOS)**: `GOOS=darwin GOARCH=arm64 go list -deps` of `fleetbox`,
  `fleetboxtest`, and `cmd/fleetbox` excludes `internal/backend/vz` and `third_party/vz`,
  and the three build with `CGO_ENABLED=0` (§6.9, ADR-0017).
- **Dependencies**: direct requires in `go.mod` match the deps named in §5 module
  sections (currently: pilat/cloudiso, pilat/go-ext4fs — the fixture **writer** (§5.14),
  go-qcow2reader, x/crypto,
  spf13/cobra — the CLI framework, §5.3, ADR-0022, pulling spf13/pflag + inconshreveable/
  mousetrap as indirects — vishvananda/netlink + google/nftables — the Linux host
  networking, §5.12, ADR-0025, pulling vishvananda/netns + mdlayher/netlink + mdlayher/
  socket + x/sync + google/go-cmp as indirects — diskfs/go-diskfs — the arm64 in-process
  kernel/initrd **read** for direct boot (§5.12, ADR-0027), surgically imported (only
  `backend`+`backend/file`+`partition/gpt`+`filesystem/ext4`), pulling google/uuid as its
  one new indirect — and x/sys — now a direct dep, used by the
  cloud-hypervisor backend (netlink errno matching), `internal/helperdist` (quarantine
  xattr) and the darwin capability probes, as well as by the in-module vendored vz; plus
  go-infinity-channel + x/mod pulled in by vz). The vz fork itself is not a require — it is
  vendored in-module (see below).
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
