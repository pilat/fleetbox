//go:build darwin

package helperdist

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// stripQuarantine removes the com.apple.quarantine xattr Gatekeeper sets on a
// downloaded file, so the signed helper runs without a user prompt. A missing
// attribute is not an error (the file may already be clean). Removing an xattr
// does not alter the file's bytes, so the mach-o signature stays valid (R8).
func stripQuarantine(path string) error {
	if err := unix.Removexattr(path, "com.apple.quarantine"); err != nil {
		if errors.Is(err, unix.ENOATTR) || errors.Is(err, unix.ENODATA) {
			return nil
		}
		return fmt.Errorf("removexattr: %w", err)
	}
	return nil
}
