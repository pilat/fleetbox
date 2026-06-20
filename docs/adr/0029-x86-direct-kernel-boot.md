# ADR: Direct Kernel Boot on x86_64; Remove rust-hypervisor-firmware

**Date:** 2026-06-20
**Status:** Accepted (amends [ADR-0011](0011-linux-cloud-hypervisor-backend.md) — the
x86_64 firmware boot is replaced by a direct kernel boot; amends
[ADR-0024](0024-arm64-direct-kernel-boot.md) — the "keep the firmware on x86_64" half is
withdrawn, both arches now direct-boot; amends
[ADR-0027](0027-arm64-kernel-extraction-in-process.md) — the in-process go-diskfs
extraction is no longer arm64-only, it runs on both arches and its go-diskfs wiring moves
to `extract_linux.go`)

## Context

The Linux backend booted x86_64 VMs by handing cloud-hypervisor the pinned
`rust-hypervisor-firmware` as `--kernel`; the firmware then chain-loaded the guest
kernel+initrd from the disk (ADR-0011). arm64 already direct-boots (ADR-0024). Every VM
test and CI lane booted only `debian-12` (kernel 6.1, small initrd), which the firmware
still handles — so the firmware path looked healthy.

It is not. On a modern image (`ubuntu-26.04`, kernel 7.0, a 69 MB initrd) the firmware
loads the kernel but **fails to hand over a working initrd**, so the kernel cannot resolve
`root=LABEL=cloudimg-rootfs` and panics. Reproduced live on the amd64 `pet` box: the
serial log shows `VFS: Cannot open root device` → `Kernel panic … Unable to mount root
fs`, with the call trace going straight `prepare_namespace → mount_root` at ~1.3 s — i.e.
no initramfs ever came up (a working initrd would exec `/init` and resolve the label via
udev). Extracting that image's own `vmlinuz` (a bzImage) + initrd and booting
cloud-hypervisor directly (`--kernel <bzImage> --initramfs <initrd> --cmdline
"console=ttyS0 root=/dev/vda1 rw"`) reaches systemd — root mounts, panic gone. The
firmware *class* is the limit (it cannot load a large modern initrd), the same conclusion
ADR-0024 reached on arm64 for a different reason.

## Decision

**Both arches direct-boot the guest kernel; delete `rust-hypervisor-firmware` entirely.**

- `bootArgs` is **unified** (one body in `extract_linux.go`, `//go:build linux`): it
  extracts the image's own kernel+initrd once (cached next to `disk.raw`, short-circuited
  on a cache hit) and returns `--kernel <vmlinux> --initramfs <initrd> --cmdline
  <bootCmdline>`. Only `bootCmdline` is per-arch (`console=ttyS0` on x86_64, `console=ttyAMA0`
  on aarch64; both `root=/dev/vda1 rw`). The in-process pure-Go go-diskfs extraction of
  ADR-0027 (`extractKernel`/`diskPartitions`, the GPT→ext4 read, the whole-device fallback)
  moves out of the arm64-only file into `extract_linux.go` and now links into both linux
  backends. `copyKernel`'s magic-based gunzip is unchanged: a bzImage's magic is not gzip,
  so the x86 kernel passes through verbatim while the arm64 gzip `Image` is still
  decompressed — no arch branch.
- `root=/dev/vda1` stays **hardcoded per-arch**, not derived from the image. Every catalog
  image (debian 11/12/13, ubuntu 22.04/24.04/26.04) puts root on partition 1, and the real
  initrd's udev mounts it. Deriving `root=`/cmdline from an arbitrary `WithImage(url)` image
  is **deferred** (the BYO-image case), matching arm64's existing shortcut.
- `rust-hypervisor-firmware` is removed from both arches: it was already *fetched but unused*
  on arm64 (ADR-0024), and is now unused on x86_64 too. `ensureBinaries` returns only the
  cloud-hypervisor binary; `fwBinaries`/`fwVersion`/`VM.fwPath` are gone.

## Alternatives Considered

**A newer rust-hypervisor-firmware.** Rejected: the firmware class is the failure (it cannot
hand over a large modern initrd), and a direct boot sidesteps it entirely — the path
cloud-hypervisor supports first-class. The same reasoning retired the firmware on arm64
(ADR-0024).

**Derive `root=` from the image during extraction.** Rejected for now: every catalog image
roots on p1, so the hardcoded cmdline is correct for the whole supported set; deriving it is
only needed for non-catalog BYO images and is deferred with the rest of that case.

**Keep the firmware artifact "just in case."** Rejected: it is dead on both arches after this
change, and a checksum-pinned download nobody uses is pure maintenance cost (the user called
it a "мёртвый артефакт"). Its history is here if the decision ever needs revisiting.

## Consequences

- **Modern images boot on x86_64.** `ubuntu-26.04`/`debian-13` (and any future large-initrd
  image) boot to SSH-ready; `debian-12` still boots. The catalog matrix (ADR-0030) is what
  keeps a non-bootable entry from shipping green behind `debian-12` again.
- **x86_64 pays the same one-time per-image extraction arm64 does** (read the image, copy out
  kernel+initrd), cached in the VM dir so reboots skip it. The stale-kernel tradeoff of
  ADR-0024 now applies to both arches: the extracted kernel is the image's kernel at first
  boot; a guest that later updates its own kernel boots the original until `rm`+`up`.
- **go-diskfs now links into the amd64 holder too** (arm64 only before). Pure Go, no cgo,
  still behind the linux-only backend — it never reaches the pure-Go client. ADR-0027's
  "arm64 only" qualifier on the dependency no longer holds.
- **One fewer pinned download.** `ensureBinaries` fetches only the cloud-hypervisor binary;
  no firmware artifact, no `~/.fleetbox/bin` firmware file.
- **Deferred:** deriving `root=`/cmdline from an arbitrary BYO image. Until then a
  non-catalog image whose root is not on p1 will not boot — a known, accepted gap.
- Touches **only** the cloud-hypervisor backend. The VZ (macOS) path, the public API, and the
  on-wire holder protocol are unchanged.
