//go:build darwin && arm64

package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/pem"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Code-Hex/vz/v3"
	cloudiso "github.com/pilat/cloudiso"
	"golang.org/x/crypto/ssh"
)

const (
	fleetboxDir = ".fleetbox"
	imagesDir   = "images"
	vmsDir      = "vms"

	defaultCPUs   = 2
	defaultMemGB  = 4
	defaultDiskGB = 20

	debian12URL    = "https://cloud.debian.org/images/cloud/bookworm/latest/debian-12-generic-arm64.raw"
	debian12SHA256 = "" // We'll skip verification in spike
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		name   = flag.String("name", "spike-test", "VM name")
		count  = flag.Int("n", 1, "number of VMs to create")
		cpus   = flag.Int("cpus", defaultCPUs, "number of CPUs")
		memGB  = flag.Int("mem", defaultMemGB, "memory in GB")
		diskGB = flag.Int("disk", defaultDiskGB, "disk size in GB")
	)
	flag.Parse()

	// Check nested virtualization support
	if !vz.IsNestedVirtualizationSupported() {
		return fmt.Errorf("nested virtualization not supported on this hardware (requires M3+ and macOS 15+)")
	}
	fmt.Println("[OK] Nested virtualization is supported")

	// Setup directories
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("get home dir: %w", err)
	}
	baseDir := filepath.Join(home, fleetboxDir)
	imagesPath := filepath.Join(baseDir, imagesDir)
	vmsPath := filepath.Join(baseDir, vmsDir)

	for _, dir := range []string{imagesPath, vmsPath} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("create dir %s: %w", dir, err)
		}
	}

	// Generate or load SSH key
	sshKeyPath := filepath.Join(baseDir, "id_ed25519")
	pubKey, privKey, err := ensureSSHKey(sshKeyPath)
	if err != nil {
		return fmt.Errorf("ensure ssh key: %w", err)
	}
	fmt.Printf("[OK] SSH key ready: %s\n", sshKeyPath)

	// Download image
	imagePath := filepath.Join(imagesPath, "debian-12-arm64.raw")
	if err := ensureImage(imagePath, debian12URL); err != nil {
		return fmt.Errorf("ensure image: %w", err)
	}
	fmt.Printf("[OK] Image ready: %s\n", imagePath)

	// Create VMs
	var vms []*vmInstance
	for i := 1; i <= *count; i++ {
		vmName := *name
		if *count > 1 {
			vmName = fmt.Sprintf("%s-%d", *name, i)
		}

		vm, err := createVM(vmName, vmsPath, imagePath, sshKeyPath, pubKey, *cpus, uint64(*memGB)*1024*1024*1024, uint64(*diskGB)*1024*1024*1024)
		if err != nil {
			return fmt.Errorf("create VM %s: %w", vmName, err)
		}
		vms = append(vms, vm)
		fmt.Printf("[OK] VM created: %s\n", vmName)
	}

	// Boot VMs
	for _, vm := range vms {
		if err := vm.boot(); err != nil {
			return fmt.Errorf("boot VM %s: %w", vm.name, err)
		}
		fmt.Printf("[OK] VM booted: %s\n", vm.name)
	}

	// Wait for IP addresses
	fmt.Println("\nWaiting for VMs to get IP addresses...")
	for _, vm := range vms {
		ip, err := waitForIP(vm.mac, vm.name, 120*time.Second)
		if err != nil {
			return fmt.Errorf("wait for IP %s: %w", vm.name, err)
		}
		vm.ip = ip
		fmt.Printf("[OK] VM %s got IP: %s\n", vm.name, ip)
	}

	// Wait for SSH to be ready and test
	fmt.Println("\nWaiting for SSH to be ready...")
	for _, vm := range vms {
		if err := waitForSSH(vm.ip, privKey, 120*time.Second); err != nil {
			return fmt.Errorf("wait for SSH %s: %w", vm.name, err)
		}
		fmt.Printf("[OK] SSH ready on %s\n", vm.name)

		// Run uname
		out, err := runSSHCommand(vm.ip, privKey, "uname -a")
		if err != nil {
			return fmt.Errorf("run uname on %s: %w", vm.name, err)
		}
		fmt.Printf("[OK] %s: %s", vm.name, out)

		// Check nested virtualization
		out, err = runSSHCommand(vm.ip, privKey, "ls -la /dev/kvm 2>&1 || echo 'NO KVM'")
		if err != nil {
			return fmt.Errorf("check kvm on %s: %w", vm.name, err)
		}
		if strings.Contains(out, "NO KVM") {
			return fmt.Errorf("nested virtualization not working on %s: /dev/kvm not found", vm.name)
		}
		fmt.Printf("[OK] Nested virt works on %s: %s", vm.name, out)
	}

	// Test VM-to-VM connectivity if multiple VMs
	if len(vms) > 1 {
		fmt.Println("\nTesting VM-to-VM connectivity...")
		fmt.Println("[KNOWN LIMITATION] VZ NAT does not support VM-to-VM connectivity.")
		fmt.Println("    VMs can reach the host and internet, but not each other.")
		fmt.Println("    For VM-to-VM, need bridged networking (requires com.apple.vm.networking entitlement)")
		fmt.Println("    or FileHandleNetworkDeviceAttachment with socket_vmnet.")
		fmt.Println("[SKIP] VM-to-VM test skipped - requires networking architecture decision")
	}

	fmt.Println("\n=== ALL CHECKS PASSED ===")
	fmt.Println("\nVMs are still running. Press Ctrl+C to stop them.")

	// Keep running until interrupted
	select {}
}

type vmInstance struct {
	name      string
	dir       string
	mac       string
	ip        string
	vm        *vz.VirtualMachine
	serialLog *os.File
}

func (v *vmInstance) boot() error {
	if err := v.vm.Start(); err != nil {
		return fmt.Errorf("start vm: %w", err)
	}

	// Wait for running state
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		state := v.vm.State()
		if state == vz.VirtualMachineStateRunning {
			return nil
		}
		if state == vz.VirtualMachineStateError {
			return fmt.Errorf("vm entered error state")
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("timeout waiting for vm to start")
}

func createVM(name, vmsPath, imagePath, sshKeyPath, pubKey string, cpus int, memBytes, diskBytes uint64) (*vmInstance, error) {
	vmDir := filepath.Join(vmsPath, name)
	if err := os.MkdirAll(vmDir, 0755); err != nil {
		return nil, fmt.Errorf("create vm dir: %w", err)
	}

	// Generate stable MAC from name
	mac := generateMAC(name)

	// Copy disk image
	diskPath := filepath.Join(vmDir, "disk.raw")
	if _, err := os.Stat(diskPath); os.IsNotExist(err) {
		if err := copyFile(imagePath, diskPath); err != nil {
			return nil, fmt.Errorf("copy disk: %w", err)
		}
		// Resize disk
		if err := resizeDisk(diskPath, diskBytes); err != nil {
			return nil, fmt.Errorf("resize disk: %w", err)
		}
	}

	// Create cloud-init seed ISO
	seedPath := filepath.Join(vmDir, "seed.iso")
	if _, err := os.Stat(seedPath); os.IsNotExist(err) {
		if err := createSeedISO(seedPath, name, pubKey); err != nil {
			return nil, fmt.Errorf("create seed iso: %w", err)
		}
	}

	// Create EFI variable store
	efiPath := filepath.Join(vmDir, "efi.nvram")
	var efiStore *vz.EFIVariableStore
	var err error
	if _, statErr := os.Stat(efiPath); os.IsNotExist(statErr) {
		efiStore, err = vz.NewEFIVariableStore(efiPath, vz.WithCreatingEFIVariableStore())
	} else {
		efiStore, err = vz.NewEFIVariableStore(efiPath)
	}
	if err != nil {
		return nil, fmt.Errorf("create efi store: %w", err)
	}

	// Create bootloader
	bootloader, err := vz.NewEFIBootLoader(vz.WithEFIVariableStore(efiStore))
	if err != nil {
		return nil, fmt.Errorf("create bootloader: %w", err)
	}

	// Create VM configuration
	vmConfig, err := vz.NewVirtualMachineConfiguration(bootloader, uint(cpus), memBytes)
	if err != nil {
		return nil, fmt.Errorf("create vm config: %w", err)
	}

	// Platform configuration with nested virtualization
	platform, err := vz.NewGenericPlatformConfiguration()
	if err != nil {
		return nil, fmt.Errorf("create platform config: %w", err)
	}
	if err := platform.SetNestedVirtualizationEnabled(true); err != nil {
		return nil, fmt.Errorf("enable nested virt: %w", err)
	}
	vmConfig.SetPlatformVirtualMachineConfiguration(platform)

	// Serial console - output to both file and stdout for debugging
	serialLogPath := filepath.Join(vmDir, "serial.log")
	serialLog, err := os.OpenFile(serialLogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return nil, fmt.Errorf("open serial log: %w", err)
	}

	serialRead, serialWrite, err := os.Pipe()
	if err != nil {
		serialLog.Close()
		return nil, fmt.Errorf("create serial pipe: %w", err)
	}
	// Write to both file and stdout
	go func() {
		multiWriter := io.MultiWriter(serialLog, os.Stdout)
		io.Copy(multiWriter, serialRead)
	}()

	serialAttachment, err := vz.NewFileHandleSerialPortAttachment(os.Stdin, serialWrite)
	if err != nil {
		serialLog.Close()
		return nil, fmt.Errorf("create serial attachment: %w", err)
	}
	serialConfig, err := vz.NewVirtioConsoleDeviceSerialPortConfiguration(serialAttachment)
	if err != nil {
		serialLog.Close()
		return nil, fmt.Errorf("create serial config: %w", err)
	}
	vmConfig.SetSerialPortsVirtualMachineConfiguration([]*vz.VirtioConsoleDeviceSerialPortConfiguration{serialConfig})

	// Disk
	diskAttachment, err := vz.NewDiskImageStorageDeviceAttachmentWithCacheAndSync(diskPath, false, vz.DiskImageCachingModeAutomatic, vz.DiskImageSynchronizationModeFsync)
	if err != nil {
		serialLog.Close()
		return nil, fmt.Errorf("create disk attachment: %w", err)
	}
	diskConfig, err := vz.NewVirtioBlockDeviceConfiguration(diskAttachment)
	if err != nil {
		serialLog.Close()
		return nil, fmt.Errorf("create disk config: %w", err)
	}

	// Seed ISO
	seedAttachment, err := vz.NewDiskImageStorageDeviceAttachment(seedPath, true)
	if err != nil {
		serialLog.Close()
		return nil, fmt.Errorf("create seed attachment: %w", err)
	}
	seedConfig, err := vz.NewVirtioBlockDeviceConfiguration(seedAttachment)
	if err != nil {
		serialLog.Close()
		return nil, fmt.Errorf("create seed config: %w", err)
	}

	vmConfig.SetStorageDevicesVirtualMachineConfiguration([]vz.StorageDeviceConfiguration{diskConfig, seedConfig})

	// Network
	natAttachment, err := vz.NewNATNetworkDeviceAttachment()
	if err != nil {
		serialLog.Close()
		return nil, fmt.Errorf("create nat attachment: %w", err)
	}
	netConfig, err := vz.NewVirtioNetworkDeviceConfiguration(natAttachment)
	if err != nil {
		serialLog.Close()
		return nil, fmt.Errorf("create net config: %w", err)
	}
	macAddr, err := vz.NewMACAddress(net.HardwareAddr(parseMACBytes(mac)))
	if err != nil {
		serialLog.Close()
		return nil, fmt.Errorf("create mac address: %w", err)
	}
	netConfig.SetMACAddress(macAddr)
	vmConfig.SetNetworkDevicesVirtualMachineConfiguration([]*vz.VirtioNetworkDeviceConfiguration{netConfig})

	// Entropy
	entropyConfig, err := vz.NewVirtioEntropyDeviceConfiguration()
	if err != nil {
		serialLog.Close()
		return nil, fmt.Errorf("create entropy config: %w", err)
	}
	vmConfig.SetEntropyDevicesVirtualMachineConfiguration([]*vz.VirtioEntropyDeviceConfiguration{entropyConfig})

	// Validate
	if valid, err := vmConfig.Validate(); !valid || err != nil {
		serialLog.Close()
		return nil, fmt.Errorf("validate config: %w", err)
	}

	// Create VM
	vm, err := vz.NewVirtualMachine(vmConfig)
	if err != nil {
		serialLog.Close()
		return nil, fmt.Errorf("create vm: %w", err)
	}

	return &vmInstance{
		name:      name,
		dir:       vmDir,
		mac:       mac,
		vm:        vm,
		serialLog: serialLog,
	}, nil
}

func generateMAC(name string) string {
	h := sha256.Sum256([]byte("fleetbox:" + name))
	// Use locally administered, unicast MAC (set bits in first octet)
	mac := []byte{
		(h[0] & 0xfe) | 0x02, // Clear multicast bit, set local bit
		h[1],
		h[2],
		h[3],
		h[4],
		h[5],
	}
	return fmt.Sprintf("%02x:%02x:%02x:%02x:%02x:%02x", mac[0], mac[1], mac[2], mac[3], mac[4], mac[5])
}

func parseMACBytes(mac string) []byte {
	parts := strings.Split(mac, ":")
	result := make([]byte, 6)
	for i, p := range parts {
		var b byte
		fmt.Sscanf(p, "%02x", &b)
		result[i] = b
	}
	return result
}

func ensureSSHKey(path string) (pubKey string, privKey []byte, err error) {
	pubPath := path + ".pub"

	// Check if key exists
	if _, err := os.Stat(path); err == nil {
		privKey, err := os.ReadFile(path)
		if err != nil {
			return "", nil, fmt.Errorf("read private key: %w", err)
		}
		pubKeyBytes, err := os.ReadFile(pubPath)
		if err != nil {
			return "", nil, fmt.Errorf("read public key: %w", err)
		}
		return strings.TrimSpace(string(pubKeyBytes)), privKey, nil
	}

	// Generate new key
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", nil, fmt.Errorf("generate key: %w", err)
	}

	// Marshal private key
	privPEM, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		return "", nil, fmt.Errorf("marshal private key: %w", err)
	}
	privKeyBytes := pem.EncodeToMemory(privPEM)

	// Marshal public key
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return "", nil, fmt.Errorf("create ssh public key: %w", err)
	}
	pubKeyStr := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub)))

	// Write keys
	if err := os.WriteFile(path, privKeyBytes, 0600); err != nil {
		return "", nil, fmt.Errorf("write private key: %w", err)
	}
	if err := os.WriteFile(pubPath, []byte(pubKeyStr+"\n"), 0644); err != nil {
		return "", nil, fmt.Errorf("write public key: %w", err)
	}

	return pubKeyStr, privKeyBytes, nil
}

func ensureImage(path, url string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	}

	fmt.Printf("Downloading image from %s...\n", url)

	resp, err := http.Get(url)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download failed: %s", resp.Status)
	}

	tmpPath := path + ".tmp"
	f, err := os.Create(tmpPath)
	if err != nil {
		return fmt.Errorf("create file: %w", err)
	}

	_, err = io.Copy(f, resp.Body)
	f.Close()
	if err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("write file: %w", err)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename: %w", err)
	}

	return nil
}

func copyFile(src, dst string) error {
	srcF, err := os.Open(src)
	if err != nil {
		return err
	}
	defer srcF.Close()

	dstF, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer dstF.Close()

	_, err = io.Copy(dstF, srcF)
	return err
}

func resizeDisk(path string, size uint64) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Truncate(int64(size))
}

func createSeedISO(path, hostname, pubKey string) error {
	now := time.Now().UTC()

	w := &cloudiso.Writer{
		VolumeID:     "cidata",
		Publisher:    "fleetbox",
		CreationTime: now,
	}

	if err := w.AddDir("/", now); err != nil {
		return fmt.Errorf("add root dir: %w", err)
	}

	metaData := fmt.Sprintf("instance-id: %s\nlocal-hostname: %s\n", hostname, hostname)
	if err := w.AddFile("meta-data", []byte(metaData), now); err != nil {
		return fmt.Errorf("add meta-data: %w", err)
	}

	userData := fmt.Sprintf(`#cloud-config
users:
  - name: fleetbox
    sudo: ALL=(ALL) NOPASSWD:ALL
    shell: /bin/bash
    ssh_authorized_keys:
      - %s
`, pubKey)
	if err := w.AddFile("user-data", []byte(userData), now); err != nil {
		return fmt.Errorf("add user-data: %w", err)
	}

	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create file: %w", err)
	}
	defer f.Close()

	if err := w.Write(f); err != nil {
		return fmt.Errorf("write iso: %w", err)
	}

	return nil
}

func waitForIP(mac, hostname string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		// Try by MAC first
		ip, err := lookupIPByMAC(mac)
		if err == nil && ip != "" {
			return ip, nil
		}
		// Fall back to hostname (cloud-init sets it via DHCP)
		ip, err = lookupIPByHostname(hostname)
		if err == nil && ip != "" {
			return ip, nil
		}
		time.Sleep(time.Second)
	}
	return "", fmt.Errorf("timeout waiting for IP (MAC: %s, hostname: %s)", mac, hostname)
}

func lookupIPByMAC(mac string) (string, error) {
	data, err := os.ReadFile("/var/db/dhcpd_leases")
	if err != nil {
		return "", err
	}

	// Parse dhcpd_leases format
	// Format: hw_address=1,aa:bb:cc:dd:ee:ff (traditional) or hw_address=ff,... (DUID-based)
	// The MAC is prefixed with "1,"
	macLower := strings.ToLower(mac)
	hwPattern := regexp.MustCompile(`hw_address=1,([0-9a-f:]+)`)
	ipPattern := regexp.MustCompile(`ip_address=([0-9.]+)`)

	entries := strings.Split(string(data), "}")
	for _, entry := range entries {
		hwMatch := hwPattern.FindStringSubmatch(entry)
		ipMatch := ipPattern.FindStringSubmatch(entry)
		if hwMatch != nil && ipMatch != nil {
			if strings.ToLower(hwMatch[1]) == macLower {
				return ipMatch[1], nil
			}
		}
	}

	return "", fmt.Errorf("MAC not found")
}

func lookupIPByHostname(hostname string) (string, error) {
	data, err := os.ReadFile("/var/db/dhcpd_leases")
	if err != nil {
		return "", err
	}

	namePattern := regexp.MustCompile(`name=(\S+)`)
	ipPattern := regexp.MustCompile(`ip_address=([0-9.]+)`)
	leasePattern := regexp.MustCompile(`lease=0x([0-9a-f]+)`)

	var latestIP string
	var latestLease int64

	entries := strings.Split(string(data), "}")
	for _, entry := range entries {
		nameMatch := namePattern.FindStringSubmatch(entry)
		ipMatch := ipPattern.FindStringSubmatch(entry)
		leaseMatch := leasePattern.FindStringSubmatch(entry)
		if nameMatch != nil && ipMatch != nil && leaseMatch != nil {
			if nameMatch[1] == hostname {
				var lease int64
				fmt.Sscanf(leaseMatch[1], "%x", &lease)
				if lease > latestLease {
					latestLease = lease
					latestIP = ipMatch[1]
				}
			}
		}
	}

	if latestIP == "" {
		return "", fmt.Errorf("hostname not found")
	}
	return latestIP, nil
}

func waitForSSH(ip string, privKey []byte, timeout time.Duration) error {
	signer, err := ssh.ParsePrivateKey(privKey)
	if err != nil {
		return fmt.Errorf("parse key: %w", err)
	}

	config := &ssh.ClientConfig{
		User:            "fleetbox",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	}

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		client, err := ssh.Dial("tcp", net.JoinHostPort(ip, "22"), config)
		if err == nil {
			client.Close()
			return nil
		}
		time.Sleep(2 * time.Second)
	}

	return fmt.Errorf("timeout waiting for SSH")
}

func runSSHCommand(ip string, privKey []byte, cmd string) (string, error) {
	signer, err := ssh.ParsePrivateKey(privKey)
	if err != nil {
		return "", fmt.Errorf("parse key: %w", err)
	}

	config := &ssh.ClientConfig{
		User:            "fleetbox",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         10 * time.Second,
	}

	client, err := ssh.Dial("tcp", net.JoinHostPort(ip, "22"), config)
	if err != nil {
		return "", fmt.Errorf("dial: %w", err)
	}
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		return "", fmt.Errorf("session: %w", err)
	}
	defer session.Close()

	var stdout, stderr bytes.Buffer
	session.Stdout = &stdout
	session.Stderr = &stderr

	if err := session.Run(cmd); err != nil {
		return stdout.String() + stderr.String(), err
	}

	return stdout.String(), nil
}
