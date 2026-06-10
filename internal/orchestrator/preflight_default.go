//go:build !linux && !fleetbox_fake

package orchestrator

// preflight is a no-op off Linux: macOS needs no host capability (the helper holds
// the virtualization entitlement and vmnet manages itself), and an unsupported
// platform errors later at helperExe. Only the Linux backend has host requirements
// it must check up front (preflight_linux.go).
func preflight() error {
	return nil
}
