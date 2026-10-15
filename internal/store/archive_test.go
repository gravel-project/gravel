package store_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/url"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/gravel-project/gravel/internal/archive"
	"github.com/gravel-project/gravel/internal/demo"
	"github.com/gravel-project/gravel/internal/store"
	"github.com/gravel-project/gravel/internal/store/storetest"
)

// emptyDatabase is a second, empty database on the test server, for an import.
func emptyDatabase(t *testing.T, name string) (*store.Store, *pgx.Conn) {
	t.Helper()
	ctx := context.Background()
	base := storetest.DatabaseURL(t)
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close(ctx) })
	ident := pgx.Identifier{name}.Sanitize()
	if _, err := admin.Exec(ctx, "DROP DATABASE IF EXISTS "+ident+" WITH (FORCE)"); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+ident); err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(base)
	u.Path = "/" + name
	st, err := store.Open(ctx, u.String(), 4, 10*time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	conn, err := pgx.Connect(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(ctx) })
	return st, conn
}

// fingerprint is a table's row count and a digest of every row, in a stable order.
func fingerprint(t *testing.T, conn *pgx.Conn, table string) (int64, string) {
	t.Helper()
	var n int64
	var sum string
	if err := conn.QueryRow(context.Background(), `SELECT count(*), md5(coalesce(string_agg(x::text, E'\n' ORDER BY x::text), '')) FROM `+
		pgx.Identifier{table}.Sanitize()+` x`).Scan(&n, &sum); err != nil {
		t.Fatalf("%s: %v", table, err)
	}
	return n, sum
}

// The exit promise (gravel#213): a hub exported and loaded into an empty database is the same hub,
// table by table and row by row, but for its logins; the loaded one goes on (its sequences), and a
// second import is refused.
func TestExportImportRoundTrip(t *testing.T) {
	st := storetest.Open(t)
	storetest.Reset(t, st)
	ctx := context.Background()
	o := newOrg(t, st)
	seedServers(t, st, o.ID)
	now := time.Date(2026, 10, 15, 20, 0, 0, 0, time.UTC)
	if _, _, err := demo.Seed(ctx, st, o.ID, true, demo.Options{Days: 3, Members: 8, Seed: 7, Now: now}); err != nil {
		t.Fatal(err)
	}
	playResult(t, st, o.ID, 1, now.Add(-30*time.Hour), true, map[int][]string{1: {"1", "2"}})
	gen := now.Add(-2 * time.Hour)
	if err := st.SaveCloudBill(ctx, store.CloudBill{OrganizationID: o.ID, LocationID: "do-sfo3", TakenOn: now, Month: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		GeneratedAt: &gen, TakenAt: now, Lines: []store.CloudBillLine{{Name: "wd-1", Kind: "compute", USD: 12.5, ServerID: "wd-1"}}}); err != nil {
		t.Fatal(err)
	}
	src, err := pgx.Connect(ctx, storetest.DatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = src.Close(ctx) }()
	var someone uuid.UUID
	if err := src.QueryRow(ctx, `SELECT id FROM users LIMIT 1`).Scan(&someone); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddPseudonym(ctx, o.ID, store.PlayerKey{Provider: "steam", Subject: "76561190000000001"}, "Brave Falcon", now); err != nil {
		t.Fatal(err)
	}
	// A login: it doesn't travel.
	if err := st.CreateSession(ctx, store.Session{ID: uuid.New(), UserID: someone, TokenHash: []byte("hash-of-a-token-0123456789abcdef"), CSRFToken: "csrf",
		CreatedAt: now, ExpiresAt: now.Add(time.Hour), LastSeenAt: now}); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	m, err := archive.Write(ctx, st, &buf, "0.10.0-test", "HTG", []archive.File{{Name: "settings.yaml", Data: []byte("version: 1\n")}}, now)
	if err != nil {
		t.Fatal(err)
	}
	latest, err := st.SchemaVersion(ctx)
	if err != nil || m.Schema != latest || m.Format != archive.Format {
		t.Fatalf("manifest %+v, latest %d, %v", m, latest, err)
	}
	names := make([]string, 0, len(m.Tables))
	for _, tb := range m.Tables {
		names = append(names, tb.Name)
	}
	for _, left := range store.NotArchived {
		if slices.Contains(names, left) {
			t.Errorf("%s travels", left)
		}
	}
	// The history the test makes reaches the tables that matter, so the comparison means something.
	rows := map[string]int64{}
	for _, tb := range m.Tables {
		rows[tb.Name] = tb.Rows
	}
	for _, tb := range []string{"organizations", "users", "identities", "games", "managed_servers", "matches", "match_stats", "audit_log", "bans", "events",
		"server_occupancy", "pseudonyms", "match_results", "match_result_players", "cloud_bills", "cloud_bill_lines"} {
		if rows[tb] == 0 {
			t.Errorf("%s has no rows to carry", tb)
		}
	}

	dst, conn := emptyDatabase(t, "gravel_test_import_roundtrip")
	got, err := archive.Load(ctx, dst, bytes.NewReader(buf.Bytes()), latest)
	if err != nil {
		t.Fatal(err)
	}
	if got.Schema != m.Schema || len(got.Tables) != len(m.Tables) {
		t.Fatalf("loaded %+v", got)
	}
	for _, tb := range m.Tables {
		wn, ws := fingerprint(t, src, tb.Name)
		gn, gs := fingerprint(t, conn, tb.Name)
		if wn != gn || ws != gs {
			t.Errorf("%s: %d rows (%s) exported, %d (%s) loaded", tb.Name, wn, ws, gn, gs)
		}
	}
	if n, _ := fingerprint(t, conn, "sessions"); n != 0 {
		t.Errorf("%d sessions loaded", n)
	}
	// The loaded hub goes on: a new event takes the next id, and the keys hold (the owner's
	// organization, a match's server).
	if err := dst.AppendEvent(ctx, store.Event{OrganizationID: o.ID, At: now, Kind: "backup.recovered", Summary: "backups run again after the move"}); err != nil {
		t.Errorf("an event after the import: %v", err)
	}
	// A second import into the same database is refused, and changes nothing.
	if _, err := archive.Load(ctx, dst, bytes.NewReader(buf.Bytes()), latest); !errors.Is(err, store.ErrNotEmpty) {
		t.Errorf("a second import: %v", err)
	}
	// An archive from a newer gravel is refused before anything is touched.
	if _, err := archive.Load(ctx, dst, bytes.NewReader(buf.Bytes()), latest-1); !errors.Is(err, archive.ErrUnsupported) {
		t.Errorf("a newer archive: %v", err)
	}
}

// Every foreign key may be deferred, which is what lets an import load the tables in any order
// (migration 34): a new one must be DEFERRABLE too.
func TestEveryForeignKeyIsDeferrable(t *testing.T) {
	st := storetest.Open(t)
	storetest.Reset(t, st)
	conn, err := pgx.Connect(context.Background(), storetest.DatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	rows, err := conn.Query(context.Background(), `SELECT conrelid::regclass::text || '.' || conname FROM pg_constraint
		WHERE contype = 'f' AND connamespace = 'public'::regnamespace AND NOT condeferrable ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	bad, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	if len(bad) > 0 {
		t.Errorf("not DEFERRABLE (add it: CONSTRAINT … REFERENCES … DEFERRABLE INITIALLY IMMEDIATE): %v", bad)
	}
}
