// Package dhcp parses macOS dhcpd_leases file to discover VM IP addresses.
package dhcp

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

const leasesPath = "/var/db/dhcpd_leases"

// Lease represents a DHCP lease entry.
type Lease struct {
	Name      string
	IPAddress string
	HWAddress string
	LeaseTime int64
}

// LookupByHostname returns the IP address for the given hostname.
// Returns the first match (file order = newest first in macOS dhcpd_leases).
func LookupByHostname(hostname string) (string, error) {
	leases, err := ParseLeases()
	if err != nil {
		return "", err
	}

	for i := range leases {
		if leases[i].Name == hostname {
			return leases[i].IPAddress, nil
		}
	}

	return "", fmt.Errorf("hostname %q not found in DHCP leases", hostname)
}

// LookupByMAC returns the IP address for the given MAC address.
// MAC should be in format "aa:bb:cc:dd:ee:ff".
func LookupByMAC(mac string) (string, error) {
	leases, err := ParseLeases()
	if err != nil {
		return "", err
	}

	macLower := strings.ToLower(mac)
	var latest *Lease
	for i := range leases {
		if strings.EqualFold(leases[i].HWAddress, macLower) {
			if latest == nil || leases[i].LeaseTime > latest.LeaseTime {
				latest = &leases[i]
			}
		}
	}

	if latest == nil {
		return "", fmt.Errorf("MAC %q not found in DHCP leases", mac)
	}
	return latest.IPAddress, nil
}

// ParseLeases reads and parses /var/db/dhcpd_leases.
func ParseLeases() ([]Lease, error) {
	return ParseLeasesFile(leasesPath)
}

// ParseLeasesFile parses a dhcpd_leases file from the given path.
func ParseLeasesFile(path string) ([]Lease, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read leases file: %w", err)
	}
	return ParseLeasesData(string(data))
}

// ParseLeasesData parses dhcpd_leases content.
func ParseLeasesData(data string) ([]Lease, error) {
	var leases []Lease

	namePattern := regexp.MustCompile(`name=(\S+)`)
	ipPattern := regexp.MustCompile(`ip_address=([0-9.]+)`)
	hwPattern := regexp.MustCompile(`hw_address=1,([0-9a-fA-F:]+)`)
	leasePattern := regexp.MustCompile(`lease=0x([0-9a-fA-F]+)`)

	entries := strings.Split(data, "}")
	for _, entry := range entries {
		entry = strings.TrimSpace(entry)
		if entry == "" || !strings.Contains(entry, "{") {
			continue
		}

		var lease Lease

		if m := namePattern.FindStringSubmatch(entry); m != nil {
			lease.Name = m[1]
		}
		if m := ipPattern.FindStringSubmatch(entry); m != nil {
			lease.IPAddress = m[1]
		}
		if m := hwPattern.FindStringSubmatch(entry); m != nil {
			lease.HWAddress = m[1]
		}
		if m := leasePattern.FindStringSubmatch(entry); m != nil {
			_, _ = fmt.Sscanf(m[1], "%x", &lease.LeaseTime)
		}

		if lease.IPAddress != "" {
			leases = append(leases, lease)
		}
	}

	return leases, nil
}
