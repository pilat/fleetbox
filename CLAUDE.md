# fleetbox — Agent Brief

Linux VMs as Go test fixtures, on macOS (Apple Silicon, via Apple
Virtualization.framework) and on Linux (amd64/arm64, via cloud-hypervisor) behind one
backend-neutral Go API. Library-first: the Go package is the product, the CLI is a
wrapper. Think "testcontainers-go, but for real VMs" — real kernel, real systemd, real
KVM via nested virtualization.

Module: `github.com/pilat/fleetbox`

## Read order

1. `ai/tasks/2026-06-03-v0-spec.md` — the v0 spec. All design decisions live there;
   do not re-litigate them. New specs land in `ai/tasks/YYYY-MM-DD-name.md`.
   `ai/tasks/` is gitignored — specs are local working files and exist only on this
   machine, not in the repo.
2. `README.md` — the user-facing story.
3. `ARCHITECTURE.md` — what the system looks like today (descriptive): module map,
   system model, invariants, sync rules.
4. `docs/coding-style.md` — how new code must be written (prescriptive). The
   machine-checkable subset is enforced by `.golangci.yml` (`make lint`).
5. `docs/adr/` — why the architecture is the way it is. The v0 spec's decisions
   live here as numbered ADRs — their durable, in-repo home.

## Core principles (violations = bugs)

- **Library-first.** Every capability exists in the Go API; the CLI only wraps it.
- **Backend-neutral public API.** Hypervisor types must never appear in exported
  signatures. The vendored vz fork (`third_party/vz`) is allowed only in
  `internal/backend/vz`; cloud-hypervisor specifics only in
  `internal/backend/cloudhypervisor`.
- **Nothing of ours inside the guest.** No agent, no helper binary, no host↔guest
  protocol. The guest is a stock distro configured once by cloud-init.
- **No port forwarding.** VMs get directly routable IPs — vmnet SharedMode on macOS 26+,
  a shared Linux bridge + tap on Linux.
- **No yaml, no templates, no per-distro code paths.** Flags, defaults, and a dumb
  alias→URL image map.
- **Clusters are a naming convention** (`prefix-N`), never an entity with state.
- **Cattle with persistence.** `up` is idempotent, disks survive reboots, `rm` is the
  only destructive command.

## Architecture summary

```
fleetbox.go                     public API (neutral): VM, Cluster, Options/Option/Fixture (aliases to opts), With*, StartN/StartCluster, NestedVirtSupported, Prune, ErrClustersUnsupported
fleetbox_{linux,darwin_arm64,unsupported}.go  per-platform Start/NewCluster/prune/nestedVirtSupported + the VM/Cluster impl behind the build-tagged seam (linux: wraps orchestrator in-process; darwin: a pure-Go client driving the helper) (ADR-0017)
fleetboxtest/                   testing.TB fixtures: Start(t, image), StartN, SkipIfShort
internal/opts                   backend-free option data + Encode/Decode for the helper boundary (ADR-0017)
internal/control                backend-free CLIENT half of the holder protocol: Spawn/Status/WaitMembers/Stop/AddMember + bound-mode bind/version handshake (ADR-0017)
internal/orchestrator           VM-owning logic (resolveStartDeps/startOnNetwork/allocateIP/fixtures/Cluster) + compile-time backend selection; the ONLY package that links a backend (ADR-0017)
internal/holder                 SERVER half of the holder protocol (Run/holder/bootMember) + bound-lifetime teardown; in cmd/fleetbox-helper on darwin, reached by CLI re-exec on linux (ADR-0017)
internal/helperdist             macOS helper catalog + download/verify/quarantine-strip + FLEETBOX_HELPER override (ADR-0017)
internal/backend                Backend interface (CreateNetwork/Create/NestedVirtSupported/SupportsClustering)
internal/backend/vz             VZ implementation, darwin/arm64 (the only vz import site)
internal/backend/cloudhypervisor cloud-hypervisor implementation, linux (the only CH import site)
internal/fetch                  shared download → verify(sha256) → atomic-cache primitive
internal/image                  cloud image download/verify/qcow2→raw/cache (per-arch catalog)
internal/seed                   cloud-init NoCloud seed ISO + static network-config (via pilat/cloudiso)
internal/fixture                host dir → read-only ext4 payload image (via pilat/go-ext4fs); fixtures (ADR-0015)
internal/store                  ~/.fleetbox/{clusters,images,bin}/ state, config.json, locking; cluster-rooted layout clusters/<cluster>/<member>/ (ADR-0014)
internal/dhcp                   /var/db/dhcpd_leases parsing (hostname → IP); darwin-only
internal/sshkey                 keypair + x/crypto/ssh client
cmd/fleetbox                    CLI: up/down/ls/ssh/cp/ssh-config/rm (pure-Go client; on darwin drives the helper, on linux re-execs itself as the holder)
cmd/fleetbox-helper             darwin VM host: links vz, signed, downloaded; runs internal/holder (ADR-0017)
```

On darwin the importable package and the CLI are pure Go — they link neither `vz` nor
`internal/orchestrator`; the only darwin binary that links vz is `cmd/fleetbox-helper`
(`GOOS=darwin GOARCH=arm64 go list -deps` of root/fleetboxtest/cmd-fleetbox excludes
`internal/backend/vz` and `third_party/vz`, and they build with `CGO_ENABLED=0`).

Key external deps: the vendored vz fork `third_party/vz` (macOS, helper only), `pilat/cloudiso`
(seed ISO), `pilat/go-ext4fs` (fixture ext4 payload), `go-qcow2reader`, `golang.org/x/crypto/ssh`,
`golang.org/x/sys/unix` (sysctl probes, quarantine xattr). The Linux path and the darwin client
stay pure Go — no cgo; cgo lives only in the darwin helper.

## Build & test notes

- **macOS** user binaries (CLI and test binaries) are pure Go and need NO entitlement and
  NO codesign — that's the ADR-0017 sever. Only `cmd/fleetbox-helper` carries the
  `com.apple.security.virtualization` entitlement (ad-hoc codesign). For dev/VM tests,
  `make helper` builds+signs it and `make test-vm` points the library at it via
  `FLEETBOX_HELPER`; the published helper auto-downloads once Task-7 publishing lands.
- The module compiles on `darwin/arm64`, `linux/amd64`, and `linux/arm64`. Other targets
  (incl. `darwin/amd64`) compile but error at runtime with "unsupported platform". Unit
  tests (`make test`) and `make lint` run on darwin/arm64; lint the Linux code with
  `GOOS=linux golangci-lint run ./...`.
- VM-boot tests on macOS (`make test-vm`) need darwin/arm64, M3+, macOS 26+. Linux VM
  tests need a host with `/dev/kvm` + `CAP_NET_ADMIN` (a real Linux box, a Lima VM with
  `nestedVirtualization: true`, or a KVM-enabled CI runner) — not the macOS dev box and
  not Docker Desktop (no `/dev/kvm`).
- CI (macos-26 GitHub runner) cannot boot VZ VMs — it runs lint + build + unit tests only.
  Do not switch the macOS CI to ubuntu (it must keep building/linting the darwin code).
  Unlike VZ, the cloud-hypervisor backend *is* CI-testable on Linux runners with KVM — a
  future win, out of v1 scope.
- Commands: `make test` (unit), `make build` (compile the pure-Go CLI, no signing),
  `make helper` (build + ad-hoc-sign `cmd/fleetbox-helper`, darwin only), `make test-vm`
  (builds+signs the helper, exports `FLEETBOX_HELPER`, boots real VMs), `make lint`,
  `make vendor-vz` (regenerate the vendored vz fork — maintenance only). No test binary is
  ever signed now — the sever moved the entitlement into the helper.

## Go style

Follow the global conventions (docs/coding-style.md): declaration order
const→var→type→exported→unexported, `var _ Iface = (*impl)(nil)`, New() constructors,
flat error handling with `fmt.Errorf("context: %w", err)`, sentinel errors caught once
at the caller. Every exported symbol gets a doc comment — this is a library.

## Known deviations from spec

- **macOS signed-helper sever: the importable package is pure Go (no cgo, no codesign).**
  All Virtualization.framework work moved out of the importable package into a separately
  distributed, ad-hoc-signed `cmd/fleetbox-helper` that the library downloads at first use
  (like cloud-hypervisor on Linux) and drives over a unix socket. The darwin root package
  and CLI are thin pure-Go clients: they spawn the helper, hand it `Options`, poll for the
  IP, then dial the VM's IP directly for SSH/cp (the helper protocol never proxies SSH).
  Library mode spawns the helper *bound* (attached + parent-PID watch + control-conn EOF) so
  it reaps itself and its in-process VMs when the test process dies, even on `kill -9`; the
  CLI keeps the detached/persistent holder. Linux stays in-process (nothing to sign; CH is
  already the downloaded VMM) with a `/dev/kvm`+`CAP_NET_ADMIN` preflight. The public API is
  unchanged; `VM`/`Cluster` are defined once over a build-tagged unexported impl, and
  `Options`/`Option`/`Fixture`/`With*` are aliases over `internal/opts`. Supersedes ADR-0006
  and ADR-0009 on macOS (both still in force on Linux). Until the signed helper is published,
  dev/CI use the `FLEETBOX_HELPER` override. See `docs/adr/0017`.

- **Cross-platform: macOS (VZ) + Linux (cloud-hypervisor) behind one API.** The module
  is no longer `darwin && arm64`-only. The backend is selected at compile time per
  platform (now inside `internal/orchestrator`: `backend_darwin_arm64.go` → vz,
  `backend_linux.go` → cloud-hypervisor, `backend_unsupported.go` → clear error). On Linux the VMM is a downloaded,
  checksum-pinned cloud-hypervisor binary + firmware (cached in `~/.fleetbox/bin/`),
  run as a subprocess and controlled over its unix-socket REST API with stdlib — pure
  Go, no cgo. Linux networking is a shared bridge + per-VM tap with static IPs injected
  via cloud-init `network-config`. IP discovery moved behind the backend
  (`backend.VM.WaitForIP`). See `docs/adr/0011`.

- **Read-only fixtures replace live mounts (cross-platform).** ADR-0010's macOS-only,
  live read-write `WithMount` (virtiofs) is gone — deleted, no alias. In its place
  `WithFixture(hostDir, guestPath)` packs a host dir into a read-only ext4 image
  (`internal/fixture` via `pilat/go-ext4fs`), attaches it read-only on both backends (the
  way `seed.iso` is attached), and the stock guest mounts it by `LABEL=FBFIX<i>` via
  cloud-init `mounts:`. cloud-hypervisor has no daemon-free *live* share (no built-in
  virtio-fs, no clean static `virtiofsd` to pin), so rather than add a host daemon on Linux
  to mirror macOS's free virtio-fs, live mounts were dropped on both platforms. Fixtures are
  world-readable (`0444`/`0555`, uid 0), per-member (`clusters/<c>/<m>/fixture-<i>.img`,
  wiped by `store.Delete`), the set frozen at create but rebuilt from the host dir every
  boot (no cache). The `--fixture host:guest` CLI flag mirrors the old `--mount`. See
  `docs/adr/0015` (supersedes `0010`).

- **Platform matrix: clusters need macOS 26+ or Linux; single VM works on macOS <26.**
  vmnet SharedMode (VM↔VM) is macOS 26+ only; below 26 the vz backend uses a resurrected
  `VZNATNetworkDeviceAttachment` for a single, isolated VM, and `SupportsClustering()`
  is false so a 2nd cluster member errors (`ErrClustersUnsupported`). Linux clusters work
  via the shared bridge. Intel macOS is unsupported. See `docs/adr/0012` (and `0008`,
  `0004`, which it references). Both `StartN` and the CLI (`fleetbox up <prefix> -n N`)
  boot interconnected clusters where clustering is supported.

- **CLI clusters run in one holder process (not one runner per VM).** ADR-0006's
  "one runner per VM" became "one holder per `up` group": a CLI cluster's VMs share one
  in-process `vmnet.Network` (the same object the library `StartN` uses), so the XPC /
  `CopySerialization` cross-process path ADR-0008 sketched for "Phase 2" was not needed.
  The holder serves a per-member socket+pidfile, so `ls`/`ssh`/`down`/`rm` address each
  member by name; `up`-ing a stopped member re-joins the live cluster via an `addmember`
  socket command. Tradeoff: a holder crash takes the whole cluster down. See
  `docs/adr/0009`.

- **IP discovery uses hostname, not MAC.** VZ uses DUID-based identifiers in
  dhcpd_leases (hw_address=ff,...) instead of traditional MAC format. cloud-init sets
  the hostname via DHCP, so we look up by hostname instead. Retained unchanged under
  vmnet SharedMode — it rides the same bootpd/bridge machinery as NAT did (ADR-0007).

- **Cluster-rooted store ("always a cluster").** State is no longer flat
  `~/.fleetbox/vms/<name>/`; every VM is a member under
  `~/.fleetbox/clusters/<cluster>/<member>/`, with `<cluster>` derived from the member name
  (strip a trailing `-<digits>`; solo VM = cluster of one). The holder's control socket and
  pidfile moved into the member dir (`sock`/`pid`) — no more loose `sock-*`/`pid-*` in the
  base dir. Store method signatures are unchanged (bodies only); the public API is
  unchanged. Breaking, no migration (pre-release: delete `~/.fleetbox` by hand). See
  `docs/adr/0014`.

## Related projects (same author, reuse experience)

- `github.com/pilat/cloudiso` — cloudiso library (seed ISO generation)

## Architecture docs

`ARCHITECTURE.md` (descriptive), `docs/coding-style.md` (prescriptive), and
`docs/adr/` (decisions) must stay accurate:

- A PR that changes the package list, public API, CLI surface, on-disk layout, or
  dependencies updates the corresponding `ARCHITECTURE.md §5` section in the same
  PR (checklist in its §8), or run `/pilat:arch-sync` after the change.
- New design decisions get an ADR in `docs/adr/` (next sequential number).
  Decisions made in local specs (`ai/tasks/`) must graduate to ADRs to survive —
  `ai/tasks/` is gitignored and does not travel with the repo.
- Style questions are settled by `docs/coding-style.md`.
