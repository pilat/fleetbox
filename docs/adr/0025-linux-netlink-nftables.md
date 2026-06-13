# ADR: Linux Host Networking via netlink + nftables, Scoped Forwarding, Self-Protecting Filter

**Date:** 2026-06-13
**Status:** Accepted (amends [ADR-0011](0011-linux-cloud-hypervisor-backend.md) and
[ADR-0013](0013-crash-safe-linux-network-lifecycle.md) — the Linux backend no longer
shells out to `ip`/`iptables`, and the global `ip_forward` flip is replaced by
per-interface forwarding)

## Context

The Linux backend (ADR-0011) built its shared bridge, per-VM taps, and egress
masquerade by shelling out to the `ip` and `iptables` binaries. That is fragile on the
"unpredictable" hosts fleetbox targets — a dev laptop, a KVM box, a CI runner, a server,
a VM: the binaries may be absent, or live in `/sbin`/`/usr/sbin`, which a regular user's
`PATH` omits (the reason commit `43a8580` had to band-aid a `PATH` fix-up). A test
fixture library should not require the operator to install or locate host networking
tools.

The same code also had a security posture problem worth fixing in the same pass. It
flipped the **global** `net.ipv4.ip_forward` switch, turning the whole host into a
router across *every* interface (a second LAN, a VPN, a management NIC), and it left the
FORWARD plane wide open — so a LAN attacker who guessed the guest `/24` could reach the
guests directly. Two distinct risks: (1) unsolicited inbound to the guest subnet, and
(2) transit routing across all the host's interfaces.

## Decision

**Replace every networking shell-out with direct kernel programming from pure Go.** The
Linux path stays cgo-free and depends on no host binary.

1. **`ip` → `github.com/vishvananda/netlink`.** Bridge create/address/up, tap
   create/enslave/up, and link delete are netlink calls. The tap is persistent (the
   netlink default — `NonPersist` is deliberately not set) so cloud-hypervisor reopens it
   by name on boot.

2. **`iptables` → `github.com/google/nftables`** (nf_tables over netlink, no `nft`
   binary). fleetbox owns one native nft table per bridge, in the **`ip` family** (IPv4
   only — our subnets are IPv4, and `ip` is strictly more compatible than `inet`, whose
   NAT chains need kernel ≥ 5.2 in a way the nf_tables probe would not catch). A single
   shared `nftTableName(bridge)` helper (`fbx-<id>` → `fbx_<id>`; nft identifiers dislike
   hyphens) names the table for create, teardown, and reconcile so they cannot drift.

3. **Per-interface forwarding; never the global switch.** On a clean host (global
   `conf.all.forwarding == 0`) fleetbox leaves it `0` and enables forwarding only on the
   bridge and the discovered uplink (`conf.<iface>.forwarding = 1` via `os.WriteFile` —
   no `sysctl` binary). On a host already forwarding globally (a router, a Docker host) it
   touches nothing. The uplink is discovered with `netlink.RouteGet(1.1.1.1)` → the
   route's link index → `net.InterfaceByIndex` (a routing-table query; no packet is
   sent); an offline host has no uplink, so uplink forwarding is skipped (masquerade is
   moot, VM↔VM and VM↔host still work). The uplink needs forwarding because NAT return
   traffic ingresses there — the kernel's effective decision is
   `all.forwarding OR conf.<ingress-iface>.forwarding`. The bridge's flag is ephemeral
   (it dies with the bridge — no restore); the uplink's original is preserved
   first-writer-wins in a per-uplink marker (`fwd-<iface>.orig`) and restored only when no
   fleetbox network remains.

4. **A self-protecting forward filter (closes risk 1).** The table's `filter`/`forward`
   chain has **policy accept** (it clamps nothing — `accept` is non-terminal, so it
   shadows no other table and breaks no other bridge) and holds one targeted drop:
   `ct state new` AND `iifname != <bridge>` AND `ip daddr <subnet>` → `drop`. This drops
   unsolicited inbound to the guests from any non-bridge interface while letting
   VM-originated traffic and established/related returns through.

5. **Masquerade for egress.** The `nat`/`postrouting` chain holds
   `ip saddr <subnet> oifname != <bridge> masquerade` — the equivalent of the old
   `-t nat -I POSTROUTING -s <subnet> ! -o <bridge> -j MASQUERADE`.

6. **The write-ahead record (ADR-0013) is preserved and extended.** It drops the now-moot
   `masquerade` boolean (the firewall is one nft table deleted whole by name) and gains
   the uplink interface name and its observed original forwarding value, written *before*
   the flip. Teardown and reconcile delete the nft table whole, `LinkDel` the taps and
   bridge, and restore the uplink's forwarding when nothing of ours remains. Reconcile
   stays idempotent and self-healing.

All of this lives inside `internal/backend/cloudhypervisor`. The public API, the control
protocol, and the holder/orchestrator are unchanged.

## Alternatives Considered

**`coreos/go-iptables`.** Still `exec`s the `iptables` binary — it solves none of the
"no host binary, no `PATH` games" problem. Rejected.

**Punching `DOCKER-USER` / iptables-nft compatibility chains.** Unsafe: iptables-nft
ownership checks abort with "table incompatible" and break the host's own tooling,
Docker's native-nftables backend has no `DOCKER-USER` at all, and it would mean
per-tool code paths (which the project forbids). Rejected in favor of our own
independent table.

**The global `ip_forward` switch (ADR-0013's original choice).** Simpler, and what
Docker/libvirt/LXC reach for, but it turns the host into an all-interface router — risk
2. ADR-0013 weighed per-interface forwarding and declined it as "more surface for little
gain." This ADR reverses that: with the shell-out gone we are rewriting this code anyway,
and per-interface forwarding is the only way to enable routed egress without the global
clamp.

**Clamping the host FORWARD policy to DROP (Docker's self-protection).** That is exactly
what breaks every other bridge on the host. We get most of the protection from a
subnet-scoped drop in our own table without touching anyone else's traffic.

**Detecting a Docker/ufw FORWARD-DROP and warning the user.** A host where Docker or ufw
has clamped FORWARD to DROP will block our guests' egress regardless of our rules (nft
`accept` is non-terminal — it cannot override a foreign table's `drop`). Every tool
(systemd, libvirt, netavark, LXD, Tailscale) lives with this ceiling. Detection was
explored and de-scoped: it would expand the control protocol (no warning channel exists
today) for a heuristic with real blind spots (legacy-xtables, firewalld rule-drops). The
ceiling is a documented limitation, not a code path.

## Consequences

- **Zero host-side runtime dependencies for networking.** No `ip`, no `iptables`, no
  `nft`, no `sysctl` — everything is netlink and `/proc` writes from pure Go (the Go
  *module* deps are compiled in, not host prerequisites). The `ensureSbinInPath` `PATH`
  band-aid's reason for existing is gone, though removing it (it touches the elevation
  path, ADR-0023) is left to a follow-up.
- **Strictly better host security posture.** The host is never turned into an
  all-interface router (risk 2 closed for the common clean-host case), and unsolicited
  inbound to the guest subnet is dropped (risk 1 closed) — while we remain polite: no
  global clamp, so no other bridge on the host breaks.
- **Two errno-discriminated capability probes.** The first netlink write (the bridge
  create) is the CAP_NET_ADMIN gate (`errors.Is(err, EPERM)` → "needs root"); an nf_tables
  probe runs right after and discriminates `EPERM` (needs root) from
  `EOPNOTSUPP`/`ENOENT` (kernel lacks nf_tables), so a non-root user sees one coherent
  error.
- **A read-back guard against a silent nftables footgun.** `google/nftables` surfaces
  errors only on `Flush()`, and an unsupported expression can make `Flush()` return nil
  while the kernel drops the rule (issue #170). After install we read the ruleset back and
  fail loudly unless the table, both chains, the masquerade verdict, and the forward
  drop's match all survived.
- **Accepted egress ceiling.** On a host where Docker or ufw has clamped FORWARD to DROP,
  the guests cannot reach the internet. Documented, not worked around.
- **Irreducible uplink-transit residual.** Keeping the uplink's forwarding flag on permits
  uplink-ingress transit to the host's other routed networks — the inherent cost of routed
  egress without a global clamp. Documented, not chased.
- **No cross-version migration.** The new reconcile deletes nft tables and restores
  per-uplink forwarding; it will not clean a record written by the old iptables-era code
  (its `MASQUERADE` rule and global `ip_forward` flip are invisible to the new sweep).
  fleetbox is pre-release: delete `~/.fleetbox` by hand when upgrading across this change.
- **Dogfood-proven, not just compiled.** For network code, compile and lint prove nothing.
  The VM-boot CI (`vm-linux.yml`) now asserts internet egress over SSH from the booted
  guest (`ping 1.1.1.1`), so a missing or silently-dropped masquerade rule fails CI rather
  than passing a green build.
