# fleetbox

**Real Linux VMs as Go test fixtures on macOS.**

[![Go Reference](https://pkg.go.dev/badge/github.com/pilat/fleetbox.svg)](https://pkg.go.dev/github.com/pilat/fleetbox)
![Platform](https://img.shields.io/badge/platform-macOS%20arm64-black)
![License](https://img.shields.io/badge/license-MIT-blue)

fleetbox boots stock Linux cloud images on Apple Silicon through Apple's
Virtualization.framework, hands them to your tests over SSH, and tears them down when the
test ends. Think testcontainers — except instead of a container you get a whole machine:
real kernel, real systemd, real `/dev/kvm`.

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

- **macOS 15** (Sequoia) or newer
- **Apple Silicon**, M3 or newer — nested virtualization (`/dev/kvm` in the guest) needs it
- **Go 1.24+**

Intel Macs and Linux hosts aren't supported; the module is build-tagged `darwin && arm64`
and won't compile elsewhere.

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
instead of recreating it. State lives under `~/.fleetbox/vms/<name>/` and survives reboots
— `Destroy` (or `fleetbox rm`) is the only thing that deletes a disk. Full API on
[pkg.go.dev](https://pkg.go.dev/github.com/pilat/fleetbox).

### From the command line

The CLI wraps the same library for manual work:

```bash
make build                                   # compiles + signs ./bin/fleetbox

./bin/fleetbox up web                        # create & boot a VM
./bin/fleetbox up node -n 3                  # node-1, node-2, node-3
./bin/fleetbox ssh web                       # interactive shell
./bin/fleetbox ssh web -- systemctl status   # …or run a command
./bin/fleetbox cp ./mybinary web:/usr/local/bin/
./bin/fleetbox ls                            # NAME  IP  STATE  CPUS  MEM  DISK  AGE
./bin/fleetbox ssh-config >> ~/.ssh/config   # then plain `ssh web` works
./bin/fleetbox down web                      # stop, keep the disk
./bin/fleetbox rm web                        # destroy
```

## Images

Use a built-in alias or any direct URL to a raw / qcow2 cloud image:

| Alias | Image |
|-------|-------|
| `debian-12` *(default)* | Debian 12 generic cloud, arm64 |
| `ubuntu-24.04` | Ubuntu 24.04 server cloud, arm64 |

```go
fleetboxtest.Start(t, fleetbox.Debian12)
fleetboxtest.Start(t, "https://example.com/my-cloud-image.qcow2")
```

Images are downloaded and cached once in `~/.fleetbox/images/`, with qcow2 converted to raw
on the way in. Adding a distro is adding a catalog entry — there are no per-distro code
paths.

## How it works

`Start` runs a short, boring pipeline: ensure the SSH key → download and cache the image →
generate a cloud-init seed ISO → boot via EFI with a NAT NIC → find the VM's IP in
`/var/db/dhcpd_leases` by hostname → wait for SSH. No daemon: in library mode the test
process owns its VMs; the CLI re-execs a tiny holder process per VM so they outlive the
command.

For the full picture, read [ARCHITECTURE.md](ARCHITECTURE.md); for *why* it's built this
way, the decision log lives in [docs/adr/](docs/adr/).

## Limitations

v0 boots and SSHes into single VMs. Mind the sharp edges:

- **No mounts, and library file transfer is SSH-only.** From a test you run commands with
  `vm.SSH`; there's no copy or mount in the library yet (the CLI has `cp` over scp). Getting
  a large build artifact into a VM from Go isn't ergonomic today — mounts are the planned
  fix.
- **VM-to-VM networking doesn't work.** Virtualization.framework's NAT isolates guests from
  one another — they reach the host and the internet, but not their neighbours. So v0 is for
  single-VM testing, not clusters.
- **Apple Silicon M3+ only.** Nested virtualization requires it; older chips and Intel Macs
  are out of scope.
- **v0 API.** Expect breaking changes until it stabilizes.

CI note: GitHub-hosted macOS runners can't nest virtualization, so VM-boot tests run
locally via `make test-vm`, while CI sticks to lint, build, and unit tests.

## Roadmap

Roughly in priority order:

- **Mounts (virtiofs).** Share a host directory straight into the guest — the intended way
  to hand a VM your build output or fixtures, no copying around.
- **Real cluster testing.** The whole point of fixing VM-to-VM networking: boot N VMs that
  genuinely talk to each other and test an actual cluster — kubeadm, etcd, a Raft group — on
  real nodes over a real network, not mocks or a single-host simulation. VZ NAT isolates
  guests today; the fix is bridged or vmnet-socket networking.
- **Programmatic file copy** for the cases a mount doesn't fit.

## License

[MIT](LICENSE).
