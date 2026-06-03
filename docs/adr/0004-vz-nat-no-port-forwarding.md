# ADR: Native VZ NAT Networking, No Port Forwarding

**Date:** 2026-06-03
**Status:** Accepted

## Context

In comparable VM tooling, networking is the biggest source of fragility: a host agent
process does SSH port forwarding and userspace networking (slirp-style), which is slow,
opaque, and breaks in interesting ways. The whole reason fleetbox exists is to not have
that.

Apple's VZ gives VMs a NAT network (`VZNATNetworkDeviceAttachment`): every VM lands on
macOS `bridge100`, gets a DHCP address from bootpd, and is directly routable from the
host.

## Decision

1. **Native VZ NAT only.** Every VM gets a real IP on `bridge100`. Host→VM and
   VM→internet work with zero forwarding code.
2. **No port forwarding. Ever.** Consumers connect to the VM's IP directly. There is no
   port-mapping API, no forwarding config, and there never will be.

## Alternatives Considered

**SSH port forwarding.** Rejected: it's the pain being solved. Forwarding requires a
persistent agent, port allocation bookkeeping, and produces "works on my machine"
failure modes.

**Bridged networking (`com.apple.vm.networking`).** Deferred, not rejected: it makes
VMs reachable from the LAN and from each other, but the entitlement requires a paid
Developer ID and notarization — unacceptable friction for a dev-tool that should work
with an ad-hoc codesign.

**socket_vmnet / FileHandleNetworkDeviceAttachment.** Deferred: adds an external
daemon dependency; candidate for v1 multi-node support.

## Consequences

- Zero networking code in fleetbox beyond reading the DHCP lease file (ADR-0007).
- VMs are not reachable from other machines on the LAN. Accepted — test fixtures don't
  need to be.
- **Discovered after the spec was written: VM→VM traffic does NOT work over VZ NAT.**
  The spec assumed VMs on the same NAT could reach each other; in reality VZ isolates
  them. v0 initially targeted single-VM testing; multi-node cluster testing was deferred.

**Update (ADR-0008): the NAT-attachment choice and the VM→VM limitation are
overturned.** As of macOS 26, fleetbox uses `VZVmnetNetworkDeviceAttachment`
(vmnet SharedMode) instead of `VZNATNetworkDeviceAttachment`; VMs on a shared
network now reach each other, so multi-node clusters work (`StartN`). This ADR
stays **Accepted** because its core decision — **no port forwarding, ever** —
still stands; only the specific NAT attachment and its VM→VM consequence are
superseded. See ADR-0008 for the replacement and its rationale.
