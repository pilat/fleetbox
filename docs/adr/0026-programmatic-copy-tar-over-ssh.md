# ADR: Programmatic File Copy via tar-over-SSH

**Date:** 2026-06-13
**Status:** Accepted

## Context

fleetbox is library-first: every capability must exist in the Go API, and the CLI is
only a wrapper ([ADR-0001](0001-library-first-standalone-repo.md)). File copy violated
that. It existed **only** as a CLI shell-out to the system `scp` binary
(`cmd/fleetbox/cp.go`); the library had no way to move a file in or out of a VM. The
project's own dogfood gate had to work around the hole by shelling out to `scp` and
hand-reconstructing fleetbox's private-key path. `WithFixture`
([ADR-0015](0015-fixture-payload-ext4.md)) only covers read-only host→guest at boot —
there was no copy-out at all, and no copy-in to a running VM.

We want `VM.CopyTo`/`VM.CopyFrom` on the public API — universal (file **or** directory,
both directions) — and then to move the CLI `cp` and the dogfood onto the same primitive
so copy stops shelling out to `scp`. The project is deliberately frugal about
dependencies (the zero-cgo Linux path, netlink/nftables instead of shelling out —
[ADR-0025](0025-linux-netlink-nftables.md)), so a new copy mechanism should add nothing
to `go.mod`.

## Decision

**Copy is `tar` streamed over the SSH session fleetbox already holds.** `CopyTo` builds
a tar stream in-process with stdlib `archive/tar` and pipes it to a guest `tar -x`;
`CopyFrom` runs a guest `tar -c`, reads its stdout, and extracts in-process. tar carries
files, directories, modes, and symlinks for free, so one mechanism is universal. It
rides `golang.org/x/crypto/ssh` (already a dependency, already powering `vm.SSH`) plus
stdlib and adds **zero** Go module dependencies. The only guest-side requirement is
`tar`, present on every image in the catalog — the same "stock distro tool" category
`vm.SSH` already assumes.

The primitive lives on `sshkey.Client` (which already wraps a live `*ssh.Client`),
decomposed for testability into pure tar build/extract, pure command builders, and a
small SSH transport interface the `Client` implements. The public `VM.CopyTo`/`CopyFrom`
delegate through `orchestrator.VM`, which dials the VM IP directly exactly as `SSH`
does. All three consumers — the library `VM`, the CLI `cp`, and the dogfood — share this
one implementation.

**Purely client-side.** The control protocol, the helper, and the backends are not
touched. `orchestrator.VM.SSH` already dials the VM IP directly and the control wire
carries no exec/ssh/copy verb; the orchestrator runs client-side on both platforms after
[ADR-0020](0020-helper-thin-backend-server.md), so copy needs the same inputs as SSH —
the VM IP and the per-installation key — and is identical on darwin and linux.

Pinned semantics:

- **Exact-destination, not cp's "copy into a directory."** `CopyTo(file, /a/b/c)` makes
  the file `c`; `CopyTo(dir, /a/b/c)` makes the directory `c`; `CopyFrom` is symmetric.
  The library does not inspect the destination and copy *inside* it. The guest commands
  are `mkdir -p <dir> && tar -x -p --no-same-owner -C <dir>` (the archive is rooted at
  the destination's basename) and `tar -c -C <dir(guestPath)> <base(guestPath)>` (the
  in-process extractor renames the top component to the host destination's basename).
- **The CLI keeps scp's convenience above the library.** `fleetbox cp web:/x .` and
  `… web:/x ./dir/` still copy into a directory destination. The CLI resolves a *local*
  destination that is `.`, `..`, ends in a separator, or is an existing directory to
  `<dir>/<base(source)>` **before** calling the exact-path library method.
- **Modes preserved, ownership not.** Permission bits ride the tar header and are
  restored (`-p` on the guest, explicit chmod in the in-process extractor), so an
  executable stays executable. Extraction runs as the connecting user
  (`--no-same-owner`; never chown in-process). This is the copy-path analogue of the
  README "preserve host permissions" roadmap item — it is about `CopyTo`/`CopyFrom`, NOT
  fixtures (fixtures stay world-readable uid-0 per ADR-0015, unchanged).
- **Safe extraction.** The in-process extractor (writing onto the host in `CopyFrom`)
  rejects any entry whose name is absolute or contains `..`, any symlink whose target
  escapes the destination root, and any entry whose parent path — as it exists on disk —
  traverses a symlink (so a hostile archive cannot plant a symlink as an early entry and
  route a later entry through it to escape; a text-only name check is not enough).
- **Streaming.** The tar producer/consumer and the SSH session are wired with `io.Pipe`
  + a goroutine so neither direction buffers the whole payload; large binaries and logs
  stream.

## Alternatives Considered

**`github.com/pkg/sftp`.** Rejected. It adds two modules (`pkg/sftp` + `kr/fs`) and a
*new* guest-side assumption (the sshd `sftp` subsystem), and buys nothing tar-over-session
does not already give universally, at a dependency cost the project is deliberately frugal
about. Revisit only if a future need for random-access or resumable transfer actually
appears.

**Keep shelling out to `scp`.** Rejected. It requires `scp` on `PATH`, leaves the library
hole open, and keeps the library-first violation. Killing it is half the point.

**`io.Reader`/`io.Writer` copy variants instead of path-based.** Deferred, not chosen for
v1. The exported surface is kept minimal (every exported symbol is a documented library
commitment); the path-based pair covers the motivating cases. Reader/writer variants are
a possible later follow-up.

## Consequences

- The Go API gains `VM.CopyTo(ctx, hostPath, guestPath)` and
  `VM.CopyFrom(ctx, guestPath, hostPath)`. The CLI `cp` and the dogfood now use them; no
  code shells out to `scp` anymore (the user may still run system `scp` themselves —
  fleetbox writes a usable key and the guest runs sshd).
- `git diff go.mod go.sum` is empty: zero new dependencies. The client path stays
  cgo-free on both platforms.
- Strong hermetic coverage: the pure tar marshalling/extraction, the pure guest-command
  strings, and the SSH transport are separated so the copy round-trip and the exact guest
  commands are unit-tested without a VM. Real GNU `tar` (mode preservation across the hop,
  guest-side `mkdir -p`, `--no-same-owner`) is exercised by the VM-tier conformance test,
  which is therefore required coverage.
- `guestPath` must be absolute (a POSIX guest path, validated with `path.IsAbs`); a
  relative one is rejected with a clear error. Guest paths use `path`; host paths use
  `path/filepath`.

Deferred / out of scope (revisit when a concrete need appears):

- `io.Reader`/`io.Writer` copy variants.
- Honoring `ctx` cancellation mid-transfer. v1 matches `VM.SSH`, which accepts `ctx` for
  signature consistency but does not cancel an in-flight transfer; the dial uses the same
  handshake-timeout pattern as SSH.
- Tolerating GNU tar exit 1 ("file changed as we read it") when copying an
  actively-written file. v1 treats any non-zero guest `tar` exit as an error; copying a
  quiescent file/dir is the supported path.
- VM-to-VM copy (the CLI already rejects it; it stays rejected).
- sftp — only if random-access or resumable transfer becomes a concrete need.
