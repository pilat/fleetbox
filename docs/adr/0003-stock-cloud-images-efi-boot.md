# ADR: Stock Cloud Images, EFI Boot, and a Dumb Image Catalog

**Date:** 2026-06-03
**Status:** Accepted

## Context

A VM harness needs guest images. The options range from shipping custom-built images
(full control, heavy maintenance) to booting whatever the distros publish (zero
maintenance, must cope with what they provide). Some tools patch cloud images and
maintain per-distro templates; others extract kernel/initrd and boot them directly.

## Decision

1. **Boot stock genericcloud images, unmodified**, via `VZEFIBootLoader` — the image's
   own GRUB does the rest. No custom kernels, no kernel/initrd extraction, no image
   patching.
2. **The image catalog is a dumb map**: alias → URL + optional sha256
   (`internal/image.Catalog`). Built-in aliases (`debian-12`, `ubuntu-24.04`) plus any
   user-supplied URL. One code path for all images.
3. **Raw images are used as-is; qcow2 is converted** to raw once at download time using
   a pure-Go qcow2 reader. The default image (Debian 12 generic arm64) is raw and needs
   no conversion.
4. **Adding a distro = adding a map entry.** Per-distro code paths are forbidden.

## Alternatives Considered

**Custom-built images with software pre-installed.** Rejected for v0: maintenance
burden (rebuilds for every distro update), hosting question, and it breaks the "guest
is a stock distro" property that makes test results trustworthy. The v1 idea of a
payload disk (go-ext4fs, second disk with pre-built binaries) achieves the same speed
goal without touching the boot image.

**Kernel/initrd extraction.** Rejected: requires per-distro knowledge of where kernels
live inside images, breaks on image layout changes, and bypasses the distro's own boot
path (GRUB config, initramfs hooks).

**Template/config system for image definitions.** Rejected: yaml templates are exactly
the moving part this project exists to remove.

## Consequences

- Any distro that publishes an EFI-bootable arm64 cloud image with cloud-init works.
  Distros that don't, don't — there is no escape hatch, by design.
- First boot of a new image pays a download (and possibly a qcow2→raw conversion);
  subsequent boots hit the cache in `~/.fleetbox/images/`.
- Checksums are only verified when the catalog entry has one; "latest" URLs can't have
  stable checksums. Accepted trade-off for tracking distro updates automatically.
