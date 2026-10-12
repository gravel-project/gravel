package objectstoretest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gravel-project/gravel/internal/objectstore"
)

// Download is what a GET of a signed URL served.
type Download struct {
	Body               []byte
	ContentType        string
	ContentDisposition string
}

// Options say how the suite runs against one Store.
type Options struct {
	// Prefix starts every key the suite uses, "objectstoretest" when empty. A run against a
	// shared bucket picks one no other run uses; the suite deletes every object it made.
	Prefix string
	// Download fetches a signed URL. nil skips the checks that need one: what a URL serves, its
	// Content-Disposition, and that a URL for a deleted object no longer serves it.
	Download func(ctx context.Context, url string) (Download, error)
	// LargeSize, when above zero, adds a step that stores an object of that many bytes, once
	// with the size given and once without. Set it above an implementation's multipart
	// threshold to prove the multipart path.
	LargeSize int64
}

// HTTPDownload fetches a signed URL with client (http.DefaultClient when nil). A 404 wraps
// objectstore.ErrNotFound.
func HTTPDownload(client *http.Client) func(ctx context.Context, url string) (Download, error) {
	if client == nil {
		client = http.DefaultClient
	}
	return func(ctx context.Context, url string) (Download, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return Download{}, err
		}
		resp, err := client.Do(req)
		if err != nil {
			return Download{}, err
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return Download{}, err
		}
		switch {
		case resp.StatusCode == http.StatusNotFound:
			return Download{}, fmt.Errorf("GET %s: %w", resp.Status, objectstore.ErrNotFound)
		case resp.StatusCode != http.StatusOK:
			return Download{}, fmt.Errorf("GET %s: %.200s", resp.Status, body)
		}
		return Download{
			Body:               body,
			ContentType:        resp.Header.Get("Content-Type"),
			ContentDisposition: resp.Header.Get("Content-Disposition"),
		}, nil
	}
}

// Run is the conformance suite. newStore is called once per step; it may return the same
// store each time.
func Run(t *testing.T, newStore func(t *testing.T) objectstore.Store, opts Options) {
	t.Helper()
	if opts.Prefix == "" {
		opts.Prefix = "objectstoretest"
	}
	steps := []struct {
		name string
		fn   func(*testing.T, *suite)
	}{
		{"PutAndStat", testPutAndStat},
		{"UnknownSize", testUnknownSize},
		{"Replace", testReplace},
		{"SizeMismatch", testSizeMismatch},
		{"Missing", testMissing},
		{"Delete", testDelete},
		{"SignedURL", testSignedURL},
		{"Invalid", testInvalid},
		{"Canceled", testCanceled},
	}
	if opts.LargeSize > 0 {
		steps = append(steps, struct {
			name string
			fn   func(*testing.T, *suite)
		}{"Large", testLarge})
	}
	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			s := &suite{t: t, store: newStore(t), opts: opts}
			step.fn(t, s)
		})
	}
}

// suite is one step's store and options.
type suite struct {
	t     *testing.T
	store objectstore.Store
	opts  Options
}

// key is a key unique to the step, so steps never see each other's objects.
func (s *suite) key(name string) string {
	return s.opts.Prefix + "/" + s.t.Name() + "/" + name
}

// put stores data under key, failing the test on an error, and deletes it when the test ends.
func (s *suite) put(key string, data []byte, size int64, contentType string) {
	s.t.Helper()
	s.cleanup(key)
	if err := s.store.Put(s.t.Context(), key, onlyReader(data), size, contentType); err != nil {
		s.t.Fatalf("Put(%q, %d bytes, size %d): %v", key, len(data), size, err)
	}
}

// cleanup deletes key when the test ends, whatever happened to it.
func (s *suite) cleanup(key string) {
	s.t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := s.store.Delete(ctx, key); err != nil {
			s.t.Errorf("cleanup: Delete(%q): %v", key, err)
		}
	})
}

// stat reads key, failing the test on an error.
func (s *suite) stat(key string) objectstore.Object {
	s.t.Helper()
	o, err := s.store.Stat(s.t.Context(), key)
	if err != nil {
		s.t.Fatalf("Stat(%q): %v", key, err)
	}
	return o
}

// wantMissing checks that key holds nothing.
func (s *suite) wantMissing(key string) {
	s.t.Helper()
	if o, err := s.store.Stat(s.t.Context(), key); !errors.Is(err, objectstore.ErrNotFound) {
		s.t.Fatalf("Stat(%q) = %+v, %v; want ErrNotFound", key, o, err)
	}
}

// wantObject checks Stat's answer for an object just stored, and what a signed URL serves.
func (s *suite) wantObject(key string, data []byte, contentType string, start time.Time) {
	s.t.Helper()
	o := s.stat(key)
	if o.Key != key || o.Size != int64(len(data)) || o.ContentType != contentType {
		s.t.Errorf("Stat(%q) = %+v; want key %q, size %d, content type %q", key, o, key, len(data), contentType)
	}
	// Last-Modified has a precision of a second, and a live service's clock is not ours.
	if o.LastModified.Before(start.Add(-5*time.Minute)) || o.LastModified.After(time.Now().Add(5*time.Minute)) {
		s.t.Errorf("Stat(%q).LastModified = %s; want about %s", key, o.LastModified, start)
	}
	if s.opts.Download == nil {
		return
	}
	d := s.download(key, "")
	if !bytes.Equal(d.Body, data) {
		s.t.Errorf("the signed URL for %q served %d bytes that differ from the %d stored", key, len(d.Body), len(data))
	}
	if d.ContentType != contentType {
		s.t.Errorf("the signed URL for %q served content type %q, want %q", key, d.ContentType, contentType)
	}
}

// download fetches key through a signed URL, failing the test on an error.
func (s *suite) download(key, filename string) Download {
	s.t.Helper()
	u, err := s.store.SignedURL(s.t.Context(), key, time.Minute, filename)
	if err != nil {
		s.t.Fatalf("SignedURL(%q): %v", key, err)
	}
	d, err := s.opts.Download(s.t.Context(), u)
	if err != nil {
		s.t.Fatalf("downloading %q from its signed URL: %v", key, err)
	}
	return d
}

func testPutAndStat(t *testing.T, s *suite) {
	key := s.key("match-1.dem")
	data := payload(64<<10, 1)
	start := time.Now()
	s.put(key, data, int64(len(data)), "application/octet-stream")
	s.wantObject(key, data, "application/octet-stream", start)

	// An empty content type is stored as the default, and an empty object is an object.
	empty := s.key("empty")
	s.put(empty, nil, 0, "")
	s.wantObject(empty, nil, objectstore.DefaultContentType, start)
}

func testUnknownSize(t *testing.T, s *suite) {
	key := s.key("unknown.dem")
	data := payload(100<<10, 2)
	start := time.Now()
	s.put(key, data, -1, "application/octet-stream")
	s.wantObject(key, data, "application/octet-stream", start)
}

func testReplace(t *testing.T, s *suite) {
	key := s.key("replaced.txt")
	s.put(key, payload(4<<10, 3), -1, "application/octet-stream")
	data := []byte("the second version\n")
	start := time.Now()
	s.put(key, data, int64(len(data)), "text/plain; charset=utf-8")
	s.wantObject(key, data, "text/plain; charset=utf-8", start)
}

func testSizeMismatch(t *testing.T, s *suite) {
	data := payload(10<<10, 4)
	for _, tc := range []struct {
		name string
		size int64
	}{
		{"short", int64(len(data)) + 1},
		{"long", int64(len(data)) - 1},
	} {
		key := s.key(tc.name)
		s.cleanup(key)
		if err := s.store.Put(t.Context(), key, onlyReader(data), tc.size, ""); err == nil {
			t.Errorf("Put(%q) of %d bytes with size %d: no error", key, len(data), tc.size)
		}
		s.wantMissing(key)
	}
}

func testMissing(t *testing.T, s *suite) {
	key := s.key("never-stored")
	s.wantMissing(key)
	if err := s.store.Delete(t.Context(), key); err != nil {
		t.Errorf("Delete(%q) of a missing key: %v", key, err)
	}
	// A signed URL for a missing object is still a URL: signing does not look.
	u, err := s.store.SignedURL(t.Context(), key, time.Minute, "")
	if err != nil || u == "" {
		t.Fatalf("SignedURL(%q) of a missing key = %q, %v", key, u, err)
	}
	if s.opts.Download != nil {
		if _, err := s.opts.Download(t.Context(), u); !errors.Is(err, objectstore.ErrNotFound) {
			t.Errorf("downloading a missing object: %v, want ErrNotFound", err)
		}
	}
}

func testDelete(t *testing.T, s *suite) {
	key := s.key("deleted.dem")
	s.put(key, payload(1<<10, 5), -1, "")
	u, err := s.store.SignedURL(t.Context(), key, time.Minute, "")
	if err != nil {
		t.Fatalf("SignedURL(%q): %v", key, err)
	}
	if err := s.store.Delete(t.Context(), key); err != nil {
		t.Fatalf("Delete(%q): %v", key, err)
	}
	s.wantMissing(key)
	if err := s.store.Delete(t.Context(), key); err != nil {
		t.Errorf("Delete(%q) again: %v", key, err)
	}
	if s.opts.Download != nil {
		if _, err := s.opts.Download(t.Context(), u); !errors.Is(err, objectstore.ErrNotFound) {
			t.Errorf("a signed URL made before the delete: %v, want ErrNotFound", err)
		}
	}
}

func testSignedURL(t *testing.T, s *suite) {
	key := s.key("2026-10-12_de_mirage.dem")
	data := payload(8<<10, 6)
	s.put(key, data, int64(len(data)), "")

	u1, err := s.store.SignedURL(t.Context(), key, time.Hour, "")
	if err != nil {
		t.Fatalf("SignedURL(%q): %v", key, err)
	}
	if u2, err := s.store.SignedURL(t.Context(), key, objectstore.MaxSignedURLTTL, "other.dem"); err != nil || u2 == u1 {
		t.Errorf("SignedURL with another lifetime and filename = %q, %v; want a URL that differs from %q", u2, err, u1)
	}
	if s.opts.Download == nil {
		return
	}
	for _, tc := range []struct{ filename, want string }{
		// The key's last segment by default.
		{"", `attachment; filename=2026-10-12_de_mirage.dem`},
		{"HTG Season 02 - Mirage.dem", `attachment; filename="HTG Season 02 - Mirage.dem"`},
		{"mirage-ünïcode.dem", `attachment; filename*=utf-8''mirage-%C3%BCn%C3%AFcode.dem`},
	} {
		d := s.download(key, tc.filename)
		if d.ContentDisposition != tc.want {
			t.Errorf("filename %q: Content-Disposition %q, want %q", tc.filename, d.ContentDisposition, tc.want)
		}
		if !bytes.Equal(d.Body, data) {
			t.Errorf("filename %q: the signed URL served %d bytes that differ from the %d stored", tc.filename, len(d.Body), len(data))
		}
	}
}

func testInvalid(t *testing.T, s *suite) {
	ctx := t.Context()
	for _, key := range []string{
		"",
		"/leading-slash",
		"a/../b",
		"..",
		"a/./b",
		"a//b",
		"trailing/",
		"tab\there",
		"nul\x00",
		"bad-utf8-\xff",
		strings.Repeat("k", objectstore.MaxKeyLength+1),
	} {
		if err := s.store.Put(ctx, key, onlyReader([]byte("x")), 1, ""); !errors.Is(err, objectstore.ErrInvalid) {
			t.Errorf("Put(%q): %v, want ErrInvalid", key, err)
		}
		if _, err := s.store.Stat(ctx, key); !errors.Is(err, objectstore.ErrInvalid) {
			t.Errorf("Stat(%q): %v, want ErrInvalid", key, err)
		}
		if err := s.store.Delete(ctx, key); !errors.Is(err, objectstore.ErrInvalid) {
			t.Errorf("Delete(%q): %v, want ErrInvalid", key, err)
		}
		if _, err := s.store.SignedURL(ctx, key, time.Minute, ""); !errors.Is(err, objectstore.ErrInvalid) {
			t.Errorf("SignedURL(%q): %v, want ErrInvalid", key, err)
		}
	}
	// The longest key is fine, and so are spaces and other printable characters.
	for _, key := range []string{
		s.key("x") + strings.Repeat("k", objectstore.MaxKeyLength-len(s.key("x"))),
		s.key("a name with spaces & ünïcode (1).dem"),
	} {
		if _, err := s.store.SignedURL(ctx, key, time.Minute, "x.dem"); err != nil {
			t.Errorf("SignedURL(%q): %v", key, err)
		}
	}

	key := s.key("valid")
	if err := s.store.Put(ctx, key, onlyReader(nil), -2, ""); !errors.Is(err, objectstore.ErrInvalid) {
		t.Errorf("Put with size -2: %v, want ErrInvalid", err)
	}
	if err := s.store.Put(ctx, key, onlyReader(nil), 0, "not a media type"); !errors.Is(err, objectstore.ErrInvalid) {
		t.Errorf("Put with a bad content type: %v, want ErrInvalid", err)
	}
	s.wantMissing(key)
	for _, ttl := range []time.Duration{0, -time.Minute, time.Millisecond, objectstore.MaxSignedURLTTL + time.Second} {
		if _, err := s.store.SignedURL(ctx, key, ttl, ""); !errors.Is(err, objectstore.ErrInvalid) {
			t.Errorf("SignedURL with ttl %s: %v, want ErrInvalid", ttl, err)
		}
	}
	for _, name := range []string{"dir/x.dem", `dir\x.dem`, "new\nline.dem"} {
		if _, err := s.store.SignedURL(ctx, key, time.Minute, name); !errors.Is(err, objectstore.ErrInvalid) {
			t.Errorf("SignedURL with filename %q: %v, want ErrInvalid", name, err)
		}
	}
}

func testCanceled(t *testing.T, s *suite) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	key := s.key("canceled")
	s.cleanup(key)
	if err := s.store.Put(ctx, key, onlyReader([]byte("x")), 1, ""); err == nil {
		t.Errorf("Put with a canceled context: no error")
	}
	s.wantMissing(key)
	if _, err := s.store.Stat(ctx, key); err == nil || errors.Is(err, objectstore.ErrNotFound) {
		t.Errorf("Stat with a canceled context: %v, want the context's error", err)
	}
}

func testLarge(t *testing.T, s *suite) {
	data := payload(int(s.opts.LargeSize), 7)
	for _, tc := range []struct {
		name string
		size int64
	}{
		{"known", int64(len(data))},
		{"unknown", -1},
	} {
		key := s.key(tc.name + ".dem")
		start := time.Now()
		s.put(key, data, tc.size, "application/octet-stream")
		s.wantObject(key, data, "application/octet-stream", start)
	}
	// A size that does not match fails on the multipart path too, though the parts before the
	// last are whole.
	for _, tc := range []struct {
		name string
		size int64
	}{
		{"short", int64(len(data)) + 1},
		{"long", int64(len(data)) - 1},
	} {
		key := s.key(tc.name + ".dem")
		s.cleanup(key)
		if err := s.store.Put(t.Context(), key, onlyReader(data), tc.size, ""); err == nil {
			t.Errorf("Put(%q) of %d bytes with size %d: no error", key, len(data), tc.size)
		}
		s.wantMissing(key)
	}
}

// payload is n bytes that differ with seed and along their length, so a part out of order or
// missing changes them.
func payload(n int, seed byte) []byte {
	b := make([]byte, n)
	x := uint32(seed) + 1
	for i := range b {
		x ^= x << 13
		x ^= x >> 17
		x ^= x << 5
		b[i] = byte(x)
	}
	return b
}

// onlyReader hides every method but Read, the way an upload from a request body arrives, so
// an implementation cannot seek or size the data itself.
func onlyReader(b []byte) io.Reader {
	return struct{ io.Reader }{bytes.NewReader(b)}
}
