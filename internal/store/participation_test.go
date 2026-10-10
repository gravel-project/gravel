package store_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gravel-project/gravel/internal/store"
	"github.com/gravel-project/gravel/internal/store/storetest"
)

// newApp stores a registered app, which a participation event must belong to.
func newApp(t *testing.T, st *store.Store, orgID uuid.UUID, name string) uuid.UUID {
	t.Helper()
	a := store.App{ID: uuid.New(), OrganizationID: orgID, Name: name, ClientID: "gravel_" + name, SecretHash: []byte("x"),
		Scopes: []string{"participation:write"}, CreatedAt: time.Now()}
	if err := st.CreateApp(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	return a.ID
}

// participation is an event of kind by a Steam subject, at, for seconds.
func participation(orgID, appID uuid.UUID, key, kind, subject string, at time.Time, seconds int) store.ParticipationEvent {
	return store.ParticipationEvent{OrganizationID: orgID, AppID: appID, Key: key, Kind: kind, Provider: "steam", Subject: subject,
		OccurredAt: at, DurationSeconds: seconds, ReceivedAt: at.Add(time.Minute), Source: "first_party", SchemaVersion: 1}
}

func record(t *testing.T, st *store.Store, events ...store.ParticipationEvent) []bool {
	t.Helper()
	got, err := st.RecordParticipation(context.Background(), events)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// An app's key is stored once: a repeat, in a later call or the same one, is a duplicate and
// leaves the first row as it was; another app's same key is its own.
func TestRecordParticipation(t *testing.T) {
	st := storetest.Open(t)
	storetest.Reset(t, st)
	ctx := context.Background()
	o := newOrg(t, st)
	seeder, muster := newApp(t, st, o.ID, "seeder"), newApp(t, st, o.ID, "muster")
	at := time.Date(2026, 10, 9, 20, 0, 0, 0, time.UTC)

	first := participation(o.ID, seeder, "s-1", "seeding", "1", at, 600)
	first.GameID, first.ServerID, first.Session = "wardogs", "wd-1", "session 42"
	if got := record(t, st, first, participation(o.ID, seeder, "s-2", "seeding", "2", at, 300)); !reflect.DeepEqual(got, []bool{true, true}) {
		t.Errorf("first call = %v", got)
	}
	changed := participation(o.ID, seeder, "s-1", "attendance", "9", at.Add(time.Hour), 60) // same key, other fields
	if got := record(t, st, changed, participation(o.ID, seeder, "s-3", "seeding", "3", at, 60), participation(o.ID, seeder, "s-3", "seeding", "3", at, 60),
		participation(o.ID, muster, "s-1", "attendance", "1", at, 0)); !reflect.DeepEqual(got, []bool{false, true, false, true}) {
		t.Errorf("second call = %v, want the repeat and the in-call repeat duplicates, the other app's key recorded", got)
	}
	rows, err := st.ParticipationEvents(ctx, o.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 4 {
		t.Fatalf("rows = %+v", rows)
	}
	var kept store.ParticipationEvent
	for _, r := range rows {
		if r.AppID == seeder && r.Key == "s-1" {
			kept = r
		}
	}
	want := first
	want.OccurredAt, want.ReceivedAt = kept.OccurredAt, kept.ReceivedAt
	if !reflect.DeepEqual(kept, want) || !kept.OccurredAt.Equal(at) {
		t.Errorf("the first s-1 changed: %+v", kept)
	}

	// The schema holds what the domain checks, should anything else write.
	for name, e := range map[string]store.ParticipationEvent{
		"a long duration": participation(o.ID, seeder, "bad-1", "seeding", "1", at, 86401),
		"another source": func() store.ParticipationEvent {
			e := participation(o.ID, seeder, "bad-2", "seeding", "1", at, 1)
			e.Source = "server_adapter"
			return e
		}(),
		"an unknown app": participation(o.ID, uuid.New(), "bad-3", "seeding", "1", at, 1),
	} {
		if _, err := st.RecordParticipation(ctx, []store.ParticipationEvent{e}); err == nil {
			t.Errorf("%s was stored", name)
		}
	}
	if got, err := st.RecordParticipation(ctx, nil); err != nil || got != nil {
		t.Errorf("no events: %v %v", got, err)
	}
}

// A participation board ranks one kind by time, then times, then identity, over raw rows and
// rolled-up periods, filtered by game and window; a profile sums an identity set per kind; and no
// participation ever reaches a combat board (ADR-0011's trust mapping).
func TestParticipationBoardAndTotals(t *testing.T) {
	st := storetest.Open(t)
	storetest.Reset(t, st)
	ctx := context.Background()
	o := newOrg(t, st)
	seedServers(t, st, o.ID)
	app := newApp(t, st, o.ID, "seeder")
	oct := time.Date(2026, 10, 9, 20, 0, 0, 0, time.UTC)
	sep := time.Date(2026, 9, 9, 20, 0, 0, 0, time.UTC)
	wd := func(e store.ParticipationEvent) store.ParticipationEvent { e.GameID = "wardogs"; return e }
	record(t, st,
		wd(participation(o.ID, app, "1", "seeding", "1", oct, 600)),
		wd(participation(o.ID, app, "2", "seeding", "1", sep, 600)),
		wd(participation(o.ID, app, "3", "seeding", "2", oct, 1200)),
		participation(o.ID, app, "4", "seeding", "3", oct, 300),
		participation(o.ID, app, "5", "seeding", "3", oct, 300),
		participation(o.ID, app, "6", "seeding", "4", oct, 600),
		participation(o.ID, app, "7", "attendance", "1", oct, 0),
		participation(o.ID, app, "8", "attendance", "1", sep, 0))
	board := func(q store.ParticipationBoardQuery) []store.ParticipationRow {
		t.Helper()
		q.OrganizationID, q.Limit = o.ID, 10
		rows, err := st.ParticipationBoard(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}
	// 1: 1200 s in 2; 2: 1200 s in 1 (fewer times ranks second); 3: 600 s in 2 beats 4: 600 s in 1.
	if got := board(store.ParticipationBoardQuery{Kind: "seeding"}); !reflect.DeepEqual(got, []store.ParticipationRow{
		{"steam", "1", 2, 1200}, {"steam", "2", 1, 1200}, {"steam", "3", 2, 600}, {"steam", "4", 1, 600}}) {
		t.Errorf("all time = %+v", got)
	}
	octFrom := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	if got := board(store.ParticipationBoardQuery{Kind: "seeding", GameID: "wardogs", From: octFrom, To: octFrom.AddDate(0, 1, 0)}); !reflect.DeepEqual(got,
		[]store.ParticipationRow{{"steam", "2", 1, 1200}, {"steam", "1", 1, 600}}) {
		t.Errorf("War Dogs in October = %+v", got)
	}
	if got := board(store.ParticipationBoardQuery{Kind: "attendance", Offset: 1}); len(got) != 0 {
		t.Errorf("a page past the end = %+v", got)
	}
	if got := board(store.ParticipationBoardQuery{Kind: "unknown"}); len(got) != 0 {
		t.Errorf("an unrecorded kind = %+v", got)
	}
	if _, err := st.ParticipationBoard(ctx, store.ParticipationBoardQuery{OrganizationID: o.ID, Limit: 10}); err == nil {
		t.Error("a board without a kind")
	}
	if kinds, err := st.ParticipationKinds(ctx, o.ID); err != nil || !reflect.DeepEqual(kinds, []string{"attendance", "seeding"}) {
		t.Errorf("kinds = %v, %v", kinds, err)
	}
	if kinds, err := st.ParticipationKinds(ctx, uuid.New()); err != nil || len(kinds) != 0 {
		t.Errorf("another organization's kinds = %v, %v", kinds, err)
	}

	keys := []store.PlayerKey{{Provider: "steam", Subject: "1"}, {Provider: "steam", Subject: "3"}}
	totals, err := st.ParticipationTotals(ctx, store.ParticipationTotalsQuery{OrganizationID: o.ID, Keys: keys})
	if err != nil || !reflect.DeepEqual(totals, []store.KindTotals{{"attendance", 2, 0}, {"seeding", 4, 1800}}) {
		t.Errorf("all-time totals = %+v, %v", totals, err)
	}
	totals, err = st.ParticipationTotals(ctx, store.ParticipationTotalsQuery{OrganizationID: o.ID, Keys: keys, GameID: "wardogs", From: octFrom, To: octFrom.AddDate(0, 1, 0)})
	if err != nil || !reflect.DeepEqual(totals, []store.KindTotals{{"seeding", 1, 600}}) {
		t.Errorf("War Dogs October totals = %+v, %v", totals, err)
	}
	if totals, err := st.ParticipationTotals(ctx, store.ParticipationTotalsQuery{OrganizationID: o.ID}); err != nil || totals != nil {
		t.Errorf("no identities = %+v, %v", totals, err)
	}

	// The trust mapping: participation feeds no combat board, of any scope, metric or window.
	for _, metric := range []string{store.BoardKills, store.BoardDeaths, store.BoardKD, store.BoardTime, store.BoardMatches} {
		for _, q := range []store.BoardQuery{{}, {GameID: "wardogs"}, {ServerID: "wd-1"}, {From: octFrom, To: octFrom.AddDate(0, 1, 0)}} {
			q.OrganizationID, q.Metric, q.Trusts, q.MinMatches, q.Limit = o.ID, metric, []string{"official", "community"}, 0, 10
			if rows, err := st.Board(ctx, q); err != nil || len(rows) != 0 {
				t.Errorf("combat board %+v counted participation: %+v %v", q, rows, err)
			}
		}
	}
	if row, err := st.PlayerTotals(ctx, store.PlayerTotalsQuery{OrganizationID: o.ID, Keys: keys, Trusts: []string{"official", "community"}}); err != nil || row != (store.BoardRow{}) {
		t.Errorf("combat profile totals counted participation: %+v %v", row, err)
	}
}

// The rollup moves old participation into monthly periods without changing a board over a season
// or all time, or a profile's totals, and a second run moves nothing (ADR-0012 §6, ADR-0014).
func TestRollUpParticipation(t *testing.T) {
	st := storetest.Open(t)
	storetest.Reset(t, st)
	ctx := context.Background()
	o := newOrg(t, st)
	app := newApp(t, st, o.ID, "seeder")
	chicago, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatal(err)
	}
	// 03:00Z on January 15th is the 14th in Chicago, before the season edge on the 15th.
	jan14 := time.Date(2025, 1, 15, 3, 0, 0, 0, time.UTC)
	jan20 := time.Date(2025, 1, 20, 20, 0, 0, 0, time.UTC)
	feb10 := time.Date(2025, 2, 10, 20, 0, 0, 0, time.UTC)
	oct26 := time.Date(2026, 10, 5, 20, 0, 0, 0, time.UTC)
	wd := func(e store.ParticipationEvent) store.ParticipationEvent { e.GameID = "wardogs"; return e }
	record(t, st,
		wd(participation(o.ID, app, "1", "seeding", "1", jan14, 600)),
		wd(participation(o.ID, app, "2", "seeding", "1", jan20, 300)),
		wd(participation(o.ID, app, "3", "seeding", "1", jan20.Add(time.Hour), 300)),
		participation(o.ID, app, "4", "seeding", "2", jan20, 900),
		participation(o.ID, app, "5", "attendance", "2", feb10, 0),
		wd(participation(o.ID, app, "6", "seeding", "2", feb10, 100)),
		participation(o.ID, app, "7", "seeding", "1", oct26, 60))

	seasonFrom := time.Date(2025, 1, 15, 0, 0, 0, 0, chicago)
	seasonTo := time.Date(2025, 3, 1, 0, 0, 0, 0, chicago)
	queries := map[string]store.ParticipationBoardQuery{
		"all time":          {Kind: "seeding"},
		"season":            {Kind: "seeding", From: seasonFrom, To: seasonTo},
		"season, War Dogs":  {Kind: "seeding", GameID: "wardogs", From: seasonFrom, To: seasonTo},
		"before the season": {Kind: "seeding", From: time.Date(2025, 1, 1, 0, 0, 0, 0, chicago), To: seasonFrom},
		"attendance":        {Kind: "attendance"},
	}
	board := func(q store.ParticipationBoardQuery) []store.ParticipationRow {
		t.Helper()
		q.OrganizationID, q.Timezone, q.Limit = o.ID, "America/Chicago", 10
		rows, err := st.ParticipationBoard(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}
	keys := []store.PlayerKey{{Provider: "steam", Subject: "1"}, {Provider: "steam", Subject: "2"}}
	totals := func() []store.KindTotals {
		t.Helper()
		got, err := st.ParticipationTotals(ctx, store.ParticipationTotalsQuery{OrganizationID: o.ID, Keys: keys, From: seasonFrom, To: seasonTo, Timezone: "America/Chicago"})
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	before := map[string][]store.ParticipationRow{}
	for name, q := range queries {
		before[name] = board(q)
	}
	if b := before["before the season"]; len(b) != 1 || b[0].Seconds != 600 {
		t.Fatalf("the fixture is not what the test needs: %+v", before)
	}
	totalsBefore := totals()
	if oldest, err := st.OldestRawParticipation(ctx, o.ID); err != nil || !oldest.Equal(jan14) {
		t.Errorf("oldest = %v, %v", oldest, err)
	}

	periods := []store.Period{
		{From: day2025(1, 1), To: day2025(1, 15)},
		{From: day2025(1, 15), To: day2025(2, 1)},
		{From: day2025(2, 1), To: day2025(3, 1)},
	}
	moved, err := st.RollUpParticipation(ctx, o.ID, "America/Chicago", periods)
	if err != nil || moved != 6 {
		t.Fatalf("moved = %d, %v; want 6", moved, err)
	}
	for name, q := range queries {
		if after := board(q); !reflect.DeepEqual(after, before[name]) {
			t.Errorf("%s changed:\nbefore %+v\nafter  %+v", name, before[name], after)
		}
	}
	if after := totals(); !reflect.DeepEqual(after, totalsBefore) {
		t.Errorf("profile totals changed: %+v, then %+v", totalsBefore, after)
	}
	if kinds, err := st.ParticipationKinds(ctx, o.ID); err != nil || !reflect.DeepEqual(kinds, []string{"attendance", "seeding"}) {
		t.Errorf("kinds after the rollup = %v, %v", kinds, err)
	}
	rows, err := st.ParticipationEvents(ctx, o.ID)
	if err != nil || len(rows) != 1 || rows[0].Key != "7" {
		t.Errorf("raw rows left = %+v, %v", rows, err)
	}
	if again, err := st.RollUpParticipation(ctx, o.ID, "America/Chicago", periods); err != nil || again != 0 {
		t.Errorf("a second rollup = %d, %v", again, err)
	}
	// A later event in a rolled period adds to its row.
	record(t, st, wd(participation(o.ID, app, "8", "seeding", "1", jan20.Add(2*time.Hour), 50)))
	if moved, err := st.RollUpParticipation(ctx, o.ID, "America/Chicago", periods); err != nil || moved != 1 {
		t.Errorf("a late event = %d, %v", moved, err)
	}
	if got := board(queries["season, War Dogs"]); len(got) != 2 || got[0] != (store.ParticipationRow{Provider: "steam", Subject: "1", Events: 3, Seconds: 650}) {
		t.Errorf("after the late event = %+v", got)
	}
	if moved, err := st.RollUpParticipation(ctx, o.ID, "UTC", nil); err != nil || moved != 0 {
		t.Errorf("no periods = %d, %v", moved, err)
	}
	if _, err := st.OldestRawParticipation(ctx, uuid.New()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("an organization without participation: %v", err)
	}
}

// An erasure re-keys participation, raw and rolled up, and keeps its numbers (ADR-0012 §7).
func TestEraseParticipation(t *testing.T) {
	st := storetest.Open(t)
	storetest.Reset(t, st)
	ctx := context.Background()
	o := newOrg(t, st)
	app := newApp(t, st, o.ID, "seeder")
	const victim = "76561190000000001"
	record(t, st, participation(o.ID, app, "1", "seeding", victim, time.Date(2025, 1, 20, 20, 0, 0, 0, time.UTC), 600),
		participation(o.ID, app, "2", "seeding", "2", time.Date(2025, 1, 20, 20, 0, 0, 0, time.UTC), 60))
	if _, err := st.RollUpParticipation(ctx, o.ID, "UTC", []store.Period{{From: day2025(1, 1), To: day2025(2, 1)}}); err != nil {
		t.Fatal(err)
	}
	record(t, st, participation(o.ID, app, "3", "seeding", victim, time.Date(2026, 10, 5, 20, 0, 0, 0, time.UTC), 300),
		participation(o.ID, app, "4", "attendance", victim, time.Date(2026, 10, 6, 20, 0, 0, 0, time.UTC), 0))

	const token = "0123456789abcdef0123456789abcdef"
	got, err := st.EraseIdentity(ctx, o.ID, store.PlayerKey{Provider: "steam", Subject: victim}, token)
	if err != nil {
		t.Fatal(err)
	}
	if got != (store.Erasure{ParticipationRows: 2, ParticipationMonthlyRows: 1}) {
		t.Errorf("erasure = %+v", got)
	}
	rows, err := st.ParticipationBoard(ctx, store.ParticipationBoardQuery{OrganizationID: o.ID, Kind: "seeding", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rows, []store.ParticipationRow{{store.ErasedProvider, token, 2, 900}, {"steam", "2", 1, 60}}) {
		t.Errorf("board = %+v", rows)
	}
	if left, err := st.ParticipationTotals(ctx, store.ParticipationTotalsQuery{OrganizationID: o.ID, Keys: []store.PlayerKey{{Provider: "steam", Subject: victim}}}); err != nil || len(left) != 0 {
		t.Errorf("the identity still has participation: %+v %v", left, err)
	}
}
