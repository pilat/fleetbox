package fleetboxtest_test

import (
	"context"
	"net"
	"strings"
	"testing"

	"github.com/pilat/fleetbox"
	"github.com/pilat/fleetbox/fleetboxtest"
)

// TestVMClusterConnectivity proves cluster networking on BOTH backends: VMs in a
// StartN cluster share one network and reach each other by IP — over vmnet
// SharedMode on macOS 26+ (ADR-0008) and over the shared Linux bridge on linux
// (ADR-0011). It also boots a second cluster to verify the subnet detector hands
// out distinct /24s (R2) and that the two networks are isolated from one another.
//
// Cross-platform (no build tag): the same VM↔VM contract must hold whichever
// backend runs. Named with the TestVM prefix (no longer load-bearing — there is no
// -run selector). Boots real VMs: skipped on hosts that cannot boot one (via StartN →
// SkipIfCannotBootVM) and in -short. On a backend without clustering (macOS < 26)
// StartN returns ErrClustersUnsupported and the fixture skips it.
func TestVMClusterConnectivity(t *testing.T) {
	fleetboxtest.SkipIfShort(t, "boots real VMs")

	// Cluster A: two nodes on one shared network. Members are deliberately small
	// (WithMemoryGB(2), WithCPUs(1)) so all four VMs across both clusters fit 8 GB —
	// inside a 16 GB amd64 CI runner and inside the nested guest (Decision 8).
	clusterA := fleetboxtest.StartN(t, "node", 2,
		fleetbox.WithImage("debian-12"), fleetbox.WithMemoryGB(2), fleetbox.WithCPUs(1))
	if len(clusterA) != 2 {
		t.Fatalf("expected 2 VMs in cluster A, got %d", len(clusterA))
	}

	// VM↔VM: node-1 pings node-2 by IP over the shared network.
	assertPingOK(t, clusterA[0], clusterA[1].IP())

	// Cluster B: a second cluster started in the same process. R2 requires it
	// to land on a distinct subnet rather than colliding with cluster A.
	clusterB := fleetboxtest.StartN(t, "other", 2,
		fleetbox.WithImage("debian-12"), fleetbox.WithMemoryGB(2), fleetbox.WithCPUs(1))
	if len(clusterB) != 2 {
		t.Fatalf("expected 2 VMs in cluster B, got %d", len(clusterB))
	}

	if subnet24(clusterA[0].IP()) == subnet24(clusterB[0].IP()) {
		t.Fatalf("clusters share a /24: A=%s B=%s", clusterA[0].IP(), clusterB[0].IP())
	}

	// Cluster B is internally connected too.
	assertPingOK(t, clusterB[0], clusterB[1].IP())

	// The two networks are separate: traffic must not cross between them.
	assertPingFails(t, clusterA[0], clusterB[0].IP())
}

// assertPingOK fails the test unless `from` can ping target with no packet loss.
func assertPingOK(t *testing.T, from *fleetbox.VM, target net.IP) {
	t.Helper()
	out, err := from.SSH(context.Background(), "ping -c3 -W2 "+target.String())
	if err != nil {
		t.Fatalf("ping %s from %s failed: %v\n%s", target, from.Name(), err, out)
	}
	if !strings.Contains(out, "0% packet loss") {
		t.Fatalf("ping %s from %s did not reach it (no 0%% packet loss):\n%s", target, from.Name(), out)
	}
}

// assertPingFails fails the test only if `from` unexpectedly reaches target —
// a cross-network ping should either error (non-zero exit) or report loss.
func assertPingFails(t *testing.T, from *fleetbox.VM, target net.IP) {
	t.Helper()
	out, err := from.SSH(context.Background(), "ping -c3 -W2 "+target.String())
	if err == nil && strings.Contains(out, "0% packet loss") {
		t.Fatalf("cross-cluster ping %s from %s unexpectedly succeeded:\n%s", target, from.Name(), out)
	}
}

// subnet24 returns the /24 prefix of an IPv4 address as a string (first three
// octets), for comparing which network a VM landed on.
func subnet24(ip net.IP) string {
	v4 := ip.To4()
	if v4 == nil {
		return ip.String()
	}
	return net.IP{v4[0], v4[1], v4[2], 0}.String()
}
