package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/gravel-project/gravel/internal/store"
	"github.com/gravel-project/gravel/internal/store/storetest"
)

func TestAuditLogIsAppendOnly(t *testing.T) {
	st := storetest.Open(t)
	storetest.Reset(t, st)
	ctx := context.Background()
	o := newOrg(t, st)
	t0 := time.Date(2026, 10, 10, 1, 0, 0, 0, time.UTC)
	srv := store.ManagedServer{ID: "wd-1", GameID: "wardogs", Name: "War Dogs #1", Driver: "wardogs", Location: "slc",
		Endpoint: "http://203.0.113.10:7789", CredentialFile: "/run/secrets/wd", PollInterval: 15 * time.Second, Trust: "official"}
	two := srv
	two.ID = "wd-2"
	if err := st.ApplyServers(ctx, o.ID, []store.Game{{ID: "wardogs", Bands: json.RawMessage(`[]`)}}, []store.ManagedServer{srv, two}, t0); err != nil {
		t.Fatal(err)
	}
	u, err := st.RegisterUser(ctx, store.User{ID: uuid.New(), OrganizationID: o.ID, DisplayName: "Owner"}, ident("discord", "1", t0))
	if err != nil {
		t.Fatal(err)
	}
	app := store.App{ID: uuid.New(), OrganizationID: o.ID, Name: "htg-bot", ClientID: "gravel_bot", SecretHash: []byte("x"), CreatedAt: t0}
	if err := st.CreateApp(ctx, app); err != nil {
		t.Fatal(err)
	}

	kick := store.AuditEntry{OrganizationID: o.ID, At: t0, ActorUserID: &u.ID, ServerID: "wd-1", Action: "kick",
		TargetProvider: "steam", TargetSubject: "76561190000000001", Reason: "spawn camping", RequestID: "req-1"}
	id1, err := st.InsertAuditEntry(ctx, kick)
	if err != nil {
		t.Fatal(err)
	}
	msg := store.AuditEntry{OrganizationID: o.ID, At: t0.Add(time.Second), ActorAppID: &app.ID, OnBehalfProvider: "discord", OnBehalfSubject: "42",
		ServerID: "wd-2", Action: "broadcast", Detail: json.RawMessage(`{"message": "restart in 5"}`)}
	id2, err := st.InsertAuditEntry(ctx, msg)
	if err != nil {
		t.Fatal(err)
	}
	if id2 <= id1 {
		t.Errorf("ids %d then %d", id1, id2)
	}

	// Exactly one actor; a server that exists.
	both := kick
	both.ActorAppID = &app.ID
	if _, err := st.InsertAuditEntry(ctx, both); err == nil {
		t.Error("an entry with two actors was stored")
	}
	none := kick
	none.ActorUserID = nil
	if _, err := st.InsertAuditEntry(ctx, none); err == nil {
		t.Error("an entry with no actor was stored")
	}
	ghost := kick
	ghost.ServerID = "nope"
	if _, err := st.InsertAuditEntry(ctx, ghost); err == nil {
		t.Error("an entry for an unknown server was stored")
	}

	// The outcome is written once.
	if err := st.FinishAuditEntry(ctx, o.ID, id1, "ok", t0.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := st.FinishAuditEntry(ctx, o.ID, id1, "error", t0.Add(3*time.Second)); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a second outcome: %v", err)
	}
	if err := st.FinishAuditEntry(ctx, uuid.New(), id2, "ok", t0); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("another organization's entry: %v", err)
	}

	// Nothing else changes, nothing is deleted, whoever asks.
	conn, err := pgx.Connect(ctx, storetest.DatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(ctx) })
	for _, q := range []string{
		`UPDATE audit_log SET reason = 'edited' WHERE id = ` + itoa(id1),
		`UPDATE audit_log SET outcome = 'error' WHERE id = ` + itoa(id1),
		`UPDATE audit_log SET outcome = 'ok', finished_at = now(), reason = 'sneaky' WHERE id = ` + itoa(id2),
		`DELETE FROM audit_log WHERE id = ` + itoa(id1),
		`TRUNCATE audit_log CASCADE`, // CASCADE: past the bans foreign key, to the trigger
	} {
		if _, err := conn.Exec(ctx, q); err == nil || !strings.Contains(err.Error(), "append-only") {
			t.Errorf("%s: %v", q, err)
		}
	}

	entries, err := st.ListAuditEntries(ctx, o.ID, store.AuditFilter{Limit: 10})
	if err != nil || len(entries) != 2 {
		t.Fatalf("entries = %+v, %v", entries, err)
	}
	b, a := entries[0], entries[1]
	if b.ID != id2 || b.ActorAppName != "htg-bot" || b.ActorUserID != nil || b.OnBehalfSubject != "42" || b.Outcome != "" || b.FinishedAt != nil {
		t.Errorf("newest = %+v", b)
	}
	var d map[string]string
	if err := json.Unmarshal(b.Detail, &d); err != nil || d["message"] != "restart in 5" {
		t.Errorf("detail = %s, %v", b.Detail, err)
	}
	if a.ID != id1 || a.Outcome != "ok" || a.FinishedAt == nil || !a.FinishedAt.Equal(t0.Add(2*time.Second)) || a.Reason != "spawn camping" ||
		*a.ActorUserID != u.ID || a.ActorAppName != "" || string(a.Detail) != "{}" || a.RequestID != "req-1" {
		t.Errorf("oldest = %+v", a)
	}

	page, err := st.ListAuditEntries(ctx, o.ID, store.AuditFilter{BeforeID: id2, Limit: 10})
	if err != nil || len(page) != 1 || page[0].ID != id1 {
		t.Errorf("before %d = %+v, %v", id2, page, err)
	}
	one, err := st.ListAuditEntries(ctx, o.ID, store.AuditFilter{ServerID: "wd-2", Limit: 10})
	if err != nil || len(one) != 1 || one[0].ID != id2 {
		t.Errorf("wd-2 = %+v, %v", one, err)
	}
	if other, err := st.ListAuditEntries(ctx, uuid.New(), store.AuditFilter{Limit: 10}); err != nil || len(other) != 0 {
		t.Errorf("another organization = %+v, %v", other, err)
	}

	// A server taken out of the manifest keeps its entries (it is marked removed, not deleted).
	if err := st.ApplyServers(ctx, o.ID, []store.Game{{ID: "wardogs", Bands: json.RawMessage(`[]`)}}, []store.ManagedServer{two}, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if kept, err := st.ListAuditEntries(ctx, o.ID, store.AuditFilter{ServerID: "wd-1", Limit: 10}); err != nil || len(kept) != 1 {
		t.Errorf("after removing wd-1 = %+v, %v", kept, err)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
