.PHONY: all build helper test test-vm lint vendor-vz clean

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
test:
	go test -short ./...

# Run VM integration tests (requires darwin/arm64, M3+, macOS 26+).
# Builds and ad-hoc-signs the helper, then points the library at it via
# FLEETBOX_HELPER. The test binary itself links no hypervisor and needs neither
# cgo nor codesign — that is the whole point of the sever (ADR-0017).
# Timeout is well above Go's 10m default: a cluster test boots several VMs.
test-vm: helper
	FLEETBOX_HELPER=$(CURDIR)/bin/fleetbox-helper \
		go test -count=1 -v -timeout 30m -run TestVM ./fleetboxtest

# Run linter
lint:
	golangci-lint run ./...

# Regenerate the vendored Code-Hex/vz fork under third_party/vz from pinned
# sources: stock vz + the vmnet-SharedMode patch (PR #205), renamed into this
# module's import path. The output is committed; rerun only to re-sync upstream,
# then commit the result. Pins and recipe live in hack/vendor-vz.sh.
vendor-vz:
	./hack/vendor-vz.sh

# Clean build artifacts
clean:
	rm -rf bin/
