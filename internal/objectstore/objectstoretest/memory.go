// Package objectstoretest holds Memory, an in-memory objectstore.Store for tests, and Run, the
// conformance suite every Store passes: Memory and the S3 store against an emulator in CI, and
// the S3 store against a real bucket when GRAVEL_TEST_S3_ENDPOINT says where one is.
package objectstoretest

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gravel-project/gravel/internal/objectstore"
)

// Memory is an objectstore.Store that keeps objects in memory. Its signed URLs are fake but
// deterministic: the same key, lifetime and filename give the same URL, which Download resolves.
type Memory struct {
	mu      sync.Mutex
	objects map[string]memObject
	now     func() time.Time
}

type memObject struct {
	data         []byte
	contentType  string
	lastModified time.Time
}

var _ objectstore.Store = (*Memory)(nil)

// NewMemory is an empty store.
func NewMemory() *Memory {
	return &Memory{objects: map[string]memObject{}, now: time.Now}
}

// SetClock replaces the clock that stamps LastModified.
func (m *Memory) SetClock(now func() time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.now = now
}

// Put reads all of r before storing it, so a failed read stores nothing.
func (m *Memory) Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error {
	if err := objectstore.ValidateKey(key); err != nil {
		return err
	}
	if err := objectstore.ValidateSize(size); err != nil {
		return err
	}
	ct, err := objectstore.ContentType(contentType)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("objectstoretest: put %q: %w", key, err)
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("objectstoretest: put %q: %w", key, err)
	}
	if size >= 0 && int64(len(data)) != size {
		return fmt.Errorf("objectstoretest: put %q: the reader yielded %d bytes, not %d", key, len(data), size)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("objectstoretest: put %q: %w", key, err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	// S3 keeps Last-Modified to the second; so does Memory, so tests see the same values.
	m.objects[key] = memObject{data: data, contentType: ct, lastModified: m.now().UTC().Truncate(time.Second)}
	return nil
}

// SignedURL is memory://objectstoretest/<key> with the lifetime and the Content-Disposition in
// the query.
func (m *Memory) SignedURL(ctx context.Context, key string, ttl time.Duration, filename string) (string, error) {
	if err := objectstore.ValidateKey(key); err != nil {
		return "", err
	}
	if err := objectstore.ValidateTTL(ttl); err != nil {
		return "", err
	}
	cd, err := objectstore.ContentDisposition(key, filename)
	if err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	u := url.URL{
		Scheme:   "memory",
		Host:     "objectstoretest",
		Path:     "/" + key,
		RawQuery: url.Values{"response-content-disposition": {cd}, "ttl": {ttl.String()}}.Encode(),
	}
	return u.String(), nil
}

// Delete removes the object if there is one.
func (m *Memory) Delete(ctx context.Context, key string) error {
	if err := objectstore.ValidateKey(key); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objects, key)
	return nil
}

// Stat describes the object.
func (m *Memory) Stat(ctx context.Context, key string) (objectstore.Object, error) {
	if err := objectstore.ValidateKey(key); err != nil {
		return objectstore.Object{}, err
	}
	if err := ctx.Err(); err != nil {
		return objectstore.Object{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.objects[key]
	if !ok {
		return objectstore.Object{}, fmt.Errorf("objectstoretest: stat %q: %w", key, objectstore.ErrNotFound)
	}
	return objectstore.Object{Key: key, Size: int64(len(o.data)), ContentType: o.contentType, LastModified: o.lastModified}, nil
}

// Bytes is a copy of the object's content, and whether there is one.
func (m *Memory) Bytes(key string) ([]byte, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.objects[key]
	return bytes.Clone(o.data), ok
}

// Keys are the stored keys in order.
func (m *Memory) Keys() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	keys := make([]string, 0, len(m.objects))
	for k := range m.objects {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// Download is what a GET of one of Memory's signed URLs would serve. It ignores the lifetime.
func (m *Memory) Download(ctx context.Context, rawURL string) (Download, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return Download{}, err
	}
	if u.Scheme != "memory" || u.Host != "objectstoretest" {
		return Download{}, fmt.Errorf("objectstoretest: %q is not a Memory signed URL", rawURL)
	}
	key := strings.TrimPrefix(u.Path, "/")
	if err := ctx.Err(); err != nil {
		return Download{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.objects[key]
	if !ok {
		return Download{}, fmt.Errorf("objectstoretest: download %q: %w", key, objectstore.ErrNotFound)
	}
	return Download{
		Body:               bytes.Clone(o.data),
		ContentType:        o.contentType,
		ContentDisposition: u.Query().Get("response-content-disposition"),
	}, nil
}
