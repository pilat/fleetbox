# ADR: Crash-Safe Linux Network Lifecycle, Guest DNS, and Download-Aware Readiness

**Date:** 2026-06-07
**Status:** Accepted (amended by [ADR-0020](0020-helper-thin-backend-server.md) — the network lifecycle and reconcile are now owned by the helper process, keyed on the helper PID; amended by [ADR-0025](0025-linux-netlink-nftables.md) — teardown/reconcile now program netlink + nftables instead of `ip`/`iptables`, the `netRecord` drops `masquerade` and gains the uplink + its original forwarding value, and the global `ip_forward` marker becomes a per-uplink forwarding marker)

## Context

ADR-0011 added the Linux cloud-hypervisor backend. A real-hardware shakedown (an
amd64 Ubuntu 22.04 box) surfaced three gaps the unit tests could not:

1. **Guests could not resolve DNS.** The seed's `network-config` pinned the bridge
   gateway (e.g. 192.168.1.1) as the guest nameserver. On Linux that gateway is
   just an address on the host with masquerade — it runs no resolver (fleetbox
   deliberately has no dnsmasq, ADR-0011). Guests reached the internet by IP but
   every name lookup failed, breaking `apt`, `curl <host>`, etc.

2. **The first `up` falsely reported failure.** The CLI readiness wait
   (`waitForMembers`, 5 min) charged the one-time image + VMM download against the
   per-boot budget. A multi-GB image (debian-12 is a ~3.2 GB raw) took ~5 min to
   fetch, exhausting the budget before boot even started; the CLI printed
   `timeout waiting for: vm1` while the holder went on to boot the VM fine.

3. **A crashed holder leaked host state irreversibly.** All teardown (bridge, taps,
   iptables rules, ip_forward) ran from the holder's `Network.Close`. If the holder
   was killed (`kill -9`, OOM, reboot) nothing ran it: the bridge, taps, egress
   rules, and the orphaned cloud-hypervisor VMs all leaked, with no command to
   reclaim them. Separately, `ip_forward` was set to 1 unconditionally and never
   restored.

These are hardening of ADR-0011, decided together after the shakedown.

## Decision

1. **Guest DNS uses public resolvers, not the gateway.** `buildNetworkConfig`
   emits `nameservers: [1.1.1.1, 8.8.8.8]`; the gateway stays the default route
   only. Hardcoded, not host-derived: the host's own resolver is often a loopback
   stub (systemd-resolved 127.0.0.53) unreachable from the guest, and fleetbox's
   "dumb defaults" philosophy favors a fixed value over per-distro probing.

2. **Readiness separates the download from the boot budget.** The runner gains a
   `downloading` member state. The holder marks members `downloading` around the
   one-time `NewCluster` pull and flips them to `starting` before booting.
   `waitForMembers` does not spend the per-boot deadline while any member is
   `downloading` (a separate 30-min hard cap still bounds a stuck pull), and the
   CLI prints `Pulling …`. `GetStatus` now treats a live holder as authoritative —
   it reads the holder socket *before* checking on-disk `config.json`, because a
   downloading member has a live socket but no config yet.

3. **A VM never outlives its holder.** Each cloud-hypervisor child is started with
   `Pdeathsig: SIGKILL`, so the kernel kills it the instant its parent dies — even
   on `SIGKILL`/OOM/panic, which no `Close` can catch. Because that signal fires on
   the death of the parent *thread* (and Go multiplexes goroutines across threads),
   the holder pins its main goroutine with `runtime.LockOSThread`; every VM is
   booted from that stable, holder-lived thread, so the signal fires only when the
   holder actually exits. This removes the expensive leak (running VMs) entirely,
   without anyone running a command.

4. **Inert host resources follow a write-ahead lifecycle, reconciled on up and
   down — never by the user.** The cloud-hypervisor backend persists one record per
   bridge under `~/.fleetbox/networks/<bridge>.json` (`{bridge, subnet, owner_pid,
   masquerade, taps}`), owned entirely by the backend — nothing leaks into the
   neutral per-VM `config.json`. The record is written *before* the matching
   `ip`/`iptables` commands and deleted *only after* teardown is verified (the
   bridge confirmed gone via `linkExists`), so the record is always a superset of
   what exists on the host. A reconcile runs automatically at the start of every
   `up` (inside `CreateNetwork`) and every `down`: for each record whose `owner_pid`
   is dead it removes the taps, egress rules, and bridge (and as a belt-and-braces
   also SIGKILLs any cloud-hypervisor still naming those taps), then deletes the
   record. A live owner's network is never touched, so reconcile is safe while other
   clusters run. There is **no user-facing cleanup command** — cleanup is the tool's
   job, triggered by normal use.

5. **ip_forward is flipped only when off and restored by the last network out.**
   `enableMasquerade` reads `ip_forward`; only if it is `0` does it set `1` and
   record the original in a first-writer-wins (`O_EXCL`) marker
   `~/.fleetbox/networks/ipforward.orig`. It is restored to the marker value once
   no network record remains *and* no `fbx-*` bridge exists on the host (a
   cross-process backstop). A host that already had forwarding on (routers, Docker
   hosts) is never touched and never restored.

`backend.Backend` gains `Reconcile() error` (vz: no-op — vmnet owns its state),
called automatically on `up` and `down`. `fleetbox.Prune()` exposes it for library
callers; there is deliberately no CLI `prune` command.

## Alternatives Considered

**DNS from the host's upstream resolvers.** Respects corporate/internal DNS, but
fragile and platform-specific: on systemd-resolved hosts `/etc/resolv.conf` is the
127.0.0.53 stub, unreachable from the guest, so it would need `resolvectl` parsing
per distro. Rejected for a fixed public default.

**A DNS forwarder on the gateway.** Would let the gateway-as-nameserver config
stand, but reintroduces the dnsmasq-class dependency ADR-0011 explicitly avoided.

**Just raise the readiness timeout.** A flat larger budget still cannot tell a
huge/slow download from a hung boot, and makes a genuinely stuck VM wait the full
budget before erroring. The `downloading` state is precise.

**Deterministic interface names (recompute on recovery instead of recording).**
Tempting — reuse the same bridge/tap names on re-up — but "a cluster is a naming
convention, not an entity" (ADR-0011) leaves no stable key for a bridge name. The
write-ahead record makes names recoverable without recomputation, so they stay
random.

**Reactive host-scan sweep (find `fbx-*`/`fbt-*`, guess ownership).** Without a
recorded owner there is no safe way to tell a live cluster's bridge from a leaked
one. The write-ahead record carries `owner_pid`, making "is this an orphan?" a
definite `pidAlive` test rather than a guess.

**A user-facing `prune` command (the first cut had one).** Rejected: it offloads
onto the user what the tool should handle itself. With `Pdeathsig` killing VMs on
holder death and reconcile running on every `up` and `down`, cleanup happens as a
side effect of normal use; a manual command would only ever be needed for "clean up
now without touching a VM," which is not worth a command. `fleetbox.Prune()` remains
for library callers that want to sweep explicitly.

**Per-interface forwarding (bridge + uplink) instead of the global switch.** More
surgical on multi-homed hosts, but NAT return traffic enters on the uplink, so the
kernel needs forwarding enabled there too (confirmed: a guest with `ip_forward=0`
and per-interface forwarding only on the bridge cannot reach the internet). That
means touching the host's real uplink NIC and tracking default-route changes —
more surface for little gain, since the FORWARD chain (not the sysctl) is the real
control. The global switch, flipped only when off and restored crisply, is simpler
and is what Docker/libvirt/LXC all do. Fencing the VM off the host LAN (dropping
RFC1918 in FORWARD) was considered and declined — the home-server operator wants
the VM to reach the LAN.

**Restore ip_forward inside disableMasquerade (per network).** Wrong with more than
one fleetbox network or another holder: the first teardown would disable forwarding
under a still-running cluster. Restoring only when the *last* record and bridge are
gone fixes both the in-process and cross-process cases.

## Consequences

- **Nothing of ours outlives a crash, and cleanup is automatic.** `Pdeathsig` kills
  a crashed holder's VMs instantly; its inert leftovers (bridge, taps, rules) are
  reclaimed on the next `up` or `down`, and `ip_forward` returns to its original
  once nothing of ours remains. Verified on real hardware: `kill -9` of a cluster
  holder left zero zombie VMs without any command run (pdeathsig), and a following
  `down`/`up` removed its bridge/taps/rules/record — while a second live VM's
  network and process were left fully intact.
- **New on-disk layout:** `~/.fleetbox/networks/` (per-bridge records +
  `ipforward.orig`), Linux-only, created on first network create. macOS never grows
  it; `config.json` is unchanged (no backend specifics leak in).
- **New public surface:** `fleetbox.Prune()`, `backend.Backend.Reconcile()`, and the
  runner `downloading` state. No new CLI command. On Linux `down` now also performs
  the `ip`/`iptables` reconcile, so (like `up`) it runs with root.
- **ip_forward is best-effort across a wiped store.** If `~/.fleetbox/networks/` is
  deleted while forwarding is on, the marker is lost and the value is not restored;
  the records are also the only orphan index, so a host-scan fallback is left to a
  future enhancement.
- **PID reuse is an accepted risk**, identical to the existing `runner.IsRunning`:
  reconcile trusts `owner_pid` + signal-0, so a reused PID could make a dead network
  look live. Negligible in practice.
- **macOS is unaffected:** vz `Reconcile` is a no-op, there are no network records,
  DNS still comes from DHCP (no `network-config` emitted), and the `downloading`
  state simply does not trigger when the image is already cached.
