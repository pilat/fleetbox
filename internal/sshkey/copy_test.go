package sshkey

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeGuest stands in for a VM's `tar` over SSH so the copy round-trip is
// exercised without booting a VM. toGuest extracts the incoming tar into
// extractRoot (mimicking `tar -x -C dir(guestPath)`, no rename); fromGuest tars
// srcBase out of srcRoot (mimicking `tar -c -C dir(guestPath) base(guestPath)`).
// A non-empty failStderr makes either direction fail like a non-zero guest tar.
type fakeGuest struct {
	extractRoot string
	srcRoot     string
	srcBase     string
	failStderr  string
}

var _ transport = (*fakeGuest)(nil)

func (f *fakeGuest) toGuest(_ string, stdin io.Reader) error {
	if f.failStderr != "" {
		return guestError(f.failStderr, errors.New("exit status 2"))
	}
	return extractTar(stdin, f.extractRoot, "", "")
}

func (f *fakeGuest) fromGuest(_ string, stdout io.Writer) error {
	if f.failStderr != "" {
		return guestError(f.failStderr, errors.New("exit status 2"))
	}
	return buildTar(stdout, filepath.Join(f.srcRoot, f.srcBase), f.srcBase)
}

// recordingGuest records whether either transport method was called, so a test can
// prove validation happens before any transport.
type recordingGuest struct{ called bool }

var _ transport = (*recordingGuest)(nil)

func (r *recordingGuest) toGuest(string, io.Reader) error   { r.called = true; return nil }
func (r *recordingGuest) fromGuest(string, io.Writer) error { r.called = true; return nil }

func TestCopyToCmd(t *testing.T) {
	cases := []struct {
		guestPath string
		want      string
	}{
		{"/a/b", "mkdir -p '/a' && tar -x -f - -p --no-same-owner -C '/a'"},
		{"/srv/app", "mkdir -p '/srv' && tar -x -f - -p --no-same-owner -C '/srv'"},
		{"/a b/c", "mkdir -p '/a b' && tar -x -f - -p --no-same-owner -C '/a b'"},
		{"/a'b/c", `mkdir -p '/a'\''b' && tar -x -f - -p --no-same-owner -C '/a'\''b'`},
	}
	for _, tc := range cases {
		t.Run(tc.guestPath, func(t *testing.T) {
			if got := copyToCmd(tc.guestPath); got != tc.want {
				t.Errorf("copyToCmd(%q) =\n  %q\nwant\n  %q", tc.guestPath, got, tc.want)
			}
		})
	}
}

func TestCopyFromCmd(t *testing.T) {
	cases := []struct {
		guestPath string
		want      string
	}{
		{"/a/b", "tar -c -f - -C '/a' 'b'"},
		{"/var/log/x", "tar -c -f - -C '/var/log' 'x'"},
		{"/a/b c", "tar -c -f - -C '/a' 'b c'"},
		{"/a/b'c", `tar -c -f - -C '/a' 'b'\''c'`},
	}
	for _, tc := range cases {
		t.Run(tc.guestPath, func(t *testing.T) {
			if got := copyFromCmd(tc.guestPath); got != tc.want {
				t.Errorf("copyFromCmd(%q) =\n  %q\nwant\n  %q", tc.guestPath, got, tc.want)
			}
		})
	}
}

func TestCopyToFileModePreserved(t *testing.T) {
	for _, mode := range []fs.FileMode{0o644, 0o755} {
		t.Run(mode.String(), func(t *testing.T) {
			src := filepath.Join(t.TempDir(), "src")
			writeFile(t, src, "hello copy", mode)

			guestRoot := t.TempDir()
			fake := &fakeGuest{extractRoot: guestRoot}
			if err := copyTo(fake, src, "/dst/copied"); err != nil {
				t.Fatalf("copyTo: %v", err)
			}

			got := filepath.Join(guestRoot, "copied")
			assertFile(t, got, "hello copy", mode)
		})
	}
}

func TestCopyFromFileRename(t *testing.T) {
	guestRoot := t.TempDir()
	writeFile(t, filepath.Join(guestRoot, "x"), "from guest", 0o600)

	fake := &fakeGuest{srcRoot: guestRoot, srcBase: "x"}
	host := filepath.Join(t.TempDir(), "y")
	if err := copyFrom(fake, "/g/x", host); err != nil {
		t.Fatalf("copyFrom: %v", err)
	}
	assertFile(t, host, "from guest", 0o600)
}

func TestCopyToDirectoryTree(t *testing.T) {
	src := t.TempDir()
	mustMkdir(t, filepath.Join(src, "sub"), 0o755)
	mustMkdir(t, filepath.Join(src, "empty"), 0o700)
	writeFile(t, filepath.Join(src, "top.txt"), "top", 0o644)
	writeFile(t, filepath.Join(src, "sub", "exec.sh"), "#!/bin/sh\n", 0o755)
	writeFile(t, filepath.Join(src, "sub", "blank"), "", 0o644)

	guestRoot := t.TempDir()
	fake := &fakeGuest{extractRoot: guestRoot}
	if err := copyTo(fake, src, "/dst/tree"); err != nil {
		t.Fatalf("copyTo: %v", err)
	}

	got := filepath.Join(guestRoot, "tree")
	assertTreeEqual(t, src, got)
	// The empty directory survived as a directory.
	if info, err := os.Stat(filepath.Join(got, "empty")); err != nil || !info.IsDir() {
		t.Errorf("empty dir not preserved: info=%v err=%v", info, err)
	}
}

func TestCopyToSymlinkPreserved(t *testing.T) {
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "real"), "target", 0o644)
	mustSymlink(t, "real", filepath.Join(src, "link"))
	// A symlink to a directory must NOT be recursed into, and a self-referential
	// link must not hang the walk.
	mustMkdir(t, filepath.Join(src, "d"), 0o755)
	writeFile(t, filepath.Join(src, "d", "inner"), "inner", 0o644)
	mustSymlink(t, "d", filepath.Join(src, "dlink"))
	mustSymlink(t, ".", filepath.Join(src, "self"))

	guestRoot := t.TempDir()
	fake := &fakeGuest{extractRoot: guestRoot}
	if err := copyTo(fake, src, "/dst/tree"); err != nil {
		t.Fatalf("copyTo: %v", err)
	}

	got := filepath.Join(guestRoot, "tree")
	assertSymlink(t, filepath.Join(got, "link"), "real")
	assertSymlink(t, filepath.Join(got, "dlink"), "d")
	assertSymlink(t, filepath.Join(got, "self"), ".")
	// The real directory was walked normally.
	assertFile(t, filepath.Join(got, "d", "inner"), "inner", 0o644)

	// dlink was stored as a symlink, not recursed into: the archive itself must
	// carry no entry beneath dlink (a path check would be fooled by dlink->d
	// resolving to the real d/inner). Building over the self-referential link also
	// proves the walk does not follow symlinks into a cycle.
	for _, name := range tarEntryNames(t, src, "tree") {
		if strings.HasPrefix(name, "tree/dlink/") {
			t.Errorf("symlink to dir was recursed: archive has entry %q", name)
		}
	}
}

// tarEntryNames builds the archive buildTar would send and returns its entry names.
func tarEntryNames(t *testing.T, hostPath, topName string) []string {
	t.Helper()
	var buf bytes.Buffer
	if err := buildTar(&buf, hostPath, topName); err != nil {
		t.Fatalf("buildTar: %v", err)
	}
	var names []string
	tr := tar.NewReader(&buf)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("read tar: %v", err)
		}
		names = append(names, hdr.Name)
	}
	return names
}

func TestCopyToRenameToDestinationBasename(t *testing.T) {
	// CopyTo(file, /a/b/c) lands content at c; CopyTo(dir, /a/b/c) makes c the dir.
	t.Run("file", func(t *testing.T) {
		src := filepath.Join(t.TempDir(), "orig")
		writeFile(t, src, "data", 0o644)
		guestRoot := t.TempDir()
		if err := copyTo(&fakeGuest{extractRoot: guestRoot}, src, "/a/b/c"); err != nil {
			t.Fatalf("copyTo: %v", err)
		}
		assertFile(t, filepath.Join(guestRoot, "c"), "data", 0o644)
	})
	t.Run("dir", func(t *testing.T) {
		src := t.TempDir()
		writeFile(t, filepath.Join(src, "f"), "data", 0o644)
		guestRoot := t.TempDir()
		if err := copyTo(&fakeGuest{extractRoot: guestRoot}, src, "/a/b/c"); err != nil {
			t.Fatalf("copyTo: %v", err)
		}
		assertFile(t, filepath.Join(guestRoot, "c", "f"), "data", 0o644)
	})
}

func TestCopyFromCreatesHostParent(t *testing.T) {
	// The guest-side mkdir -p for CopyTo is covered in the VM-tier test; here we
	// cover only the host-side MkdirAll for CopyFrom.
	guestRoot := t.TempDir()
	writeFile(t, filepath.Join(guestRoot, "x"), "payload", 0o644)

	fake := &fakeGuest{srcRoot: guestRoot, srcBase: "x"}
	host := filepath.Join(t.TempDir(), "does", "not", "exist", "y")
	if err := copyFrom(fake, "/g/x", host); err != nil {
		t.Fatalf("copyFrom: %v", err)
	}
	assertFile(t, host, "payload", 0o644)
}

func TestCopyToLargeFileStreams(t *testing.T) {
	// Teeth: copyTo wires the producer to the transport through an unbuffered
	// io.Pipe and runs the producer in a goroutine. A regression that dropped that
	// goroutine — building the whole tar into the pipe before starting the
	// transport — would block on the first write and deadlock here (caught as a
	// test timeout), not pass. The payload is comfortably larger than any internal
	// buffer so correctness on a multi-MB stream is also exercised.
	const size = 6 << 20
	payload := make([]byte, size)
	for i := range payload {
		payload[i] = byte(i*31 + 7)
	}
	src := filepath.Join(t.TempDir(), "big.bin")
	if err := os.WriteFile(src, payload, 0o644); err != nil {
		t.Fatalf("write big file: %v", err)
	}

	guestRoot := t.TempDir()
	if err := copyTo(&fakeGuest{extractRoot: guestRoot}, src, "/dst/big.bin"); err != nil {
		t.Fatalf("copyTo: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(guestRoot, "big.bin"))
	if err != nil {
		t.Fatalf("read copied: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("large file content mismatch: got %d bytes, want %d", len(got), len(payload))
	}
}

func TestCopyToRejectsRelativeGuestPath(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src")
	writeFile(t, src, "x", 0o644)

	rec := &recordingGuest{}
	err := copyTo(rec, src, "relative/path")
	if err == nil || !strings.Contains(err.Error(), "must be absolute") {
		t.Fatalf("copyTo relative guest path: got %v, want 'must be absolute' error", err)
	}
	if rec.called {
		t.Error("transport was called despite an invalid guest path")
	}
}

func TestCopyFromRejectsRelativeGuestPath(t *testing.T) {
	rec := &recordingGuest{}
	err := copyFrom(rec, "relative/path", filepath.Join(t.TempDir(), "y"))
	if err == nil || !strings.Contains(err.Error(), "must be absolute") {
		t.Fatalf("copyFrom relative guest path: got %v, want 'must be absolute' error", err)
	}
	if rec.called {
		t.Error("transport was called despite an invalid guest path")
	}
}

func TestCopyToMissingSource(t *testing.T) {
	rec := &recordingGuest{}
	err := copyTo(rec, filepath.Join(t.TempDir(), "nope"), "/dst")
	if err == nil || !strings.Contains(err.Error(), "source") {
		t.Fatalf("copyTo missing source: got %v, want a source error", err)
	}
	if rec.called {
		t.Error("transport was called despite a missing source")
	}
}

func TestCopyToGuestError(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src")
	writeFile(t, src, "x", 0o644)

	err := copyTo(&fakeGuest{failStderr: "tar: /dst: Cannot mkdir"}, src, "/dst/x")
	if err == nil || !strings.Contains(err.Error(), "Cannot mkdir") {
		t.Fatalf("copyTo guest error: got %v, want it to wrap the guest stderr", err)
	}
}

func TestCopyFromGuestError(t *testing.T) {
	err := copyFrom(&fakeGuest{failStderr: "tar: /g/x: No such file"}, "/g/x", filepath.Join(t.TempDir(), "y"))
	if err == nil || !strings.Contains(err.Error(), "No such file") {
		t.Fatalf("copyFrom guest error: got %v, want it to wrap the guest stderr", err)
	}
}

func TestExtractTarRejectsTraversal(t *testing.T) {
	t.Run("dotdot member", func(t *testing.T) {
		dest := t.TempDir()
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		writeHeader(t, tw, &tar.Header{Name: "../escape.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: 4})
		if _, err := tw.Write([]byte("evil")); err != nil {
			t.Fatalf("write body: %v", err)
		}
		mustClose(t, tw)

		err := extractTar(&buf, dest, "", "")
		if err == nil || !strings.Contains(err.Error(), "unsafe") {
			t.Fatalf("extractTar dotdot: got %v, want an unsafe-entry error", err)
		}
		if _, statErr := os.Stat(filepath.Join(dest, "..", "escape.txt")); !errors.Is(statErr, fs.ErrNotExist) {
			t.Errorf("dotdot entry was written outside dest: %v", statErr)
		}
	})

	t.Run("escaping symlink", func(t *testing.T) {
		dest := t.TempDir()
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		writeHeader(t, tw, &tar.Header{
			Name: "evil", Typeflag: tar.TypeSymlink, Linkname: "../../etc/passwd", Mode: 0o777,
		})
		mustClose(t, tw)

		err := extractTar(&buf, dest, "", "")
		if err == nil || !strings.Contains(err.Error(), "unsafe symlink") {
			t.Fatalf("extractTar escaping symlink: got %v, want an unsafe-symlink error", err)
		}
		if _, statErr := os.Lstat(filepath.Join(dest, "evil")); !errors.Is(statErr, fs.ErrNotExist) {
			t.Errorf("escaping symlink was written: %v", statErr)
		}
	})

	t.Run("absolute symlink", func(t *testing.T) {
		dest := t.TempDir()
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		writeHeader(t, tw, &tar.Header{
			Name: "evil", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd", Mode: 0o777,
		})
		mustClose(t, tw)

		err := extractTar(&buf, dest, "", "")
		if err == nil || !strings.Contains(err.Error(), "unsafe symlink") {
			t.Fatalf("extractTar absolute symlink: got %v, want an unsafe-symlink error", err)
		}
	})

	t.Run("symlink parent traversal", func(t *testing.T) {
		// The escape that a text-only name check misses: plant a symlink (d/p -> x,
		// which passes the lexical target check), then route later entries *through*
		// it. d/p/up materializes at the real path x/up; its "../../pwned" target then
		// climbs out of dest. The parent-symlink guard must refuse it.
		dest := filepath.Join(t.TempDir(), "root")
		if err := os.Mkdir(dest, 0o755); err != nil {
			t.Fatalf("mkdir dest: %v", err)
		}
		outside := filepath.Join(filepath.Dir(dest), "pwned")

		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		writeHeader(t, tw, &tar.Header{Name: "d/", Typeflag: tar.TypeDir, Mode: 0o755})
		writeHeader(t, tw, &tar.Header{Name: "d/p", Typeflag: tar.TypeSymlink, Linkname: "../x", Mode: 0o777})
		writeHeader(t, tw, &tar.Header{Name: "x/", Typeflag: tar.TypeDir, Mode: 0o755})
		writeHeader(t, tw, &tar.Header{Name: "d/p/up", Typeflag: tar.TypeSymlink, Linkname: "../../pwned", Mode: 0o777})
		writeHeader(t, tw, &tar.Header{Name: "d/p/up/evil", Typeflag: tar.TypeReg, Mode: 0o644, Size: 4})
		if _, err := tw.Write([]byte("evil")); err != nil {
			t.Fatalf("write body: %v", err)
		}
		mustClose(t, tw)

		err := extractTar(&buf, dest, "", "")
		if err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("extractTar symlink-parent traversal: got %v, want a symlink-parent rejection", err)
		}
		if _, statErr := os.Stat(outside); !errors.Is(statErr, fs.ErrNotExist) {
			t.Errorf("escape wrote outside dest at %q: %v", outside, statErr)
		}
	})
}

func TestExtractTarRestoresReadOnlyDirMode(t *testing.T) {
	// A read-only (0555) directory entry that precedes its child must not block the
	// child's write — the extractor creates dirs writable and restores their modes
	// only after extraction — and the directory's mode is preserved (defeating both
	// the EACCES trap and umask).
	dest := t.TempDir()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	writeHeader(t, tw, &tar.Header{Name: "ro/", Typeflag: tar.TypeDir, Mode: 0o555})
	writeHeader(t, tw, &tar.Header{Name: "ro/f.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: 6})
	if _, err := tw.Write([]byte("inside")); err != nil {
		t.Fatalf("write body: %v", err)
	}
	mustClose(t, tw)

	if err := extractTar(&buf, dest, "", ""); err != nil {
		t.Fatalf("extractTar read-only dir: %v", err)
	}
	roDir := filepath.Join(dest, "ro")
	// Let t.TempDir cleanup remove the now-read-only directory.
	t.Cleanup(func() { _ = os.Chmod(roDir, 0o755) })

	assertFile(t, filepath.Join(roDir, "f.txt"), "inside", 0o644)
	if info, err := os.Stat(roDir); err != nil {
		t.Fatalf("stat ro: %v", err)
	} else if info.Mode().Perm() != 0o555 {
		t.Errorf("ro dir mode = %o, want 555", info.Mode().Perm())
	}
}

// --- helpers ---

func writeFile(t *testing.T, path, content string, mode fs.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatalf("write %q: %v", path, err)
	}
	// WriteFile's mode is masked by umask; chmod to make the test deterministic.
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod %q: %v", path, err)
	}
}

func mustMkdir(t *testing.T, path string, mode fs.FileMode) {
	t.Helper()
	if err := os.Mkdir(path, mode); err != nil {
		t.Fatalf("mkdir %q: %v", path, err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod %q: %v", path, err)
	}
}

func mustSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink %q -> %q: %v", link, target, err)
	}
}

func writeHeader(t *testing.T, tw *tar.Writer, hdr *tar.Header) {
	t.Helper()
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatalf("write header %q: %v", hdr.Name, err)
	}
}

func mustClose(t *testing.T, tw *tar.Writer) {
	t.Helper()
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
}

func assertFile(t *testing.T, path, content string, mode fs.FileMode) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %q: %v", path, err)
	}
	if string(got) != content {
		t.Errorf("%q content = %q, want %q", path, got, content)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %q: %v", path, err)
	}
	if info.Mode().Perm() != mode {
		t.Errorf("%q mode = %o, want %o", path, info.Mode().Perm(), mode)
	}
}

func assertSymlink(t *testing.T, path, wantTarget string) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat %q: %v", path, err)
	}
	if info.Mode()&fs.ModeSymlink == 0 {
		t.Errorf("%q is not a symlink (mode %v)", path, info.Mode())
		return
	}
	got, err := os.Readlink(path)
	if err != nil {
		t.Fatalf("readlink %q: %v", path, err)
	}
	if got != wantTarget {
		t.Errorf("%q -> %q, want %q", path, got, wantTarget)
	}
}

// assertTreeEqual compares two directory trees by relative path, mode (perm bits
// for files, link target for symlinks), and file content. The top directory's own
// name differs between source and destination, so comparison is by relative path.
func assertTreeEqual(t *testing.T, want, got string) {
	t.Helper()
	wantTree := snapshot(t, want)
	gotTree := snapshot(t, got)
	for rel, w := range wantTree {
		g, ok := gotTree[rel]
		if !ok {
			t.Errorf("missing entry %q in copied tree", rel)
			continue
		}
		if w != g {
			t.Errorf("entry %q mismatch:\n want %+v\n  got %+v", rel, w, g)
		}
	}
	for rel := range gotTree {
		if _, ok := wantTree[rel]; !ok {
			t.Errorf("unexpected extra entry %q in copied tree", rel)
		}
	}
}

type treeEntry struct {
	isDir   bool
	mode    fs.FileMode
	content string
	link    string
}

func snapshot(t *testing.T, root string) map[string]treeEntry {
	t.Helper()
	out := map[string]treeEntry{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		e := treeEntry{mode: info.Mode().Perm()}
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			e.link, _ = os.Readlink(p)
			e.mode = 0 // symlink perms are not meaningful/portable
		case info.IsDir():
			e.isDir = true
		default:
			b, readErr := os.ReadFile(p)
			if readErr != nil {
				return readErr
			}
			e.content = string(b)
		}
		out[filepath.ToSlash(rel)] = e
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot %q: %v", root, err)
	}
	return out
}
