package cloudhypervisor

// This file carries the pure, library-agnostic seam of the arm64 direct-kernel-boot
// extraction: locating and copying the guest kernel+initrd out of an already-opened
// guest filesystem. The only non-test caller is extractKernel (boot_arm64.go,
// //go:build linux && arm64), so — exactly like purehelpers.go — these helpers live
// untagged and get darwin-runnable tests (bootextract_test.go) against a fake bootFS,
// their only caller on the dev box. go-diskfs is NOT imported here (the seam stays
// library-agnostic); the *ext4.FileSystem that satisfies bootFS, and the disk/partition
// wiring around it, live in boot_arm64.go.

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
	"time"
)

// bootFS is the read-only filesystem surface the extraction needs from the guest
// root image: list a directory, open a file, and read a symlink's target. It is
// satisfied by go-diskfs's *ext4.FileSystem (the satisfaction check lives in
// boot_arm64.go, the only place go-diskfs is imported). Paths are io/fs-style:
// slash-separated, no leading slash, root is ".".
type bootFS interface {
	ReadDir(string) ([]fs.DirEntry, error)
	Open(string) (fs.File, error)
	ReadLink(string) (string, error)
}

// fileExists reports whether p names an existing host path. bootArgs uses it to
// short-circuit re-extraction when the cached kernel and initrd are both present.
func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// findBootDir returns the directory within fsys that holds the vmlinuz* kernel:
// "boot" when /boot lives inside the root filesystem (debian), "." when the
// filesystem is itself a separate /boot partition, or ("", false) when neither
// has a kernel.
func findBootDir(fsys bootFS) (string, bool) {
	for _, dir := range []string{"boot", "."} {
		if dirHasKernel(fsys, dir) {
			return dir, true
		}
	}
	return "", false
}

// resolveBootFile picks the real boot file for a name like "vmlinuz" or
// "initrd.img" within dir: it prefers the unversioned symlink (resolving it to its
// target and confirming the target is a regular file), else the newest versioned
// file, skipping ".old" links. Selection is by modification time with a >= tie-break,
// matching the historical loopback-mount behavior.
func resolveBootFile(fsys bootFS, dir, name string) (string, error) {
	entries, err := fsys.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", dir, err)
	}

	if target, ok := resolveBootUnversioned(fsys, dir, name, entries); ok {
		return target, nil
	}

	prefix := name + "-"
	var best string
	var bestTime time.Time
	for _, e := range entries {
		n := e.Name()
		if !strings.HasPrefix(n, prefix) || strings.HasSuffix(n, ".old") {
			continue
		}
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if mt := info.ModTime(); best == "" || !mt.Before(bestTime) {
			bestTime, best = mt, path.Join(dir, n)
		}
	}
	if best == "" {
		return "", fmt.Errorf("no %s under %s", name, dir)
	}
	return best, nil
}

// copyKernel copies the kernel at src in fsys to the host path dst, decompressing
// it first if it is a gzip Image: cloud-hypervisor's aarch64 --kernel needs an
// uncompressed Image (debian ships one; ubuntu gzips it).
func copyKernel(fsys bootFS, src, dst string) error {
	in, err := fsys.Open(src)
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
	return atomicWrite(dst, reader)
}

// copyFile copies the file at src in fsys to the host path dst verbatim (no gzip
// handling — used for the initrd).
func copyFile(fsys bootFS, src, dst string) error {
	in, err := fsys.Open(src)
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer func() { _ = in.Close() }()
	return atomicWrite(dst, in)
}

// atomicWrite streams r into dst via a sibling temp file renamed on success, so an
// interrupted copy (I/O error, ENOSPC, a SIGKILL from the holder's Pdeathsig)
// never leaves a truncated file behind. A later boot would treat that file as a
// valid cache (fileExists short-circuits re-extraction) and boot a corrupt kernel;
// the rename makes the cache all-or-nothing.
func atomicWrite(dst string, r io.Reader) error {
	tmp := dst + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("create %s: %w", tmp, err)
	}
	defer func() {
		_ = out.Close()
		_ = os.Remove(tmp) // best-effort: a no-op once renamed, cleanup on failure
	}()
	if _, err := io.Copy(out, r); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		return fmt.Errorf("rename %s to %s: %w", tmp, dst, err)
	}
	return nil
}

// dirHasKernel reports whether dir in fsys lists at least one vmlinuz* entry.
func dirHasKernel(fsys bootFS, dir string) bool {
	entries, err := fsys.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "vmlinuz") {
			return true
		}
	}
	return false
}

// resolveBootUnversioned resolves the unversioned dir/name when it exists as a usable
// boot file: a plain regular file with that exact name is returned directly, a symlink
// is followed to its target and accepted only if the target is a regular file. It
// returns ("", false) when name is absent, is a directory, is a symlink to a
// non-regular target, or cannot be read — so the caller falls through to the
// versioned-file branch. This preserves the historical EvalSymlinks + IsRegular guard,
// which accepted both a plain `vmlinuz` file and a `vmlinuz` symlink.
func resolveBootUnversioned(fsys bootFS, dir, name string, entries []fs.DirEntry) (string, bool) {
	var entry fs.DirEntry
	for _, e := range entries {
		if e.Name() == name {
			entry = e
			break
		}
	}
	if entry == nil {
		return "", false
	}
	full := path.Join(dir, name)
	if entry.Type()&fs.ModeSymlink == 0 {
		// A plain file (or directory) named exactly `name` — accept it only if it is
		// a regular file, matching the old IsRegular guard.
		if isRegularFile(fsys, full) {
			return full, true
		}
		return "", false
	}
	target, err := fsys.ReadLink(full)
	if err != nil {
		return "", false
	}
	resolved := resolveLinkTarget(dir, target)
	if !isRegularFile(fsys, resolved) {
		return "", false
	}
	return resolved, true
}

// resolveLinkTarget turns a symlink target into an io/fs path: an absolute target
// is taken from the filesystem root, a relative one from the link's directory.
func resolveLinkTarget(dir, target string) string {
	if rooted, ok := strings.CutPrefix(target, "/"); ok {
		return path.Clean(rooted)
	}
	return path.Clean(path.Join(dir, target))
}

// isRegularFile reports whether p in fsys opens as a regular file.
func isRegularFile(fsys bootFS, p string) bool {
	f, err := fsys.Open(p)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	return err == nil && fi.Mode().IsRegular()
}
