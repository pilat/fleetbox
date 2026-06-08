package runner

import (
	"strings"
	"testing"

	"github.com/pilat/fleetbox"
)

func TestEncodeDecodeOptionsFixtures(t *testing.T) {
	opts := []fleetbox.Option{
		fleetbox.WithImage("debian-12"),
		fleetbox.WithCPUs(4),
		fleetbox.WithFixture("/abs/host", "/work"),
		fleetbox.WithFixture("/abs/host2", "/data"),
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

	want := []fleetbox.Fixture{
		{HostPath: "/abs/host", GuestPath: "/work"},
		{HostPath: "/abs/host2", GuestPath: "/data"},
	}
	if len(o.Fixtures) != len(want) {
		t.Fatalf("Fixtures len = %d, want %d", len(o.Fixtures), len(want))
	}
	for i := range want {
		if o.Fixtures[i] != want[i] {
			t.Errorf("Fixtures[%d] = %+v, want %+v", i, o.Fixtures[i], want[i])
		}
	}
}

func TestEncodeOptionsNoFixturesOmitsKey(t *testing.T) {
	encoded, err := encodeOptions([]fleetbox.Option{fleetbox.WithImage("debian-12")})
	if err != nil {
		t.Fatalf("encodeOptions: %v", err)
	}
	if strings.Contains(encoded, "fixtures") {
		t.Errorf("encoded options should omit fixtures when empty: %s", encoded)
	}
}
