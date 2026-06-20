// Package seed creates cloud-init NoCloud seed ISOs.
package seed

import (
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	cloudiso "github.com/pilat/cloudiso"
)

const (
	volumeID  = "cidata"
	publisher = "fleetbox"

	// publicDNS is the resolver list injected into the guest's network-config.
	// The Linux bridge gateway is just an address on the host with masquerade —
	// it runs no DNS service (fleetbox uses static addressing, not dnsmasq), so
	// pointing the guest at the gateway leaves it unable to resolve names even
	// though routed egress works. Real public resolvers fix that (ADR-0013).
	publicDNS = "1.1.1.1, 8.8.8.8"
)

// Config specifies the cloud-init configuration for a VM.
type Config struct {
	Hostname string
	User     string
	SSHKey   string
	// Fixtures are read-only ext4 images to mount in the guest via cloud-init's
	// mounts: directive (which writes /etc/fstab, so they re-mount on every boot
	// without a cloud-init re-run), each addressed by its volume LABEL. Empty for
	// a VM with no fixtures.
	Fixtures []Fixture
	// Network, when non-nil, makes the guest configure its NIC with a static
	// IPv4 address via a NoCloud network-config (Linux/cloud-hypervisor). Nil
	// means "no network-config", leaving the guest on DHCP (macOS).
	Network *NetworkConfig
}

// Fixture is a guest-side read-only ext4 mount: the volume Label (assigned
// host-side) and the absolute GuestPath where cloud-init mounts it by LABEL.
type Fixture struct {
	Label     string
	GuestPath string
}

// NetworkConfig is a static IPv4 assignment for the guest's single NIC, matched
// by MAC so it applies regardless of the kernel's interface naming.
type NetworkConfig struct {
	MAC     string
	IP      string
	Gateway string
	Netmask string
}

// Create generates a NoCloud seed ISO at the given path.
func Create(path string, cfg Config) error {
	now := time.Now().UTC()

	w := &cloudiso.Writer{
		VolumeID:     volumeID,
		Publisher:    publisher,
		CreationTime: now,
	}

	if err := w.AddDir("/", now); err != nil {
		return fmt.Errorf("add root dir: %w", err)
	}

	metaData := fmt.Sprintf("instance-id: %s\nlocal-hostname: %s\n", cfg.Hostname, cfg.Hostname)
	if err := w.AddFile("meta-data", []byte(metaData), now); err != nil {
		return fmt.Errorf("add meta-data: %w", err)
	}

	if err := w.AddFile("user-data", []byte(buildUserData(cfg)), now); err != nil {
		return fmt.Errorf("add user-data: %w", err)
	}

	// A static network-config is emitted only when requested (Linux); its absence
	// leaves the guest on DHCP, keeping the macOS seed byte-for-byte unchanged.
	if cfg.Network != nil {
		if err := w.AddFile("network-config", []byte(buildNetworkConfig(*cfg.Network)), now); err != nil {
			return fmt.Errorf("add network-config: %w", err)
		}
	}

	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create file: %w", err)
	}
	defer func() { _ = f.Close() }()

	if err := w.Write(f); err != nil {
		return fmt.Errorf("write iso: %w", err)
	}

	return nil
}

// buildUserData renders the cloud-init user-data. With no fixtures the output is
// byte-identical to fleetbox's original template (login user + SSH key); a
// mounts: block is appended only when fixtures are set, each line mounting a
// read-only ext4 image by its volume LABEL (ADR-0015).
func buildUserData(cfg Config) string {
	var b strings.Builder

	b.WriteString("#cloud-config\n")
	b.WriteString("users:\n")
	fmt.Fprintf(&b, "  - name: %s\n", cfg.User)
	b.WriteString("    sudo: ALL=(ALL) NOPASSWD:ALL\n")
	b.WriteString("    shell: /bin/bash\n")
	b.WriteString("    ssh_authorized_keys:\n")
	fmt.Fprintf(&b, "      - %s\n", cfg.SSHKey)

	if len(cfg.Fixtures) > 0 {
		b.WriteString("mounts:\n")
		for _, f := range cfg.Fixtures {
			fmt.Fprintf(&b, "  - [ LABEL=%s, %s, ext4, \"ro,nofail\", \"0\", \"0\" ]\n", f.Label, f.GuestPath)
		}
	}

	return b.String()
}

// buildNetworkConfig renders a NoCloud network-config (netplan v2). It pins a
// single static-IPv4 ethernet matched by MAC and renamed to eth0, with the
// gateway as the default route and public resolvers for DNS (the gateway runs
// no resolver — see publicDNS). cloud-init applies it through the distro's
// renderer, so the same config works across the supported images.
func buildNetworkConfig(n NetworkConfig) string {
	var b strings.Builder

	b.WriteString("version: 2\n")
	b.WriteString("ethernets:\n")
	b.WriteString("  primary:\n")
	b.WriteString("    match:\n")
	fmt.Fprintf(&b, "      macaddress: \"%s\"\n", n.MAC)
	b.WriteString("    set-name: eth0\n")
	b.WriteString("    dhcp4: false\n")
	b.WriteString("    addresses:\n")
	fmt.Fprintf(&b, "      - %s/%d\n", n.IP, prefixLen(n.Netmask))
	b.WriteString("    routes:\n")
	// Spell the default route as 0.0.0.0/0, not the "default" keyword: debian-11's
	// cloud-init 20.4.1 mistranslates "to: default" into a bogus "0.0.0.0/24" ifupdown
	// route, leaving the guest with no default gateway (no egress). 0.0.0.0/0 renders
	// correctly on that old renderer and is identical on netplan-native distros.
	b.WriteString("      - to: 0.0.0.0/0\n")
	fmt.Fprintf(&b, "        via: %s\n", n.Gateway)
	b.WriteString("    nameservers:\n")
	fmt.Fprintf(&b, "      addresses: [%s]\n", publicDNS)

	return b.String()
}

// prefixLen converts a dotted-quad netmask to its CIDR prefix length, defaulting
// to /24 if the netmask is unparseable.
func prefixLen(netmask string) int {
	ip := net.ParseIP(netmask)
	if ip == nil || ip.To4() == nil {
		return 24
	}
	ones, _ := net.IPMask(ip.To4()).Size()
	return ones
}
