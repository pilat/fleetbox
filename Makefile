.PHONY: all build test test-vm lint clean

# Default target
all: build

# Build the CLI
build:
	go build -o bin/fleetbox ./cmd/fleetbox
	codesign --entitlements entitlements.plist --force -s - bin/fleetbox

# Run unit tests (no VMs, works on any machine)
test:
	go test ./...

# Run VM integration tests (requires darwin/arm64, M3+, macOS 15+)
# These tests boot real VMs and require the virtualization entitlement.
test-vm:
	go test -c -o bin/fleetbox.test ./fleetboxtest
	codesign --entitlements entitlements.plist --force -s - bin/fleetbox.test
	./bin/fleetbox.test -test.v -test.run TestVM

# Run linter
lint:
	golangci-lint run ./...

# Clean build artifacts
clean:
	rm -rf bin/
	rm -f fleetbox.test
