//go:build fleetbox_fake

package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/pilat/fleetbox/internal/backend"
	"github.com/pilat/fleetbox/internal/opts"
	"github.com/pilat/fleetbox/internal/sshkey"
	"github.com/pilat/fleetbox/internal/store"
)

var (
	_ backend.Network = (*stubNetwork)(nil)
	_ backend.Backend = (*stubBackend)(nil)
)

// stubNetwork is a DHCP-like network (empty Subnet, so startOnNetwork emits no
// static network-config) used to drive startOnNetwork without a real backend.
type stubNetwork struct{}

func (stubNetwork) Close() error   { return nil }
func (stubNetwork) Subnet() string { return "" }
func (stubNetwork) Reserve(_, _ string) (ip, mac string, err error) {
	return "", "02:00:00:00:00:01", nil
}

// stubBackend hands back a fixed stub VM from Create, enough for startOnNetwork
// to reach the point where it stamps createdThisCall and returns the VM.
type stubBackend struct{ vm backend.VM }

func (b stubBackend) CreateNetwork() (backend.Network, error)                    { return stubNetwork{}, nil }
func (b stubBackend) Create(backend.Config, backend.Network) (backend.VM, error) { return b.vm, nil }
func (stubBackend) NestedVirtSupported() bool                                    { return false }
func (stubBackend) SupportsClustering() bool                                     { return true }
func (stubBackend) Reconcile() error                                             { return nil }

// TestStartOnNetworkSetsCreatedThisCall pins the other half of the data-loss fix:
// the flag the rollback branches on must be derived from whether the member
// pre-existed (createdThisCall = !st.Exists(name)). It runs under fleetbox_fake so
// skipSSHWait short-circuits the SSH dial against the stub's unroutable IP.
func TestStartOnNetworkSetsCreatedThisCall(t *testing.T) {
	cases := []struct {
		name      string
		preCreate bool
		want      bool
	}{
		{name: "new member", preCreate: false, want: true},
		{name: "existing member", preCreate: true, want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st, err := store.NewAt(t.TempDir())
			if err != nil {
				t.Fatalf("store.NewAt: %v", err)
			}

			// A tiny stand-in base image; DiskGB 0 means CopyDisk copies it as-is
			// with no multi-GB truncate.
			imgPath := filepath.Join(t.TempDir(), "base.raw")
			if err := os.WriteFile(imgPath, []byte("disk"), 0o644); err != nil {
				t.Fatalf("write base image: %v", err)
			}

			if tc.preCreate {
				if err := st.Create(&store.VM{Name: "m-1", Image: "debian-12", CPUs: 1, MemoryMB: 1024}); err != nil {
					t.Fatalf("pre-create: %v", err)
				}
			}

			deps := &startDeps{
				options:   &opts.Options{Image: "debian-12", CPUs: 1, MemGB: 1, DiskGB: 0},
				store:     st,
				sshMgr:    sshkey.NewManager(st.SSHKeyPath()),
				pubKey:    "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAID test@fleetbox",
				imagePath: imgPath,
			}

			vm, err := startOnNetwork(
				context.Background(), "m-1", stubNetwork{}, deps, stubBackend{vm: &stubBackendVM{}},
			)
			if err != nil {
				t.Fatalf("startOnNetwork: %v", err)
			}
			if vm.createdThisCall != tc.want {
				t.Errorf("createdThisCall = %v, want %v", vm.createdThisCall, tc.want)
			}
		})
	}
}
