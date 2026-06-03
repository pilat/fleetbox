// Package seed creates cloud-init NoCloud seed ISOs.
package seed

import (
	"fmt"
	"os"
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

	userData := fmt.Sprintf(`#cloud-config
users:
  - name: %s
    sudo: ALL=(ALL) NOPASSWD:ALL
    shell: /bin/bash
    ssh_authorized_keys:
      - %s
`, cfg.User, cfg.SSHKey)

	if err := w.AddFile("user-data", []byte(userData), now); err != nil {
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
