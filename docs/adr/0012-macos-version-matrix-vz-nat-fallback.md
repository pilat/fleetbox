# ADR: macOS Version Matrix — VZ NAT Single-VM Fallback Below 26, Intel Unsupported

**Date:** 2026-06-07
**Status:** Accepted

## Context

ADR-0008 moved networking to vmnet SharedMode and raised the platform floor to
macOS 26, trading away single-VM use on macOS 13–15 that VZ NAT had supported. It
also removed the NAT code path entirely. SharedMode is the only release that gives
VM↔VM connectivity (the NAT bridge marked each member `PRIVATE`), so clusters
genuinely require macOS 26.

Adding the Linux backend (ADR-0011) was the moment to lock the *whole* support
matrix rather than leave "<26 is unsupported" as an incidental consequence of
ADR-0008. Two facts drove the shape:

- Pre-26 Apple Silicon machines still exist and can run a single VM fine over VZ
  NAT — only VM↔VM is missing. Refusing to boot at all on them is stricter than
  necessary.
- Apple's Virtualization.framework never exposed nested virtualization on Intel,
  and macOS 26 barely runs on Intel hardware. An Intel backend is not worth
  building.

## Decision

1. **Resurrect a VZ NAT path for macOS < 26, capped at a single VM.** The vz
   backend detects the host major version once in `New` via
   `syscall.Sysctl("kern.osproductversion")` and caches it. `CreateNetwork` and
   the per-VM network attachment branch on it: **26+** → vmnet SharedMode (today's
   behavior, bit-for-bit); **< 26** → `VZNATNetworkDeviceAttachment`, a single
   isolated VM. On a sysctl/parse failure the version is treated as 0 (the
   conservative NAT path). IP discovery is unchanged for both (hostname /
   `dhcpd_leases`, ADR-0007) — NAT and SharedMode ride the same bootpd machinery.

2. **Clustering is a backend capability the public layer checks before booting.**
   `backend.Backend` gains `SupportsClustering() bool` (vz≥26 → true, vz<26 →
   false, cloud-hypervisor → true). `StartCluster`/`StartN`/`Cluster.Add` return
   `ErrClustersUnsupported` ("clusters require macOS 26+") when a *second* member
   is requested on a non-clustering backend — surfaced up front, not as a deep boot
   failure. A bare `Start` (single VM) never checks it, so single-VM use works on
   macOS < 26.

3. **Intel macOS is unsupported, permanently.** `darwin/amd64` compiles (so the
   module builds everywhere) but `newBackend` returns a clear "unsupported
   platform (darwin/amd64)" error at runtime. No Intel backend will be written.

## Alternatives Considered

**Leave < 26 entirely unsupported (status quo after ADR-0008).** Simpler, but
needlessly refuses a single VM on pre-26 Apple Silicon that VZ NAT handles fine.
The NAT attachment is a few lines behind a version branch; the cost of supporting
single-VM < 26 is low and the capability already existed before ADR-0008.

**Detect clustering capability by attempting a boot and catching the failure.**
Wasteful and opaque — it would boot the first member, then fail on the second.
A `SupportsClustering()` gate rejects up front with one clear error.

**A real `Network.Close` for the NAT path.** Unnecessary: the `< 26` path returns
a no-op `backend.Network` holder (the NAT attachment is per-VM), so there is no
shared object to release. SharedMode keeps its real vmnet holder.

**An Intel macOS backend.** Apple never exposed nested virt on Intel and macOS 26
barely runs there; not worth the maintenance.

## Consequences

- **The support matrix is explicit:** Apple Silicon macOS 26+ → clusters (vmnet
  SharedMode); Apple Silicon macOS < 26 → a single VM (VZ NAT); Intel macOS →
  unsupported; Linux amd64/arm64 → clusters (cloud-hypervisor, ADR-0011).
- **macOS ≥ 26 behavior is unchanged** — the version branch selects exactly the
  prior SharedMode path; only `< 26` takes the new NAT branch.
- **References, does not amend, ADR-0008 and ADR-0004.** ADRs are immutable. This
  one reintroduces a NAT *attachment* (per-VM, single-VM) that ADR-0008 removed; it
  does not revive ADR-0004's NAT-as-default decision or its since-overturned "VM↔VM
  is impossible" framing. ADR-0004's no-port-forwarding decision still stands.
- **The < 26 NAT path is best-effort verified.** It is exercised by unit tests of
  the version gate and the clustering rejection (with an injected version); a real
  pre-26 boot depends on having such hardware and is noted per-PR when done.
- **`ErrClustersUnsupported` is part of the public API** so callers (and the CLI)
  can branch on "this host can't cluster" via `errors.Is`.
