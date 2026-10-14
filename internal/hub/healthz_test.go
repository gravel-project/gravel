package hub

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"os"
	"testing"
)

// /healthz says what the hub is (gravel#214): the image its deployment names, the digest that
// image is pinned to, and the hash of the executable actually running.
func TestHealthzSaysWhatRuns(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	self := hex.EncodeToString(sum[:])
	const digest = "sha256:440148d47a9731b5526bbcea1db96eebc4b356e295ab8c8a9bb490856042b2a2"

	for _, c := range []struct {
		image string
		want  map[string]string
	}{
		{"", map[string]string{"status": "ok", "version": "v1", "binary_sha256": self}},
		{"ghcr.io/gravel-project/gravel-hub@" + digest,
			map[string]string{"status": "ok", "version": "v1", "binary_sha256": self, "image": "ghcr.io/gravel-project/gravel-hub@" + digest, "digest": digest}},
		{"ghcr.io/gravel-project/gravel-hub:0.9.0@" + digest,
			map[string]string{"status": "ok", "version": "v1", "binary_sha256": self, "image": "ghcr.io/gravel-project/gravel-hub:0.9.0@" + digest, "digest": digest}},
		// By tag only: the image, and no digest the hub would have to guess.
		{"localhost/gravel-hub:dev", map[string]string{"status": "ok", "version": "v1", "binary_sha256": self, "image": "localhost/gravel-hub:dev"}},
		{"x@sha256:short", map[string]string{"status": "ok", "version": "v1", "binary_sha256": self, "image": "x@sha256:short"}},
	} {
		h := &Hub{version: "v1", image: c.image}
		rec := httptest.NewRecorder()
		h.healthz(rec, httptest.NewRequestWithContext(t.Context(), "GET", "/healthz", nil))
		var got map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if len(got) != len(c.want) {
			t.Errorf("%q: %v, want %v", c.image, got, c.want)
			continue
		}
		for k, v := range c.want {
			if got[k] != v {
				t.Errorf("%q: %s = %q, want %q", c.image, k, got[k], v)
			}
		}
	}
}
