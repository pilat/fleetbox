package fetch

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// serve returns an httptest server handing out body, plus a counter of how many
// times it was hit (to prove cache no-ops).
func serve(t *testing.T, body []byte) (srv *httptest.Server, hits *int) {
	t.Helper()
	n := 0
	hits = &n
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n++
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv, hits
}

func TestEnsureDownloadsVerifiesAndCaches(t *testing.T) {
	body := []byte("pinned binary payload")
	srv, hits := serve(t, body)
	cacheDir := t.TempDir()

	path, err := Ensure(cacheDir, "blob", srv.URL, sha256Hex(body))
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if path != filepath.Join(cacheDir, "blob") {
		t.Errorf("path = %q, want %q", path, filepath.Join(cacheDir, "blob"))
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read cached file: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("cached content = %q, want %q", got, body)
	}

	// Second call is a cache hit: no extra download.
	if _, err := Ensure(cacheDir, "blob", srv.URL, sha256Hex(body)); err != nil {
		t.Fatalf("Ensure (cache hit): %v", err)
	}
	if *hits != 1 {
		t.Errorf("server hit %d times, want 1 (second call should be cached)", *hits)
	}
}

func TestEnsureWrongChecksumErrorsAndLeavesNothing(t *testing.T) {
	body := []byte("pinned binary payload")
	srv, _ := serve(t, body)
	cacheDir := t.TempDir()

	wrong := strings.Repeat("0", 64)
	_, err := Ensure(cacheDir, "blob", srv.URL, wrong)
	if err == nil {
		t.Fatal("Ensure(wrong sha) = nil error, want error")
	}
	if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Errorf("error %q does not name the mismatch cause", err)
	}

	// No partial file: neither the final name nor the .download temp survives.
	if _, statErr := os.Stat(filepath.Join(cacheDir, "blob")); !os.IsNotExist(statErr) {
		t.Errorf("cached file exists after checksum failure (stat err = %v), want not-exist", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(cacheDir, "blob.download")); !os.IsNotExist(statErr) {
		t.Errorf("temp .download survives after checksum failure (stat err = %v), want not-exist", statErr)
	}
}

func TestEnsureEmptyChecksumSkipsVerification(t *testing.T) {
	body := []byte("unpinned latest image")
	srv, _ := serve(t, body)
	cacheDir := t.TempDir()

	path, err := Ensure(cacheDir, "img.raw", srv.URL, "")
	if err != nil {
		t.Fatalf("Ensure(empty sha): %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read cached file: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("cached content = %q, want %q", got, body)
	}
}

func TestEnsureHTTPErrorLeavesNothing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	cacheDir := t.TempDir()

	if _, err := Ensure(cacheDir, "blob", srv.URL, sha256Hex([]byte("x"))); err == nil {
		t.Fatal("Ensure(404) = nil error, want error")
	}
	if _, statErr := os.Stat(filepath.Join(cacheDir, "blob.download")); !os.IsNotExist(statErr) {
		t.Errorf("temp .download survives after HTTP error (stat err = %v), want not-exist", statErr)
	}
}
