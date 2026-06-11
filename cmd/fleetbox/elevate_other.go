//go:build !linux

package main

// ensurePrivileged is a no-op off Linux. macOS uses the signed-helper model (a
// downloaded, entitled fleetbox-helper), never sudo, so the CLI elevates nothing
// there (ADR-0023). The pure decideElevation lives in elevate.go and is exercised
// by elevate_test.go on every platform.
func ensurePrivileged() error {
	return nil
}
