.PHONY: all build test test-vm lint vendor-vz clean

# Default target
all: build

# Build the CLI. On macOS the binary needs the virtualization entitlement
# (ad-hoc codesign); on Linux there is nothing to sign, so codesign is skipped.
build:
	go build -o bin/fleetbox ./cmd/fleetbox
	@if [ "$$(uname)" = "Darwin" ]; then \
		codesign --entitlements entitlements.plist --force -s - bin/fleetbox; \
	fi

# Run unit tests (no VMs, works on any machine).
# -short skips VM-boot tests (they call fleetboxtest.SkipIfShort), so this stays
# VM-free even on nested-virt-capable hardware. VM tests run via `make test-vm`.
test:
	go test -short ./...

# Run VM integration tests (requires darwin/arm64, M3+, macOS 26+)
# These tests boot real VMs and require the virtualization entitlement.
# Timeout is well above Go's 10m default: a cluster test boots several VMs across
# back-to-back StartN calls (each budgeting n*5m), which can exceed 10m.
test-vm:
	go test -c -o bin/fleetbox.test ./fleetboxtest
	codesign --entitlements entitlements.plist --force -s - bin/fleetbox.test
	./bin/fleetbox.test -test.v -test.timeout 30m -test.run TestVM

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
	rm -f fleetbox.test
