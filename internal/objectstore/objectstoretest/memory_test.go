package objectstoretest_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gravel-project/gravel/internal/objectstore"
	"github.com/gravel-project/gravel/internal/objectstore/objectstoretest"
)

func TestConformance(t *testing.T) {
	m := objectstoretest.NewMemory()
	t.Cleanup(func() {
		// The suite deletes everything it made.
		if keys := m.Keys(); len(keys) > 0 {
			t.Errorf("after the suite Memory holds %q", keys)
		}
	})
	objectstoretest.Run(t, func(*testing.T) objectstore.Store { return m }, objectstoretest.Options{
		Download:  m.Download,
		LargeSize: 17 << 20,
	})
}

func TestMemorySignedURLIsDeterministic(t *testing.T) {
	m := objectstoretest.NewMemory()
	ctx := context.Background()
	a, err := m.SignedURL(ctx, "demos/1/match 1.dem", time.Hour, "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := m.SignedURL(ctx, "demos/1/match 1.dem", time.Hour, "")
	if err != nil {
		t.Fatal(err)
	}
	const want = "memory://objectstoretest/demos/1/match%201.dem?response-content-disposition=attachment%3B+filename%3D%22match+1.dem%22&ttl=1h0m0s"
	if a != b || a != want {
		t.Errorf("SignedURL = %q then %q, want %q both times", a, b, want)
	}
}

func TestMemoryClockAndBytes(t *testing.T) {
	m := objectstoretest.NewMemory()
	at := time.Date(2026, 10, 12, 18, 30, 15, 500_000_000, time.UTC)
	m.SetClock(func() time.Time { return at })
	ctx := context.Background()
	if err := m.Put(ctx, "demos/a.dem", strings.NewReader("abc"), 3, ""); err != nil {
		t.Fatal(err)
	}
	o, err := m.Stat(ctx, "demos/a.dem")
	if err != nil {
		t.Fatal(err)
	}
	if want := at.Truncate(time.Second); !o.LastModified.Equal(want) {
		t.Errorf("LastModified = %s, want %s", o.LastModified, want)
	}
	if b, ok := m.Bytes("demos/a.dem"); !ok || string(b) != "abc" {
		t.Errorf("Bytes = %q, %v", b, ok)
	}
	if _, ok := m.Bytes("demos/b.dem"); ok {
		t.Error("Bytes of a missing key: ok")
	}
}
