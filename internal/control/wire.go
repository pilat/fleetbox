package control

// The per-member command socket speaks newline-delimited JSON (NDJSON): each
// request is one JSON object followed by '\n', and each reply is one JSON object
// followed by '\n'. A connection carries exactly one request and one reply, then
// closes, so a fresh json.Decoder per read never drops buffered bytes. The bind
// handshake on the separate .ctl socket keeps its tiny text protocol
// (bind→version→ack); only the command socket is JSON.
//
// Every request carries a Cmd discriminator. The message set:
//
//	{"cmd":"status"}                          -> {"status":{...}}
//	{"cmd":"stop"}                            -> {} | {"error":"..."}
//	{"cmd":"createnetwork"}                   -> {"subnet":"192.168.5.0/24"} | {"subnet":""}
//	{"cmd":"reserve","name":"n","ip_hint":""} -> {"reservation":{"ip":"...","mac":"..."}}
//	{"cmd":"boot-member","spec":{...}}        -> {} | {"error":"..."}
//
// Adding a member to a live cluster is just reserve + boot-member on the running
// helper (no dedicated command); createnetwork is idempotent so the client can
// reach an existing network's subnet the same way.
//
// MemberSpec is the resolved, backend-neutral per-member boot payload the client
// hands the helper: ready paths and sizing, no image alias, no MAC, no IP. The
// helper holds the MAC and IP from the createnetwork/reserve reservation it made
// itself (Decision 5/6), so they are deliberately absent here.

import (
	"encoding/json"
	"fmt"
	"io"
)

const (
	// CmdCreateNetwork asks the helper to create the cluster's shared network and
	// report its subnet (empty on a DHCP backend such as vz).
	CmdCreateNetwork = "createnetwork"

	// CmdReserve asks the helper to allocate one member's address on the live
	// network (Linux static IP honoring the client's hint, or just a MAC on the
	// DHCP/vz path) and return the reservation. It replaces the orchestrator's
	// old client-side allocateIP (Decision 5).
	CmdReserve = "reserve"

	// CmdBootMember asks the helper to create and start one member's VM from a
	// resolved MemberSpec on the shared network, using the address it reserved.
	CmdBootMember = "boot-member"
)

// MemberSpec is the resolved per-member boot payload sent over the wire. Every
// field is a ready value the helper consumes verbatim — the client has already
// resolved the image, copied the disk, built the seed and fixtures, and chosen
// the store paths. EFIPath is the vz EFI variable-store path (store-derived by
// the client; the helper creates/loads the file there); cloud-hypervisor ignores
// it (PVH boot). MAC and IP are intentionally absent: the helper owns them from
// the reservation it made (Decisions 5 and 6).
type MemberSpec struct {
	Name          string   `json:"name"`
	DiskPath      string   `json:"disk_path"`
	SeedPath      string   `json:"seed_path"`
	FixturePaths  []string `json:"fixture_paths,omitempty"`
	EFIPath       string   `json:"efi_path,omitempty"`
	CPUs          int      `json:"cpus"`
	MemoryBytes   uint64   `json:"memory_bytes"`
	SerialLogPath string   `json:"serial_log_path,omitempty"`
}

// Reservation is the address the helper allocated for a member on the shared
// network. IP is the static IPv4 the client bakes into the seed's
// network-config (Linux); it is empty on the DHCP/vz path, where the runtime IP
// is discovered post-boot and reported via status. MAC is the NIC address the
// helper will set, returned so the client's seed and the helper's NIC agree
// without both sides recomputing (Decision 6).
type Reservation struct {
	IP  string `json:"ip,omitempty"`
	MAC string `json:"mac"`
}

// Request is the NDJSON command envelope. Cmd selects the command; the remaining
// fields are populated only for the commands that use them (Name + IPHint for
// reserve, Spec for boot-member).
type Request struct {
	Cmd    string      `json:"cmd"`
	Name   string      `json:"name,omitempty"`
	IPHint string      `json:"ip_hint,omitempty"`
	Spec   *MemberSpec `json:"spec,omitempty"`
}

// Response is the NDJSON reply envelope. A non-empty Error means the command
// failed; otherwise the command-specific field is set (Status for status, Subnet
// for createnetwork, Reservation for reserve) and stop/boot-member reply with an
// empty object on success.
type Response struct {
	Error       string       `json:"error,omitempty"`
	Status      *Status      `json:"status,omitempty"`
	Subnet      string       `json:"subnet,omitempty"`
	Reservation *Reservation `json:"reservation,omitempty"`
}

// WriteMessage encodes v as one NDJSON message (json.Encoder.Encode appends the
// trailing newline). It is shared by the client (control) and server (holder)
// halves so the two ends frame messages identically.
func WriteMessage(w io.Writer, v any) error {
	if err := json.NewEncoder(w).Encode(v); err != nil {
		return fmt.Errorf("write message: %w", err)
	}
	return nil
}

// ReadMessage decodes one NDJSON message from r into v. Each connection carries a
// single request and a single reply, so a fresh decoder per call never strands
// buffered bytes. Any read deadline set on the underlying conn still bounds it.
func ReadMessage(r io.Reader, v any) error {
	if err := json.NewDecoder(r).Decode(v); err != nil {
		return fmt.Errorf("read message: %w", err)
	}
	return nil
}
