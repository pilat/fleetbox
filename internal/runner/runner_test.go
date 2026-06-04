package runner

import (
	"strings"
	"testing"

	"github.com/pilat/fleetbox"
)

func TestEncodeDecodeOptionsMounts(t *testing.T) {
	opts := []fleetbox.Option{
		fleetbox.WithImage("debian-12"),
		fleetbox.WithCPUs(4),
		fleetbox.WithMount("/abs/host", "/work"),
		fleetbox.WithMount("/abs/host2", "/data"),
	}

	encoded, err := encodeOptions(opts)
	if err != nil {
		t.Fatalf("encodeOptions: %v", err)
	}
	decoded, err := decodeOptions(encoded)
	if err != nil {
		t.Fatalf("decodeOptions: %v", err)
	}

	var o fleetbox.Options
	for _, opt := range decoded {
		opt(&o)
	}

	if o.Image != "debian-12" {
		t.Errorf("Image = %q, want debian-12", o.Image)
	}
	if o.CPUs != 4 {
		t.Errorf("CPUs = %d, want 4", o.CPUs)
	}

	want := []fleetbox.Mount{
		{HostPath: "/abs/host", GuestPath: "/work"},
		{HostPath: "/abs/host2", GuestPath: "/data"},
	}
	if len(o.Mounts) != len(want) {
		t.Fatalf("Mounts len = %d, want %d", len(o.Mounts), len(want))
	}
	for i := range want {
		if o.Mounts[i] != want[i] {
			t.Errorf("Mounts[%d] = %+v, want %+v", i, o.Mounts[i], want[i])
		}
	}
}

func TestEncodeOptionsNoMountsOmitsKey(t *testing.T) {
	encoded, err := encodeOptions([]fleetbox.Option{fleetbox.WithImage("debian-12")})
	if err != nil {
		t.Fatalf("encodeOptions: %v", err)
	}
	if strings.Contains(encoded, "mounts") {
		t.Errorf("encoded options should omit mounts when empty: %s", encoded)
	}
}
