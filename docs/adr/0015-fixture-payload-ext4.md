# ADR: Read-Only Fixture Payloads via ext4 (replacing live mounts)

**Date:** 2026-06-08
**Status:** Accepted

## Context

ADR-0010 gave macOS a live, read-write `WithMount` folder share built on VZ-native
virtiofs. It was macOS-only by construction: ADR-0011 deferred mounts on Linux because
cloud-hypervisor has **no built-in virtio-fs server** — `--fs` needs an external
`virtiofsd` daemon, and there is no clean prebuilt static `virtiofsd` binary for
amd64+arm64 to checksum-pin the way we pin cloud-hypervisor (upstream ships source only;
the one fork is x86_64-debug-only). cloud-hypervisor has no virtio-9p either. The only
daemon-free host→guest paths it offers are a read-only block image or virtio-pmem — both
snapshot-at-boot, not live.

So the cross-platform options were: take a `virtiofsd` host dependency on Linux just to
mirror macOS's *free* built-in virtio-fs, or drop live mounts and standardize on something
both backends do natively. The constraints from earlier decisions still bind: the public
API stays backend-neutral (ADR-0002), nothing of ours runs in the guest (ADR-0005), and
the store is now cluster-rooted at `clusters/<cluster>/<member>/` (ADR-0014), which
ADR-0014 itself flagged as the foundation this payload work would build on.

## Decision

Replace `WithMount` entirely with a read-only, snapshot-at-boot **fixture**:
`WithFixture(hostDir, guestPath)`. It works identically on macOS (VZ) and Linux
(cloud-hypervisor) through one code path. `WithMount` and all its plumbing are deleted —
no deprecation, no alias, no compat (the project is pre-release; a developer with old
on-disk state deletes `~/.fleetbox` by hand, the ADR-0014 stance).

- **Mechanism: dir → ext4 image → read-only block device → cloud-init `mounts:` by LABEL.**
  At boot the host directory is packed into an ext4 filesystem image, attached to the VM as
  an additional **read-only** block device (the same way `seed.iso` is attached read-only on
  both backends), and mounted by stock cloud-init with a `mounts:` line
  `- [ LABEL=<label>, <guestPath>, ext4, "ro,nofail", "0", "0" ]`. The `ext4` driver is in
  every Linux kernel; nothing is installed in the guest. The guest mounts **by `LABEL=`,
  never by `/dev/vdX`** — adding disks shifts device names, and the guest must not depend on
  order. go-ext4fs images carry no journal and are written clean, so a plain `-o ro` mount
  needs no `norecovery`/`noload`.

- **Why read-only copy-in is the right model, not a compromise.** For the test-fixture use
  case it is arguably better than a live mount: immutable and reproducible, no pollution of
  the host tree, and parallel-safe across cluster members (each gets its own snapshot). The
  output direction is already covered by `fleetbox cp` / scp; the coherent story is
  "fixtures in, `cp` out," both one-shot and daemon-free.

- **ext4 via `go-ext4fs`, not ISO9660 via `cloudiso`.** cloudiso's writer (which we already
  use for the seed) rejects any path component that isn't `[A-Za-z0-9._-]` or is >31 chars
  and has no symlink support — real fixture dirs (long names, spaces, unicode, symlinked
  `node_modules`) would fail to pack. `go-ext4fs` (the same author's pure-Go ext4 writer)
  lifts all of that: 255-byte names, arbitrary bytes, symlinks, nesting, per-entry
  uid/gid/mode — verified by its Docker e2e suite that mounts the images in a real Linux
  kernel. It is pure Go, no cgo, no external `mke2fs`, so it works on the macOS dev box too,
  keeping the "Linux path is pure Go" property (ADR-0011) intact. It joins cloudiso as the
  second small pure-Go image library. The seed ISO **stays** on cloudiso — NoCloud requires
  an `iso9660`/`vfat` filesystem labeled `cidata`; only the fixture payload is ext4.

- **Per-member image, not a cluster-level shared file.** Each fixture image lives at
  `clusters/<cluster>/<member>/fixture-<i>.img`. A cluster-root artifact would be wrong on
  two counts: (1) `clusterName` is lossy — independent solo VMs `node` and `node-2024` both
  resolve to `clusters/node/`, so a shared image would leak one stranger's fixtures to the
  other (the collision ADR-0014 flagged for exactly this task); (2) a file at the cluster
  root would make `clusters/<cluster>/` non-empty, breaking `store.Delete`'s
  `os.Remove(clusterDir)` empty-cluster cleanup. Per-member means each image is wiped for
  free by the existing `store.Delete` → `os.RemoveAll(memberDir)` — zero new teardown code.
  The cost is N identical copies for an N-member homogeneous cluster (small, cheap to build;
  hardlink-dedup is a possible later optimization, out of scope).

- **No size pre-computation, no cache: generous canvas, then fit.** The image is built on a
  16 GiB sparse canvas, all files written, then `Resize(MinSize())` shrinks it to fit
  (`MinSize` is a high-water mark over both the highest data block and the highest allocated
  inode, so it shrinks safely even for empty-file-heavy trees) and `Save()`. The on-disk
  file ends up a few MiB for typical fixtures; the 16 GiB canvas never materializes (freshly
  truncated inode tables stay sparse). This removes the ext4 "compute a size + inode budget"
  problem entirely. The image is **rebuilt on every boot** — no content-hash cache. A test
  cluster boots exactly once and is then fully destroyed (`t.Cleanup → Destroy`), so there
  is no second boot to optimize; a CLI cluster is long-lived and rebuilt only on `up`, where
  building a small image is negligible. Documented limit: a fixture payload larger than
  16 GiB is unsupported (the resize bracket).

- **Frozen set, refreshed content.** Mirroring how mounts and cpu/mem/disk already behave:
  at first create the fixtures are validated, absolutized, assigned a stable label
  `FBFIX<i>` (i = index), and persisted in `store.VM.Fixtures`. On a later boot of an
  existing VM the persisted list wins and a different `WithFixture` set is silently ignored.
  But because there is no cache, each boot rebuilds each image from the persisted `HostPath`,
  so the guest sees the host dir as it is *at that boot* (refreshed per boot, never live
  within a boot). The label is the single source of truth shared between the image's volume
  label and the guest's `LABEL=` mount line — computed once, persisted, never re-derived.
  The `mounts:` line is written into the seed only at first create; it persists via the
  guest's `/etc/fstab`, so reboots re-mount without re-running cloud-init while the rebuilt
  image keeps the same label.

- **Guest readability is explicit, host perms are not preserved.** go-ext4fs takes per-entry
  `mode, uid, gid`; the builder sets every file to `0444`, every directory to `0555` (the
  exec bit dirs need to be traversable), uid/gid `0`, so any guest user (uid 1000) can read
  the whole tree. This makes ADR-0010's uid-alignment hack unnecessary — it is deleted, not
  ported. Host mode/exec bits are **not** carried (everything arrives world-readable,
  non-executable); preserving them is a possible later enhancement (go-ext4fs supports it),
  out of scope. Symlinks are emitted as symlinks.

## Alternatives Considered

- **Take a `virtiofsd` dependency on Linux** to keep live mounts cross-platform. Rejected:
  no clean prebuilt static binary to checksum-pin for both arches, and it would add a host
  daemon dependency to mirror a capability macOS gets for free — the opposite of the
  download-and-pin discipline (ADR-0011). It would also keep the read-write/identity
  complexity (uid alignment) that fixtures make moot.
- **virtio-pmem** instead of a read-only block disk. Also daemon-free on cloud-hypervisor,
  but a block disk is the path both backends already use for the seed, so it is one
  attachment pattern, not two. No upside for fixtures.
- **ISO9660 via cloudiso** (already a dependency, no new module). Rejected: its 31-char,
  charset-restricted, symlink-less filenames fail on real fixture trees — the very reason to
  reach for ext4.
- **Cluster-level shared image** (one `fixture.img` per cluster). Rejected: lossy
  `clusterName` collisions leak fixtures between unrelated solo VMs, and a cluster-root file
  breaks empty-cluster cleanup (see Decision).
- **Content-hash cache / hardlink dedup** of per-member images. Rejected for v1: a test VM
  boots once then is destroyed, so there is nothing to cache; the homogeneous-cluster
  duplication is cheap. A possible later optimization.

## Consequences

- Test authors get `fleetboxtest.Start(t, image, fleetbox.WithFixture(dir, "/work"))` and
  CLI users `fleetbox up dev --fixture ./src:/work` — a read-only, world-readable copy of a
  host directory at a guest path, identical on macOS and Linux. The first cross-platform
  host→guest data path.
- **Live read-write sharing is gone with no replacement.** A caller that wants edits to flow
  back has `fleetbox cp` / scp for the output direction; there is no live bind mount on
  either platform. This is the deliberate trade for one daemon-free cross-platform path.
- **A new module dependency.** `github.com/pilat/go-ext4fs` provides the ext4 writer (with
  the custom volume label + two-way resize this design relies on). Pure Go, no cgo — the
  Linux path stays cgo-free.
- **Fixture filenames are unconstrained** (255-byte names, spaces, unicode, symlinks) — the
  ISO9660 limitation that prompted the switch is resolved.
- **Host file permissions and exec bits are not preserved** — everything arrives `0444` /
  `0555`, uid 0. Sufficient for read-only fixtures; perm-preservation is a possible later
  enhancement.
- The fixtureless path is unchanged: no extra block device, no `mounts:` block, no `uid:`
  line — a VM with no fixtures and its seed output are byte-for-byte identical.
- ADR-0010 is **superseded** by this ADR; ADR-0011's "virtio-fs deferred / `ErrMountsUnsupported`"
  note is dropped (mounts are not deferred — they are gone). This builds on the
  cluster-rooted store (ADR-0014), resolving the per-member-vs-cluster-root collision it
  flagged.
