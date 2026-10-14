package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gravel-project/gravel/internal/store"
	"github.com/gravel-project/gravel/internal/store/storetest"
)

// The operator's admin actions (#195) are events, each in its own transaction: an apply that
// changed something, an erasure (never whose), and staff logging a member out.
func TestAdminEvents(t *testing.T) {
	st := storetest.Open(t)
	storetest.Reset(t, st)
	ctx := context.Background()
	o := newOrg(t, st)
	t0 := time.Date(2026, 10, 14, 1, 0, 0, 0, time.UTC)
	jo, err := st.RegisterUser(ctx, store.User{ID: uuid.New(), OrganizationID: o.ID, DisplayName: "Jo"}, ident("discord", "1", t0))
	if err != nil {
		t.Fatal(err)
	}
	by := store.Actor{UserID: &jo.ID, Via: store.ViaAPI, RequestID: "req-1"}

	// An apply with no changes records nothing; one with changes records them, by who.
	if err := st.ApplyServersRecorded(ctx, o.ID, []store.Game{{ID: "cs2"}}, nil, []store.ManagedServer{cloudServer()}, t0, store.Applied{By: by}); err != nil {
		t.Fatal(err)
	}
	if err := st.ApplyServersRecorded(ctx, o.ID, []store.Game{{ID: "cs2"}}, nil, nil, t0.Add(time.Minute),
		store.Applied{By: by, Changes: []string{"server cs2-1 removed"}, Servers: []string{"cs2-1"}}); err != nil {
		t.Fatal(err)
	}

	// An erasure: the event says how much, never whose.
	victim := "76561190000000777"
	if _, err := st.EraseIdentity(ctx, o.ID, store.PlayerKey{Provider: "steam", Subject: victim}, strings.Repeat("t", 32), by, t0.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}

	// Staff log Jo out everywhere; an unknown member is not found and records nothing.
	if _, err := st.RevokeUserSessions(ctx, jo.ID, store.Actor{Via: store.ViaAPI}, t0.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RevokeUserSessions(ctx, uuid.New(), by, t0.Add(4*time.Minute)); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("an unknown member: %v", err)
	}

	evs, _, err := st.ListEvents(ctx, o.ID, store.EventQuery{Kinds: []string{"servers", "stats", "sessions"}, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, e := range evs {
		kinds = append(kinds, e.Kind)
		raw, _ := json.Marshal(e)
		if strings.Contains(string(raw), victim) {
			t.Errorf("%s carries the erased identity: %s", e.Kind, raw)
		}
	}
	if strings.Join(kinds, " ") != "sessions.revoked stats.erased servers.applied" {
		t.Fatalf("events: %v", kinds)
	}
	applied, erased, revoked := evs[2], evs[1], evs[0]
	if applied.ActorUserID == nil || *applied.ActorUserID != jo.ID || applied.RequestID != "req-1" || applied.Detail["via"] != "api" ||
		applied.Summary != "The servers manifest was applied: 1 changes" {
		t.Errorf("servers.applied: %+v", applied)
	}
	if erased.Detail["provider"] != "steam" || erased.SubjectUserID != nil {
		t.Errorf("stats.erased: %+v", erased)
	}
	if revoked.SubjectUserID == nil || *revoked.SubjectUserID != jo.ID || revoked.Summary != "Jo was logged out everywhere by staff" {
		t.Errorf("sessions.revoked: %+v", revoked)
	}
}
