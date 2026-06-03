# ADR: IP Discovery via /var/db/dhcpd_leases, Keyed by Hostname

**Date:** 2026-06-03
**Status:** Accepted

## Context

With NAT networking and no guest agent (ADR-0004, ADR-0005), fleetbox needs a way to
learn which IP a VM received from DHCP — purely from the host side. macOS bootpd writes
every lease it hands out to `/var/db/dhcpd_leases`, a plain-text file.

The original plan (same approach as Tart): generate a fixed MAC per VM, store it in
config.json, and find the lease whose `hw_address` matches that MAC.

## Decision

1. **Parse `/var/db/dhcpd_leases`** (read-only) to discover VM IPs. No guest-side
   reporting, no ARP scanning, no network probing beyond a TCP :22 reachability check.
2. **Look leases up by hostname, not MAC.** Discovered during implementation: VZ VMs
   appear in dhcpd_leases with DUID-based identifiers (`hw_address=ff,...`) rather than
   their plain interface MAC, so MAC matching does not work reliably. cloud-init sets
   the VM's hostname (which equals the fleetbox VM name), and bootpd records that
   hostname in the lease — so the name itself is the lookup key.
3. **The stable per-VM MAC is still generated and stored** (`backend.GenerateMAC`,
   derived from the name) — it keeps the VM's identity stable across boots so DHCP
   re-issues the same address, and `LookupByMAC` exists for backends/situations where
   MAC matching does work.

## Alternatives Considered

**Lookup by MAC (the Tart approach, and the original spec).** Doesn't work with VZ:
the DUID-form `hw_address` entries don't match the interface MAC. Kept as a secondary
code path (`LookupByMAC`) since the parsing is shared.

**Guest agent reporting its IP.** Rejected: violates ADR-0005 (no guest agent).

**ARP table scanning / ping sweep of bridge100.** Rejected: slower, noisier, racy
against DHCP, and requires knowing the subnet — the lease file is authoritative and
free.

## Consequences

- IP discovery depends on two facts: cloud-init sets the hostname, and the hostname is
  unique per VM. Both hold for fleetbox-created VMs (names are enforced unique by the
  store; the seed ISO sets the hostname).
- VM names must be valid hostnames — `fleetboxtest.safeName()` sanitizes test names
  accordingly.
- If macOS ever changes the dhcpd_leases format or location, IP discovery breaks. The
  parser (`internal/dhcp`) is isolated and unit-tested against the current format.
- Discovery is polling-based (1s interval, 2min timeout) — a small, bounded boot-time
  cost with no steady-state overhead.
