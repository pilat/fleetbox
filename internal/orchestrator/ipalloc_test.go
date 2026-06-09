package orchestrator

import (
	"testing"

	"github.com/pilat/fleetbox/internal/store"
)

func TestAllocateIP(t *testing.T) {
	st, err := store.NewAt(t.TempDir())
	if err != nil {
		t.Fatalf("store.NewAt: %v", err)
	}

	// Empty store: the first address is .2 (.1 is the gateway).
	a, err := allocateIP(st, "192.168.5.0/24")
	if err != nil {
		t.Fatalf("allocateIP: %v", err)
	}
	if a.ip != "192.168.5.2" || a.gateway != "192.168.5.1" || a.netmask != "255.255.255.0" {
		t.Fatalf("first allocation = %+v, want {192.168.5.2 192.168.5.1 255.255.255.0}", a)
	}

	// A VM already holding .2 pushes the next allocation to .3.
	if err := st.Create(&store.VM{Name: "n1", IP: "192.168.5.2"}); err != nil {
		t.Fatalf("create n1: %v", err)
	}
	a2, err := allocateIP(st, "192.168.5.0/24")
	if err != nil {
		t.Fatalf("allocateIP: %v", err)
	}
	if a2.ip != "192.168.5.3" {
		t.Fatalf("second allocation = %s, want 192.168.5.3", a2.ip)
	}

	// A VM in a different subnet is irrelevant to this one.
	if err := st.Create(&store.VM{Name: "other", IP: "192.168.9.7"}); err != nil {
		t.Fatalf("create other: %v", err)
	}
	a3, err := allocateIP(st, "192.168.5.0/24")
	if err != nil {
		t.Fatalf("allocateIP: %v", err)
	}
	if a3.ip != "192.168.5.3" {
		t.Fatalf("allocation = %s, want 192.168.5.3 (other subnet ignored)", a3.ip)
	}
}

// TestAllocateIPReservesGatewayAndBroadcast uses a /30 (exactly one usable host)
// to pin that .1 (gateway) and .3 (broadcast) are skipped and exhaustion errors.
func TestAllocateIPReservesGatewayAndBroadcast(t *testing.T) {
	st, err := store.NewAt(t.TempDir())
	if err != nil {
		t.Fatalf("store.NewAt: %v", err)
	}

	a, err := allocateIP(st, "192.168.5.0/30")
	if err != nil {
		t.Fatalf("allocateIP: %v", err)
	}
	if a.ip != "192.168.5.2" || a.netmask != "255.255.255.252" {
		t.Fatalf("allocation = %+v, want ip 192.168.5.2 / mask 255.255.255.252", a)
	}

	if err := st.Create(&store.VM{Name: "n1", IP: "192.168.5.2"}); err != nil {
		t.Fatalf("create n1: %v", err)
	}
	if _, err := allocateIP(st, "192.168.5.0/30"); err == nil {
		t.Fatal("allocateIP on a full /30 = nil error, want exhaustion error")
	}
}
