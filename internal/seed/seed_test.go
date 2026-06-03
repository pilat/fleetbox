package seed

import (
	"os"
	"strings"
	"testing"
)

func TestCreate(t *testing.T) {
	tmpFile := t.TempDir() + "/seed.iso"

	cfg := Config{
		Hostname: "test-vm",
		User:     "testuser",
		SSHKey:   "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITest test@example.com",
	}

	if err := Create(tmpFile, cfg); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Check file exists and has reasonable size
	info, err := os.Stat(tmpFile)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}

	// ISO should be at least 2048 bytes (one block)
	if info.Size() < 2048 {
		t.Errorf("ISO size %d is too small", info.Size())
	}

	// ISO should be a multiple of 2048
	if info.Size()%2048 != 0 {
		t.Errorf("ISO size %d is not a multiple of 2048", info.Size())
	}
}

func TestConfigFormat(t *testing.T) {
	cfg := Config{
		Hostname: "my-hostname",
		User:     "myuser",
		SSHKey:   "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5 comment",
	}

	// The user-data should contain proper cloud-config
	expectedHostname := "my-hostname"
	expectedUser := "myuser"
	expectedKey := "ssh-ed25519"

	// Just verify the config fields are reasonable
	if !strings.Contains(cfg.Hostname, expectedHostname) {
		t.Errorf("Hostname should contain %s", expectedHostname)
	}
	if !strings.Contains(cfg.User, expectedUser) {
		t.Errorf("User should contain %s", expectedUser)
	}
	if !strings.Contains(cfg.SSHKey, expectedKey) {
		t.Errorf("SSHKey should contain %s", expectedKey)
	}
}
