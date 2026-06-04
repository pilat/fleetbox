// Package seed creates cloud-init NoCloud seed ISOs.
package seed

import (
	"fmt"
	"os"
	"strings"
	"time"

	cloudiso "github.com/pilat/cloudiso"
)

const (
	volumeID  = "cidata"
	publisher = "fleetbox"
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
}

// Mount is a guest-side virtiofs mount: the device Tag (assigned host-side) and
// the absolute GuestPath where it is mounted.
type Mount struct {
	Tag       string
	GuestPath string
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
