package opts

import (
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
