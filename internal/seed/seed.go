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
	// Mounts are virtiofs shares to mount in the guest via cloud-init's mounts:
	// directive (which writes /etc/fstab, so they re-mount on every boot without
	// a cloud-init re-run). Empty for a mountless VM.
	Mounts []Mount
	// UID, when non-zero, pins the guest login user's uid so host-owned files in
	// a virtiofs mount line up with the guest user (virtiofs is identity
	// pass-through with no uid mapping). Zero means "let the image decide" — the
	// macOS login user is never uid 0, so 0 is a safe "unset" sentinel.
	UID int
	// Network, when non-nil, makes the guest configure its NIC with a static
	// IPv4 address via a NoCloud network-config (Linux/cloud-hypervisor). Nil
	// means "no network-config", leaving the guest on DHCP (macOS).
	Network *NetworkConfig
}

// Mount is a guest-side virtiofs mount: the device Tag (assigned host-side) and
// the absolute GuestPath where it is mounted.
type Mount struct {
	Tag       string
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

// buildUserData renders the cloud-init user-data. With no mounts and no UID the
// output is byte-identical to fleetbox's original template; the uid: line and the
// mounts: block are emitted only when set, keeping mountless VMs unchanged.
func buildUserData(cfg Config) string {
	var b strings.Builder

	b.WriteString("#cloud-config\n")
	b.WriteString("users:\n")
	fmt.Fprintf(&b, "  - name: %s\n", cfg.User)
	if cfg.UID != 0 {
		fmt.Fprintf(&b, "    uid: %d\n", cfg.UID)
	}
	b.WriteString("    sudo: ALL=(ALL) NOPASSWD:ALL\n")
	b.WriteString("    shell: /bin/bash\n")
	b.WriteString("    ssh_authorized_keys:\n")
	fmt.Fprintf(&b, "      - %s\n", cfg.SSHKey)

	if len(cfg.Mounts) > 0 {
		b.WriteString("mounts:\n")
		for _, m := range cfg.Mounts {
			fmt.Fprintf(&b, "  - [ %s, %s, virtiofs, \"defaults,nofail\", \"0\", \"0\" ]\n", m.Tag, m.GuestPath)
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
	b.WriteString("      - to: default\n")
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
