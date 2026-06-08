# fleetbox

**Real Linux VMs as Go test fixtures — on macOS and Linux.**

[![Go Reference](https://pkg.go.dev/badge/github.com/pilat/fleetbox.svg)](https://pkg.go.dev/github.com/pilat/fleetbox)
![Platform](https://img.shields.io/badge/platform-macOS%20arm64%20%7C%20linux%20amd64%2Farm64-black)
![License](https://img.shields.io/badge/license-MIT-blue)

fleetbox boots stock Linux cloud images — on macOS (Apple Silicon) through Apple's
Virtualization.framework, on Linux through cloud-hypervisor — hands them to your tests
over SSH, and tears them down when the test ends. Think testcontainers — except instead
of a container you get a whole machine: real kernel, real systemd, real `/dev/kvm`. The
Go API is the same on both platforms.

```go
func TestAgainstRealLinux(t *testing.T) {
	vm := fleetboxtest.Start(t, fleetbox.Debian12)

	out, err := vm.SSH(context.Background(), "uname -a")
	if err != nil {
		t.Fatal(err)
	}
	t.Log(out) // Linux ... aarch64 GNU/Linux
}
```

One line gets you a booted Debian box, reachable over SSH (fleetbox generates its own
keypair — it never touches your `~/.ssh`). The VM is destroyed automatically when the test
returns.

> **Status: v0.** It works, but the API will change and there are no compatibility
> promises yet. See [Limitations](#limitations).

## Why

Containers are wonderful right up until you need to test something a container can't give
you — a kernel module, a systemd unit, an nftables ruleset, `kubeadm`, a VPN, anything
that wants `/dev/kvm`. The usual fallback is a VM tool that comes with a yaml file, a
background agent forwarding SSH ports, and patched images. That's a lot of moving parts to
keep working.

fleetbox takes the opposite line:

- **Real VMs, not containers.** It EFI-boots unmodified cloud images through their own
  bootloader. Real kernel, real init, and nested virtualization on M3+ — you can run KVM
  *inside* the guest.
- **Every VM gets a routable IP.** Virtualization.framework's NAT drops each VM onto a
  bridge with a real DHCP address. No port forwarding, no `-p` flags, no tunnel daemon —
  call `vm.IP()` and connect.
- **Nothing of ours runs in the guest.** No agent, no helper binary, no host↔guest
  protocol. A VM is configured exactly once by cloud-init; after that it's a plain distro
  you reach over SSH.
- **Library-first.** The Go package is the product; the CLI is a thin wrapper over the
  same calls. Fixtures clean themselves up through `t.Cleanup`.

It's opinionated on purpose: no yaml, no templates, no per-distro code paths — just flags
and sane defaults.

## Requirements

One of:

- **macOS, Apple Silicon** — clusters (VM↔VM) need macOS 26+ (vmnet SharedMode); macOS
  below 26 runs a single VM via VZ NAT. Nested virtualization (`/dev/kvm` in the guest)
  needs M3 or newer. Intel Macs are not supported.
- **Linux, amd64 or arm64** — needs `/dev/kvm` (be in the `kvm` group) and `CAP_NET_ADMIN`
  (to create the bridge and taps). The cloud-hypervisor binary and firmware are downloaded
  and checksum-pinned to `~/.fleetbox/bin/` on first use.

Plus **Go 1.24+**. The module compiles on `darwin/arm64` and `linux/{amd64,arm64}`; other
targets build but return a clear "unsupported platform" error.

## Install

```bash
go get github.com/pilat/fleetbox
```

### The entitlement (don't skip this)

Any binary that boots a VM — *including your `go test` binary* — must carry the
`com.apple.security.virtualization` entitlement or it dies on launch. Ad-hoc codesigning is
enough for local dev:

```bash
go test -c -o bin/mypkg.test ./mypkg
codesign --entitlements entitlements.plist --force -s - bin/mypkg.test
./bin/mypkg.test -test.v
```

The repo ships an `entitlements.plist`; `make test-vm` does the compile-sign-run dance for
its own suite if you want a reference.

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
	vm := fleetboxtest.Start(t, fleetbox.Debian12,
		fleetbox.WithCPUs(2),
		fleetbox.WithMemoryGB(4),
	)

	// A real machine, not a container: real init (systemd as PID 1), real
	// kernel, nested KVM available.
	out, err := vm.SSH(context.Background(),
		"cat /proc/1/comm && uname -r && test -e /dev/kvm && echo ok")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	t.Log(out) // systemd / 6.1.0-… / ok
}
```

`fleetboxtest.Start` registers `t.Cleanup` to destroy the VM, derives a collision-safe name
from the test name (parallel-test friendly), and skips automatically when the hardware can't
run it. `SkipIfShort` opts a test out under `go test -short`. `StartN` boots several
independent VMs at once — though note they can't reach each other yet
([Limitations](#limitations)).

### As a library (no testing.T)

```go
vm, err := fleetbox.Start(ctx, "builder",
	fleetbox.WithImage(fleetbox.Ubuntu2404),
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

_ = vm.Stop(ctx)                  // graceful shutdown, disk preserved
// vm.Destroy(ctx) deletes it entirely
```

`Start` is idempotent: call it again with the same name and it boots the existing VM
instead of recreating it. State lives under `~/.fleetbox/clusters/<cluster>/<name>/` and survives reboots
— `Destroy` (or `fleetbox rm`) is the only thing that deletes a disk. Full API on
[pkg.go.dev](https://pkg.go.dev/github.com/pilat/fleetbox).

### Handing a VM a directory

`WithFixture` packs a host directory into the guest as a read-only fixture — the natural
way to hand a VM your test data, config, or build output without a daemon. It works
identically on macOS and Linux: at boot the directory is snapshotted into an ext4 image,
attached read-only, and mounted by the guest at the path you give:

```go
dir := t.TempDir()
os.WriteFile(filepath.Join(dir, "input.json"), payload, 0o644)

vm := fleetboxtest.Start(t, fleetbox.Debian12, fleetbox.WithFixture(dir, "/work"))
out, _ := vm.SSH(context.Background(), "cat /work/input.json")  // reads the snapshot
```

From the CLI it's a repeatable `--fixture host:guest` flag:

```bash
./bin/fleetbox up dev --fixture ./src:/work --fixture ./fixtures:/data
```

The fixture is **read-only** and **world-readable** (every file `0444`, every dir `0555`,
owned by root), so any guest user can read it; host permission and exec bits are not
preserved. The set of fixtures is frozen when the VM is first created (change it with `rm` +
recreate), but the content is re-snapshotted from the host directory on every boot — so a
reboot picks up host-side changes, though never live within a single boot. To get data back
out of the guest, use `fleetbox cp` / scp. The guest path must be absolute, and host paths
must not contain colons (the value is split on the last colon).

### From the command line

The CLI wraps the same library for manual work:

```bash
make build                                   # compiles + signs ./bin/fleetbox

./bin/fleetbox up web                        # create & boot a VM
./bin/fleetbox up node -n 3                  # interconnected cluster: node-1, node-2, node-3
./bin/fleetbox ssh node-2                     # address a cluster member by name
./bin/fleetbox ssh node-1 -- ping -c1 node-2 # nodes reach each other by IP
./bin/fleetbox ssh web -- systemctl status   # …or run a command
./bin/fleetbox cp ./mybinary web:/usr/local/bin/
./bin/fleetbox ls                            # NAME  IP  STATE  CPUS  MEM  DISK  AGE
./bin/fleetbox ssh-config >> ~/.ssh/config   # then plain `ssh web` works
./bin/fleetbox down node-1                    # stop one member; the rest keep running
./bin/fleetbox rm node                        # destroy the whole cluster (prefix match)
```

A cluster runs in one holder process sharing one network (a vmnet network on macOS, a
Linux bridge on Linux), so its VMs reach each other by IP; `down`/`ssh`/`rm` still address
each member by name.

## Images

Use a built-in alias or any direct URL to a raw / qcow2 cloud image:

| Alias | Image |
|-------|-------|
| `debian-12` *(default)* | Debian 12 generic cloud (amd64/arm64, per host) |
| `ubuntu-24.04` | Ubuntu 24.04 server cloud (amd64/arm64, per host) |

```go
fleetboxtest.Start(t, fleetbox.Debian12)
fleetboxtest.Start(t, "https://example.com/my-cloud-image.qcow2")
```

Images are downloaded and cached once in `~/.fleetbox/images/`, with qcow2 converted to raw
on the way in. Adding a distro is adding a catalog entry — there are no per-distro code
paths.

## How it works

`Start` runs a short, boring pipeline: ensure the SSH key → download and cache the image →
generate a cloud-init seed ISO → boot the VM (macOS: EFI on a vmnet SharedMode NIC; Linux:
cloud-hypervisor with a tap on a shared bridge) → get the VM's IP (macOS: from
`/var/db/dhcpd_leases` by hostname; Linux: the statically assigned address) → wait for SSH.
No daemon: in library mode the test process owns its VMs; the CLI re-execs a tiny holder
process per `up` group (a single VM, or a whole cluster sharing one network) so they
outlive the command.

For the full picture, read [ARCHITECTURE.md](ARCHITECTURE.md); for *why* it's built this
way, the decision log lives in [docs/adr/](docs/adr/).

## Limitations

Both the library (`StartN`) and the CLI (`up -n N`) boot interconnected clusters whose
VMs reach each other by IP. Mind the sharp edges:

- **Fixtures are read-only and frozen at creation.** `WithFixture` / `--fixture` copies a
  host directory into the guest read-only (an ext4 image), on both macOS and Linux. There is
  no live read-write share — edits inside the guest don't flow back to the host; use `cp` /
  scp for the output direction. The set of fixtures is fixed when the VM is created (`rm` and
  recreate to change it), though the content re-snapshots on every boot. Files arrive
  world-readable, owned by root; host permission and exec bits aren't preserved.
- **A CLI cluster shares one holder process.** All members of a cluster live in one process
  to share one network, so a holder crash takes the whole cluster down (a single VM is
  unaffected); on Linux a SIGKILL'd holder also leaves its bridge/taps behind. Members
  started by separate `up` commands have separate networks and can't be merged into one
  cluster afterwards — bring a cluster up together.
- **Platform matrix.** macOS Apple Silicon 26+ (clusters), macOS Apple Silicon <26 (single
  VM only), Linux amd64/arm64 (clusters); Intel macOS unsupported. On Linux, a stopped VM
  brought back up needs its `/24` to still be free — on a contended host the auto-picked
  subnet can shift and the rebooted VM won't be reachable; bring clusters up fresh. arm64
  Linux boot via rust-hypervisor-firmware is not yet validated on hardware.
- **v0 API.** Expect breaking changes until it stabilizes.

CI note: GitHub-hosted macOS runners can't nest virtualization, so VM-boot tests run
locally via `make test-vm`, while CI sticks to lint, build, and unit tests.

## Roadmap

Roughly in priority order:

- **Programmatic file copy** — a library-side copy in/out for cases a fixture doesn't fit
  (the CLI already has `cp` over scp).
- **Preserve host permissions** in fixtures (they currently arrive world-readable, uid 0).

Done recently: read-only host→guest fixtures (`WithFixture` / `--fixture`, an ext4 payload,
identical on macOS and Linux); VM-to-VM networking over a real network (vmnet SharedMode);
and CLI clustering (`fleetbox up node -n 3`) — boot an actual cluster (kubeadm, etcd, a Raft
group) on real interconnected nodes, not mocks or a single-host simulation.

## License

[MIT](LICENSE).
