//go:build linux && arm64

package cloudhypervisor

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// arm64RootCmdline is the kernel command line for a direct boot of fleetbox's
// debian-shaped cloud image. console=ttyAMA0 targets the pl011 UART
// cloud-hypervisor exposes on aarch64 (so the serial log fills); root is the first
// virtio-blk partition — the disk is the first --disk value (vda) and the debian
// cloud image puts root on p1. The seed and fixtures are later --disk values
// (vdb+), mounted by LABEL, so they do not affect root=.
const arm64RootCmdline = "console=ttyAMA0 root=/dev/vda1 rw"

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

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// extractKernel pulls the kernel and initrd out of the raw disk image into
// kernelOut/initrdOut. It loopback-attaches the image with partition scanning,
// finds the partition holding the kernel (debian keeps /boot inside root; ubuntu
// uses a separate /boot partition), copies the kernel (decompressing a gzip Image,
// which cloud-hypervisor needs raw) and the initrd, then detaches. It runs as root
// — the Linux holder is root (ADR-0023), the only context that can losetup+mount.
func extractKernel(diskPath, kernelOut, initrdOut string) error {
	loop, err := loopAttach(diskPath)
	if err != nil {
		return err
	}
	defer func() { _ = loopDetach(loop) }()

	mnt, err := os.MkdirTemp("", "fb-kernel-")
	if err != nil {
		return fmt.Errorf("temp mount dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(mnt) }()

	bootDir, unmount, err := mountKernelPartition(loop, mnt)
	if err != nil {
		return err
	}
	defer unmount()

	kSrc, err := resolveBootFile(bootDir, "vmlinuz")
	if err != nil {
		return fmt.Errorf("locate kernel: %w", err)
	}
	iSrc, err := resolveBootFile(bootDir, "initrd.img")
	if err != nil {
		return fmt.Errorf("locate initrd: %w", err)
	}

	if err := copyKernel(kSrc, kernelOut); err != nil {
		return fmt.Errorf("copy kernel: %w", err)
	}
	if err := copyFile(iSrc, initrdOut); err != nil {
		return fmt.Errorf("copy initrd: %w", err)
	}
	return nil
}

// loopAttach binds the image to a loop device with partition scanning and returns
// the device path (e.g. /dev/loop3).
func loopAttach(diskPath string) (string, error) {
	out, err := exec.Command("losetup", "--find", "--partscan", "--show", diskPath).Output()
	if err != nil {
		return "", fmt.Errorf("losetup %s: %w", diskPath, err)
	}
	loop := strings.TrimSpace(string(out))
	if loop == "" {
		return "", fmt.Errorf("losetup returned no device for %s", diskPath)
	}
	// losetup --partscan creates the per-partition device nodes (loopNp1, …)
	// asynchronously via udev, so a Glob right after attach can race and find
	// none. Wait for them to appear before mountKernelPartition looks.
	waitForLoopPartitions(loop)
	return loop, nil
}

// waitForLoopPartitions blocks until the loop device's partition nodes appear (or
// a short timeout), settling the udev race after losetup --partscan.
func waitForLoopPartitions(loop string) {
	_ = exec.Command("udevadm", "settle", "--timeout=5").Run() // best-effort
	for range 50 {
		if hits, _ := filepath.Glob(loop + "p*"); len(hits) > 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func loopDetach(loop string) error {
	if err := exec.Command("losetup", "--detach", loop).Run(); err != nil {
		return fmt.Errorf("losetup --detach %s: %w", loop, err)
	}
	return nil
}

// mountKernelPartition mounts whichever scanned partition of loop holds the
// kernel and returns the directory to read it from (the partition's /boot, or the
// partition root when /boot is itself a separate partition) plus an unmount
// closure. It tries each loopNpX partition until one yields a vmlinuz.
func mountKernelPartition(loop, mnt string) (bootDir string, unmount func(), err error) {
	parts, _ := filepath.Glob(loop + "p*")
	if len(parts) == 0 {
		parts = []string{loop} // no partition table: try the whole device
	}
	for _, part := range parts {
		if mErr := mount(part, mnt); mErr != nil {
			continue
		}
		for _, dir := range []string{filepath.Join(mnt, "boot"), mnt} {
			if hits, _ := filepath.Glob(filepath.Join(dir, "vmlinuz*")); len(hits) > 0 {
				return dir, func() { _ = unmountPath(mnt) }, nil
			}
		}
		_ = unmountPath(mnt)
	}
	return "", nil, fmt.Errorf("no partition with a kernel in %s", loop)
}

func mount(dev, mnt string) error {
	if err := exec.Command("mount", "-o", "ro", dev, mnt).Run(); err != nil {
		return fmt.Errorf("mount %s: %w", dev, err)
	}
	return nil
}

func unmountPath(mnt string) error {
	if err := exec.Command("umount", mnt).Run(); err != nil {
		return fmt.Errorf("umount %s: %w", mnt, err)
	}
	return nil
}

// resolveBootFile picks the real boot file for a name like "vmlinuz" or
// "initrd.img": it prefers the unversioned symlink (resolving it to its target),
// else the newest versioned file, skipping ".old" links.
func resolveBootFile(dir, name string) (string, error) {
	link := filepath.Join(dir, name)
	if target, err := filepath.EvalSymlinks(link); err == nil {
		if fi, err := os.Stat(target); err == nil && fi.Mode().IsRegular() {
			return target, nil
		}
	}
	hits, _ := filepath.Glob(filepath.Join(dir, name+"-*"))
	var best string
	var bestTime int64
	for _, h := range hits {
		if strings.HasSuffix(h, ".old") {
			continue
		}
		fi, err := os.Stat(h)
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		if mt := fi.ModTime().UnixNano(); mt >= bestTime {
			bestTime, best = mt, h
		}
	}
	if best == "" {
		return "", fmt.Errorf("no %s under %s", name, dir)
	}
	return best, nil
}

// copyKernel copies src to dst, decompressing it first if it is a gzip image:
// cloud-hypervisor's aarch64 --kernel needs an uncompressed Image (debian ships
// one; ubuntu gzips it).
func copyKernel(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer func() { _ = in.Close() }()

	magic := make([]byte, 2)
	if _, err := io.ReadFull(in, magic); err != nil {
		return fmt.Errorf("read kernel magic: %w", err)
	}
	reader := io.MultiReader(bytes.NewReader(magic), in)
	if magic[0] == 0x1f && magic[1] == 0x8b {
		gz, gzErr := gzip.NewReader(reader)
		if gzErr != nil {
			return fmt.Errorf("gunzip kernel: %w", gzErr)
		}
		defer func() { _ = gz.Close() }()
		reader = gz
	}

	out, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("create %s: %w", dst, err)
	}
	defer func() { _ = out.Close() }()
	if _, err := io.Copy(out, reader); err != nil {
		return fmt.Errorf("write kernel: %w", err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("close %s: %w", dst, err)
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer func() { _ = in.Close() }()
	out, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("create %s: %w", dst, err)
	}
	defer func() { _ = out.Close() }()
	if _, err := io.Copy(out, in); err != nil {
		return fmt.Errorf("copy %s: %w", src, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("close %s: %w", dst, err)
	}
	return nil
}
