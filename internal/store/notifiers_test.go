package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/gravel-project/gravel/internal/store"
	"github.com/gravel-project/gravel/internal/store/storetest"
)

func TestNotifierCursors(t *testing.T) {
	st := storetest.Open(t)
	storetest.Reset(t, st)
	ctx := context.Background()
	o := newOrg(t, st)
	t0 := time.Date(2026, 10, 13, 22, 0, 0, 0, time.UTC)
	add := func(kind, server string) {
		t.Helper()
		if err := st.AppendEvent(ctx, store.Event{OrganizationID: o.ID, At: t0, Kind: kind, ServerID: server, Summary: kind, Source: "test"}); err != nil {
			t.Fatal(err)
		}
	}

	// On an empty log the cursor starts at 0, and following from 0 reads oldest first.
	c, err := st.NotifierCursor(ctx, o.ID, "phone", t0)
	if err != nil || c.AfterID != 0 || c.Name != "phone" || !c.CreatedAt.Equal(t0) {
		t.Fatalf("a new cursor: %+v %v", c, err)
	}
	add("server.unreachable", "a")
	add("server.recovered", "a")
	add("server.unreachable", "b")
	evs, head, err := st.ListEvents(ctx, o.ID, store.EventQuery{Follow: true, Limit: 2})
	if err != nil || head != 3 || len(evs) != 2 || evs[0].ID != 1 || evs[1].ID != 2 {
		t.Fatalf("following from 0: %+v head %d %v", evs, head, err)
	}
	if evs, _, _ := st.ListEvents(ctx, o.ID, store.EventQuery{Follow: true, Servers: []string{"b"}}); len(evs) != 1 || evs[0].ServerID != "b" {
		t.Errorf("by servers: %+v", evs)
	}

	// A notifier first seen now starts at the head; one that has a cursor keeps it.
	if c, _ := st.NotifierCursor(ctx, o.ID, "mail", t0); c.AfterID != 3 {
		t.Errorf("a new notifier's cursor: %d, want the head 3", c.AfterID)
	}
	if c, _ := st.NotifierCursor(ctx, o.ID, "phone", t0.Add(time.Hour)); c.AfterID != 0 || !c.CreatedAt.Equal(t0) {
		t.Errorf("an old cursor moved: %+v", c)
	}

	// A failure counts and waits; a delivery clears it; the cursor never moves back.
	next := t0.Add(10 * time.Second)
	for range 2 {
		if err := st.NotifierFailed(ctx, o.ID, "phone", "answered 503", t0, next); err != nil {
			t.Fatal(err)
		}
	}
	c, _ = st.NotifierCursor(ctx, o.ID, "phone", t0)
	if c.Failures != 2 || c.LastError != "answered 503" || !c.FailingSince.Equal(t0) || !c.NextAttemptAt.Equal(next) || c.AfterID != 0 {
		t.Fatalf("after failures: %+v", c)
	}
	sent := t0.Add(time.Minute)
	if err := st.AdvanceNotifier(ctx, o.ID, "phone", 2, &sent); err != nil {
		t.Fatal(err)
	}
	if err := st.AdvanceNotifier(ctx, o.ID, "phone", 1, nil); err != nil {
		t.Fatal(err)
	}
	c, _ = st.NotifierCursor(ctx, o.ID, "phone", t0)
	if c.AfterID != 2 || c.Failures != 0 || c.LastError != "" || c.FailingSince != nil || c.NextAttemptAt != nil || !c.LastSentAt.Equal(sent) {
		t.Errorf("after a delivery: %+v", c)
	}
	all, err := st.NotifierCursors(ctx, o.ID)
	if err != nil || len(all) != 2 || all[0].Name != "mail" || all[1].Name != "phone" {
		t.Errorf("every cursor: %+v %v", all, err)
	}
}
