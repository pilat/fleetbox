# ADR: arm64 Kernel Extraction In-Process via go-diskfs (no loopback mount)

**Date:** 2026-06-15
**Status:** Accepted (amends [ADR-0024](0024-arm64-direct-kernel-boot.md) — the
direct-boot decision and boot ABI are unchanged; only the *mechanism* that extracts the
guest kernel+initrd changes)

## Context

On `linux/arm64` fleetbox boots stock cloud images by direct kernel boot (ADR-0024): it
pulls the guest image's own `vmlinuz` and `initrd.img` out of the raw root disk and hands
them to cloud-hypervisor as `--kernel`/`--initramfs`. The original extraction
loopback-mounted the disk — `losetup --find --partscan`, `udevadm settle`, `mount -o ro`,
then `umount` + `losetup --detach`.

That was the last shell-out in the Linux backend (ADR-0025 removed every networking one),
and it carried the same costs ADR-0025 set out to eliminate: it needs `losetup`/`mount`
on `PATH` and root to run, and it mutates **global host state** — a loop device and a
kernel mount that leak if the holder is SIGKILL'd mid-extraction (its `Pdeathsig`,
ADR-0011/0020). It also worked around a udev race (the partition device nodes appear
asynchronously after `--partscan`) with `udevadm settle` plus a sleep-poll loop.

## Decision

**Read the kernel and initrd in-process with pure-Go `github.com/diskfs/go-diskfs`** —
no loopback mount, no shell-out. `extractKernel` opens the raw disk read-only
(`backend/file.OpenFromPath`), enumerates partitions from the GPT (`partition/gpt`), and
reads each as ext4 (`filesystem/ext4`) at its byte offset inside the whole-disk handle
until one yields a kernel — the same "debian keeps /boot inside root, ubuntu uses a
separate /boot partition" search the mount path did, now over an `fs`-style interface
instead of a mounted tree. A defensive whole-device fallback covers a disk with no usable
GPT (the catalog is GPT-only, ADR-0019, so it never triggers in practice). The gzip-Image
gunzip (cloud-hypervisor's aarch64 `--kernel` needs the Image raw; Debian ships it raw,
Ubuntu gzips it) and the atomic temp-then-rename of the cached output are preserved.

Only the **surgical** sub-packages are imported (`backend`, `backend/file`,
`partition/gpt`, `filesystem/ext4` — `backend` only for the `backend.Storage` type),
not the umbrella `diskfs` package — the umbrella pulls ~10 transitive
modules (logrus, lz4, xz, …); the surgical set adds only `github.com/google/uuid` as a new
external module. go-diskfs is pinned at `v1.9.3` and imported only from the `linux && arm64`
file, so it links into that binary alone. Pure Go, no cgo. The pure, library-agnostic
search/copy seam lives untagged in `bootextract.go` (mirroring `purehelpers.go`) so it is
unit-tested on the darwin dev box; the go-diskfs wiring stays in `boot_arm64.go`.

go-ext4fs stays the fixture **writer** (`internal/fixture`, ADR-0015) — its 0-dependency
ext4 write is its unique value. The split is deliberate: **go-diskfs reads, go-ext4fs
writes.**

## Alternatives Considered

**go-ext4fs for the read too** (it has gained byte-identical foreign-ext4 read support).
Rejected: keeping go-ext4fs a focused 0-dependency writer is worth more than saving the one
`uuid` dependency, and it avoids committing go-ext4fs to maintaining a general ext4 reader.

**go-diskfs for both, dropping go-ext4fs.** Rejected: porting the fixture writer means
replacing go-ext4fs's "16 GiB canvas → `Resize(MinSize())` shrink" trick (go-diskfs has no
ext4 resize) with new pre-sizing logic on go-diskfs's younger write path, disturbing an
already-working fixture path for no benefit to this change.

**`syscall.Mount`/loop device instead of shelling out.** Rejected: still mutates global
host state (leaks on SIGKILL) and still needs root — it addresses none of the core costs.

**Pin kernel+initrd in the catalog to skip reading ext4.** Rejected: the matching initrd
is generated per-image by initramfs-tools and is not separately publishable; the working
arm64 boot relies on the image's own initrd. Out of scope (catalog/image).

## Consequences

- **No shell-out.** This completes the ADR-0025 posture — the Linux backend now shells out
  to nothing for setup. (vm.go still `exec`s the cloud-hypervisor binary itself; that is the
  VMM process, not a host utility.)
- **No global host-state leak on SIGKILL.** There is no loop device and no kernel mount to
  leave behind when the holder's `Pdeathsig` fires mid-extraction. The only artifact a kill
  can leave is an unfinished `.tmp` in the VM dir, which the atomic rename already makes safe.
- **No udev race.** Reading the partition table and filesystem directly removes the
  `losetup --partscan` device-node settle, so there is no `udevadm settle` and no sleep-poll.
- **Extraction no longer needs root.** Read-only file access suffices; ADR-0024's
  "the extraction runs as root" no longer holds for this path (the holder is still root for
  the networking and `/dev/kvm` reasons in ADR-0023, but extraction does not require it).
- **One new dependency:** `github.com/diskfs/go-diskfs` (surgical import → `google/uuid` the
  only new external module), pure Go, no cgo, linked only into the `linux && arm64` binary.
- The direct-boot decision, the boot ABI (`--kernel`/`--initramfs`/`--cmdline
  "console=ttyAMA0 root=/dev/vda1 rw"`), the cache location, and the stale-kernel tradeoff
  from ADR-0024 are all unchanged.
- **Test coverage matches the seam's reach:** the search/copy seam is exercised by untagged
  unit tests (darwin, `make test`); the byte-identical extraction and the gunzip branch are
  re-proven on real debian-12 / ubuntu arm64 images; the only end-to-end gate is the local
  nested dogfood (`make test-vm`, M3+/macOS 26). There is **no CI lane** for the nested
  arm64 path — `vm-linux.yml` exercises the amd64 firmware path, not this code.
