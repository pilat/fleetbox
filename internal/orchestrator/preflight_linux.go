//go:build linux && !fleetbox_fake

package orchestrator

import (
	"fmt"
	"os"
)

// preflight fails fast with an actionable message when the two host requirements
// the Linux backend cannot work around are missing: an openable /dev/kvm and
// root. It replaces the cryptic downstream EPERM a later boot would otherwise
// produce (ADR-0017; the "no daemon" cost of ADR-0011/D6). It runs client-side
// before any spawn, so a clear error reaches the caller without launching a
// helper. The fleetbox_fake build skips it (preflight_fake.go), since the fake
// boots no VM and touches no host network state.
//
// Root (euid==0), not CAP_NET_ADMIN, is the honest gate (ADR-0023): the backend
// shells out to ip/iptables, which do not inherit file capabilities across exec,
// and it writes the DAC-gated /proc/sys/net/ipv4/ip_forward — so only root
// actually works. An effective-capability probe would pass for a binary granted
// the file capability and then fail in the ip subprocess; requiring root never
// lies (see requireRoot in preflight.go).
func preflight() error {
	f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf(
			"/dev/kvm is not accessible (%w): enable KVM and add your user to the 'kvm' group "+
				"(sudo usermod -aG kvm $USER, then re-login)", err)
	}
	_ = f.Close()
	return requireRoot(os.Geteuid())
}
