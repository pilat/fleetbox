# ADR: macOS Signed-Helper Sever (Pure-Go Client, Downloaded VM Host)

**Date:** 2026-06-09
**Status:** Accepted (supersedes ADR-0006 and ADR-0009 on macOS; the orchestrator-in-helper half is inverted by [ADR-0020](0020-helper-thin-backend-server.md), which keeps and generalizes this ADR's pure-Go sever)

## Context

On macOS the importable package linked the cgo `vz` fork, so a user's `go test`
binary (and the CLI) had to be compiled with cgo **and** ad-hoc codesigned with
the `com.apple.security.virtualization` entitlement, or it died on launch with no
output. That was the single biggest adoption blocker: a curious developer ran
`go get` + `go test ./...`, got a binary that died silently, and left.

The Linux backend already showed the way out: its VMM (cloud-hypervisor) is a
downloaded, checksum-pinned binary run as a subprocess and driven over a socket —
nothing the user builds links it. macOS could do the same with Apple
Virtualization.framework if all VZ work moved out of the importable package into a
separately distributed, signed helper.

Two grounded facts made this clean. (1) SSH/cp/IP already reach a VM by **direct
dial from a process that is not the holder** (the CLI's `ssh`/`cp` ask the holder
only for the IP, then dial `fleetbox@<ip>` themselves), so vmnet SharedMode IPs
are host-wide reachable and the helper protocol need not proxy SSH. (2) The
capability probes (`NestedVirtSupported`, `SupportsClustering`) are called on the
test-skip path *before* booting anything, so they must answer without VZ and
without downloading the helper.

## Decision

On macOS, **the importable package is pure Go and needs neither cgo nor
codesign.** All Virtualization.framework work moves into a separately distributed,
ad-hoc-signed `fleetbox-helper` binary that the library downloads at first use
(like cloud-hypervisor on Linux) and drives as a subprocess over a unix socket.
The public API is unchanged.

1. **Full sever.** The darwin root package links no `vz` and imports no
   orchestrator; `vz` lives ONLY in `cmd/fleetbox-helper`. No in-process VZ path
   is retained — two parallel paths is exactly the per-platform divergence the
   project forbids, and only the full sever removes both cgo and codesign for
   users.

2. **All modes go through the helper.** Library single-VM, library cluster, and
   CLI all own VMs via the downloaded signed helper subprocess on macOS — the same
   shape cloud-hypervisor already has on Linux.

3. **The helper runs the orchestration; the client is thin.** The helper hosts the
   holder logic (`internal/holder` over `internal/orchestrator`): image download,
   disk copy, seed, fixtures, network, boot, IP/SSH readiness. The client (root
   package on darwin) spawns the helper, hands it `Options`, polls status for the
   IP, then **dials the VM's IP directly** for SSH/cp. The helper protocol stays
   small (it does not proxy SSH).

4. **The helper is signed; the user's binary is not.** Distribution signature is
   ad-hoc (`codesign -s - --entitlements entitlements.plist`), free, with no tie
   of macOS distribution to an Apple identity. The entitlement is unrestricted, so
   an ad-hoc signature carries it on any Mac. Developer ID + notarization is the
   documented fallback for shops whose policy refuses an unquarantined ad-hoc
   binary — re-sign the same binary, nothing else changes.

5. **Download-on-first-use is the delivery mechanism.** Nothing can be placed
   alongside the user's tests (their binary is built by `go` and contains only our
   pure-Go client), and `go:embed` of a signed mach-o is strictly worse (bloats
   every `go get`, ships a signed binary via the module proxy, couples helper
   updates to module releases). The helper arrives by runtime download, cached and
   checksum-pinned under `~/.fleetbox/bin`, version-stamped so the client runs the
   exact helper its protocol matches. A `FLEETBOX_HELPER` env var points at a
   pre-staged/local helper (offline escape hatch and dev bootstrap); the bind
   handshake exchanges a protocol version so a stale override fails loudly. An
   empty catalog checksum is refused — a binary that runs with the virtualization
   entitlement is never used unverified.

6. **Bound lifetime in library mode.** macOS has no `PR_SET_PDEATHSIG`, so the
   library spawns the helper **attached** (no `Setsid`), passes the client PID, and
   the helper reaps itself and its in-process VMs when the parent goes away — it
   watches both a reparent (the robust, PID-reuse-proof signal that handles
   `kill -9`) and a long-lived control connection's EOF (the faster path). The CLI
   keeps the **detached/persistent** holder (cattle-with-persistence: VMs outlive
   the command). Same helper binary, two spawn/lifetime modes.

7. **Linux stays in-process.** Nothing on Linux needs signing and
   cloud-hypervisor is already the downloaded subprocess VMM, so the orchestration
   keeps running in the test/CLI process (pure Go). The Linux privilege
   requirement (`CAP_NET_ADMIN`, `/dev/kvm`) is inherent to "no daemon" and is not
   solved by a helper; it is handled with a preflight health-check and a clear
   error. The public API hides the macOS/Linux asymmetry.

The internal split that carries this: `internal/opts` (backend-free option data +
encode/decode), `internal/control` (backend-free client half of the holder
protocol), `internal/orchestrator` (the VM-owning logic, the one package that
links a backend), `internal/holder` (the server half, compiled into the helper on
darwin and reached by CLI re-exec on Linux), `internal/helperdist` (catalog +
download + quarantine-strip + override), and `cmd/fleetbox-helper`. The root
package re-exposes `Options`/`Option`/`Fixture`/`With*` as aliases over
`internal/opts` and defines `VM`/`Cluster` once over a build-tagged unexported
impl.

## Alternatives Considered

**Keep in-process VZ, add the helper as an option.** Rejected: two parallel paths
is the per-platform divergence the project forbids, and it keeps cgo + codesign in
the user's binary — capturing none of the prize.

**Remote the `backend.Backend` interface, keep orchestration in the client
(Seam X).** Rejected: a chunkier 3-interface RPC, an awkward crossing of the
`SerialOut io.Writer` and file-path handles, and far less reuse of the proven
holder than running the whole orchestration in the helper (Seam Y).

**`go:embed` the signed helper.** Rejected: bloats every `go get` (including users
who never boot a VM), ships a signed mach-o through the module proxy, and turns a
helper update into a module release.

**Notarize-first.** Rejected as the default: the entitlement is unrestricted, so
ad-hoc carries it cross-machine; notarization is kept as a documented fallback,
not a requirement.

**A Linux helper for internal uniformity.** Rejected: it buys nothing (nothing
needs signing), does not solve `CAP_NET_ADMIN`, and adds a process hop.

## Consequences

- **macOS first-run DX matches the genre.** `go get` + `go test ./...` just works:
  no cgo, no codesign, no entitlement on the user's binary. The helper (and image)
  download once, cached under `~/.fleetbox`. Verified at runtime: an **unsigned**
  `go test` binary boots a real VM through the signed helper, runs `vm.SSH`, and
  tears it down; a `kill -9` of the test process reaps the helper and its VM within
  a few seconds with no leak.

- **Supersedes ADR-0006 and ADR-0009 on macOS.** The darwin CLI no longer
  re-execs itself as a holder; the holder is the separate downloaded
  `cmd/fleetbox-helper`. Both ADRs still stand on Linux, where the CLI re-execs
  itself (`internal/holder.IsRunner`/`Run`). The holder protocol is unchanged
  (`status`/`stop`/`addmember`) plus a holder-wide `bind` connection used only in
  bound mode.

- **References ADR-0008/0011 (networking unchanged: vmnet SharedMode on macOS,
  shared bridge on Linux; SSH/cp dial the IP directly), and ADR-0013
  (parent-death lifetime — the macOS bound helper is the no-PDEATHSIG analogue of
  the Linux child's `PR_SET_PDEATHSIG`).** ADR-0015 fixtures and ADR-0014 store
  layout are unaffected; on macOS the orchestration that builds them now runs in
  the helper.

- **The catalog is empty until the artifact is published.** Until the signed
  helper is released and its url+sha256 wired into `internal/helperdist`, the
  download path refuses to run (empty checksum), and development/CI use the
  `FLEETBOX_HELPER` override pointed at the locally built, ad-hoc-signed helper
  (`make test-vm` does this).

- **Control sockets moved to `~/.fleetbox/run/` (amends ADR-0014).** ADR-0014 put
  the holder socket in the member dir (`clusters/<cluster>/<member>/sock`), but that
  path plus a long cluster-member name and a long home dir exceeds the 104-byte unix
  `sun_path` limit — `net.Listen("unix", …)` fails with "invalid argument", which the
  refactor's bound control socket would have hit too. Sockets now live in `run/`
  under a hash of the name (`SocketPath`/`ControlSocketPath`), short and bounded
  regardless of name length; pidfiles stay in the member dir. A cluster VM-boot test
  with a long auto-derived name now passes.

- **Capability probes are advisory on macOS.** `NestedVirtSupported` parses the
  CPU brand string (Apple M3+ on macOS 15+) and `SupportsClustering` reads the
  macOS major version — pure Go, no VZ, no download — so the common skip never
  pulls the helper. An unrecognized future chip is treated optimistically; the
  helper's authoritative `vz.IsNestedVirtualizationSupported` rejects it at boot
  if the optimism was wrong.

- **Signing decision (D4) recorded.** Ad-hoc was chosen over notarize-first on the
  strength of the unrestricted-entitlement fact; the cross-machine spike (a second
  Mac) was not run before the refactor (maintainer's call — treated as low risk),
  with Developer ID + notarization standing as the documented fallback if a
  downloaded ad-hoc helper is ever rejected in the field.
