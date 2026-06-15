//go:build linux && arm64

package cloudhypervisor

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/diskfs/go-diskfs/backend"
	"github.com/diskfs/go-diskfs/backend/file"
	"github.com/diskfs/go-diskfs/filesystem/ext4"
	"github.com/diskfs/go-diskfs/partition/gpt"
)

// arm64RootCmdline is the kernel command line for a direct boot of fleetbox's
// debian-shaped cloud image. console=ttyAMA0 targets the pl011 UART
// cloud-hypervisor exposes on aarch64 (so the serial log fills); root is the first
// virtio-blk partition — the disk is the first --disk value (vda) and the debian
// cloud image puts root on p1. The seed and fixtures are later --disk values
// (vdb+), mounted by LABEL, so they do not affect root=.
const arm64RootCmdline = "console=ttyAMA0 root=/dev/vda1 rw"

// diskSectorSize is the disk's logical/physical sector size used to read the GPT
// and to window the partition for ext4.Read. It is the sector size, not the ext4
// block size — ext4.Read reads the real block size from the superblock itself.
const diskSectorSize = 512

// bootFS is satisfied by go-diskfs's *ext4.FileSystem. This is the one place the
// seam (bootextract.go) is tied to go-diskfs, keeping the ext4 package imported
// only by the linux && arm64 binary (the seam stays library-agnostic).
var _ bootFS = (*ext4.FileSystem)(nil)

// diskPartition is a byte range within the disk to probe for an ext4 filesystem.
type diskPartition struct {
	start int64
	size  int64
}

// bootArgs returns an aarch64 DIRECT KERNEL boot for cloud-hypervisor, bypassing
// rust-hypervisor-firmware. The firmware's aarch64 build does not execute the
// guest under Apple-Silicon nested virtualization (the vCPUs start but produce no
// output and never boot) and the aarch64 firmware path is untested on bare metal;
// a direct kernel boot is the path cloud-hypervisor natively supports and which
// boots reliably (ADR-0024). It extracts the image's own kernel+initrd once
// (cached in the VM dir next to disk.raw) and hands them to cloud-hypervisor.
//
// Tradeoff: the extracted kernel is the image's kernel at first boot; a guest that
// later updates its own kernel still boots the original until rm+up. Acceptable
// for cattle VMs (noted in ADR-0024).
func (v *VM) bootArgs() ([]string, error) {
	vmDir := filepath.Dir(v.diskPath)
	kernel := filepath.Join(vmDir, "vmlinux")
	initrd := filepath.Join(vmDir, "initrd.img")
	if !fileExists(kernel) || !fileExists(initrd) {
		if err := extractKernel(v.diskPath, kernel, initrd); err != nil {
			return nil, fmt.Errorf("extract guest kernel for direct boot: %w", err)
		}
	}
	return []string{
		"--kernel", kernel,
		"--initramfs", initrd,
		"--cmdline", arm64RootCmdline,
	}, nil
}

// extractKernel pulls the kernel and initrd out of the raw disk image into
// kernelOut/initrdOut, reading the image in-process with pure-Go go-diskfs (no
// loopback mount, no shell-out, no global host state to leak on SIGKILL, no root
// required — read-only file access suffices; ADR-0026). It opens the disk
// read-only, enumerates its partitions, reads each as ext4 until one holds a
// kernel (debian keeps /boot inside root; ubuntu uses a separate /boot partition),
// then copies the kernel (decompressing a gzip Image, which cloud-hypervisor needs
// raw) and the initrd.
func extractKernel(diskPath, kernelOut, initrdOut string) error {
	stor, err := file.OpenFromPath(diskPath, true)
	if err != nil {
		return fmt.Errorf("open %s: %w", diskPath, err)
	}
	defer func() { _ = stor.Close() }()

	// lastErr keeps the most recent partition failure so a total miss reports the
	// real cause (a corrupt superblock, a missing initrd) rather than the bare
	// "no partition with a kernel"; it is only consulted if no partition succeeds.
	var lastErr error
	for _, p := range diskPartitions(stor, diskPath) {
		efs, err := ext4.Read(stor, p.size, p.start, diskSectorSize)
		if err != nil {
			lastErr = err // not ext4 (e.g. the vfat ESP) — try the next partition
			continue
		}
		dir, ok := findBootDir(efs)
		if !ok {
			continue
		}
		kSrc, err := resolveBootFile(efs, dir, "vmlinuz")
		if err != nil {
			lastErr = err
			continue
		}
		iSrc, err := resolveBootFile(efs, dir, "initrd.img")
		if err != nil {
			lastErr = err
			continue
		}
		if err := copyKernel(efs, kSrc, kernelOut); err != nil {
			return fmt.Errorf("copy kernel: %w", err)
		}
		if err := copyFile(efs, iSrc, initrdOut); err != nil {
			return fmt.Errorf("copy initrd: %w", err)
		}
		return nil
	}
	if lastErr != nil {
		return fmt.Errorf("no partition with a kernel in %s: %w", diskPath, lastErr)
	}
	return fmt.Errorf("no partition with a kernel in %s", diskPath)
}

// diskPartitions enumerates the byte ranges to probe for an ext4 root filesystem.
// It reads the GPT and returns each partition's (start, size) in bytes; if the disk
// has no usable GPT (a read error or zero usable partitions) it falls back to
// treating the whole device as one bare ext4 at offset 0 — mirroring the old mount
// path's "no partition table → try the whole device" branch. The catalog is
// GPT-only (ADR-0019), so the fallback is purely defensive; ext4.Read on a
// partitioned disk with start=0 simply errors (the superblock check fails), so the
// whole-device entry is harmless on a GPT disk.
func diskPartitions(stor backend.Storage, diskPath string) []diskPartition {
	whole := func() []diskPartition {
		fi, err := os.Stat(diskPath)
		if err != nil {
			return nil
		}
		return []diskPartition{{start: 0, size: fi.Size()}}
	}

	tbl, err := gpt.Read(stor, diskSectorSize, diskSectorSize)
	if err != nil {
		return whole()
	}
	var parts []diskPartition
	for _, p := range tbl.GetPartitions() {
		if p.GetSize() <= 0 {
			continue
		}
		parts = append(parts, diskPartition{start: p.GetStart(), size: p.GetSize()})
	}
	if len(parts) == 0 {
		return whole()
	}
	return parts
}
