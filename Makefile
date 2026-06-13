.PHONY: all build helper test test-fake test-fake-linux test-vm lint lint-fake catalog vendor-vz clean

# Default target
all: build

# Build the CLI. It is a pure-Go client on every platform now — it drives the VM
# holder over a socket and links no hypervisor — so there is nothing to codesign
# here. The virtualization entitlement lives only in the helper (ADR-0017).
build:
	go build -o bin/fleetbox ./cmd/fleetbox

# Build and ad-hoc-sign the macOS VM helper: the only binary that links Apple
# Virtualization.framework, so the only one that needs the
# com.apple.security.virtualization entitlement. darwin/arm64 only.
#
# Distribution uses this same ad-hoc signature — the entitlement is unrestricted,
# so an ad-hoc signature carries it on any Mac (ADR-0017). For a shop whose policy
# refuses an unquarantined ad-hoc binary, re-sign the same binary with a Developer
# ID and notarize it; nothing else changes.
helper:
	go build -o bin/fleetbox-helper ./cmd/fleetbox-helper
	codesign --entitlements entitlements.plist --force -s - bin/fleetbox-helper

# Run unit tests (no VMs, works on any machine).
# -short skips VM-boot tests (they call fleetboxtest.SkipIfShort), so this stays
# VM-free even on nested-virt-capable hardware. VM tests run via `make test-vm`.
# -race instruments the holder's goroutines (exercised by the new control/holder
# protocol tests); it needs cgo, which is the default on darwin — set explicitly
# so a stray CGO_ENABLED=0 in the environment cannot silently drop -race.
test:
	CGO_ENABLED=1 go test -short -race ./...

# Coordination tests against the build-tagged fake backend (ADR-0018/0020): the
# whole cross-process client<->helper path — the orchestrator driving a fake helper
# over the real protocol (createnetwork/reserve/boot-member/status/stop) — runs with
# no VM boot and no codesign, so it gates teardown + protocol on a stock CI runner.
# Two pieces, both under -race (-race on the parent test process does NOT instrument
# the spawned subprocess, so the helper is built -race too; the fake helper links
# the race runtime via cgo but NOT vz, so it still needs no entitlement):
#   1. build the fake helper binary (the holder + fake backend, no vz);
#   2. drive it from the client over FLEETBOX_HELPER (FLEETBOX_FAKE_HELPER ungates
#      the test). The client is also built -tags fleetbox_fake so skipSSHWait short-
#      circuits the dial against the fake's unroutable IP — it still links the remote
#      proxy, not the fake backend (the fake lives only in the helper now, ADR-0020).
test-fake:
	CGO_ENABLED=1 go build -tags fleetbox_fake -race -o bin/fleetbox-helper-fake ./cmd/fleetbox-helper
	FLEETBOX_FAKE_HELPER=1 FLEETBOX_HELPER=$(CURDIR)/bin/fleetbox-helper-fake \
		CGO_ENABLED=1 go test -race -tags fleetbox_fake -run TestCoord ./fleetboxtest
	# The orchestrator's createdThisCall test needs the fake tag too (skipSSHWait);
	# it uses local stubs, not the fake helper, so no FLEETBOX_HELPER is required.
	CGO_ENABLED=1 go test -race -tags fleetbox_fake ./internal/orchestrator/

# The Linux equivalent of test-fake. There is no separate fake helper binary on
# Linux: the test binary self-reexecs into the fake holder via internal/holder's
# init() interceptor (helperExe is os.Executable), so the same coord tests run with
# just FLEETBOX_FAKE_HELPER set and NO FLEETBOX_HELPER. The fake boots no VM and
# touches no host network state, so this needs neither /dev/kvm nor root — it gates
# the protocol + teardown on a stock Linux runner (ADR-0020).
test-fake-linux:
	FLEETBOX_FAKE_HELPER=1 CGO_ENABLED=1 go test -race -tags fleetbox_fake -run TestCoord ./fleetboxtest
	# Same fake-tagged orchestrator test as test-fake (local stubs, no helper).
	CGO_ENABLED=1 go test -race -tags fleetbox_fake ./internal/orchestrator/

# Run the full, capability-driven VM suite (requires darwin/arm64, M3+, macOS 26+).
# Builds and ad-hoc-signs the helper, then points the library at it via
# FLEETBOX_HELPER. There is NO -run selector: each test self-skips on capability and
# speed, so this runs everything the host supports — conformance + cluster, PLUS the
# nested dogfood (TestNestedLinuxBoot, folded in from the old `make test-nested`),
# which boots an outer guest and runs this same suite inside it on cloud-hypervisor.
# The test binary itself links no hypervisor and needs neither cgo nor codesign — the
# whole point of the sever (ADR-0017). The -timeout is a generous backstop, not a per-test
# budget: each VM boot is capped by FLEETBOX_IP_WAIT_TIMEOUT / BootTimeout and the nested
# orchestrator self-caps at 40m, so a true hang is caught long before this fires. It must
# clear the SUM of the direct boots (conformance + cluster + fixtures) AND the nested
# orchestrator's 40m, because a Go -timeout kills the binary without running t.Cleanup —
# leaking VMs. Hence 90m.
test-vm: helper
	FLEETBOX_HELPER=$(CURDIR)/bin/fleetbox-helper \
		go test -count=1 -v -timeout 90m ./fleetboxtest

# Refresh the pinned cloud-image catalog (internal/image/catalog.json). The
# human-authored keys decide which OSes exist; the tool only refreshes the values
# — dated snapshot, snapshot-stamped per-arch URLs, and SHA256. A scheduled CI job
# runs the same tool and opens a PR when upstream moves (ADR-0019). A Debian
# refresh streams the images through the hashers to compute the sha256 Debian does
# not publish; nothing is persisted.
catalog:
	go run ./contrib/catalog

# Run linter
lint:
	golangci-lint run ./...

# Lint the files behind the fleetbox_fake build tag — backend_fake.go,
# sshwait_fake.go, and the *_fake_test.go — which the default `make lint` cannot
# see (golangci-lint does not analyze tagged files without --build-tags).
lint-fake:
	golangci-lint run --build-tags fleetbox_fake ./...

# Regenerate the vendored Code-Hex/vz fork under third_party/vz from pinned
# sources: stock vz + the vmnet-SharedMode patch (PR #205), renamed into this
# module's import path. The output is committed; rerun only to re-sync upstream,
# then commit the result. Pins and recipe live in hack/vendor-vz.sh.
vendor-vz:
	./hack/vendor-vz.sh

# Clean build artifacts
clean:
	rm -rf bin/
