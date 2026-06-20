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
  alias→data image map (embedded `internal/image/catalog.json`). Carve (ADR-0019): that
  JSON is *internal data compiled into the binary* — not user-side config — so it does
  not violate "no yaml"; per-distro logic lives only in the build-time `contrib/catalog`
  tool, never in the runtime library.
- **Clusters are a naming convention** (`prefix-N`), never an entity with state.
- **Cattle with persistence.** `up` is idempotent, disks survive reboots, `rm` is the
  only destructive command.

## Architecture summary

```
fleetbox.go                     public API (neutral): VM, Cluster, Options/Option/Fixture (aliases to opts), With*, StartN/StartCluster, NestedVirtSupported, Prune, ErrClustersUnsupported
fleetbox_supported.go           the ONE client impl (darwin||linux): Start/NewCluster delegating to the client-side orchestrator + the clusterState wrapper (ADR-0020)
fleetbox_{darwin_arm64,linux,unsupported}.go  per-platform host probes only (nestedVirtSupported/supportsClusteringHost/prune) — pure-Go, client-side, never spawn the helper (ADR-0017 R7); linux blank-imports holder for its self-reexec init()
fleetboxtest/                   testing.TB fixtures: Start(t, image), StartN, SkipIfShort, SkipIfCannotBootVM, BootTimeout; capability-driven skips (no -run/build-tag selectors)
internal/opts                   backend-free option data + Encode/Decode (ADR-0017)
internal/control                backend-free CLIENT half of the holder protocol: Spawn/Status/WaitMembers/Stop/SendCommand + NDJSON wire types (createnetwork/reserve/boot-member, MemberSpec/Reservation) + bound-mode bind/version handshake (ADR-0017/0020)
internal/orchestrator           CLIENT-side VM sequencer (resolveStartDeps/startOnNetwork/spawnHelper/fixtures/Cluster, helperExe/preflight per platform); drives the helper via the remote-proxy backend — links NO concrete backend (ADR-0020)
internal/backend/remote         pure-Go remote-proxy backend: turns backend.Backend/Network/VM calls into control-protocol RPCs (ADR-0020)
internal/holder                 the BACKEND-SERVER: links the real backend (newRealBackend per platform/tag), serves createnetwork/reserve/boot-member/status/stop + bound-lifetime teardown + linux self-reexec init(); in cmd/fleetbox-helper on darwin (ADR-0020)
internal/helperdist             macOS helper catalog + download/verify/quarantine-strip + FLEETBOX_HELPER override (ADR-0017)
internal/backend                Backend interface (CreateNetwork/Create/NestedVirtSupported/SupportsClustering/Reconcile); Network adds Reserve(name,ipHint)→{ip,mac}; Config carries SerialLogPath (helper opens it)
internal/backend/vz             VZ implementation, darwin/arm64 (the only vz import site; helper-only)
internal/backend/cloudhypervisor cloud-hypervisor implementation, linux (the only CH import site; helper-only; owns the network + ADR-0013 reconcile + IP allocation via Reserve)
internal/backend/fake           instant pure-Go backend behind the helper under -tags fleetbox_fake; records args to FLEETBOX_FAKE_RECORD for cross-process assertions (ADR-0018/0020)
internal/fetch                  shared download → verify(sha256) → atomic-cache primitive
internal/image                  cloud image download/verify/qcow2→raw/cache; pinned per-arch catalog as embedded JSON (catalog.json) — now CLIENT-side (ADR-0019/0020)
internal/seed                   cloud-init NoCloud seed ISO + static network-config (via pilat/cloudiso) — client-side
internal/fixture                host dir → read-only ext4 payload image (via pilat/go-ext4fs); fixtures (ADR-0015) — client-side
internal/store                  ~/.fleetbox/{clusters,images,bin}/ state, config.json, locking; cluster-rooted layout clusters/<cluster>/<member>/ (ADR-0014)
internal/dhcp                   /var/db/dhcpd_leases parsing (hostname → IP); darwin-only, helper-side
internal/sshkey                 keypair + x/crypto/ssh client (client-side)
cmd/fleetbox                    CLI: up/down/ls/ssh/cp/ssh-config/rm/version (pure-Go client; drives a DETACHED helper via the client orchestrator on both platforms; on linux self-reexecs into the holder via internal/holder's init())
cmd/fleetbox-helper             darwin VM host: links vz, signed, downloaded; runs internal/holder as a backend-server (ADR-0017/0020)
contrib/catalog                 build-time tool (not in any runtime binary): refreshes internal/image/catalog.json (ADR-0019)
```

The orchestrator now runs **client-side on both platforms**, driving the helper by RPC
through `internal/backend/remote`; the real backend lives only behind the helper
(ADR-0020 inverts ADR-0017's orchestrator-in-helper). The macOS sever inverts with it:
the darwin client now **includes** `internal/{image,seed,fixture,orchestrator}` and still
**excludes** `internal/backend/vz` + `third_party/vz`
(`GOOS=darwin GOARCH=arm64 go list -deps` of root/fleetboxtest/cmd-fleetbox; `CGO_ENABLED=0`).
The darwin helper is the mirror: it **includes** vz and **excludes** image/seed/fixture/orchestrator
(the catalog-out-of-the-signed-helper payoff). On Linux there is NO backend-free sever — the
single binary self-reexecs into the holder, so it links cloud-hypervisor + orchestrator + image
(accepted: CH is pure-Go, nothing is signed).

Key external deps: the vendored vz fork `third_party/vz` (macOS, helper only), `pilat/cloudiso`
(seed ISO), `pilat/go-ext4fs` (fixture ext4 payload), `go-qcow2reader`, `golang.org/x/crypto/ssh`,
`golang.org/x/sys/unix` (sysctl probes, quarantine xattr). The Linux path and the darwin client
stay pure Go — no cgo; cgo lives only in the darwin helper.

## Build & test notes

- **macOS** user binaries (CLI and test binaries) are pure Go and need NO entitlement and
  NO codesign — that's the ADR-0017 sever, kept and generalized by ADR-0020. Only
  `cmd/fleetbox-helper` carries the `com.apple.security.virtualization` entitlement (ad-hoc
  codesign). For dev/VM tests, `make helper` builds+signs it and `make test-vm` points the
  library at it via `FLEETBOX_HELPER`. The published helper (release `helper-v0.2.1` — the
  `0.2.x` line carries the protocol-v2 inversion, `0.2.1` adds `FLEETBOX_HOME` support per
  ADR-0028; the old `helper-v0.1.0` is rejected at the version handshake) auto-downloads on
  first use, no override needed. The signed helper is now a thin
  backend-server: the client resolves images and builds disks/seeds/fixtures, so the catalog
  is NOT in the signed binary.
- The module compiles on `darwin/arm64`, `linux/amd64`, and `linux/arm64`. Other targets
  (incl. `darwin/amd64`) compile but error at runtime with "unsupported platform". Unit
  tests (`make test`) and `make lint` run on darwin/arm64; lint the Linux code with
  `GOOS=linux golangci-lint run ./...`.
- **Two test tiers, capability-driven — no `-run` filters, no build-tag selectors.** Each
  test decides for itself whether to boot a VM, from the speed tier (`testing.Short()`) and
  a runtime host probe (`/dev/kvm` openable on Linux; vz M3+/macOS 26 via
  `NestedVirtSupported()` on darwin — `fleetboxtest.SkipIfCannotBootVM`), plus the public
  `ErrClustersUnsupported` sentinel for the cluster test (so it self-skips on macOS < 26).
  So there are exactly two entry points: `make test` (`-short -race`, quick, VM-free — the
  dev inner loop and the CI unit lanes) and `make test-vm` (the full `go test ./fleetboxtest`
  with NO selector — runs everything the host supports). The old `make test-nested` and the
  `fleetbox_nested` build tag are gone, folded into `make test-vm`. (`make test-fake` /
  `test-fake-linux` are a separate build-time *backend swap*, not part of this tiering — left
  untouched.) The per-VM boot budget honors `FLEETBOX_IP_WAIT_TIMEOUT` (one knob widens both
  the holder's IP-wait and the fixtures' boot context — `fleetboxtest.BootTimeout`), which the
  nested orchestrator sets to 20m inside the slow guest.
- VM-boot tests on macOS (`make test-vm`) need darwin/arm64, M3+, macOS 26+. `make test-vm`
  runs the full suite: the single-VM conformance check (lifecycle + egress), the multi-node
  cluster (VM↔VM + subnet isolation), and the nested dogfood (`TestNestedLinuxBoot`) — which
  boots an outer Linux guest and runs this SAME unified suite inside it on the cloud-hypervisor
  backend (the inner pass/fail is the inner binary's exit code, not a string match). Linux VM
  tests need a host with `/dev/kvm` + `CAP_NET_ADMIN` (a real Linux box, a KVM-enabled CI
  runner, or a Lima VM with `nestedVirtualization: true`) — NOT Docker Desktop (no
  `/dev/kvm`). The **Apple-Silicon dev box (M4, macOS 26) CAN run the whole thing locally**:
  M4 supports vz nested virtualization, so the nested dogfood's outer guest gets a real
  `/dev/kvm` and the cross-compiled Linux `fleetboxtest` binary boots true nested VMs through
  the cloud-hypervisor backend. So the Linux netlink/nftables path is dogfooded on this Mac by
  `make test-vm` itself — do not claim "needs a separate Linux box". (The guest is arm64, so
  this exercises the arm64 direct-kernel-boot path, ADR-0024 — which the amd64 CI
  `vm-linux.yml` does not cover.) For an isolated manual run you can still cross-build
  `GOOS=linux GOARCH=arm64 go test -c ./fleetboxtest`, copy it into a Lima guest, and run it
  under `sudo`.
- CI (`ci.yml`) has two jobs. The **macOS** job (macos-26) cannot boot VZ VMs, so it runs
  lint (darwin/linux/fake) + build + linux cross-build + unit + the fake coordination
  (`make test-fake`, the protocol gate with no VM). Do not switch it to ubuntu (it must keep
  building/linting the darwin code). The **linux** job (ubuntu) runs build + unit + the fake
  coordination via self-reexec (`make test-fake-linux`, no KVM needed). Unlike VZ, the
  cloud-hypervisor backend *is* CI-testable with KVM: `vm-linux.yml` boots real VMs on an
  x86-64 ubuntu runner — the full capability-driven suite (conformance + a multi-node cluster,
  so amd64 now has VM↔VM coverage; the nested dogfood self-skips there) over the full
  client↔helper protocol (the test binary is its own helper via self-reexec). Releases run on
  tags: `release-helper.yml` (helper, macOS,
  codesign) and `release.yml` (CLI, goreleaser) — two independent channels (`helper-v*` / `v*`).
- Commands: `make test` (unit), `make build` (compile the pure-Go CLI, no signing),
  `make helper` (build + ad-hoc-sign `cmd/fleetbox-helper`, darwin only), `make test-vm`
  (builds+signs the helper, exports `FLEETBOX_HELPER`, runs the full capability-driven suite —
  conformance + cluster + nested), `make lint`,
  `make catalog` (refresh the pinned image catalog — `go run ./contrib/catalog`;
  streams the Debian images through the hashers to compute the sha256 Debian does not
  publish, so it pulls several GB when snapshots move; maintenance/CI only),
  `make vendor-vz` (regenerate the vendored vz fork — maintenance only). No test binary is
  ever signed now — the sever moved the entitlement into the helper.

## Go style

Follow the global conventions (docs/coding-style.md): declaration order
const→var→type→exported→unexported, `var _ Iface = (*impl)(nil)`, New() constructors,
flat error handling with `fmt.Errorf("context: %w", err)`, sentinel errors caught once
at the caller. Every exported symbol gets a doc comment — this is a library.

## Git conventions

Commits follow **Conventional Commits** (`<type>[(scope)][!]: <description>`):

- **Type:** `feat` and `fix` are the two the spec mandates; we also use the standard
  Angular/commitlint set — `chore`, `docs`, `test`, `refactor`, `ci`, `build`, `perf`,
  `style`. Pick the one matching the change's PRIMARY intent.
- **Scope** is optional and in parentheses: `feat(backend): …`.
- **Breaking changes** are flagged with `!` before the colon (`feat!:`, `feat(api)!:`).
  Our subjects are a single line, so we do NOT use the multi-line `BREAKING CHANGE:` footer.
- **Branches** are not part of Conventional Commits, but we prefix them with the same
  type vocabulary plus a short kebab-case description: `feat/cluster-prune`,
  `test/fake-backend-coordination-tests`, `fix/holder-reap-race`.
- PRs are **squash-only** (`gh pr merge --squash`); the squash subject is itself a
  Conventional Commit.

## Known deviations from spec

- **Helper = thin backend-server; orchestrator runs client-side on both platforms.** The
  ADR-0017 sever (importable package pure Go, no cgo, no codesign) is **kept and generalized**
  by ADR-0020: instead of the WHOLE orchestrator living in the signed helper, the helper now
  holds only the live cluster (one shared network + its VMs) and is driven by RPC. The
  client (root + `internal/orchestrator`, both platforms) resolves the image, copies disks,
  builds seeds/fixtures, manages the store, then drives the helper over the control protocol
  via the pure-Go remote-proxy backend (`internal/backend/remote`); the real vz/CH backend
  lives only behind the helper (`internal/holder` + `newRealBackend`). macOS keeps the
  separately-distributed signed `cmd/fleetbox-helper` (downloaded via `helperdist`); Linux
  **self-reexecs** the single client binary (an `init()` interceptor in `internal/holder`,
  the docker/reexec pattern, catches `--fleetbox-runner` before the test framework/CLI
  main()). The protocol is newline-delimited JSON (`ProtocolVersion` "2") carrying a resolved
  member spec, not an image alias; the helper owns the network AND IP allocation
  (`Network.Reserve`). SSH/cp dial the VM's IP directly (never proxied). Library mode spawns
  *bound* (parent-PID watch + control-conn EOF; reaps on `kill -9`); the CLI spawns
  *detached*. The public API is unchanged. Inverts ADR-0017's orchestrator-in-helper half;
  amends ADR-0006/0008/0009/0011/0013. Published as `helper-v0.2.0`. See `docs/adr/0020`
  (and `0017` for the sever's origin).

- **Cross-platform: macOS (VZ) + Linux (cloud-hypervisor) behind one API.** The module
  is no longer `darwin && arm64`-only. The concrete backend is selected at compile time per
  platform inside the **helper** (`internal/holder/backend_darwin_arm64.go` → vz,
  `backend_linux.go` → cloud-hypervisor, `backend_fake.go` → fake under `-tags fleetbox_fake`,
  `backend_unsupported.go` → clear error) — NOT the orchestrator anymore (ADR-0020). On Linux
  the VMM is a downloaded, checksum-pinned cloud-hypervisor binary (cached in
  `~/.fleetbox/bin/`), run as a subprocess and controlled over its unix-socket REST API with
  stdlib — pure Go, no cgo. Linux networking is a shared bridge + per-VM tap with static IPs
  the helper allocates via `Network.Reserve` and the client injects via cloud-init
  `network-config`. IP discovery is behind the backend (`backend.VM.WaitForIP`), reported to
  the client through the holder's `status`. See `docs/adr/0011`, `docs/adr/0020`.

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
  `vmnet.Network` (the same object the library `StartN` uses), held in the helper. The
  holder serves a per-member socket+pidfile, so `ls`/`ssh`/`down`/`rm` address each member by
  name; `up`-ing a stopped member re-joins the live cluster via `reserve` + `boot-member` on
  the running helper (through a live sibling's socket) — the old `addmember` command is gone
  (ADR-0020). Tradeoff: a holder crash takes the whole cluster down. See `docs/adr/0009`,
  `docs/adr/0020`.

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
