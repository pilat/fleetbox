package cloudhypervisor

import (
	"bytes"
	"compress/gzip"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// fakeFS is an in-memory bootFS for testing the extraction seam on the dev box,
// where boot_arm64.go (the only non-test caller, linux && arm64) does not compile —
// the same reason purehelpers.go is tested in isolation. It mirrors the *ext4.FileSystem
// surface: io/fs-style paths (slash-separated, no leading slash, root ".").
var _ bootFS = (*fakeFS)(nil)

type fakeNode struct {
	mode    fs.FileMode
	modTime time.Time
	data    []byte
	target  string
}

type fakeFS struct {
	nodes map[string]*fakeNode
}

type fakeDirEntry struct {
	name string
	node *fakeNode
}

type fakeFileInfo struct {
	name string
	node *fakeNode
}

type fakeFile struct {
	name string
	node *fakeNode
	r    *bytes.Reader
}

func newFakeFS() *fakeFS { return &fakeFS{nodes: map[string]*fakeNode{}} }

func (ffs *fakeFS) addFile(p string, data []byte, mt time.Time) {
	ffs.ensureDir(path.Dir(p))
	ffs.nodes[p] = &fakeNode{modTime: mt, data: data}
}

func (ffs *fakeFS) addSymlink(p, target string, mt time.Time) {
	ffs.ensureDir(path.Dir(p))
	ffs.nodes[p] = &fakeNode{mode: fs.ModeSymlink, modTime: mt, target: target}
}

func (ffs *fakeFS) ensureDir(d string) {
	if d == "." || d == "" {
		return
	}
	if _, ok := ffs.nodes[d]; !ok {
		ffs.nodes[d] = &fakeNode{mode: fs.ModeDir}
		ffs.ensureDir(path.Dir(d))
	}
}

func (ffs *fakeFS) ReadDir(dir string) ([]fs.DirEntry, error) {
	if dir != "." {
		n, ok := ffs.nodes[dir]
		if !ok {
			return nil, &fs.PathError{Op: "readdir", Path: dir, Err: fs.ErrNotExist}
		}
		if !n.mode.IsDir() {
			return nil, &fs.PathError{Op: "readdir", Path: dir, Err: fs.ErrInvalid}
		}
	}
	var out []fs.DirEntry
	for p, n := range ffs.nodes {
		if p != "." && path.Dir(p) == dir {
			out = append(out, fakeDirEntry{name: path.Base(p), node: n})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out, nil
}

func (ffs *fakeFS) Open(p string) (fs.File, error) {
	n, ok := ffs.nodes[p]
	if !ok {
		return nil, &fs.PathError{Op: "open", Path: p, Err: fs.ErrNotExist}
	}
	return &fakeFile{name: path.Base(p), node: n, r: bytes.NewReader(n.data)}, nil
}

func (ffs *fakeFS) ReadLink(p string) (string, error) {
	n, ok := ffs.nodes[p]
	if !ok || n.mode&fs.ModeSymlink == 0 {
		return "", &fs.PathError{Op: "readlink", Path: p, Err: fs.ErrInvalid}
	}
	return n.target, nil
}

func (e fakeDirEntry) Name() string               { return e.name }
func (e fakeDirEntry) IsDir() bool                { return e.node.mode.IsDir() }
func (e fakeDirEntry) Type() fs.FileMode          { return e.node.mode.Type() }
func (e fakeDirEntry) Info() (fs.FileInfo, error) { return fakeFileInfo(e), nil }

func (fi fakeFileInfo) Name() string       { return fi.name }
func (fi fakeFileInfo) Size() int64        { return int64(len(fi.node.data)) }
func (fi fakeFileInfo) Mode() fs.FileMode  { return fi.node.mode }
func (fi fakeFileInfo) ModTime() time.Time { return fi.node.modTime }
func (fi fakeFileInfo) IsDir() bool        { return fi.node.mode.IsDir() }
func (fi fakeFileInfo) Sys() any           { return nil }

func (f *fakeFile) Stat() (fs.FileInfo, error) { return fakeFileInfo{name: f.name, node: f.node}, nil }
func (f *fakeFile) Read(p []byte) (int, error) { return f.r.Read(p) }
func (f *fakeFile) Close() error               { return nil }

func gzipBytes(t *testing.T, raw []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(raw); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

func TestResolveBootFileNewestVersioned(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	ffs := newFakeFS()
	ffs.addFile("boot/vmlinuz-6.1.0-1", []byte("one"), base)
	ffs.addFile("boot/vmlinuz-6.1.0-2", []byte("two"), base.Add(time.Hour)) // newest
	ffs.addFile("boot/vmlinuz-6.1.0-2.old", []byte("old"), base.Add(2*time.Hour))

	got, err := resolveBootFile(ffs, "boot", "vmlinuz")
	if err != nil {
		t.Fatalf("resolveBootFile: %v", err)
	}
	if got != "boot/vmlinuz-6.1.0-2" {
		t.Errorf("resolveBootFile = %q, want boot/vmlinuz-6.1.0-2 (newest non-.old)", got)
	}
}

func TestResolveBootFileInitrd(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	ffs := newFakeFS()
	ffs.addFile("boot/initrd.img-6.1.0-1", []byte("one"), base)
	ffs.addFile("boot/initrd.img-6.1.0-2", []byte("two"), base.Add(time.Hour)) // newest

	got, err := resolveBootFile(ffs, "boot", "initrd.img")
	if err != nil {
		t.Fatalf("resolveBootFile: %v", err)
	}
	if got != "boot/initrd.img-6.1.0-2" {
		t.Errorf("resolveBootFile = %q, want boot/initrd.img-6.1.0-2", got)
	}
}

func TestResolveBootFileAtRoot(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	ffs := newFakeFS()
	// Separate /boot partition: the kernel lives at the filesystem root, dir = ".".
	ffs.addFile("vmlinuz-6.1.0-2", []byte("k"), base)

	got, err := resolveBootFile(ffs, ".", "vmlinuz")
	if err != nil {
		t.Fatalf("resolveBootFile: %v", err)
	}
	if got != "vmlinuz-6.1.0-2" {
		t.Errorf("resolveBootFile = %q, want vmlinuz-6.1.0-2 (kernel at fs root)", got)
	}
}

func TestResolveBootFileSymlinkPreference(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	ffs := newFakeFS()
	// An older versioned file plus the unversioned symlink to the real kernel.
	ffs.addFile("boot/vmlinuz-6.1.0-1", []byte("versioned"), base)
	ffs.addFile("boot/vmlinuz-6.1.0-9", []byte("KERNELBYTES"), base.Add(time.Hour))
	ffs.addSymlink("boot/vmlinuz", "vmlinuz-6.1.0-9", base.Add(2*time.Hour))

	got, err := resolveBootFile(ffs, "boot", "vmlinuz")
	if err != nil {
		t.Fatalf("resolveBootFile: %v", err)
	}
	if got != "boot/vmlinuz-6.1.0-9" {
		t.Fatalf("resolveBootFile = %q, want boot/vmlinuz-6.1.0-9 (symlink target)", got)
	}
	// copyKernel must read the resolved target's bytes.
	dst := filepath.Join(t.TempDir(), "vmlinux")
	if err := copyKernel(ffs, got, dst); err != nil {
		t.Fatalf("copyKernel: %v", err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "KERNELBYTES" {
		t.Errorf("copyKernel wrote %q, want KERNELBYTES", b)
	}
}

func TestResolveBootFileSymlinkAbsoluteTarget(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	ffs := newFakeFS()
	// An absolute symlink target (rooted at the filesystem root, not relative to dir).
	ffs.addFile("boot/vmlinuz-6.1.0-9", []byte("ABS"), base)
	ffs.addSymlink("boot/vmlinuz", "/boot/vmlinuz-6.1.0-9", base.Add(time.Hour))

	got, err := resolveBootFile(ffs, "boot", "vmlinuz")
	if err != nil {
		t.Fatalf("resolveBootFile: %v", err)
	}
	if got != "boot/vmlinuz-6.1.0-9" {
		t.Errorf("resolveBootFile = %q, want boot/vmlinuz-6.1.0-9 (absolute target stripped to fs root)", got)
	}
}

func TestResolveBootFileSymlinkNonRegularFallsThrough(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	ffs := newFakeFS()
	// vmlinuz is a symlink pointing at a directory (not a regular file).
	ffs.ensureDir("boot/notakernel")
	ffs.addSymlink("boot/vmlinuz", "notakernel", base.Add(time.Hour))
	ffs.addFile("boot/vmlinuz-6.1.0-5", []byte("real"), base)

	got, err := resolveBootFile(ffs, "boot", "vmlinuz")
	if err != nil {
		t.Fatalf("resolveBootFile: %v", err)
	}
	if got != "boot/vmlinuz-6.1.0-5" {
		t.Errorf("resolveBootFile = %q, want boot/vmlinuz-6.1.0-5 (fell through to versioned)", got)
	}
}

func TestResolveBootFilePlainRegularFile(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	ffs := newFakeFS()
	// vmlinuz is a plain regular file (not a symlink) and there is NO versioned
	// sibling — the old EvalSymlinks path accepted this; the new code must too.
	ffs.addFile("boot/vmlinuz", []byte("PLAIN"), base)

	got, err := resolveBootFile(ffs, "boot", "vmlinuz")
	if err != nil {
		t.Fatalf("resolveBootFile: %v", err)
	}
	if got != "boot/vmlinuz" {
		t.Errorf("resolveBootFile = %q, want boot/vmlinuz (plain regular file)", got)
	}
}

func TestResolveBootFileNotFound(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	ffs := newFakeFS()
	ffs.addFile("boot/grub.cfg", []byte("x"), base) // no vmlinuz of any kind

	if _, err := resolveBootFile(ffs, "boot", "vmlinuz"); err == nil {
		t.Fatal("resolveBootFile should error when no vmlinuz exists")
	} else if !strings.Contains(err.Error(), "no vmlinuz under boot") {
		t.Errorf("error = %q, want it to mention 'no vmlinuz under boot'", err)
	}
}

func TestResolveBootFileReadDirError(t *testing.T) {
	ffs := newFakeFS() // empty: "missingdir" does not exist

	if _, err := resolveBootFile(ffs, "missingdir", "vmlinuz"); err == nil {
		t.Fatal("resolveBootFile should error when the directory cannot be read")
	} else if !strings.Contains(err.Error(), "read missingdir") {
		t.Errorf("error = %q, want it to mention 'read missingdir'", err)
	}
}

func TestCopyOpenErrors(t *testing.T) {
	ffs := newFakeFS()
	dst := filepath.Join(t.TempDir(), "out")
	if err := copyKernel(ffs, "boot/missing", dst); err == nil {
		t.Error("copyKernel should error when src is absent")
	}
	if err := copyFile(ffs, "boot/missing", dst); err == nil {
		t.Error("copyFile should error when src is absent")
	}
}

func TestCopyKernelShortAndBadGzip(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	ffs := newFakeFS()
	ffs.addFile("boot/vmlinuz-short", []byte{0x42}, base)             // < 2 bytes → magic read fails
	ffs.addFile("boot/vmlinuz-badgz", []byte{0x1f, 0x8b, 0x00}, base) // gzip magic, garbage body

	dst := filepath.Join(t.TempDir(), "out")
	if err := copyKernel(ffs, "boot/vmlinuz-short", dst); err == nil {
		t.Error("copyKernel should error on a sub-2-byte file")
	}
	if err := copyKernel(ffs, "boot/vmlinuz-badgz", dst); err == nil {
		t.Error("copyKernel should error on a corrupt gzip kernel")
	}
}

func TestAtomicWriteCreateError(t *testing.T) {
	// A dst whose parent directory does not exist → os.Create fails, no file left.
	dst := filepath.Join(t.TempDir(), "nope", "out")
	if err := atomicWrite(dst, strings.NewReader("data")); err == nil {
		t.Error("atomicWrite should error when the parent dir is missing")
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Error("atomicWrite left a file behind on a create failure")
	}
}

func TestCopyKernelGzipAndRaw(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	raw := []byte("uncompressed arm64 Image payload")
	ffs := newFakeFS()
	ffs.addFile("boot/vmlinuz-gz", gzipBytes(t, raw), base)
	ffs.addFile("boot/vmlinuz-raw", raw, base)

	// gzip kernel → decompressed on disk.
	dstGz := filepath.Join(t.TempDir(), "k-gz")
	if err := copyKernel(ffs, "boot/vmlinuz-gz", dstGz); err != nil {
		t.Fatalf("copyKernel(gz): %v", err)
	}
	if b, _ := os.ReadFile(dstGz); !bytes.Equal(b, raw) {
		t.Errorf("copyKernel(gz) wrote %q, want decompressed %q", b, raw)
	}

	// raw kernel → verbatim.
	dstRaw := filepath.Join(t.TempDir(), "k-raw")
	if err := copyKernel(ffs, "boot/vmlinuz-raw", dstRaw); err != nil {
		t.Fatalf("copyKernel(raw): %v", err)
	}
	if b, _ := os.ReadFile(dstRaw); !bytes.Equal(b, raw) {
		t.Errorf("copyKernel(raw) wrote %q, want %q", b, raw)
	}
}

func TestCopyFileVerbatim(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	// A gzip-magic-prefixed payload proves copyFile does NOT gunzip (unlike copyKernel).
	payload := gzipBytes(t, []byte("initrd contents"))
	ffs := newFakeFS()
	ffs.addFile("boot/initrd.img-6.1.0-2", payload, base)

	dst := filepath.Join(t.TempDir(), "initrd.img")
	if err := copyFile(ffs, "boot/initrd.img-6.1.0-2", dst); err != nil {
		t.Fatalf("copyFile: %v", err)
	}
	if b, _ := os.ReadFile(dst); !bytes.Equal(b, payload) {
		t.Errorf("copyFile altered the payload; want verbatim gzip bytes")
	}
}

func TestAtomicWrite(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "out")
	if err := atomicWrite(dst, strings.NewReader("hello world")); err != nil {
		t.Fatalf("atomicWrite: %v", err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "hello world" {
		t.Errorf("atomicWrite wrote %q, want hello world", b)
	}
	if _, err := os.Stat(dst + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("atomicWrite left a .tmp sibling behind")
	}
}

func TestFindBootDir(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)

	// /boot inside root (debian).
	inRoot := newFakeFS()
	inRoot.addFile("boot/vmlinuz-6.1.0-2", []byte("k"), base)
	if dir, ok := findBootDir(inRoot); !ok || dir != "boot" {
		t.Errorf("findBootDir(/boot inside root) = (%q,%v), want (boot,true)", dir, ok)
	}

	// separate /boot partition: kernel at filesystem root.
	atRoot := newFakeFS()
	atRoot.addFile("vmlinuz-6.1.0-2", []byte("k"), base)
	if dir, ok := findBootDir(atRoot); !ok || dir != "." {
		t.Errorf("findBootDir(kernel at root) = (%q,%v), want (.,true)", dir, ok)
	}

	// no kernel anywhere (e.g. the vfat ESP).
	none := newFakeFS()
	none.addFile("boot/grub/grub.cfg", []byte("x"), base)
	if dir, ok := findBootDir(none); ok {
		t.Errorf("findBootDir(no kernel) = (%q,%v), want (\"\",false)", dir, ok)
	}
}

func TestFileExists(t *testing.T) {
	p := filepath.Join(t.TempDir(), "present")
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatalf("write temp: %v", err)
	}
	if !fileExists(p) {
		t.Errorf("fileExists(%q) = false, want true", p)
	}
	if fileExists(filepath.Join(t.TempDir(), "missing")) {
		t.Errorf("fileExists(missing) = true, want false")
	}
}
