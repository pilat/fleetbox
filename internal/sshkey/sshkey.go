// Package sshkey manages SSH key generation and provides an SSH client for VM access.
package sshkey

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// Manager handles SSH key storage and client creation.
type Manager struct {
	keyPath string
}

// NewManager creates a Manager with keys stored at the given path.
// The path should be the private key path (e.g., ~/.fleetbox/id_ed25519).
func NewManager(keyPath string) *Manager {
	return &Manager{keyPath: keyPath}
}

// EnsureKey generates an ed25519 keypair if it doesn't exist.
// Returns the public key in authorized_keys format.
func (m *Manager) EnsureKey() (string, error) {
	pubKeyPath := m.keyPath + ".pub"

	// Check if key already exists
	if _, err := os.Stat(m.keyPath); err == nil {
		pubKeyBytes, err := os.ReadFile(pubKeyPath)
		if err != nil {
			return "", fmt.Errorf("read public key: %w", err)
		}
		// Repair ownership on EVERY run, not just fresh creation: a key written
		// root-owned by an earlier run (or a pre-fix /root/.fleetbox) must still
		// be handed back to the invoking user, or a non-root `ssh -i` can't read
		// it (ADR-0023). Idempotent and cheap.
		m.chownToInvoker()
		return strings.TrimSpace(string(pubKeyBytes)), nil
	}

	// Create directory if needed
	if err := os.MkdirAll(filepath.Dir(m.keyPath), 0o700); err != nil {
		return "", fmt.Errorf("create key directory: %w", err)
	}

	// Generate new keypair
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", fmt.Errorf("generate key: %w", err)
	}

	// Marshal private key
	privPEM, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		return "", fmt.Errorf("marshal private key: %w", err)
	}
	privKeyBytes := pem.EncodeToMemory(privPEM)

	// Marshal public key
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return "", fmt.Errorf("create ssh public key: %w", err)
	}
	pubKeyStr := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub)))

	// Write keys
	if err := os.WriteFile(m.keyPath, privKeyBytes, 0o600); err != nil {
		return "", fmt.Errorf("write private key: %w", err)
	}
	if err := os.WriteFile(pubKeyPath, []byte(pubKeyStr+"\n"), 0o644); err != nil {
		return "", fmt.Errorf("write public key: %w", err)
	}

	m.chownToInvoker()
	return pubKeyStr, nil
}

// PrivateKey returns the private key bytes.
func (m *Manager) PrivateKey() ([]byte, error) {
	data, err := os.ReadFile(m.keyPath)
	if err != nil {
		return nil, fmt.Errorf("read private key: %w", err)
	}

	return data, nil
}

// Path returns the private key path.
func (m *Manager) Path() string {
	return m.keyPath
}

// Client creates an SSH client connected to the given address.
type Client struct {
	client *ssh.Client
}

// Dial connects to the SSH server at the given address using the manager's key.
func (m *Manager) Dial(addr, user string, timeout time.Duration) (*Client, error) {
	privKey, err := m.PrivateKey()
	if err != nil {
		return nil, fmt.Errorf("read private key: %w", err)
	}

	signer, err := ssh.ParsePrivateKey(privKey)
	if err != nil {
		return nil, fmt.Errorf("parse private key: %w", err)
	}

	config := &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         timeout,
	}

	client, err := ssh.Dial("tcp", addr, config)
	if err != nil {
		return nil, fmt.Errorf("ssh dial %s: %w", addr, err)
	}

	return &Client{client: client}, nil
}

// Run executes a command and returns the combined stdout/stderr output.
func (c *Client) Run(cmd string) (string, error) {
	session, err := c.client.NewSession()
	if err != nil {
		return "", fmt.Errorf("create session: %w", err)
	}
	defer func() { _ = session.Close() }()

	output, err := session.CombinedOutput(cmd)
	return string(output), err
}

// Close closes the SSH connection.
func (c *Client) Close() error {
	if err := c.client.Close(); err != nil {
		return fmt.Errorf("close ssh connection: %w", err)
	}

	return nil
}

// WaitForSSH waits until SSH is available at the given address.
func (m *Manager) WaitForSSH(addr, user string, timeout time.Duration) error {
	privKey, err := m.PrivateKey()
	if err != nil {
		return fmt.Errorf("read private key: %w", err)
	}

	signer, err := ssh.ParsePrivateKey(privKey)
	if err != nil {
		return fmt.Errorf("parse private key: %w", err)
	}

	config := &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	}

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		client, err := ssh.Dial("tcp", addr, config)
		if err == nil {
			_ = client.Close()
			return nil
		}
		time.Sleep(2 * time.Second)
	}

	return fmt.Errorf("timeout waiting for SSH at %s", addr)
}

// DialIP is a convenience function that dials port 22 on the given IP.
func (m *Manager) DialIP(ip net.IP, user string, timeout time.Duration) (*Client, error) {
	addr := net.JoinHostPort(ip.String(), "22")
	return m.Dial(addr, user, timeout)
}

// chownToInvoker hands the freshly created (or repaired) key pair back to the user
// who invoked the CLI via sudo, so a non-root `ssh -i` can read the 0600 private
// key (ADR-0023). It is a no-op unless the process is root with SUDO_UID/SUDO_GID
// set — i.e. a normal user, or macOS, does nothing — and best-effort: a chown
// failure must not break key setup. It changes ONLY ownership (mode stays 0600),
// because ssh refuses a world-readable key, so loosening the mode is not an option;
// and only the two key files, never the base dir (decision 6 — 0755 already lets
// the user traverse to them).
func (m *Manager) chownToInvoker() {
	if os.Geteuid() != 0 {
		return
	}
	uid, err := strconv.Atoi(os.Getenv("SUDO_UID"))
	if err != nil {
		return
	}
	gid, err := strconv.Atoi(os.Getenv("SUDO_GID"))
	if err != nil {
		return
	}
	_ = os.Chown(m.keyPath, uid, gid)
	_ = os.Chown(m.keyPath+".pub", uid, gid)
}
