//go:build darwin && arm64

package vz

import (
	"context"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/pilat/fleetbox/third_party/vz"
)

func resetReservedSubnets(t *testing.T) {
	t.Helper()
	reservedSubnetsMu.Lock()
	orig := reservedSubnets
	reservedSubnets = map[netip.Prefix]struct{}{}
	reservedSubnetsMu.Unlock()
	t.Cleanup(func() {
		reservedSubnetsMu.Lock()
		reservedSubnets = orig
		reservedSubnetsMu.Unlock()
	})
}

// TestDetectFreeIPv4SubnetUniqueAndValid guards the subnet picker: every result
// is a distinct /24 inside 192.168.0.0/16. The random start (REPRO Bug 3, to dodge
// a leaked reservation) must not break the circular scan or the in-process dedup.
func TestDetectFreeIPv4SubnetUniqueAndValid(t *testing.T) {
	resetReservedSubnets(t)

	base := netip.MustParsePrefix("192.168.0.0/16")
	seen := map[netip.Prefix]bool{}
	for i := range 8 {
		got, err := detectFreeIPv4Subnet()
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if got.Bits() != 24 || !base.Overlaps(got) {
			t.Fatalf("call %d: got %v, want a /24 in 192.168.0.0/16", i, got)
		}
		if seen[got] {
			t.Fatalf("call %d: duplicate subnet %v", i, got)
		}
		seen[got] = true
	}
}

// TestDetectFreeIPv4SubnetExhausted pins that the circular scan terminates: with
// every /24 reserved, the picker errors instead of looping or returning a taken
// subnet regardless of the random start octet.
func TestDetectFreeIPv4SubnetExhausted(t *testing.T) {
	resetReservedSubnets(t)

	reservedSubnetsMu.Lock()
	for octet := range 256 {
		reservedSubnets[netip.PrefixFrom(netip.AddrFrom4([4]byte{192, 168, byte(octet), 0}), 24)] = struct{}{}
	}
	reservedSubnetsMu.Unlock()

	if _, err := detectFreeIPv4Subnet(); err == nil {
		t.Fatal("expected error when all /24s are reserved, got nil")
	}
}

// fakeVM is a scriptable vmController for exercising VM.Stop's escalation without
// a real VZ VM. RequestStop/Stop transition the state per the *OnACPI/*OnForce
// flags, so waitStopped observes the transition on its first poll.
type fakeVM struct {
	mu             sync.Mutex
	state          vz.VirtualMachineState
	canRequestStop bool
	stopOnACPI     bool
	stopOnForce    bool
	requestStops   int
	forceStops     int
}

func (f *fakeVM) Start(...vz.VirtualMachineStartOption) error { return nil }

func (f *fakeVM) State() vz.VirtualMachineState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state
}

func (f *fakeVM) CanRequestStop() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.canRequestStop
}

func (f *fakeVM) RequestStop() (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requestStops++
	if f.stopOnACPI {
		f.state = vz.VirtualMachineStateStopped
	}
	return true, nil
}

func (f *fakeVM) Stop() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.forceStops++
	if f.stopOnForce {
		f.state = vz.VirtualMachineStateStopped
	}
	return nil
}

// TestVMStopEscalation pins the ACPI→force-stop escalation: a guest that ignores
// ACPI must be forcefully stopped so it releases the disk (REPRO Bug 2).
func TestVMStopEscalation(t *testing.T) {
	defer swapGrace(10*time.Millisecond, 10*time.Millisecond)()

	tests := []struct {
		name         string
		fake         *fakeVM
		wantErr      bool
		wantRequests int
		wantForces   int
	}{
		{
			name: "already stopped is a no-op",
			fake: &fakeVM{state: vz.VirtualMachineStateStopped},
		},
		{
			name:         "acpi honored, no escalation",
			fake:         &fakeVM{state: vz.VirtualMachineStateRunning, canRequestStop: true, stopOnACPI: true},
			wantRequests: 1,
			wantForces:   0,
		},
		{
			name:         "hung guest escalates to force stop",
			fake:         &fakeVM{state: vz.VirtualMachineStateRunning, canRequestStop: true, stopOnForce: true},
			wantRequests: 1,
			wantForces:   1,
		},
		{
			name:       "cannot request stop goes straight to force",
			fake:       &fakeVM{state: vz.VirtualMachineStateRunning, canRequestStop: false, stopOnForce: true},
			wantForces: 1,
		},
		{
			name:         "force stop that never stops errors",
			fake:         &fakeVM{state: vz.VirtualMachineStateRunning, canRequestStop: true},
			wantErr:      true,
			wantRequests: 1,
			wantForces:   1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := &VM{vm: tt.fake}
			err := v.Stop(context.Background())
			if (err != nil) != tt.wantErr {
				t.Fatalf("Stop() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.fake.requestStops != tt.wantRequests {
				t.Errorf("RequestStop calls = %d, want %d", tt.fake.requestStops, tt.wantRequests)
			}
			if tt.fake.forceStops != tt.wantForces {
				t.Errorf("force Stop calls = %d, want %d", tt.fake.forceStops, tt.wantForces)
			}
		})
	}
}

// TestVMStopForcesDespiteCancelledContext guards the escalation against a cancelled
// caller context: a short/cancelled deadline must not skip the forceful stop and
// leave disk.raw held (REPRO Bug 2, via the context back door).
func TestVMStopForcesDespiteCancelledContext(t *testing.T) {
	defer swapGrace(10*time.Millisecond, 10*time.Millisecond)()

	f := &fakeVM{state: vz.VirtualMachineStateRunning, canRequestStop: true, stopOnForce: true}
	v := &VM{vm: f}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := v.Stop(ctx); err != nil {
		t.Fatalf("Stop(cancelled ctx) = %v, want nil", err)
	}
	if f.forceStops != 1 {
		t.Errorf("force Stop calls = %d, want 1 — escalation must survive a cancelled ctx", f.forceStops)
	}
}

// swapGrace shrinks the Stop grace windows for a test and returns a restore func.
func swapGrace(acpi, force time.Duration) func() {
	origACPI, origForce := acpiStopGrace, forceStopGrace
	acpiStopGrace, forceStopGrace = acpi, force
	return func() { acpiStopGrace, forceStopGrace = origACPI, origForce }
}

func TestParseMacOSMajor(t *testing.T) {
	tests := []struct {
		in   string
		want int
	}{
		{"26.4.1", 26},
		{"26.0", 26},
		{"26", 26},
		{"15.5", 15},
		{"13.7.6", 13},
		{"", 0},
		{"notaversion", 0},
		{".4", 0},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := parseMacOSMajor(tt.in); got != tt.want {
				t.Errorf("parseMacOSMajor(%q) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

// TestSupportsClusteringGate pins the version gate that selects SharedMode +
// clustering (26+) versus the NAT, single-VM fallback (<26) — no <26 hardware
// needed, the major version is injected directly.
func TestSupportsClusteringGate(t *testing.T) {
	tests := []struct {
		major int
		want  bool
	}{
		{0, false},
		{15, false},
		{25, false},
		{26, true},
		{27, true},
	}

	for _, tt := range tests {
		b := &Backend{macOSMajor: tt.major}
		if got := b.SupportsClustering(); got != tt.want {
			t.Errorf("Backend{macOSMajor: %d}.SupportsClustering() = %v, want %v", tt.major, got, tt.want)
		}
	}
}
