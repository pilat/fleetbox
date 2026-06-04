package sshkey

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestEnsureKeyGeneratesUsableKeypair(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "id_ed25519")
	m := NewManager(keyPath)

	pub, err := m.EnsureKey()
	if err != nil {
		t.Fatalf("EnsureKey: %v", err)
	}
	if !strings.HasPrefix(pub, "ssh-ed25519 ") {
		t.Errorf("public key %q does not start with ssh-ed25519", pub)
	}

	// The returned public key must parse as an authorized_keys line.
	if _, _, _, _, err := ssh.ParseAuthorizedKey([]byte(pub)); err != nil {
		t.Errorf("ParseAuthorizedKey: %v", err)
	}

	// Permission checks assume the CI umask of 022, under which WriteFile's
	// 0600/0644 modes survive unmasked.
	if info, err := os.Stat(keyPath); err != nil {
		t.Fatalf("stat private key: %v", err)
	} else if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("private key perm = %o, want 600", perm)
	}
	if info, err := os.Stat(keyPath + ".pub"); err != nil {
		t.Fatalf("stat public key: %v", err)
	} else if perm := info.Mode().Perm(); perm != 0o644 {
		t.Errorf("public key perm = %o, want 644", perm)
	}

	// PrivateKey returns bytes a signer accepts.
	priv, err := m.PrivateKey()
	if err != nil {
		t.Fatalf("PrivateKey: %v", err)
	}
	if _, err := ssh.ParsePrivateKey(priv); err != nil {
		t.Errorf("ParsePrivateKey: %v", err)
	}
}

func TestEnsureKeyIsIdempotent(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "id_ed25519")
	m := NewManager(keyPath)

	first, err := m.EnsureKey()
	if err != nil {
		t.Fatalf("EnsureKey #1: %v", err)
	}
	before, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("read private key: %v", err)
	}

	second, err := m.EnsureKey()
	if err != nil {
		t.Fatalf("EnsureKey #2: %v", err)
	}
	if first != second {
		t.Errorf("second EnsureKey returned a different key:\n %q\n %q", first, second)
	}

	after, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("read private key: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Error("private key file was rewritten on the second EnsureKey call")
	}
}
