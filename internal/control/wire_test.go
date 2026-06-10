package control

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

// These tests pin the NDJSON wire contract: every request/response variant
// round-trips through WriteMessage/ReadMessage unchanged, each message is framed
// as a single JSON line terminated by '\n', and a streaming decoder reads several
// back-to-back (the newline delimiting). No socket, no backend — just the codec.

func TestRequestRoundTrip(t *testing.T) {
	cases := map[string]Request{
		"status":        {Cmd: CmdStatus},
		"stop":          {Cmd: CmdStop},
		"createnetwork": {Cmd: CmdCreateNetwork},
		"reserve":       {Cmd: CmdReserve, Name: "web-1", IPHint: "192.168.5.10"},
		"boot-member": {Cmd: CmdBootMember, Spec: &MemberSpec{
			Name:          "web-1",
			DiskPath:      "/d/disk.img",
			SeedPath:      "/d/seed.iso",
			FixturePaths:  []string{"/d/fixture-0.img", "/d/fixture-1.img"},
			EFIPath:       "/d/efi.nvram",
			CPUs:          2,
			MemoryBytes:   4 << 30,
			SerialLogPath: "/d/serial.log",
		}},
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			var got Request
			roundTrip(t, req, &got)
			if !reflect.DeepEqual(got, req) {
				t.Fatalf("round-trip mismatch:\n got %+v\nwant %+v", got, req)
			}
		})
	}
}

func TestResponseRoundTrip(t *testing.T) {
	cases := map[string]Response{
		"ok":          {},
		"error":       {Error: "boot member web-1: no free IP"},
		"status":      {Status: &Status{Name: "web-1", PID: 42, Running: true, IP: "203.0.113.7", State: StateRunning}},
		"subnet":      {Subnet: "192.168.5.0/24"},
		"subnet-dhcp": {Subnet: ""},
		"reserve-ip":  {Reservation: &Reservation{IP: "192.168.5.10", MAC: "52:54:00:ab:cd:ef"}},
		"reserve-mac": {Reservation: &Reservation{MAC: "52:54:00:ab:cd:ef"}},
	}
	for name, resp := range cases {
		t.Run(name, func(t *testing.T) {
			var got Response
			roundTrip(t, resp, &got)
			if !reflect.DeepEqual(got, resp) {
				t.Fatalf("round-trip mismatch:\n got %+v\nwant %+v", got, resp)
			}
		})
	}
}

func TestWriteMessageIsNewlineDelimited(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteMessage(&buf, Request{Cmd: CmdStatus}); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}
	out := buf.Bytes()
	if n := len(out); n == 0 || out[n-1] != '\n' {
		t.Fatalf("message not newline-terminated: %q", out)
	}
	// Exactly one newline: the framing delimiter, none inside the JSON body.
	if got := bytes.Count(out, []byte{'\n'}); got != 1 {
		t.Fatalf("want exactly one newline, got %d in %q", got, out)
	}
}

func TestStreamingDecodeReadsConsecutiveMessages(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteMessage(&buf, Request{Cmd: CmdReserve, Name: "a"}); err != nil {
		t.Fatalf("WriteMessage 1: %v", err)
	}
	if err := WriteMessage(&buf, Request{Cmd: CmdReserve, Name: "b"}); err != nil {
		t.Fatalf("WriteMessage 2: %v", err)
	}
	// A single streaming decoder must read both, proving the '\n' delimiting works
	// for back-to-back messages even though production uses one message per conn.
	dec := json.NewDecoder(&buf)
	var first, second Request
	if err := dec.Decode(&first); err != nil {
		t.Fatalf("decode 1: %v", err)
	}
	if err := dec.Decode(&second); err != nil {
		t.Fatalf("decode 2: %v", err)
	}
	if first.Name != "a" || second.Name != "b" {
		t.Fatalf("got names %q, %q; want a, b", first.Name, second.Name)
	}
}

// roundTrip encodes v with WriteMessage and decodes it back into out with
// ReadMessage through an in-memory buffer.
func roundTrip(t *testing.T, v, out any) {
	t.Helper()
	var buf bytes.Buffer
	if err := WriteMessage(&buf, v); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}
	if err := ReadMessage(&buf, out); err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
}
