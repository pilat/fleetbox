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
