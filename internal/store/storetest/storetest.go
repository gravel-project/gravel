// Package storetest gives integration tests a real, private Postgres database.
//
// Tests skip unless GRAVEL_TEST_DATABASE_URL points at a Postgres the test user may create
// databases in. Each test binary gets its own database (gravel_test_<package>), dropped and
// recreated on first use, so packages can run in parallel.
package storetest

import (
	"context"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gravel-project/gravel/internal/store"
)

// EnvURL names the admin connection URL tests derive their databases from.
const EnvURL = "GRAVEL_TEST_DATABASE_URL"

var (
	once    sync.Once
	dbURL   string
	setupOK bool
)

// DatabaseURL returns the URL of this test binary's empty, freshly created database, or skips.
func DatabaseURL(t testing.TB) string {
	t.Helper()
	base := os.Getenv(EnvURL)
	if base == "" {
		t.Skipf("%s not set; see docs/hub.md", EnvURL)
	}
	once.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		u, err := url.Parse(base)
		if err != nil {
			t.Fatalf("%s: %v", EnvURL, err)
		}
		name := "gravel_test_" + sanitize(strings.TrimSuffix(filepath.Base(os.Args[0]), ".test"))
		conn, err := pgx.Connect(ctx, base)
		if err != nil {
			t.Fatalf("connect to %s: %v", EnvURL, err)
		}
		defer func() { _ = conn.Close(ctx) }()
		ident := pgx.Identifier{name}.Sanitize()
		if _, err := conn.Exec(ctx, "DROP DATABASE IF EXISTS "+ident+" WITH (FORCE)"); err != nil {
			t.Fatalf("drop %s: %v", name, err)
		}
		if _, err := conn.Exec(ctx, "CREATE DATABASE "+ident); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		u.Path = "/" + name
		dbURL = u.String()
		setupOK = true
	})
	if !setupOK {
		t.Fatal("test database setup failed earlier")
	}
	return dbURL
}

// Open connects to the test database with migrations applied.
func Open(t testing.TB) *store.Store {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	st, err := store.Open(ctx, DatabaseURL(t), 4, 10*time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return st
}

// Reset empties the schema: every migration down, then up again.
func Reset(t testing.TB, st *store.Store) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := st.MigrateDownTo(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
}

var unsafe = regexp.MustCompile(`[^a-z0-9_]+`)

func sanitize(s string) string { return unsafe.ReplaceAllString(strings.ToLower(s), "_") }
