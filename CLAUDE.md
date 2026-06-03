# fleetbox — Agent Brief

Linux VMs as Go test fixtures on macOS (Apple Silicon), powered by Apple
Virtualization.framework. Library-first: the Go package is the product, the CLI is a
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
- **Backend-neutral public API.** `Code-Hex/vz` types must never appear in exported
  signatures. The only package allowed to import vz is `internal/backend/vz`.
- **Nothing of ours inside the guest.** No agent, no helper binary, no host↔guest
  protocol. The guest is a stock distro configured once by cloud-init.
- **No port forwarding.** VMs get directly routable IPs from VZ NAT (bridge100).
- **No yaml, no templates, no per-distro code paths.** Flags, defaults, and a dumb
  alias→URL image map.
- **Clusters are a naming convention** (`prefix-N`), never an entity with state.
- **Cattle with persistence.** `up` is idempotent, disks survive reboots, `rm` is the
  only destructive command.

## Architecture summary

```
fleetbox.go                     public API: Start/StartN, VM, Options, NestedVirtSupported
fleetboxtest/                   testing.TB fixtures: Start(t, image), StartN, SkipIfShort
internal/backend                Backend interface, compile-time selection per platform
internal/backend/vz             VZ implementation (the only vz import site)
internal/image                  cloud image download/verify/qcow2→raw/cache
internal/seed                   cloud-init NoCloud seed ISO (via pilat/cloudiso)
internal/store                  ~/.fleetbox/vms/<name>/ state, config.json, locking
internal/dhcp                   /var/db/dhcpd_leases parsing (hostname → IP)
internal/sshkey                 keypair + x/crypto/ssh client
internal/runner                 CLI-mode VM holder process (re-exec, pidfile, socket)
cmd/fleetbox                    CLI: up/down/ls/ssh/cp/ssh-config/rm
spike/                          standalone throwaway prototype (own go.mod) — ignore it
```

Key external deps: `Code-Hex/vz/v3`, `pilat/cloudiso`, `go-qcow2reader`,
`golang.org/x/crypto/ssh`.

## Build & test notes

- Binaries (CLI and test binaries) need the `com.apple.security.virtualization`
  entitlement — ad-hoc codesign is enough for dev. Use the Makefile targets; never run
  unsigned VM tests and wonder why they fail.
- Tests that boot real VMs are separated (`make test-vm`) and only run on darwin/arm64
  with nested-virt-capable hardware (M3+). Plain `make test` (unit tests) also requires
  darwin/arm64 — the root package is build-tagged `darwin && arm64`, so the module does
  not compile on other platforms. CI runs on macos-latest for this reason; never switch
  it to ubuntu runners.
- CI (GitHub-hosted runners) cannot boot VZ VMs — CI runs lint + build + unit tests
  only. Do not write CI workflows that pretend otherwise.
- Commands: `make test` (unit), `make test-vm` (boots real VMs), `make lint`,
  `make build` (compile + codesign the CLI). No generic sign-test target — signing a VM
  test binary for another package is a manual `go test -c` + `codesign`.

## Go style

Follow the global conventions (docs/coding-style.md): declaration order
const→var→type→exported→unexported, `var _ Iface = (*impl)(nil)`, New() constructors,
flat error handling with `fmt.Errorf("context: %w", err)`, sentinel errors caught once
at the caller. Every exported symbol gets a doc comment — this is a library.

## Known deviations from spec

- **VM-to-VM connectivity does NOT work with VZNATNetworkDeviceAttachment.** The spec
  claimed "VM→VM" works, but VZ NAT isolates VMs from each other. VMs can reach the
  host and internet, but not other VMs on the same NAT. Options for v1:
  - Bridged networking (requires `com.apple.vm.networking` entitlement = Developer ID)
  - FileHandleNetworkDeviceAttachment with socket_vmnet
  For v0, single-VM testing is the target; multi-node cluster testing is deferred.

- **IP discovery uses hostname, not MAC.** VZ uses DUID-based identifiers in
  dhcpd_leases (hw_address=ff,...) instead of traditional MAC format. cloud-init sets
  the hostname via DHCP, so we look up by hostname instead.

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
