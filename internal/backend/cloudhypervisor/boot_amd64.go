//go:build linux && amd64

package cloudhypervisor

// bootArgs returns the x86_64 boot configuration: the pinned PVH
// rust-hypervisor-firmware as the "kernel". The firmware chain-loads the guest
// kernel from the disk's boot entry, so nothing has to be extracted. This is the
// validated path on x86_64 (ADR-0011); arm64 cannot use it (the aarch64 firmware
// does not boot under Apple-Silicon nested virt and is untested on bare metal), so
// it boots the kernel directly instead — see boot_arm64.go and ADR-0024.
func (v *VM) bootArgs() ([]string, error) {
	return []string{"--kernel", v.fwPath}, nil
}
