//go:build linux && !fleetbox_fake

package orchestrator

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/pilat/fleetbox/internal/backend"
	"github.com/pilat/fleetbox/internal/backend/cloudhypervisor"
	"github.com/pilat/fleetbox/internal/store"
)

// capNetAdmin is the Linux capability bit (CAP_NET_ADMIN) required to create the
// shared bridge and per-VM taps.
const capNetAdmin = 12

func newBackend() (backend.Backend, error) {
	st, err := store.New()
	if err != nil {
		return nil, fmt.Errorf("init store: %w", err)
	}
	return cloudhypervisor.New(st.BinDir(), st.NetworkStateDir()), nil
}

func nestedVirtSupported() bool {
	// The probe reads /dev/kvm and the KVM nested parameter; it never touches the
	// bin or network-state dirs, so no store is needed.
	return cloudhypervisor.New("", "").NestedVirtSupported()
}

// preflight fails fast with an actionable message when the two host requirements
// the Linux backend cannot work around are missing: an openable /dev/kvm and
// CAP_NET_ADMIN for the bridge/tap. It replaces the cryptic downstream EPERM a
// later boot would otherwise produce (ADR-0017, Task 6; the "no daemon" cost of
// ADR-0011/D6).
func preflight() error {
	f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf(
			"/dev/kvm is not accessible (%w): enable KVM and add your user to the 'kvm' group "+
				"(sudo usermod -aG kvm $USER, then re-login)", err)
	}
	_ = f.Close()
	if !hasNetAdmin() {
		return errors.New(
			"missing CAP_NET_ADMIN (needed for the shared bridge and per-VM tap): run as root, " +
				"or grant it to your test/CLI binary (sudo setcap cap_net_admin+ep <binary>)")
	}
	return nil
}

// hasNetAdmin reports whether the process holds CAP_NET_ADMIN, by reading the
// effective capability set from /proc/self/status. root always has it. If the
// capability set cannot be read, it returns true (don't block — the real bridge
// op will surface any genuine permission error).
func hasNetAdmin() bool {
	if os.Geteuid() == 0 {
		return true
	}
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return true
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		v, ok := strings.CutPrefix(line, "CapEff:")
		if !ok {
			continue
		}
		caps, err := strconv.ParseUint(strings.TrimSpace(v), 16, 64)
		if err != nil {
			return true
		}
		return caps&(1<<capNetAdmin) != 0
	}
	return true
}
