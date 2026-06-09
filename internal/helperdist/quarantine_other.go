//go:build !darwin

package helperdist

// stripQuarantine is a no-op off macOS: there is no Gatekeeper quarantine xattr.
// The catalog has no non-darwin helper, so Ensure never reaches here in practice;
// this keeps the package building on every platform.
func stripQuarantine(string) error {
	return nil
}
