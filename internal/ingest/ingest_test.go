package ingest

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/gravel-project/gravel/internal/store"
)

type fakeStore struct {
	mu      sync.Mutex
	batches []store.IngestBatch
	fail    bool
	cutoff  time.Time
}

func (f *fakeStore) InsertIngestBatch(_ context.Context, b store.IngestBatch) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return 0, errors.New("database away")
	}
	f.batches = append(f.batches, b)
	return int64(len(f.batches)), nil
}

func (f *fakeStore) DeleteIngestBatchesBefore(_ context.Context, cutoff time.Time) (int64, error) {
	f.cutoff = cutoff
	return 3, nil
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

type rig struct {
	h    *Handler
	keys *Keyring
	st   *fakeStore
	org  uuid.UUID
	now  time.Time
}

func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{keys: NewKeyring(), st: &fakeStore{}, org: uuid.New(), now: time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)}
	r.keys.now = func() time.Time { return r.now }
	r.keys.Update([]Feed{{Server: "wd-1", Source: "wardogs", Token: "token-one\n"}, {Server: "wd-2", Source: "wardogs", Token: "token-two"}})
	r.h = NewHandler(r.keys, r.st, r.org, prometheus.NewRegistry(), quiet())
	r.h.now = func() time.Time { return r.now }
	return r
}

func (r *rig) post(token, body, addr string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, Path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if addr != "" {
		req.RemoteAddr = addr
	}
	rec := httptest.NewRecorder()
	r.h.ServeHTTP(rec, req)
	return rec
}

func (r *rig) count(server, result string) float64 {
	src := "wardogs"
	if server == unknown {
		src = unknown
	}
	return testutil.ToFloat64(r.h.batches.WithLabelValues(server, src, result))
}

func TestStoresABatchAsReceived(t *testing.T) {
	r := newRig(t)
	body := `[{"eventId":"e1","kind":"kill"}]`
	rec := r.post("token-two", body, "")
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Fatalf("answer %d %q", rec.Code, rec.Body.String())
	}
	if len(r.st.batches) != 1 {
		t.Fatalf("stored %d", len(r.st.batches))
	}
	b := r.st.batches[0]
	if b.ServerID != "wd-2" || b.Source != "wardogs" || b.OrganizationID != r.org || string(b.Body) != body || !b.ReceivedAt.Equal(r.now) {
		t.Errorf("batch = %+v", b)
	}
	if r.count("wd-2", "stored") != 1 || testutil.ToFloat64(r.h.last.WithLabelValues("wd-2")) != float64(r.now.Unix()) {
		t.Error("stored metrics")
	}
}

func TestRefusesWithoutAValidToken(t *testing.T) {
	r := newRig(t)
	for _, tok := range []string{"", "token-three", "token-one-and-more"} {
		if rec := r.post(tok, "x", "198.51.100.7:1"); rec.Code != http.StatusUnauthorized || rec.Body.Len() != 0 {
			t.Errorf("token %q: %d %q", tok, rec.Code, rec.Body.String())
		}
	}
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, Path, strings.NewReader("x"))
	req.Header.Set("Authorization", "Basic token-one")
	rec := httptest.NewRecorder()
	r.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("a non-bearer header: %d", rec.Code)
	}
	if len(r.st.batches) != 0 || r.count(unknown, "unauthorized") != 4 {
		t.Errorf("stored %d, counted %v", len(r.st.batches), r.count(unknown, "unauthorized"))
	}
	// One address that keeps guessing is slowed down; another is not.
	got := 0
	for range 2 * failuresBurst {
		if rec := r.post("guess", "x", "198.51.100.9:1"); rec.Code == http.StatusTooManyRequests {
			got++
			if rec.Header().Get("Retry-After") == "" {
				t.Error("429 without Retry-After")
			}
		}
	}
	if got == 0 {
		t.Error("guessing was never limited")
	}
	if rec := r.post("token-one", "x", "198.51.100.9:1"); rec.Code != http.StatusOK {
		t.Errorf("a good token from a limited address: %d", rec.Code)
	}
}

func TestBodyLimits(t *testing.T) {
	r := newRig(t)
	if rec := r.post("token-one", strings.Repeat("x", MaxBody+1), ""); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("too large: %d", rec.Code)
	}
	if rec := r.post("token-one", strings.Repeat("x", MaxBody), ""); rec.Code != http.StatusOK {
		t.Errorf("at the limit: %d", rec.Code)
	}
	if rec := r.post("token-one", "", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("empty: %d", rec.Code)
	}
	if r.count("wd-1", "too_large") != 1 || r.count("wd-1", "bad_request") != 1 || len(r.st.batches) != 1 {
		t.Error("limit metrics or stores")
	}
}

func TestNotStoredIsNotOK(t *testing.T) {
	r := newRig(t)
	r.st.fail = true
	if rec := r.post("token-one", "x", ""); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("store down: %d", rec.Code)
	}
	if r.count("wd-1", "error") != 1 {
		t.Error("error not counted")
	}
}

func TestPerServerLimit(t *testing.T) {
	r := newRig(t)
	limited := 0
	for range perServerBurst + 5 {
		if rec := r.post("token-one", "x", ""); rec.Code == http.StatusTooManyRequests {
			limited++
		}
	}
	if limited == 0 {
		t.Error("a flooding server was never limited")
	}
	if rec := r.post("token-two", "x", ""); rec.Code != http.StatusOK {
		t.Errorf("another server is not limited by the first: %d", rec.Code)
	}
}

// A token file's second line, the previous token, is accepted for RotationGrace from when the
// hub first read it, across rereads of the file; the first line always.
func TestRotation(t *testing.T) {
	r := newRig(t)
	feeds := []Feed{{Server: "wd-1", Source: "wardogs", Token: "token-new\ntoken-one\n"}}
	r.keys.Update(feeds)
	for _, tok := range []string{"token-new", "token-one"} {
		if s, _, ok := r.keys.Match(tok); !ok || s != "wd-1" {
			t.Errorf("%s just after rotation: %q %v", tok, s, ok)
		}
	}
	if _, _, ok := r.keys.Match("token-two"); ok {
		t.Error("a server dropped from the feeds still matches")
	}
	r.now = r.now.Add(RotationGrace - time.Minute)
	r.keys.Update(feeds) // the job rereads the file: the grace keeps its start
	if _, _, ok := r.keys.Match("token-one"); !ok {
		t.Error("the previous token inside its grace")
	}
	r.now = r.now.Add(2 * time.Minute)
	if _, _, ok := r.keys.Match("token-one"); ok {
		t.Error("the previous token after its grace")
	}
	if _, _, ok := r.keys.Match("token-new"); !ok {
		t.Error("the current token after the grace")
	}
	r.keys.Update([]Feed{{Server: "wd-1", Token: "\n\n"}})
	if r.keys.Len() != 0 {
		t.Error("an empty token file is no feed")
	}
}

func TestPrune(t *testing.T) {
	st := &fakeStore{}
	now := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	var logs bytes.Buffer
	if err := Prune(context.Background(), st, now, slog.New(slog.NewTextHandler(&logs, nil))); err != nil {
		t.Fatal(err)
	}
	if !st.cutoff.Equal(now.Add(-30*24*time.Hour)) || !strings.Contains(logs.String(), "deleted=3") {
		t.Errorf("cutoff %v, log %s", st.cutoff, logs.String())
	}
}
