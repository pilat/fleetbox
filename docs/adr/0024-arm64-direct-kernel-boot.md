# ADR: Direct Kernel Boot on arm64 (cloud-hypervisor)

**Date:** 2026-06-12
**Status:** Accepted

## Context

The Linux backend boots a VM by handing cloud-hypervisor the pinned
`rust-hypervisor-firmware` as `--kernel`; the firmware then chain-loads the guest
kernel from the disk's boot entry (ADR-0011). This was only ever exercised on
**x86_64** — the `pet` dev box and the `vm-linux.yml` CI runner are both amd64. The
arm64 cloud-hypervisor path shipped untested.

Dogfooding fleetbox to test itself surfaced that it is broken there. On an Apple M4
(macOS 26), fleetbox boots a Debian arm64 guest with a working `/dev/kvm` (nested
virtualization), and inside it we ran `fleetbox` (linux/arm64) to boot a nested VM.
The result, reproduced by hand:

- **cloud-hypervisor + `rust-hypervisor-firmware-aarch64`**: the vCPUs start and
  cloud-hypervisor emits its `booted` event, then the guest produces **zero serial
  output** and never boots — the firmware does not execute the guest.
- **qemu + EDK2/AAVMF** (a control): a deterministic `Synchronous Exception` inside
  the firmware, identical for Debian and Ubuntu, independent of smp/highmem/pflash.
- **Direct kernel boot (no firmware)**: both Debian and Ubuntu boot cleanly to a
  login prompt; cloud-hypervisor with `--kernel`/`--initramfs`/`--cmdline` boots
  Debian through systemd.

So the firmware *approach* is the problem on arm64, not the hardware (nested virt
executes guest code fine) and not the distro.

## Decision

**On arm64, boot the guest kernel directly; keep the firmware on x86_64.**

The boot configuration is now arch-specific (`bootArgs` in `boot_amd64.go` /
`boot_arm64.go`):

- **amd64** — unchanged: `--kernel <rust-hypervisor-firmware>`.
- **arm64** — `--kernel <vmlinux> --initramfs <initrd> --cmdline "console=ttyAMA0
  root=/dev/vda1 rw"`. The kernel and initrd are extracted once from the image's
  own `/boot` (loopback-attach the raw disk with partition scanning, mount the
  partition holding the kernel, copy `vmlinuz`/`initrd.img`, decompressing a gzip
  Image since cloud-hypervisor's aarch64 `--kernel` needs it raw) and cached next to
  `disk.raw`. The extraction runs as root — the Linux holder already is (ADR-0023).

## Alternatives Considered

**A different firmware (EDK2/AAVMF instead of rust-hypervisor-firmware).** Rejected:
EDK2 also faults under Apple-Silicon nested virt (the `Synchronous Exception`
control above), and on bare-metal arm64 it would add a second pinned artifact for no
gain over a direct kernel boot.

**A newer rust-hypervisor-firmware.** Rejected: upstream marks the aarch64 build
experimental ("works with a custom Ubuntu bionic cloud image"), and the firmware
*class* is the failure here — a direct boot sidesteps it entirely and is a path
cloud-hypervisor supports first-class.

**Leave arm64 on the firmware path / drop arm64.** Rejected: fleetbox claims
linux/arm64 support, and the firmware path simply does not boot there.

## Consequences

- arm64 pays a one-time per-image extraction (loopback mount + copy) at first boot;
  it is cached in the VM dir, so reboots skip it.
- **Stale-kernel tradeoff:** the extracted kernel is the image's kernel at first
  boot. A guest that later updates its own kernel keeps booting the original until
  `rm`+`up`. Acceptable for cattle VMs (the firmware path read the on-disk kernel
  every boot; this does not).
- The cmdline assumes a debian-shaped image (`root=/dev/vda1`). fleetbox's catalog
  is debian-pinned (ADR-0019), so this holds; a future image with a different root
  layout would need the root device derived during extraction.
- The aarch64 `rust-hypervisor-firmware` is now fetched but unused on arm64
  (harmless ~180 KB; left in place to keep `ensureBinaries` arch-uniform).
- This **unblocks the nested-on-mac dogfood**: on M3+ macOS, fleetbox boots a Linux
  guest with `/dev/kvm` and fleetbox-linux now boots a nested VM inside it. That is
  the basis for an arm64 integration gate that runs the whole stack locally —
  currently **local-only** behind a build tag (no CI yet; the runner needs an M3+
  macOS host), with the vector left for a future CI lane.
- References ADR-0011 (the cloud-hypervisor backend) and ADR-0019 (the debian-pinned
  catalog). It does not change the x86_64 path or the public API.
