package store_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/gravel-project/gravel/internal/store"
	"github.com/gravel-project/gravel/internal/store/storetest"
)

func TestStatsEvents(t *testing.T) {
	st := storetest.Open(t)
	storetest.Reset(t, st)
	ctx := context.Background()
	o := newOrg(t, st)
	t0 := time.Date(2026, 10, 21, 1, 0, 0, 0, time.UTC)
	srv := store.ManagedServer{ID: "wd-1", GameID: "wardogs", Name: "War Dogs #1", Driver: "wardogs", Location: "slc",
		Endpoint: "http://203.0.113.10:7789", CredentialFile: "/run/secrets/wd", PollInterval: 15 * time.Second, Trust: "official",
		Feed: json.RawMessage(`{"url": "https://ingest.example.com", "token_file": "/run/secrets/feed"}`)}
	if err := st.ApplyServers(ctx, o.ID, []store.Game{{ID: "wardogs"}, {ID: "cs2"}}, nil, []store.ManagedServer{srv}, t0); err != nil {
		t.Fatal(err)
	}
	batch := func(source string, at time.Time) int64 {
		t.Helper()
		id, err := st.InsertIngestBatch(ctx, store.IngestBatch{OrganizationID: o.ID, ServerID: "wd-1", Source: source, ReceivedAt: at, Body: []byte(`{}`)})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	b1, b2 := batch("wardogs", t0), batch("wardogs", t0.Add(2*time.Second))
	logBatch := batch("cs2", t0)

	pending, err := st.PendingIngestBatches(ctx, o.ID, "wardogs", 10)
	if err != nil || len(pending) != 2 || pending[0].ID != b1 || pending[1].ID != b2 || pending[0].ParserVersion != 0 {
		t.Fatalf("pending = %+v, %v", pending, err)
	}
	kill := func(key, killer, victim string) store.StatsEvent {
		return store.StatsEvent{OrganizationID: o.ID, ServerID: "wd-1", GameID: "wardogs", Source: "server_adapter", SourceKey: key,
			Kind: "kill", ObservedAt: t0, ReceivedAt: t0, Trust: "official", BatchID: b1, SchemaVersion: 1,
			Player: store.PlayerKey{Provider: "steam", Subject: killer}, Target: store.PlayerKey{Provider: "steam", Subject: victim},
			Extensions: json.RawMessage(`{"weapon":"AK"}`)}
	}
	state := store.StatsEvent{OrganizationID: o.ID, ServerID: "wd-1", GameID: "wardogs", Source: "server_adapter", SourceKey: "s1",
		Kind: "match_state", ObservedAt: t0, ReceivedAt: t0, Trust: "official", BatchID: b1, SchemaVersion: 1}
	stored, err := st.SaveParsedBatch(ctx, store.ParsedBatch{BatchID: b1, State: store.IngestParsed, ParserVersion: 1,
		Events: []store.StatsEvent{kill("k1", "76561190000000001", "76561190000000002"), state, kill("k1", "x", "y")}})
	if err != nil || len(stored) != 3 || !stored[0] || !stored[1] || stored[2] {
		t.Fatalf("stored = %v, %v (the second k1 is a duplicate)", stored, err)
	}
	// A failed batch keeps nothing but its state.
	if stored, err := st.SaveParsedBatch(ctx, store.ParsedBatch{BatchID: b2, State: store.IngestFailed, ParserVersion: 1}); err != nil || len(stored) != 0 {
		t.Fatalf("failed batch: %v, %v", stored, err)
	}
	if _, err := st.SaveParsedBatch(ctx, store.ParsedBatch{BatchID: b2, State: store.IngestPending}); err == nil {
		t.Error("a batch saved back to pending")
	}
	if pending, _ := st.PendingIngestBatches(ctx, o.ID, "wardogs", 10); len(pending) != 0 {
		t.Errorf("pending after parsing = %+v", pending)
	}
	if pending, _ := st.PendingIngestBatches(ctx, o.ID, "cs2", 10); len(pending) != 1 || pending[0].ID != logBatch {
		t.Errorf("another source's pending = %+v", pending)
	}
	events, err := st.StatsEvents(ctx, o.ID, "wd-1", "", t0, t0.Add(time.Hour), 10)
	if err != nil || len(events) != 2 {
		t.Fatalf("events = %+v, %v", events, err)
	}
	if e := events[0]; e.SourceKey != "k1" || e.Player.Subject != "76561190000000001" || e.Target.Subject != "76561190000000002" ||
		e.BatchID != b1 || e.Trust != "official" || string(e.Extensions) != `{"weapon": "AK"}` {
		t.Errorf("kill = %+v", e)
	}
	if e := events[1]; e.Player != (store.PlayerKey{}) || e.Target != (store.PlayerKey{}) || string(e.Extensions) != `{}` {
		t.Errorf("match state = %+v", e)
	}
	if only, _ := st.StatsEvents(ctx, o.ID, "wd-1", "match_state", t0, t0.Add(time.Hour), 10); len(only) != 1 {
		t.Errorf("by kind = %+v", only)
	}

	// A newer parser puts back what an older one read, failed or not; another source is untouched.
	if n, err := st.RequeueIngestBatches(ctx, o.ID, "wardogs", 2); err != nil || n != 2 {
		t.Errorf("requeued %d, %v", n, err)
	}
	if n, _ := st.RequeueIngestBatches(ctx, o.ID, "wardogs", 2); n != 0 {
		t.Errorf("requeued again %d", n)
	}
	if pending, _ := st.PendingIngestBatches(ctx, o.ID, "wardogs", 10); len(pending) != 2 || pending[0].ParserVersion != 1 {
		t.Errorf("pending after the requeue = %+v", pending)
	}
	// Read again, a batch's own events take the newer parser's fields and are not new; the same key
	// from another batch is a duplicate and changes nothing.
	state.Extensions = json.RawMessage(`{"match_id":"m1"}`)
	if stored, err := st.SaveParsedBatch(ctx, store.ParsedBatch{BatchID: b1, State: store.IngestParsed, ParserVersion: 2,
		Events: []store.StatsEvent{kill("k1", "76561190000000001", "76561190000000002"), state}}); err != nil || len(stored) != 2 || stored[0] || stored[1] {
		t.Fatalf("read again: %v, %v", stored, err)
	}
	other := state
	other.BatchID, other.Extensions = b2, json.RawMessage(`{"match_id":"m2"}`)
	if stored, err := st.SaveParsedBatch(ctx, store.ParsedBatch{BatchID: b2, State: store.IngestParsed, ParserVersion: 2,
		Events: []store.StatsEvent{other}}); err != nil || stored[0] {
		t.Fatalf("another batch's copy: %v, %v", stored, err)
	}
	if only, _ := st.StatsEvents(ctx, o.ID, "wd-1", "match_state", t0, t0.Add(time.Hour), 10); len(only) != 1 ||
		string(only[0].Extensions) != `{"match_id": "m1"}` || only[0].BatchID != b1 || only[0].Trust != "official" {
		t.Errorf("match state after the newer parser = %+v", only)
	}

	// A gap no batch carried.
	gap := store.StatsEvent{OrganizationID: o.ID, ServerID: "wd-1", GameID: "wardogs", Source: "server_adapter", SourceKey: "gap:silence:1",
		Kind: "gap", ObservedAt: t0, ReceivedAt: t0.Add(time.Hour), Trust: "official", SchemaVersion: 1, Extensions: json.RawMessage(`{"reason":"silence"}`)}
	if stored, err := st.RecordStatsEvents(ctx, []store.StatsEvent{gap, gap}); err != nil || !stored[0] || stored[1] {
		t.Errorf("gap stored %v, %v", stored, err)
	}
	if gaps, _ := st.StatsEvents(ctx, o.ID, "wd-1", "gap", t0, t0.Add(time.Hour), 10); len(gaps) != 1 || gaps[0].BatchID != 0 {
		t.Errorf("gaps = %+v", gaps)
	}

	// The last batch before a time, and whether anyone was on then.
	if at, ok, err := st.LastIngestBatch(ctx, o.ID, "wd-1", t0.Add(time.Second)); err != nil || !ok || !at.Equal(t0) {
		t.Errorf("last before t0+1s = %v %v %v", at, ok, err)
	}
	if _, ok, err := st.LastIngestBatch(ctx, o.ID, "wd-1", t0); err != nil || ok {
		t.Errorf("last before the first: %v %v", ok, err)
	}
	if err := st.RecordOccupancy(ctx, o.ID, "wd-1", t0.Add(-time.Minute), 1, 3); err != nil {
		t.Fatal(err)
	}
	for at, want := range map[time.Time]bool{t0.Add(-time.Minute): true, t0.Add(30 * time.Second): true, t0.Add(time.Minute): false, t0.Add(-2 * time.Minute): false} {
		if had, err := st.HadPlayers(ctx, o.ID, "wd-1", at); err != nil || had != want {
			t.Errorf("had players at %v = %v, %v", at, had, err)
		}
	}

	// Erasure re-keys an identity in either place, once per event, a death of one's own doing in both.
	if _, err := st.SaveParsedBatch(ctx, store.ParsedBatch{BatchID: b2, State: store.IngestParsed, ParserVersion: 2,
		Events: []store.StatsEvent{kill("k2", "76561190000000002", "76561190000000001"), kill("k3", "76561190000000002", "76561190000000002")}}); err != nil {
		t.Fatal(err)
	}
	got, err := st.EraseIdentity(ctx, o.ID, store.PlayerKey{Provider: "steam", Subject: "76561190000000002"}, "0123456789abcdef0123", store.Actor{Via: store.ViaCLI}, t0)
	if err != nil || got.EventRows != 3 {
		t.Fatalf("erasure = %+v, %v", got, err)
	}
	events, _ = st.StatsEvents(ctx, o.ID, "wd-1", "kill", t0, t0.Add(time.Hour), 10)
	erased := store.PlayerKey{Provider: store.ErasedProvider, Subject: "0123456789abcdef0123"}
	if len(events) != 3 || events[0].Target != erased || events[1].Player != erased || events[1].Target.Subject != "76561190000000001" ||
		events[2].Player != erased || events[2].Target != erased {
		t.Errorf("after erasure = %+v", events)
	}

	// Retention anonymises the identities of events observed before the cutoff, each its own token;
	// an event already anonymised in both places (k3) is not touched again.
	n, err := st.RetireStatsEvents(ctx, o.ID, t0.Add(time.Second))
	if err != nil || n != 2 {
		t.Fatalf("retired %d, %v", n, err)
	}
	events, _ = st.StatsEvents(ctx, o.ID, "wd-1", "kill", t0, t0.Add(time.Hour), 10)
	seen := map[string]bool{}
	for _, e := range events {
		for _, k := range []store.PlayerKey{e.Player, e.Target} {
			if k.Provider != store.ErasedProvider || k.Subject == "" || k.Subject == "76561190000000001" {
				t.Errorf("retired = %+v", e)
			}
			seen[k.Subject] = true
		}
	}
	if len(seen) != 3 { // the erasure's token (k1's victim, k2's killer, k3), and a new one each for k1's killer and k2's victim
		t.Errorf("tokens = %v", seen)
	}
	if n, _ := st.RetireStatsEvents(ctx, o.ID, t0.Add(time.Second)); n != 0 {
		t.Errorf("retired again: %d", n)
	}
}
