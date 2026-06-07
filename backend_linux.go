//go:build linux

package fleetbox

import (
	"fmt"

	"github.com/pilat/fleetbox/internal/backend"
	"github.com/pilat/fleetbox/internal/backend/cloudhypervisor"
	"github.com/pilat/fleetbox/internal/store"
)

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
