//go:build darwin

package dhcp

import (
	"path/filepath"
	"strings"
	"testing"
)

// vzLease is a realistic VZ-form block: VZ writes a DUID-style identifier as
// hw_address=ff,... (documented in ADR-0007), which the hw_address=1, regex
// deliberately does NOT match — so HWAddress comes back empty and IP discovery
// rides the hostname instead. lease is a unix-time value in hex (0x10 == 16).
const vzLease = `{
	name=fleetbox-vm
	ip_address=192.168.64.7
	hw_address=ff,8a,1b,2c,3d,4e,5f
	identifier=ff,8a,1b,2c,3d,4e,5f
	lease=0x10
}`

// ethLease is a traditional ethernet-form block: hw_address=1,<mac> — the form
// the regex was originally written for. HWAddress is populated here.
const ethLease = `{
	name=legacy-host
	ip_address=10.0.0.42
	hw_address=1,aa:bb:cc:dd:ee:ff
	lease=0x20
}`

func TestParseLeasesDataVZForm(t *testing.T) {
	leases, err := ParseLeasesData(vzLease)
	if err != nil {
		t.Fatalf("ParseLeasesData: %v", err)
	}
	if len(leases) != 1 {
		t.Fatalf("got %d leases, want 1", len(leases))
	}
	l := leases[0]
	if l.Name != "fleetbox-vm" {
		t.Errorf("Name = %q, want fleetbox-vm", l.Name)
	}
	if l.IPAddress != "192.168.64.7" {
		t.Errorf("IPAddress = %q, want 192.168.64.7", l.IPAddress)
	}
	if l.LeaseTime != 16 {
		t.Errorf("LeaseTime = %d, want 16 (0x10)", l.LeaseTime)
	}
	// The VZ quirk: hw_address=ff,... never matches the hw_address=1, regex.
	// This is the regression anchor for the documented format.
	if l.HWAddress != "" {
		t.Errorf("HWAddress = %q, want empty for VZ-form hw_address=ff", l.HWAddress)
	}
}

func TestParseLeasesDataEthernetForm(t *testing.T) {
	leases, err := ParseLeasesData(ethLease)
	if err != nil {
		t.Fatalf("ParseLeasesData: %v", err)
	}
	if len(leases) != 1 {
		t.Fatalf("got %d leases, want 1", len(leases))
	}
	if leases[0].HWAddress != "aa:bb:cc:dd:ee:ff" {
		t.Errorf("HWAddress = %q, want aa:bb:cc:dd:ee:ff", leases[0].HWAddress)
	}
}

func TestParseLeasesDataSkipsEntriesWithoutIP(t *testing.T) {
	// A block with a name but no ip_address is dropped: discovery has nothing
	// to return for it.
	data := `{
	name=no-ip-yet
	hw_address=ff,1,2,3
}`
	leases, err := ParseLeasesData(data)
	if err != nil {
		t.Fatalf("ParseLeasesData: %v", err)
	}
	if len(leases) != 0 {
		t.Errorf("got %d leases, want 0 (entry without ip_address must be skipped)", len(leases))
	}
}

func TestParseLeasesDataEmptyInput(t *testing.T) {
	for _, in := range []string{"", "   ", "\n\t\n", "no braces here"} {
		leases, err := ParseLeasesData(in)
		if err != nil {
			t.Errorf("ParseLeasesData(%q): unexpected error %v", in, err)
		}
		if len(leases) != 0 {
			t.Errorf("ParseLeasesData(%q) = %d leases, want 0", in, len(leases))
		}
	}
}

func TestParseLeasesDataPreservesOrder(t *testing.T) {
	// File order is newest-first in macOS dhcpd_leases, and LookupByHostname
	// returns the first match — so order must be preserved.
	data := vzLease + "\n" + ethLease
	leases, err := ParseLeasesData(data)
	if err != nil {
		t.Fatalf("ParseLeasesData: %v", err)
	}
	if len(leases) != 2 {
		t.Fatalf("got %d leases, want 2", len(leases))
	}
	if leases[0].Name != "fleetbox-vm" || leases[1].Name != "legacy-host" {
		t.Errorf("order not preserved: got [%q, %q], want [fleetbox-vm, legacy-host]",
			leases[0].Name, leases[1].Name)
	}
}

func TestParseLeasesFileMissing(t *testing.T) {
	_, err := ParseLeasesFile(filepath.Join(t.TempDir(), "does-not-exist"))
	if err == nil {
		t.Fatal("ParseLeasesFile(missing) = nil error, want error")
	}
	if !strings.Contains(err.Error(), "read leases file") {
		t.Errorf("error %q does not mention the read-leases context", err)
	}
}
