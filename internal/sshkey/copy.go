package sshkey

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// transport is the SSH side of a copy, isolated behind a tiny interface so the
// pure tar marshalling and the CopyTo/CopyFrom orchestration can be unit-tested
// against a fake guest without booting a VM (the copy round-trip and the exact
// guest command strings are covered hermetically; real GNU tar runs only in the
// VM-tier test). toGuest feeds a tar stream to a guest `tar -x`; fromGuest reads a
// guest `tar -c`'s stdout. Each surfaces the guest's stderr on a non-zero exit.
type transport interface {
	toGuest(cmd string, stdin io.Reader) error
	fromGuest(cmd string, stdout io.Writer) error
}

var _ transport = (*Client)(nil)

// dirMode records a directory's intended permission for the deferred mode restore
// in extractTar (directories are created writable, then chmod-ed at the end).
type dirMode struct {
	path string
	mode fs.FileMode
}

// CopyTo copies hostPath (a file or a directory tree) into the guest, placing it
// at guestPath exactly: CopyTo("./app", "/srv/app") makes the file or directory
// "/srv/app". Missing parent directories on the guest are created. File modes ride
// the tar header and are restored (an executable stays executable); ownership is
// not preserved — the copied tree belongs to the connecting user. guestPath must
// be absolute. It streams over the existing SSH connection and adds no guest-side
// requirement beyond `tar`, present on every stock cloud image.
func (c *Client) CopyTo(hostPath, guestPath string) error {
	return copyTo(c, hostPath, guestPath)
}

// CopyFrom copies guestPath (a file or a directory tree) out of the guest, placing
// it at hostPath exactly: CopyFrom("/var/log/app", "./app") makes the host file or
// directory "./app". Missing parent directories on the host are created. File modes
// are restored from the tar header; ownership is not preserved. guestPath must be
// absolute. It streams over the existing SSH connection and rejects any archive
// entry that would escape the destination (absolute paths, "..", escaping symlinks).
func (c *Client) CopyFrom(guestPath, hostPath string) error {
	return copyFrom(c, guestPath, hostPath)
}

// copyTo drives a host→guest copy over the transport. It validates guestPath up
// front and that the local source exists before any transport, then pipes a tar
// built in-process (rooted at the destination's basename, so the guest extraction
// under dir(guestPath) lands the tree at guestPath exactly) into the guest's
// `tar -x`. The producer runs in a goroutine wired to the transport with an
// io.Pipe so neither side buffers the whole payload. On a guest failure the pipe
// is drained so the producer completes rather than deadlocking, which also keeps a
// successful producer from being masked by the transport's error: the producer's
// error is reported first (it is the root cause; a truncated-stream guest failure
// is the symptom), the transport's only when the producer succeeded.
func copyTo(t transport, hostPath, guestPath string) error {
	if !path.IsAbs(guestPath) {
		return fmt.Errorf("guest path %q must be absolute", guestPath)
	}
	if _, err := os.Lstat(hostPath); err != nil {
		return fmt.Errorf("source %q: %w", hostPath, err)
	}

	pr, pw := io.Pipe()
	buildDone := make(chan error, 1)
	go func() {
		err := buildTar(pw, hostPath, path.Base(guestPath))
		_ = pw.CloseWithError(err)
		buildDone <- err
	}()

	transportErr := t.toGuest(copyToCmd(guestPath), pr)
	// Drain anything the guest did not consume so the producer finishes instead of
	// blocking on a write nobody reads; this also avoids poisoning a successful
	// producer with the transport's failure.
	_, _ = io.Copy(io.Discard, pr)
	_ = pr.Close()
	buildErr := <-buildDone

	if buildErr != nil {
		return fmt.Errorf("build tar: %w", buildErr)
	}
	if transportErr != nil {
		return fmt.Errorf("copy to guest: %w", transportErr)
	}
	return nil
}

// copyFrom drives a guest→host copy over the transport. It validates guestPath,
// creates the host parent directory, then extracts the guest `tar -c`'s output
// in-process under dir(hostPath), renaming the archive's top component
// base(guestPath)→base(hostPath) so the tree lands at hostPath exactly. The
// transport runs in a goroutine wired to the extractor with an io.Pipe. The guest
// is the producer here, so its error is reported first (a truncated-stream extract
// failure is the symptom); the extractor's error — e.g. an unsafe entry — is
// reported only when the guest succeeded (the drain lets the guest complete so a
// host-side bail does not look like a guest failure).
func copyFrom(t transport, guestPath, hostPath string) error {
	if !path.IsAbs(guestPath) {
		return fmt.Errorf("guest path %q must be absolute", guestPath)
	}
	destDir := filepath.Dir(hostPath)
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return fmt.Errorf("create host dir %q: %w", destDir, err)
	}

	pr, pw := io.Pipe()
	transportDone := make(chan error, 1)
	go func() {
		err := t.fromGuest(copyFromCmd(guestPath), pw)
		_ = pw.CloseWithError(err)
		transportDone <- err
	}()

	extractErr := extractTar(pr, destDir, path.Base(guestPath), filepath.Base(hostPath))
	// Drain so the transport goroutine's copy completes even if the extractor
	// stopped early (e.g. an unsafe entry), avoiding a deadlock.
	_, _ = io.Copy(io.Discard, pr)
	_ = pr.Close()
	transportErr := <-transportDone

	if transportErr != nil {
		return fmt.Errorf("copy from guest: %w", transportErr)
	}
	if extractErr != nil {
		return fmt.Errorf("extract: %w", extractErr)
	}
	return nil
}

// copyToCmd is the guest command for CopyTo: create the destination's parent dir,
// then extract the tar arriving on stdin under it. -p restores modes from the
// header and --no-same-owner keeps the extracted tree owned by the connecting user
// rather than the archived uid. The whole thing is one shell string, so the path
// is shell-quoted.
func copyToCmd(guestPath string) string {
	dir := shellQuote(path.Dir(guestPath))
	return fmt.Sprintf("mkdir -p %s && tar -x -f - -p --no-same-owner -C %s", dir, dir)
}

// copyFromCmd is the guest command for CopyFrom: stream a tar of base(guestPath)
// from its parent dir to stdout. Running from -C dir(guestPath) with the basename
// as the member roots the archive at the basename, which the in-process extractor
// then renames to the host destination's basename.
func copyFromCmd(guestPath string) string {
	return fmt.Sprintf("tar -c -f - -C %s %s",
		shellQuote(path.Dir(guestPath)), shellQuote(path.Base(guestPath)))
}

// shellQuote wraps s in single quotes for safe inclusion in a single shell
// command string, escaping each embedded single quote with the close-escape-reopen
// idiom (quote, backslash-quote, quote). Go's %q is not shell-safe, so paths with
// spaces or metacharacters need this.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// buildTar writes a tar stream of hostPath to w, rooting the tree at topName (the
// caller controls the names, so the top component is renamed to the destination's
// basename). The walk uses os.Lstat semantics — it does NOT follow symlinks, which
// are preserved as tar.TypeSymlink (avoiding target-copying and symlink-cycle
// hangs). Each entry's mode is captured in the header.
func buildTar(w io.Writer, hostPath, topName string) error {
	tw := tar.NewWriter(w)

	info, err := os.Lstat(hostPath)
	if err != nil {
		return fmt.Errorf("lstat %q: %w", hostPath, err)
	}

	if !info.IsDir() {
		if err := writeTarEntry(tw, hostPath, info, topName); err != nil {
			return err
		}
		if err := tw.Close(); err != nil {
			return fmt.Errorf("close tar: %w", err)
		}
		return nil
	}

	walkErr := filepath.WalkDir(hostPath, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return fmt.Errorf("stat %q: %w", p, err)
		}
		rel, err := filepath.Rel(hostPath, p)
		if err != nil {
			return fmt.Errorf("rel %q: %w", p, err)
		}
		name := topName
		if rel != "." {
			name = path.Join(topName, filepath.ToSlash(rel))
		}
		return writeTarEntry(tw, p, fi, name)
	})
	if walkErr != nil {
		return fmt.Errorf("walk %q: %w", hostPath, walkErr)
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("close tar: %w", err)
	}
	return nil
}

// writeTarEntry writes one filesystem entry to tw under the archive name name.
// Symlinks are stored by their target (read with os.Readlink, never followed);
// regular files have their contents streamed; directories get the trailing-slash
// tar convention. The mode is carried by tar.FileInfoHeader.
func writeTarEntry(tw *tar.Writer, p string, info fs.FileInfo, name string) error {
	link := ""
	if info.Mode()&fs.ModeSymlink != 0 {
		target, err := os.Readlink(p)
		if err != nil {
			return fmt.Errorf("readlink %q: %w", p, err)
		}
		link = target
	}

	hdr, err := tar.FileInfoHeader(info, link)
	if err != nil {
		return fmt.Errorf("tar header %q: %w", p, err)
	}
	hdr.Name = name
	if info.IsDir() {
		hdr.Name = name + "/"
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("write tar header %q: %w", name, err)
	}

	if info.Mode().IsRegular() {
		f, err := os.Open(p)
		if err != nil {
			return fmt.Errorf("open %q: %w", p, err)
		}
		defer func() { _ = f.Close() }()
		if _, err := io.Copy(tw, f); err != nil {
			return fmt.Errorf("copy %q: %w", p, err)
		}
	}
	return nil
}

// extractTar reads a tar stream and writes its entries under destDir, restoring
// file modes from each header and never changing ownership. When fromTop differs
// from toTop the leading path component fromTop of every entry is rewritten to
// toTop (this is how CopyFrom lands base(guestPath) as base(hostPath)). It rejects
// any entry whose name is absolute or contains "..", and any symlink whose target
// escapes destDir, so a hostile guest archive cannot write outside the destination.
func extractTar(r io.Reader, destDir, fromTop, toTop string) error {
	tr := tar.NewReader(r)
	// Directory modes are restored only after every child is written: a directory
	// extracted read-only (e.g. 0555) would otherwise reject writes of its own
	// children, and MkdirAll's mode is umask-masked so it cannot preserve the mode
	// either. Create dirs writable, record the intended mode, chmod deepest-first
	// at the end (restoreDirModes).
	var dirs []dirMode
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read tar: %w", err)
		}

		name := hdr.Name
		if fromTop != toTop {
			name, err = renameTop(name, fromTop, toTop)
			if err != nil {
				return err
			}
		}
		target, err := safeJoin(destDir, name)
		if err != nil {
			return err
		}
		// safeJoin only constrains the entry-name text. A hostile archive can still
		// escape destDir by planting a symlink as an early entry and then routing a
		// later entry *through* it (the later entry's name is innocuous, but its
		// materialization follows the planted symlink). Reject any entry whose parent
		// path, as it now exists on disk, traverses a symlink.
		if err := guardNoSymlinkParents(destDir, target); err != nil {
			return err
		}

		mode := hdr.FileInfo().Mode().Perm()
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, mode|0o700); err != nil {
				return fmt.Errorf("mkdir %q: %w", target, err)
			}
			dirs = append(dirs, dirMode{path: target, mode: mode})
		case tar.TypeReg:
			if err := writeRegular(target, tr, mode); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := writeSymlink(destDir, target, hdr.Linkname); err != nil {
				return err
			}
		default:
			// Hardlinks/devices/fifos are out of scope for a copy primitive; skip
			// them rather than fail, so an unusual entry does not abort the copy.
		}
	}
	return restoreDirModes(dirs)
}

// restoreDirModes applies the recorded directory modes deepest-first, so a
// parent's restrictive mode lands only after its children (and any restrictive
// child) are already written. Reverse-lexicographic ordering puts every child
// before its parent (a child path is its parent path plus a longer suffix).
func restoreDirModes(dirs []dirMode) error {
	slices.SortFunc(dirs, func(a, b dirMode) int { return strings.Compare(b.path, a.path) })
	for _, d := range dirs {
		if err := os.Chmod(d.path, d.mode); err != nil {
			return fmt.Errorf("chmod %q: %w", d.path, err)
		}
	}
	return nil
}

// writeRegular writes a regular-file entry, creating any missing parent dirs and
// chmod-ing to the header mode explicitly (OpenFile's perm is masked by umask, so
// the executable/0644 distinction would otherwise be lost).
func writeRegular(target string, r io.Reader, mode fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("mkdir %q: %w", filepath.Dir(target), err)
	}
	// If the final component already exists as a symlink, O_TRUNC would follow it
	// and clobber the link target instead of replacing the link. Drop it first so
	// we write a fresh regular file in place.
	if fi, err := os.Lstat(target); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
		if err := os.Remove(target); err != nil {
			return fmt.Errorf("remove existing symlink %q: %w", target, err)
		}
	}
	f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return fmt.Errorf("create %q: %w", target, err)
	}
	if _, err := io.Copy(f, r); err != nil {
		_ = f.Close()
		return fmt.Errorf("write %q: %w", target, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %q: %w", target, err)
	}
	if err := os.Chmod(target, mode); err != nil {
		return fmt.Errorf("chmod %q: %w", target, err)
	}
	return nil
}

// writeSymlink writes a symlink entry after checking that its target stays within
// destDir (an absolute target, or a relative one that climbs out, is refused).
func writeSymlink(destDir, target, linkname string) error {
	if err := checkSymlink(destDir, target, linkname); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("mkdir %q: %w", filepath.Dir(target), err)
	}
	_ = os.Remove(target)
	if err := os.Symlink(linkname, target); err != nil {
		return fmt.Errorf("symlink %q: %w", target, err)
	}
	return nil
}

// renameTop rewrites the leading path component fromTop of a tar entry name to
// toTop. It errors if the entry is not under fromTop, which would mean the guest
// archive was not rooted where copyFromCmd places it.
func renameTop(name, fromTop, toTop string) (string, error) {
	clean := path.Clean(name)
	first, rest, found := strings.Cut(clean, "/")
	if first != fromTop {
		return "", fmt.Errorf("tar entry %q is not under %q", name, fromTop)
	}
	if !found {
		return toTop, nil
	}
	return toTop + "/" + rest, nil
}

// guardNoSymlinkParents rejects target if any of its parent path components, as
// they currently exist under root, is a symlink. This closes the symlink-traversal
// escape that safeJoin's text-only check misses: an early archive entry plants a
// symlink, then a later entry routes its writes through that symlink and out of
// root (checkSymlink validates a link's own target lexically, but cannot see that a
// *parent* component became a symlink). It stops at the first not-yet-existing
// component — everything beyond is ours to create. The final component is not
// checked here: as a parent of any deeper entry it is caught on that entry, and as
// the entry's own leaf each writer handles it (writeRegular drops a final symlink,
// writeSymlink removes-then-creates).
func guardNoSymlinkParents(root, target string) error {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return fmt.Errorf("resolve %q: %w", target, err)
	}
	parts := strings.Split(rel, string(filepath.Separator))
	cur := root
	for _, part := range parts[:len(parts)-1] {
		if part == "." || part == "" {
			continue
		}
		cur = filepath.Join(cur, part)
		fi, err := os.Lstat(cur)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("lstat %q: %w", cur, err)
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("unsafe tar entry %q: parent %q is a symlink", target, cur)
		}
	}
	return nil
}

// safeJoin resolves a tar entry name against root, refusing absolute names and any
// name that escapes root via "..". It returns the cleaned absolute host path.
func safeJoin(root, name string) (string, error) {
	if path.IsAbs(name) {
		return "", fmt.Errorf("unsafe tar entry %q: absolute path", name)
	}
	clean := filepath.Clean(filepath.FromSlash(name))
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("unsafe tar entry %q: escapes destination", name)
	}
	target := filepath.Join(root, clean)
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("unsafe tar entry %q: escapes destination", name)
	}
	return target, nil
}

// checkSymlink rejects a symlink whose target would point outside destDir: an
// absolute target, or a relative target that resolves above the destination root.
func checkSymlink(destDir, linkPath, linkname string) error {
	if path.IsAbs(linkname) {
		return fmt.Errorf("unsafe symlink %q -> %q: absolute target", linkPath, linkname)
	}
	resolved := filepath.Join(filepath.Dir(linkPath), filepath.FromSlash(linkname))
	rel, err := filepath.Rel(destDir, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("unsafe symlink %q -> %q: escapes destination", linkPath, linkname)
	}
	return nil
}

// toGuest runs cmd on the guest with stdin fed from r (a tar stream), discarding
// the guest's stdout and capturing its stderr. A non-zero exit returns an error
// wrapping that stderr. When the guest exits zero a read error on r is the
// producer's to report, not the transport's, so it is dropped here.
func (c *Client) toGuest(cmd string, r io.Reader) error {
	session, err := c.client.NewSession()
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	defer func() { _ = session.Close() }()

	var stderr bytes.Buffer
	session.Stdout = io.Discard
	session.Stderr = &stderr

	stdin, err := session.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}
	if err := session.Start(cmd); err != nil {
		return fmt.Errorf("start %q: %w", cmd, err)
	}

	_, copyErr := io.Copy(stdin, r)
	_ = stdin.Close()
	waitErr := session.Wait()

	if waitErr != nil {
		return guestError(stderr.String(), waitErr)
	}
	_ = copyErr // guest succeeded: any r read error belongs to the producer
	return nil
}

// fromGuest runs cmd on the guest, copying its stdout to w (the tar payload) and
// capturing stderr. If w stops accepting writes (the consumer bailed) the guest's
// stdout is drained so Wait does not block on a stalled remote. A non-zero exit
// returns an error wrapping stderr.
func (c *Client) fromGuest(cmd string, w io.Writer) error {
	session, err := c.client.NewSession()
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	defer func() { _ = session.Close() }()

	var stderr bytes.Buffer
	session.Stderr = &stderr
	stdout, err := session.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	if err := session.Start(cmd); err != nil {
		return fmt.Errorf("start %q: %w", cmd, err)
	}

	_, copyErr := io.Copy(w, stdout)
	if copyErr != nil {
		_, _ = io.Copy(io.Discard, stdout)
	}
	waitErr := session.Wait()

	if waitErr != nil {
		return guestError(stderr.String(), waitErr)
	}
	if copyErr != nil {
		return fmt.Errorf("stream guest output: %w", copyErr)
	}
	return nil
}

// guestError builds the error for a non-zero guest tar exit, preferring the
// captured stderr (the human-readable cause) and falling back to the exit error.
func guestError(stderr string, waitErr error) error {
	if msg := strings.TrimSpace(stderr); msg != "" {
		return fmt.Errorf("guest tar: %s", msg)
	}
	return fmt.Errorf("guest tar: %w", waitErr)
}
