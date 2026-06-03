package backend

import (
	"crypto/sha256"
	"fmt"
)

// GenerateMAC generates a stable MAC address from a VM name.
// The MAC is locally administered and unicast.
func GenerateMAC(name string) string {
	h := sha256.Sum256([]byte("fleetbox:" + name))
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
