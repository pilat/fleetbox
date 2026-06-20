package fleetboxtest

import (
	"slices"
	"testing"

	imagecatalog "github.com/pilat/fleetbox/internal/image"
)

// TestMatrixImagesDefaultsToFullCatalog locks the load-bearing rule that an empty
// FLEETBOX_TEST_IMAGES means the FULL catalog, not "none" — CI's nightly lane sets
// the var to an empty string to request every image (ADR-0030). t.Setenv cannot
// truly unset a var, but unset and empty take the same branch, so the empty case
// covers both.
func TestMatrixImagesDefaultsToFullCatalog(t *testing.T) {
	full, err := imagecatalog.Aliases()
	if err != nil {
		t.Fatalf("Aliases: %v", err)
	}

	for _, val := range []string{"", "   "} {
		t.Setenv("FLEETBOX_TEST_IMAGES", val)
		got := MatrixImages(t)
		if !slices.Equal(got, full) {
			t.Errorf("MatrixImages with FLEETBOX_TEST_IMAGES=%q = %v, want full catalog %v", val, got, full)
		}
	}
}

// TestMatrixImagesSubset confirms a non-empty value selects exactly the listed
// images, in order, tolerating surrounding whitespace and stray empty fields.
func TestMatrixImagesSubset(t *testing.T) {
	t.Setenv("FLEETBOX_TEST_IMAGES", " debian-12 , ,ubuntu-26.04")
	got := MatrixImages(t)
	want := []string{"debian-12", "ubuntu-26.04"}
	if !slices.Equal(got, want) {
		t.Errorf("MatrixImages = %v, want %v", got, want)
	}
}
