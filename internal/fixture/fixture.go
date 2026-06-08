// Package fixture packs a host directory into a read-only ext4 filesystem image
// for attaching to a VM as a block device (ADR-0015). It is the only fleetbox
// package that imports the ext4 writer (go-ext4fs); the seed ISO stays on
// cloudiso. Fixtures are read-only copy-in, not live mounts — see ADR-0015 for
// the reasoning (cloud-hypervisor has no daemon-free live-share path).
package fixture

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	ext4fs "github.com/pilat/go-ext4fs"
)

// BuildImage packs the directory tree at srcDir into a fresh ext4 filesystem
// image at imgPath, labeled label (the guest mounts the image by LABEL). Every
// file is written world-readable (0444), every directory traversable (0555),
// owned by root, so any guest user can read the whole tree; host permission and
// executable bits are not preserved. Symlinks are copied as symlinks; special
// files (device nodes, pipes, sockets) are rejected with an error.
//
// The image is built on a 16 GiB sparse canvas and then resized down to fit its
// content, so a typical fixture lands at a few MiB on disk. A payload larger than
// 16 GiB is unsupported (the resize ceiling).
func BuildImage(imgPath, srcDir, label string) error {
	root := filepath.Clean(srcDir)

	img, err := ext4fs.New(
		ext4fs.WithImagePath(imgPath),
		ext4fs.WithSizeInMB(16384),
		ext4fs.WithLabel(label),
	)
	if err != nil {
		return fmt.Errorf("create ext4 image %q: %w", imgPath, err)
	}
	defer func() { _ = img.Close() }()

	// WalkDir yields parents before children, which satisfies go-ext4fs's
	// parent-inode addressing: each entry is created under the inode recorded for
	// its parent directory. The root inode pre-exists as RootInode.
	inodes := map[string]uint32{root: ext4fs.RootInode}
	walkErr := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("fixture %q: walk: %w", p, err)
		}
		if p == root {
			return nil // root already exists as RootInode
		}

		parent, ok := inodes[filepath.Dir(p)]
		if !ok {
			return fmt.Errorf("fixture %q: parent inode not found", p)
		}
		name := d.Name()

		switch {
		case d.IsDir():
			ino, err := img.CreateDirectory(parent, name, 0o555, 0, 0)
			if err != nil {
				return fmt.Errorf("fixture %q: create directory: %w", p, err)
			}
			inodes[p] = ino
		case d.Type()&fs.ModeSymlink != 0:
			tgt, err := os.Readlink(p)
			if err != nil {
				return fmt.Errorf("fixture %q: read symlink: %w", p, err)
			}
			// A symlink is a leaf, never a parent, so its inode is discarded.
			if _, err := img.CreateSymlink(parent, name, tgt, 0, 0); err != nil {
				return fmt.Errorf("fixture %q: create symlink: %w", p, err)
			}
		case d.Type().IsRegular():
			//nolint:gosec // G122: the source is the caller's own directory packed read-only; symlinks are copied as links, not followed, so there is no TOCTOU traversal.
			b, err := os.ReadFile(p)
			if err != nil {
				return fmt.Errorf("fixture %q: read file: %w", p, err)
			}
			if _, err := img.CreateFile(parent, name, b, 0o444, 0, 0); err != nil {
				return fmt.Errorf("fixture %q: create file: %w", p, err)
			}
		default:
			return fmt.Errorf("fixture %q: unsupported file type %s", p, d.Type())
		}
		return nil
	})
	if walkErr != nil {
		//nolint:wrapcheck // the callback already wraps every error with the offending path.
		return walkErr
	}

	// Generous canvas, then fit: shrink to a high-water mark over both the highest
	// data block and the highest allocated inode, so the on-disk file is minimal.
	if err := img.Resize(img.MinSize()); err != nil {
		return fmt.Errorf("resize ext4 image %q: %w", imgPath, err)
	}
	if err := img.Save(); err != nil {
		return fmt.Errorf("save ext4 image %q: %w", imgPath, err)
	}
	return nil
}
