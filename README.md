# fleetbox

**Real Linux VMs as Go test fixtures — on macOS and Linux.**

[![Go Reference](https://pkg.go.dev/badge/github.com/pilat/fleetbox.svg)](https://pkg.go.dev/github.com/pilat/fleetbox)
![Platform](https://img.shields.io/badge/platform-macOS%20arm64%20%7C%20linux%20amd64%2Farm64-black)
![License](https://img.shields.io/badge/license-MIT-blue)

fleetbox boots stock Linux cloud images and hands them to your tests over SSH. On macOS
(Apple Silicon) it drives Apple's Virtualization.framework; on Linux, cloud-hypervisor.
The Go API is identical on both. Think testcontainers, except instead of a container you
get a whole machine: real kernel, real systemd, real `/dev/kvm`.

```go
func TestAgainstRealLinux(t *testing.T) {
	vm := fleetboxtest.Start(t, "debian-12")

	out, err := vm.SSH(context.Background(), "uname -a")
	if err != nil {
		t.Fatal(err)
	}
	t.Log(out) // Linux ... aarch64 GNU/Linux
}
```

One line gets you a booted Debian box, reachable over SSH with a keypair fleetbox
generates itself (it never touches your `~/.ssh`). When the test returns, the VM is
destroyed.

> **Status: v0.** It works, but the API will change and there are no compatibility
> promises yet. See [Limitations](#limitations).

## Why

Containers are wonderful right up until you need to test something a container can't
give you: a kernel module, a systemd unit, an nftables ruleset, `kubeadm`, a VPN,
anything that wants `/dev/kvm`. The usual fallback is a VM tool that arrives with a yaml
dialect, a background agent forwarding SSH ports, and patched images. That is a lot of
machinery, and all of it has to keep working.

fleetbox keeps the machinery small and refuses to grow it:

- **Real VMs, not containers.** It EFI-boots unmodified cloud images through their own
  bootloader; on M3+ you also get nested virtualization, so KVM works inside the guest.
- **Every VM gets a routable IP.** A vmnet SharedMode network on macOS 26+, a shared
  bridge with static addresses on Linux. No port forwarding to set up, no tunnel daemon
  to babysit: call `vm.IP()` and connect.
- **Nothing of ours runs in the guest.** No agent, no helper binary, no host/guest
  protocol. cloud-init configures the VM once at first boot; after that it's a plain
  distro you reach over SSH.
- **Library-first.** The Go package is the product; the CLI is a thin wrapper over the
  same calls. Test fixtures clean themselves up through `t.Cleanup`.

No yaml, no templates, no per-distro code paths. Flags and sane defaults.

## Requirements

- **macOS on Apple Silicon.** Clusters (VM↔VM networking) need macOS 26+; below that
  you get a single VM at a time. Nested virtualization (`/dev/kvm` in the guest) needs
  M3 or newer. Intel Macs are not supported.
- **Linux, amd64 or arm64.** Needs `/dev/kvm` (be in the `kvm` group) and **root** for the
  network — the shared bridge and per-VM taps. The CLI elevates itself: run `fleetbox up`
  and approve the one sudo password prompt. For the library and `go test`, run under sudo
  (see Install). fleetbox checks both before booting anything and fails with a clear error,
  not a cryptic boot one.

Plus Go 1.24+. The module compiles on `darwin/arm64` and `linux/{amd64,arm64}`; other
targets build but return a clear "unsupported platform" error at runtime.

## Install

```bash
go get github.com/pilat/fleetbox
```

That's the whole install: nothing to build, nothing to codesign. Your test binary stays
pure Go. On macOS, all the Virtualization.framework work lives in a small signed
`fleetbox-helper` that fleetbox downloads, checksum-pinned, into `~/.fleetbox/bin/` the
first time you boot a VM. Linux is the same story with a different binary: the
cloud-hypervisor VMM is fetched and pinned on first use.

Prefer the CLI to the library? On macOS (Apple Silicon) it ships as a Homebrew cask:

```bash
brew tap pilat/fleetbox
brew trust --cask pilat/fleetbox/fleetbox
brew install --cask fleetbox
```

On Linux (Homebrew has no casks there), install it straight from source:

```bash
go install github.com/pilat/fleetbox/cmd/fleetbox@latest
```

Either way you get the pure-Go `fleetbox` binary; the helper and VMM still auto-download
on first boot, exactly as they do for the library.

On Linux the network work needs root, but you don't run `sudo fleetbox` yourself — the CLI
re-execs its own absolute path under sudo for `up`/`down`/`rm`, so you just run `fleetbox up`
and approve the password prompt (no need to put the binary on root's `PATH`). Read-only
commands — `ls`, `ssh`, `cp`, `ssh-config` — never ask for sudo. In a non-interactive shell
(CI, a pipe, no terminal) it doesn't hang: it prints the exact `sudo …` command to run and
exits.

For the **library** on Linux there's no auto-elevation (a test must never spawn a password
prompt), so run the test binary under sudo. The invocation our own CI uses:

```bash
sudo -E env "HOME=$HOME" "PATH=$PATH" go test ./...
```

`HOME` keeps fleetbox's state under your `~/.fleetbox` (not `/root`), and `PATH` lets the
root process find the Go toolchain. The backend itself shells out to nothing — host
networking is programmed directly via netlink and nf_tables (ADR-0025).

The first boot also downloads the cloud image (a few hundred MB, cached in
`~/.fleetbox/images/`) and prints a progress line so it doesn't look like a hung test.

Hacking on fleetbox itself, or running on an air-gapped host? Build the helper locally
and point fleetbox at it:

```bash
make helper                                    # builds + ad-hoc-signs ./bin/fleetbox-helper
FLEETBOX_HELPER=$PWD/bin/fleetbox-helper go test ./...
```

## Usage

### As a test fixture

```go
import (
	"context"
	"testing"

	"github.com/pilat/fleetbox"
	"github.com/pilat/fleetbox/fleetboxtest"
)

func TestNeedsARealKernel(t *testing.T) {
	vm := fleetboxtest.Start(t, "debian-12",
		fleetbox.WithCPUs(2),
		fleetbox.WithMemoryGB(4),
	)

	// What no container can give you: systemd as PID 1, your own kernel,
	// a working /dev/kvm.
	out, err := vm.SSH(context.Background(),
		"cat /proc/1/comm && uname -r && test -e /dev/kvm && echo ok")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	t.Log(out) // systemd / 6.1.0-… / ok
}
```

`fleetboxtest.Start` registers `t.Cleanup` to destroy the VM, derives a collision-safe
name from the test name so parallel tests don't fight, and skips the test automatically
when the hardware can't run it. `SkipIfShort` opts a test out under `go test -short`.
`StartN` boots several VMs on one shared network — a real cluster whose members reach
each other by IP (see [Limitations](#limitations) for where clustering is supported).

### As a library (no testing.T)

```go
vm, err := fleetbox.Start(ctx, "builder",
	fleetbox.WithImage("ubuntu-24.04"),
	fleetbox.WithCPUs(4),
	fleetbox.WithMemoryGB(8),
	fleetbox.WithDiskGB(40),
)
if err != nil {
	log.Fatal(err)
}

fmt.Println(vm.IP())              // net.IP — directly reachable, no forwarding

out, err := vm.SSH(ctx, "sudo apt-get install -y nginx")  // user has passwordless sudo
if err != nil {
	log.Fatalf("%v\n%s", err, out)
}

_ = vm.CopyTo(ctx, "./app", "/srv/app")                // file or dir, in…
_ = vm.CopyFrom(ctx, "/var/log/app.log", "./app.log")  // …or out; modes preserved

_ = vm.Stop(ctx)                  // graceful shutdown, disk preserved
// vm.Destroy(ctx) deletes it entirely
```

`Start` is idempotent: call it again with the same name and it boots the existing VM
instead of recreating it. State lives under `~/.fleetbox/clusters/<cluster>/<name>/` and
survives reboots; `Destroy` (or `fleetbox rm`) is the only thing that deletes a disk.
(`fleetbox.SetStorageRoot` / `FLEETBOX_HOME` moves that root if a `~/.fleetbox` in your
users' home would be surprising.)
Full API on [pkg.go.dev](https://pkg.go.dev/github.com/pilat/fleetbox).

### Handing a VM a directory

`WithFixture` packs a host directory into the guest as a read-only fixture — the way to
hand a VM your test data, config, or build output. It works identically on macOS
and Linux: at boot the directory is snapshotted into an ext4 image, attached read-only,
and mounted in the guest at the path you give:

```go
dir := t.TempDir()
os.WriteFile(filepath.Join(dir, "input.json"), payload, 0o644)

vm := fleetboxtest.Start(t, "debian-12", fleetbox.WithFixture(dir, "/work"))
out, _ := vm.SSH(context.Background(), "cat /work/input.json")  // reads the snapshot
```

From the CLI it's a repeatable `--fixture host:guest` flag:

```bash
./bin/fleetbox up dev --fixture ./src:/work --fixture ./fixtures:/data
```

Fixtures are read-only and world-readable: every file `0444`, every directory `0555`,
owned by root. Host permission and exec bits are not preserved. The set of fixtures is
frozen when the VM is first created (`rm` and recreate to change it), but the content is
re-snapshotted from the host directory on every boot, so a reboot picks up host-side
changes. To get data back out of the guest, use `fleetbox cp` or scp. The guest path
must be absolute, and host paths must not contain colons (the value splits on the last
colon).

### From the command line

The CLI wraps the same library for manual work:

```bash
make build                                     # compiles ./bin/fleetbox (pure Go, nothing to sign)

./bin/fleetbox up web                          # create & boot a VM
./bin/fleetbox up node -n 3                    # boot a cluster: node-1, node-2, node-3
./bin/fleetbox ssh node-2                      # members are addressed by name
./bin/fleetbox ssh node-1 -- ping -c1 node-2   # ...and reach each other by IP
./bin/fleetbox cp ./mybinary web:/usr/local/bin/
./bin/fleetbox ls                              # NAME  IP  STATE  CPUS  MEM  DISK  AGE
./bin/fleetbox ssh-config >> ~/.ssh/config     # then plain `ssh web` works
./bin/fleetbox down node-1                     # stop one member; the rest keep running
./bin/fleetbox rm node                         # destroy the whole cluster (prefix match)
./bin/fleetbox version                         # print version, commit, build date
```

A cluster's VMs live in one holder process sharing one network (vmnet on macOS, a bridge
on Linux), which is what lets them reach each other by IP; `ssh`/`down`/`rm` still
address each member by name.

## Images

Pass a built-in alias or any direct URL to a raw / qcow2 cloud image:

| Alias | Image |
|-------|-------|
| `debian-11` | Debian 11 generic cloud (amd64/arm64, per host) |
| `debian-12` *(default)* | Debian 12 generic cloud |
| `debian-13` | Debian 13 generic cloud |
| `ubuntu-22.04` | Ubuntu 22.04 server cloud |
| `ubuntu-24.04` | Ubuntu 24.04 server cloud |
| `ubuntu-26.04` | Ubuntu 26.04 server cloud |

```go
fleetboxtest.Start(t, "debian-12")
fleetboxtest.Start(t, "https://example.com/my-cloud-image.qcow2")
```

Each alias is pinned to a dated upstream snapshot and verified by SHA256 on download, so
a given alias boots the same image on every machine and every run. A direct URL is taken
as-is (unverified) — the escape hatch for a custom or bleeding-edge image. Images are
downloaded and cached once in `~/.fleetbox/images/`, with qcow2 converted to raw on the
way in. Adding a distro is adding a catalog entry; there are no per-distro code paths.

## How it works

`Start` is a short pipeline: make sure the SSH keypair exists, download and cache the
image, write a cloud-init seed ISO, boot the VM, wait for SSH to answer. The
platform-specific step is the IP. On macOS the VM's address is read out of
`/var/db/dhcpd_leases`, looked up by hostname; on Linux fleetbox assigns a static
address on the bridge and hands it to the guest through cloud-init.

Where that pipeline runs is the part worth knowing. On Linux it runs in your process. On
macOS it runs inside the downloaded `fleetbox-helper`, the only binary that links
Virtualization.framework. The library spawns the helper *bound* to the test process, so
the helper and its VMs are reaped when the test exits, even on `kill -9`. The CLI does
the opposite: each `up` group (a single VM, or a cluster sharing one network) gets a
detached holder that outlives the command, which is what keeps VMs running between
invocations. On macOS that holder is the helper; on Linux it's the CLI re-exec'd. Either
way, SSH and `cp` dial the VM's IP directly — the helper protocol never proxies them.

For the full picture, read [ARCHITECTURE.md](ARCHITECTURE.md); for *why* it's built this
way, the decision log lives in [docs/adr/](docs/adr/).

## Limitations

- **Fixtures are read-only.** There is no live read-write share: edits inside the guest
  don't flow back to the host, and host-side edits only show up after a reboot. The
  exact semantics are under [Handing a VM a directory](#handing-a-vm-a-directory); for
  the guest→host direction, use `cp` / scp.
- **A CLI cluster is one process.** Members share one holder so they can share one
  network, so a holder crash takes the whole cluster down (a lone VM is unaffected). On
  Linux a SIGKILL'd holder also leaves its bridge and taps behind; the next `up` or
  `down` sweeps them. Members started by separate `up` commands sit on separate networks
  and can't be merged afterwards — bring a cluster up together.
- **First run downloads, then caches.** The cloud image (both platforms) and, on macOS,
  the signed helper are fetched once into `~/.fleetbox` and reused. `FLEETBOX_HELPER`
  swaps in a locally built helper (development, offline). In CI, cache
  `~/.fleetbox/{bin,images}` so cold runs don't re-download.
- **Platform matrix.** Clusters need macOS Apple Silicon 26+ or Linux amd64/arm64;
  macOS below 26 runs a single VM; Intel macOS is unsupported. On Linux, a stopped VM
  brought back up needs its `/24` to still be free — on a contended host the auto-picked
  subnet can shift and the rebooted VM won't be reachable; bring clusters up fresh.
  arm64 Linux boot is exercised only via the nested dogfood on Apple Silicon (a real
  nested-KVM boot), not yet on bare-metal arm64 hardware.
- **Docker on the Linux host blocks VM egress.** When Docker is running it sets the
  iptables `FORWARD` policy to `DROP`, which drops a fleetbox VM's traffic to the internet
  (VMs forward through their own bridge, not Docker's) — the same conflict libvirt and LXD
  hit on Docker hosts. VM↔VM and host↔VM still work; only internet egress is affected.
  fleetbox deliberately does not rewrite your host firewall, so allow its subnet range
  yourself: `sudo iptables -I DOCKER-USER -s 192.168.0.0/16 -j ACCEPT` (add the matching
  `-d 192.168.0.0/16` rule for the return path).
- **v0 API.** Expect breaking changes until it stabilizes.

### CI

GitHub-hosted macOS runners can't nest virtualization, so the macOS workflow does lint,
build, and unit tests; VM-boot tests run locally via `make test-vm`. GitHub-hosted
x86-64 Linux runners, on the other hand, expose `/dev/kvm`, so the Linux backend's
VM-boot tests run in CI after a one-time udev tweak (plus a cache, so the image and VMM
aren't re-downloaded every run):

```yaml
- run: |
    echo 'KERNEL=="kvm", GROUP="kvm", MODE="0666"' | sudo tee /etc/udev/rules.d/99-kvm.rules
    sudo udevadm control --reload-rules && sudo udevadm trigger
    # Hosted runners run Docker (FORWARD policy DROP); allow the VM subnet to forward.
    sudo iptables -I DOCKER-USER -s 192.168.0.0/16 -j ACCEPT
    sudo iptables -I DOCKER-USER -d 192.168.0.0/16 -j ACCEPT
- uses: actions/cache@v4
  with:
    path: |
      ~/.fleetbox/bin
      ~/.fleetbox/images
    key: fleetbox-${{ runner.os }}
```

arm64 hosted Linux runners do **not** have KVM ("not supported for this sku"); use an
x86-64 runner for VM-boot CI. Hosted runners also drop outbound **ICMP**, so check a
guest's internet egress over TCP (a connect to `1.1.1.1:443`), not `ping`. This is the
"develop on a Mac, test in cheap x86-64 hosted Linux CI" story.

## Roadmap

Roughly in priority order:

- **Preserve host permissions** in fixtures (they currently arrive world-readable,
  uid 0).

Recently landed: programmatic file copy in/out (`VM.CopyTo` / `VM.CopyFrom` — universal
file-or-directory, both directions, modes preserved; `fleetbox cp` is now built on it,
tar over the SSH connection, no `scp` shell-out), read-only host→guest fixtures
(`WithFixture` / `--fixture`, identical on macOS and Linux), VM-to-VM networking over a
real network (vmnet SharedMode), and CLI clustering (`fleetbox up node -n 3`) — so a
kubeadm cluster, an etcd quorum, or a Raft group runs on real interconnected nodes, not
mocks.

## License

[MIT](LICENSE).
