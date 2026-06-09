package opts

import (
	"strings"
	"testing"
)

func TestWithFixtureAppliesToOptions(t *testing.T) {
	var o Options
	WithFixture("a", "/b")(&o)
	WithFixture("c", "/d")(&o)

	want := []Fixture{{HostPath: "a", GuestPath: "/b"}, {HostPath: "c", GuestPath: "/d"}}
	if len(o.Fixtures) != len(want) {
		t.Fatalf("Fixtures len = %d, want %d", len(o.Fixtures), len(want))
	}
	for i := range want {
		if o.Fixtures[i] != want[i] {
			t.Errorf("Fixtures[%d] = %+v, want %+v", i, o.Fixtures[i], want[i])
		}
	}
}

func TestEncodeDecodeOptionsFixtures(t *testing.T) {
	options := []Option{
		WithImage("debian-12"),
		WithCPUs(4),
		WithFixture("/abs/host", "/work"),
		WithFixture("/abs/host2", "/data"),
	}

	encoded, err := Encode(options)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	decoded, err := Decode(encoded)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}

	var o Options
	for _, opt := range decoded {
		opt(&o)
	}

	if o.Image != "debian-12" {
		t.Errorf("Image = %q, want debian-12", o.Image)
	}
	if o.CPUs != 4 {
		t.Errorf("CPUs = %d, want 4", o.CPUs)
	}

	want := []Fixture{
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
	encoded, err := Encode([]Option{WithImage("debian-12")})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if strings.Contains(encoded, "fixtures") {
		t.Errorf("encoded options should omit fixtures when empty: %s", encoded)
	}
}
