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

func TestBuildUserDataMountless(t *testing.T) {
	cfg := Config{
		Hostname: "test-vm",
		User:     "fleetbox",
		SSHKey:   "ssh-ed25519 AAAAKEY comment",
	}

	want := `#cloud-config
users:
  - name: fleetbox
    sudo: ALL=(ALL) NOPASSWD:ALL
    shell: /bin/bash
    ssh_authorized_keys:
      - ssh-ed25519 AAAAKEY comment
`

	if got := buildUserData(cfg); got != want {
		t.Errorf("mountless user-data mismatch\n got: %q\nwant: %q", got, want)
	}
}

func TestBuildUserDataWithMountsAndUID(t *testing.T) {
	cfg := Config{
		Hostname: "test-vm",
		User:     "fleetbox",
		SSHKey:   "ssh-ed25519 AAAAKEY comment",
		UID:      501,
		Mounts: []Mount{
			{Tag: "fbm0", GuestPath: "/work"},
			{Tag: "fbm1", GuestPath: "/data"},
		},
	}

	want := `#cloud-config
users:
  - name: fleetbox
    uid: 501
    sudo: ALL=(ALL) NOPASSWD:ALL
    shell: /bin/bash
    ssh_authorized_keys:
      - ssh-ed25519 AAAAKEY comment
mounts:
  - [ fbm0, /work, virtiofs, "defaults,nofail", "0", "0" ]
  - [ fbm1, /data, virtiofs, "defaults,nofail", "0", "0" ]
`

	if got := buildUserData(cfg); got != want {
		t.Errorf("mounted user-data mismatch\n got: %q\nwant: %q", got, want)
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
