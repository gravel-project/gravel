package store_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gravel-project/gravel/internal/store"
	"github.com/gravel-project/gravel/internal/store/storetest"
)

// A rollup moves old rows into monthly totals without changing a board over a season or all time,
// keeps the match lists' counts, and does nothing the second time (ADR-0012 §6).
func TestRollUpKeepsTheBoards(t *testing.T) {
	st := storetest.Open(t)
	storetest.Reset(t, st)
	ctx := context.Background()
	o := newOrg(t, st)
	chicago, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatal(err)
	}
	seedServers(t, st, o.ID)

	// January 2025 holds a season edge on the 15th: one match just before it in Chicago (03:00Z on
	// the 15th is the 14th at 21:00), one after. February is inside the season; October 2026 stays
	// raw.
	jan14 := time.Date(2025, 1, 15, 3, 0, 0, 0, time.UTC)
	jan20 := time.Date(2025, 1, 20, 20, 0, 0, 0, time.UTC)
	feb10 := time.Date(2025, 2, 10, 20, 0, 0, 0, time.UTC)
	oct26 := time.Date(2026, 10, 5, 20, 0, 0, 0, time.UTC)
	playMatch(t, st, o.ID, "wd-1", jan14, "official", map[string][3]int{"1": {5, 2, 600}, "2": {1, 4, 600}})
	playMatch(t, st, o.ID, "wd-1", jan20, "official", map[string][3]int{"1": {3, 3, 900}, "3": {7, 1, 900}})
	playMatch(t, st, o.ID, "wd-2", jan20.Add(time.Hour), "community", map[string][3]int{"1": {9, 0, 300}})
	playMatch(t, st, o.ID, "wd-1", feb10, "official", map[string][3]int{"2": {2, 2, 1200}, "3": {4, 4, 1200}})
	playMatch(t, st, o.ID, "wd-1", oct26, "official", map[string][3]int{"1": {1, 1, 60}})

	seasonFrom := time.Date(2025, 1, 15, 0, 0, 0, 0, chicago)
	seasonTo := time.Date(2025, 3, 1, 0, 0, 0, 0, chicago)
	queries := map[string]store.BoardQuery{
		"all time, official":   {Trusts: []string{"official"}},
		"season, official":     {From: seasonFrom, To: seasonTo, Trusts: []string{"official"}},
		"season, wd-2":         {ServerID: "wd-2", From: seasonFrom, To: seasonTo, Trusts: []string{"official", "community"}},
		"january, both trusts": {From: time.Date(2025, 1, 1, 0, 0, 0, 0, chicago), To: seasonFrom, Trusts: []string{"official", "community"}},
		"two matches or more":  {Trusts: []string{"official"}, MinMatches: 2},
	}
	board := func(q store.BoardQuery) []store.BoardRow {
		q.OrganizationID, q.Timezone, q.Metric, q.Limit = o.ID, "America/Chicago", store.BoardKills, 50
		if q.MinMatches == 0 {
			q.MinMatches = 1
		}
		rows, err := st.Board(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}
	before := map[string][]store.BoardRow{}
	for name, q := range queries {
		before[name] = board(q)
	}
	// The Chicago edge: player 1's 03:00Z match on the 15th is the 14th's, before the season.
	if s := before["season, official"]; len(s) != 3 || s[1].Subject != "1" || s[1].Kills != 3 || before["january, both trusts"][0].Kills != 5 {
		t.Fatalf("the fixture is not what the test needs: %+v", before)
	}
	listBefore, err := st.ListMatches(ctx, o.ID, "wd-1", 0, 10)
	if err != nil {
		t.Fatal(err)
	}

	periods := []store.Period{
		{From: day2025(1, 1), To: day2025(1, 15)},
		{From: day2025(1, 15), To: day2025(2, 1)},
		{From: day2025(2, 1), To: day2025(3, 1)},
	}
	got, err := st.RollUp(ctx, o.ID, "America/Chicago", periods)
	if err != nil {
		t.Fatal(err)
	}
	if got.Matches != 4 || got.Rows != 7 {
		t.Errorf("rolled = %+v, want 4 matches and 7 rows", got)
	}
	for name, q := range queries {
		if after := board(q); !reflect.DeepEqual(after, before[name]) {
			t.Errorf("%s changed:\nbefore %+v\nafter  %+v", name, before[name], after)
		}
	}
	listAfter, err := st.ListMatches(ctx, o.ID, "wd-1", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	for i := range listBefore {
		if listBefore[i].Players != listAfter[i].Players || listBefore[i].Kills != listAfter[i].Kills {
			t.Errorf("match %d: %d players %d kills before, %d %d after", listBefore[i].ID,
				listBefore[i].Players, listBefore[i].Kills, listAfter[i].Players, listAfter[i].Kills)
		}
	}
	if oldest, err := st.OldestRawMatch(ctx, o.ID); err != nil || !oldest.Equal(oct26) {
		t.Errorf("oldest raw = %v, %v; want October 2026's", oldest, err)
	}
	if again, err := st.RollUp(ctx, o.ID, "America/Chicago", periods); err != nil || again != (store.Rollup{}) {
		t.Errorf("a second rollup = %+v, %v", again, err)
	}
}

// An erasure re-keys every stats table and the stored batches, deletes the pseudonym and keeps the
// numbers (ADR-0012 §7).
func TestEraseIdentity(t *testing.T) {
	st := storetest.Open(t)
	storetest.Reset(t, st)
	ctx := context.Background()
	o := newOrg(t, st)
	seedServers(t, st, o.ID)
	const victim = "76561190000000001"
	playMatch(t, st, o.ID, "wd-1", time.Date(2025, 1, 20, 20, 0, 0, 0, time.UTC), "official", map[string][3]int{victim: {5, 2, 600}, "2": {1, 1, 60}})
	if _, err := st.RollUp(ctx, o.ID, "UTC", []store.Period{{From: day2025(1, 1), To: day2025(2, 1)}}); err != nil {
		t.Fatal(err)
	}
	playMatch(t, st, o.ID, "wd-1", time.Date(2026, 10, 5, 20, 0, 0, 0, time.UTC), "official", map[string][3]int{victim: {3, 1, 120}})
	body := []byte(`[{"kind":"kill","killerSteamId":"` + victim + `","victimSteamId":"76561190000000002"}]`)
	if _, err := st.InsertIngestBatch(ctx, store.IngestBatch{OrganizationID: o.ID, ServerID: "wd-1", Source: "wardogs", ReceivedAt: time.Now(), Body: body}); err != nil {
		t.Fatal(err)
	}
	other := []byte(`[{"kind":"kill","killerSteamId":"76561190000000002"}]`)
	if _, err := st.InsertIngestBatch(ctx, store.IngestBatch{OrganizationID: o.ID, ServerID: "wd-1", Source: "wardogs", ReceivedAt: time.Now(), Body: other}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddPseudonym(ctx, o.ID, store.PlayerKey{Provider: "steam", Subject: victim}, "Brave Falcon", time.Now()); err != nil {
		t.Fatal(err)
	}

	const token = "0123456789abcdef0123456789abcdef"
	got, err := st.EraseIdentity(ctx, o.ID, store.PlayerKey{Provider: "steam", Subject: victim}, token)
	if err != nil {
		t.Fatal(err)
	}
	if got != (store.Erasure{MatchRows: 1, MonthlyRows: 1, IngestBatches: 1, Pseudonym: true}) {
		t.Errorf("erasure = %+v", got)
	}
	rows, err := st.Board(ctx, store.BoardQuery{OrganizationID: o.ID, Trusts: []string{"official"}, Metric: store.BoardKills, MinMatches: 1, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0] != (store.BoardRow{Provider: store.ErasedProvider, Subject: token, Kills: 8, Deaths: 3, SecondsOn: 720, Matches: 2}) {
		t.Errorf("board = %+v", rows)
	}
	batches, err := st.ListIngestBatches(ctx, o.ID, "wd-1", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range batches {
		if bytes.Contains(b.Body, []byte(victim)) {
			t.Errorf("batch %d still names the player: %s", b.ID, b.Body)
		}
		if sum := sha256.Sum256(b.Body); !bytes.Equal(sum[:], b.BodySHA256) {
			t.Errorf("batch %d: the hash no longer matches the body", b.ID)
		}
	}
	if !bytes.Equal(batches[1].Body, other) || !bytes.Contains(batches[0].Body, []byte(token)) {
		t.Errorf("batches = %s / %s", batches[0].Body, batches[1].Body)
	}
	names, err := st.Pseudonyms(ctx, o.ID, []store.PlayerKey{{Provider: "steam", Subject: victim}})
	if err != nil || len(names) != 0 {
		t.Errorf("pseudonym = %v, %v", names, err)
	}
	if _, err := st.EraseIdentity(ctx, o.ID, store.PlayerKey{Provider: "steam", Subject: victim}, "short"); err == nil {
		t.Error("a short token was accepted")
	}
	if _, err := st.EraseIdentity(ctx, o.ID, store.PlayerKey{Provider: store.ErasedProvider, Subject: token}, token+"x"); err == nil {
		t.Error("an erased identity was erased again")
	}
	if _, err := st.OldestRawMatch(ctx, uuid.New()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("an organization without matches: %v", err)
	}
}

func seedServers(t *testing.T, st *store.Store, orgID uuid.UUID) {
	t.Helper()
	srv := func(id, trust string) store.ManagedServer {
		return store.ManagedServer{ID: id, GameID: "wardogs", Name: id, Driver: "wardogs", Location: "slc", Endpoint: "http://203.0.113.10:7789",
			CredentialFile: "/run/secrets/wd", PollInterval: 15 * time.Second, Trust: trust}
	}
	if err := st.ApplyServers(context.Background(), orgID, []store.Game{{ID: "wardogs"}},
		[]store.ManagedServer{srv("wd-1", "official"), srv("wd-2", "community")}, time.Now()); err != nil {
		t.Fatal(err)
	}
}

// playMatch records one ended match: players by subject → kills, deaths, seconds on.
func playMatch(t *testing.T, st *store.Store, orgID uuid.UUID, server string, at time.Time, trust string, players map[string][3]int) {
	t.Helper()
	ctx := context.Background()
	m, err := st.StartMatch(ctx, store.Match{OrganizationID: orgID, ServerID: server, GameID: "wardogs", StartedAt: at, Map: "Ozeti"})
	if err != nil {
		t.Fatal(err)
	}
	var rows []store.MatchPlayer
	for subject, c := range players {
		rows = append(rows, store.MatchPlayer{MatchID: m.ID, Provider: "steam", Subject: subject, Kills: c[0], Deaths: c[1], SecondsOn: c[2],
			FirstSeen: at, LastSeen: at.Add(time.Duration(c[2]) * time.Second), Source: "server_adapter", Trust: trust})
	}
	if err := st.SaveMatchPlayers(ctx, rows); err != nil {
		t.Fatal(err)
	}
	if err := st.EndMatch(ctx, m.ID, at.Add(30*time.Minute)); err != nil {
		t.Fatal(err)
	}
}

// day2025 is a date in 2025, the year the rolled-up fixtures are in.
func day2025(m time.Month, d int) time.Time { return time.Date(2025, m, d, 0, 0, 0, 0, time.UTC) }

// A player's totals sum their identities over raw and rolled rows, filtered like a board; their
// recent matches list the raw ones, newest first, with the server's name.
func TestPlayerTotalsAndMatches(t *testing.T) {
	st := storetest.Open(t)
	storetest.Reset(t, st)
	ctx := context.Background()
	o := newOrg(t, st)
	seedServers(t, st, o.ID)
	old := time.Date(2025, 1, 20, 20, 0, 0, 0, time.UTC)
	playMatch(t, st, o.ID, "wd-1", old, "official", map[string][3]int{"1": {5, 2, 600}})
	if _, err := st.RollUp(ctx, o.ID, "UTC", []store.Period{{From: day2025(1, 1), To: day2025(2, 1)}}); err != nil {
		t.Fatal(err)
	}
	recent := time.Date(2026, 10, 5, 20, 0, 0, 0, time.UTC)
	playMatch(t, st, o.ID, "wd-1", recent, "official", map[string][3]int{"1": {3, 1, 300}, "2": {9, 9, 900}})
	playMatch(t, st, o.ID, "wd-2", recent.Add(time.Hour), "community", map[string][3]int{"alt": {7, 0, 120}})

	keys := []store.PlayerKey{{Provider: "steam", Subject: "1"}, {Provider: "steam", Subject: "alt"}}
	q := store.PlayerTotalsQuery{OrganizationID: o.ID, Keys: keys, Trusts: []string{"official"}}
	if got, err := st.PlayerTotals(ctx, q); err != nil || got != (store.BoardRow{Kills: 8, Deaths: 3, SecondsOn: 900, Matches: 2}) {
		t.Errorf("all time, official: %+v %v", got, err)
	}
	q.Trusts = []string{"official", "community"}
	if got, _ := st.PlayerTotals(ctx, q); got.Kills != 15 || got.Matches != 3 {
		t.Errorf("both trusts, both identities: %+v", got)
	}
	q.From, q.To = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	if got, _ := st.PlayerTotals(ctx, q); got.Kills != 10 || got.Matches != 2 {
		t.Errorf("October 2026: %+v", got)
	}
	q.From, q.To, q.GameID = time.Time{}, time.Time{}, "cs2"
	if got, _ := st.PlayerTotals(ctx, q); got != (store.BoardRow{}) {
		t.Errorf("another game: %+v", got)
	}
	if got, err := st.PlayerTotals(ctx, store.PlayerTotalsQuery{OrganizationID: o.ID, Trusts: []string{"official"}}); err != nil || got != (store.BoardRow{}) {
		t.Errorf("no identities: %+v %v", got, err)
	}

	ms, err := st.PlayerMatches(ctx, o.ID, keys, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 2 || ms[0].ServerID != "wd-2" || ms[0].ServerName != "wd-2" || ms[0].Kills != 7 || ms[1].ServerID != "wd-1" || ms[1].Kills != 3 || ms[1].Trust != "official" {
		t.Errorf("recent matches (the rolled one gone) = %+v", ms)
	}
	if ms, _ := st.PlayerMatches(ctx, o.ID, keys, 1); len(ms) != 1 {
		t.Errorf("limit: %d", len(ms))
	}
}
