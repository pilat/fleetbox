# ADR: Linux Backend via cloud-hypervisor, Provisioned by Download-and-Cache

**Date:** 2026-06-07
**Status:** Accepted (superseded in part by [ADR-0019](0019-pin-cloud-images.md) —
cloud images are now checksum-pinned to dated snapshots exactly like the binaries
below; the "unlike cloud images, which may stay unpinned for latest" aside in
Decision 3 no longer holds. Amended by [ADR-0020](0020-helper-thin-backend-server.md):
the cloud-hypervisor backend now runs inside a helper process (reached by self-reexec)
rather than in-process, driven by a client-side orchestrator.)

## Context

Until now the whole module was build-tagged `darwin && arm64` and ran only on
Apple Silicon via the VZ backend (ADR-0002, ADR-0008). The backend interface was
always meant to admit a second platform (ADR-0002: "one backend per platform,
selected at compile time"), and a Linux/KVM backend was named as the obvious next
one (ARCHITECTURE.md §7). The goal: a consumer can `go get` fleetbox and call the
same `vm.Start(...)` API on macOS *or* Linux and get a real Linux VM.

On Linux there is no in-OS hypervisor framework to bind to the way VZ is on macOS.
A VM monitor (VMM) is a separate program. The question was which one, and how to
ship it to a consumer who just `go get`s a library.

Two fleetbox invariants shaped the answer. **Stock distro, nothing of ours in the
guest** (ADR-0005): the VMM must boot an unmodified cloud image, no custom kernel
or rootfs. **Directly-routable IPs, no port forwarding** (ADR-0004): the VMM must
attach to a real L2 network, not a userspace socket-NAT shim. And **library-first
≠ VMM-in-process**: the product boundary is the Go API, not the process boundary —
a library that owns a subprocess and talks to it over a socket is still a library.

## Decision

1. **The Linux VMM is cloud-hypervisor (CH).** ~50k LOC of Apache-2.0 Rust — both
   the VMM and its firmware (`rust-hypervisor-firmware`). It boots stock cloud
   images (virtio-pci + ACPI/PVH), boots in ~200ms, and is driven over a unix
   socket REST API. Apache-2.0 sidesteps the GPL-redistribution problem QEMU would
   have created.

2. **The Linux path is pure Go — no cgo.** CH runs as a child process
   (`os/exec`); fleetbox controls it over its `--api-socket` with stdlib
   `net/http` (a custom unix-socket dialer). The whole VM configuration is passed
   on the command line, so CH boots the VM on launch; the REST API is used for
   readiness (`vm.info`) and graceful shutdown (`vm.shutdown`), then SIGTERM/SIGKILL
   as a fallback. No second binary (`ch-remote`) is fetched.

3. **The VMM binary and firmware are downloaded and cached at runtime, not
   embedded.** fleetbox already downloads the cloud image at runtime
   (`internal/image`); the VMM binary and firmware are the same kind of artifact,
   fetched the same way into `~/.fleetbox/bin/`. A new `internal/fetch` package
   holds the shared download → verify → atomic-rename → cache primitive that both
   `internal/image` and the CH backend use. **Every downloaded executable and
   firmware is checksum-pinned** (version + per-arch SHA256, in a Go table next to
   the backend). *(This originally added "unlike cloud images, which may stay unpinned
   for latest" — [ADR-0019](0019-pin-cloud-images.md) closed that gap: cloud images
   are now pinned to dated snapshots with per-arch SHA256 too, via an embedded
   catalog.json, and the snapshot is stamped into the image cache filename the same
   way the version is stamped into the binary's.)* cgo is never introduced for the
   binaries; `go:embed` is used for the image catalog data (not for any binary).

4. **Linux networking is a shared Linux bridge + per-VM tap + static IP via
   cloud-init.** `CreateNetwork` makes one bridge per cluster on a free `/24`
   (mirroring the VZ subnet detector), assigns it the gateway (`.1`), enables IPv4
   forwarding, and installs `iptables` MASQUERADE/FORWARD rules for egress.
   `Create` makes a tap, enslaves it to the bridge, and passes `--net tap=…` to CH.
   The orchestrator allocates each VM a static address from the subnet and injects
   it via a NoCloud `network-config` (netplan v2, matched by MAC). VMs on one
   bridge reach the host, each other, and the internet on one NIC — the macOS
   SharedMode property (ADR-0008), reproduced on Linux. This avoids a `dnsmasq`
   dependency and the `dhcpd_leases` discovery dance: the backend already knows the
   IP. Port forwarding / slirp is never used.

5. **IP discovery moves behind the backend.** `backend.VM` gains
   `WaitForIP(ctx) (string, error)`: vz keeps the hostname/`dhcpd_leases` lookup
   (ADR-0007); CH returns its statically-assigned address after a TCP:22 probe.
   This removes the last platform coupling (`dhcp.LookupByHostname`) from the root
   package and makes the public API genuinely platform-neutral.

6. **No live host↔guest folder share on this backend.** CH has no built-in
   virtio-fs (it needs an external `virtiofsd`) and no virtio-9p, so it ships with
   no live folder-share capability. *(This was the one capability the Linux backend
   lagged macOS on. It was resolved by [ADR-0015](0015-fixture-payload-ext4.md):
   rather than take a `virtiofsd` host dependency, live mounts were dropped on both
   platforms in favor of read-only `WithFixture` ext4 payloads, which both backends
   attach as a plain read-only block device with no daemon. There is no
   `ErrMountsUnsupported` — `WithMount` no longer exists.)*

## Alternatives Considered

**QEMU.** A sibling project embeds a static QEMU + QMP, so there was a
head start. Rejected anyway: QEMU is GPLv2 (a redistribution headache for a
`go get` library), ~2M LOC, and heavy. cloud-hypervisor is "the useful 3% of QEMU,
already rewritten in a memory-safe language" and is Apache-2.0.

**firecracker.** Does not boot stock cloud images — it needs a custom kernel and
rootfs, violating the stock-distro invariant (ADR-0005).

**libkrun.** The only in-process (cgo, C-ABI) option, and it even runs on macOS
HVF. Rejected: it bundles its own kernel (`libkrunfw`), its stated non-goal is
"conventional virtualization workloads," and its TSI socket-impersonation
networking is the opposite of routable IPs. It violates three invariants
(stock distro, nothing-of-ours-in-guest, routable IPs) and forces cgo.

**A self-written `/dev/kvm` VMM in Go.** `gokvm` shows the ~1.5k-LOC core is
possible, but the gap to booting a *stock* cloud image (virtio-pci, ACPI, PCI host
bridge, an arm64 path with GIC/PSCI/FDT, production virtqueue correctness) is
months-to-years and turns fleetbox into a hypervisor maintainer.

**`go:embed` the VMM into the module.** Would bloat every consumer's build with
committed multi-MB per-arch binaries linked into their program. Download-and-cache
preserves the `vm.Start()` "just works" experience without the weight.

## Consequences

- **fleetbox is cross-platform.** The same Go API boots VMs on macOS (Apple
  Silicon, VZ) and Linux (amd64/arm64, CH). The module compiles on `darwin/arm64`,
  `linux/amd64`, and `linux/arm64`; other targets fail with a clear "unsupported
  platform" error (see ADR-0012).
- **New packages and one new module dependency edge.** `internal/backend/
  cloudhypervisor` (the only CH import site, mirroring the vz isolation rule) and
  `internal/fetch` (the shared download primitive) join the tree. `fetch` is a
  low-level utility imported by both `internal/image` and the CH backend — a
  deliberate exception to "building-block packages don't import each other"
  (coding-style B.1.2), recorded here.
- **Host prerequisites the library cannot provision.** Linux needs `/dev/kvm`
  (user in the `kvm` group) and `CAP_NET_ADMIN` (to make the bridge and taps);
  both are probed with clear errors. CH is KVM-only with no TCG fallback, so it
  cannot run where `/dev/kvm` is absent (e.g. Docker Desktop on macOS). IPv4
  forwarding is enabled host-wide and left enabled on teardown.
- **A subprocess lifecycle to own.** The backend (library mode) and the holder
  (CLI mode) are responsible for killing the CH process and removing the tap and
  bridge; teardown is explicit (`Cluster.Close`, `VM.Destroy`) because OS network
  resources are not GC'd the way the macOS vmnet object is.
- **No cgo on the Linux path**, so cross-compilation and CI on Linux runners are
  straightforward — and, unlike the VZ backend, the CH backend is testable in CI
  (GitHub Linux runners can expose `/dev/kvm`). Wiring that CI is a follow-up.
- **No live folder share on this backend; read-only fixtures cover both platforms
  instead** (ADR-0015). cloud-hypervisor has no virtio-fs, so rather than keep mounts
  macOS-only, live mounts were dropped everywhere in favor of `WithFixture` — a read-only
  ext4 payload attached as a plain block device, no daemon.
- **Two follow-ups before Linux is production-grade**, both needing real hardware to
  validate: (1) the static IP/gateway are baked into the seed at create but the bridge
  subnet is re-picked on each `up`, so rebooting a stopped VM is only reliable while its
  `/24` stays free — persisting and reusing the subnet would close this; (2) the arm64
  boot path (cloud-hypervisor + `rust-hypervisor-firmware` aarch64) is pinned but
  unverified end-to-end. amd64 is the well-trodden path.
