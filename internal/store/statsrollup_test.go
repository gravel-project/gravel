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
	// A two-map series with the player on both maps, beside another player (ADR-0024).
	res := playResult(t, st, o.ID, 1, time.Date(2026, 10, 5, 20, 0, 0, 0, time.UTC), true, map[int][]string{1: {victim, "2"}, 2: {victim, "2"}})

	const token = "0123456789abcdef0123456789abcdef"
	got, err := st.EraseIdentity(ctx, o.ID, store.PlayerKey{Provider: "steam", Subject: victim}, token, store.Actor{Via: store.ViaCLI}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if got != (store.Erasure{MatchRows: 1, MonthlyRows: 1, ResultRows: 2, IngestBatches: 1, Pseudonym: true}) {
		t.Errorf("erasure = %+v", got)
	}
	r, err := st.GetMatchResult(ctx, o.ID, res)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range r.Players {
		erased := p.Provider == store.ErasedProvider && p.Subject == token && p.Name == "" && p.Kills == 5
		if (p.Subject == "2") == erased || p.Subject == victim {
			t.Errorf("result player %+v", p)
		}
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
	if _, err := st.EraseIdentity(ctx, o.ID, store.PlayerKey{Provider: "steam", Subject: victim}, "short", store.Actor{Via: store.ViaCLI}, time.Now()); err == nil {
		t.Error("a short token was accepted")
	}
	if _, err := st.EraseIdentity(ctx, o.ID, store.PlayerKey{Provider: store.ErasedProvider, Subject: token}, token+"x", store.Actor{Via: store.ViaCLI}, time.Now()); err == nil {
		t.Error("an erased identity was erased again")
	}
	if _, err := st.OldestRawMatch(ctx, uuid.New()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("an organization without matches: %v", err)
	}
}

// Retiring anonymises the players of every series that ended before the cutoff, each player with
// a token of their own in that series (the same on each map, a different one in the next series),
// keeps the scoreboard, leaves later and unfinished series alone, and does nothing the second time.
func TestRetireResults(t *testing.T) {
	st := storetest.Open(t)
	storetest.Reset(t, st)
	ctx := context.Background()
	o := newOrg(t, st)
	seedServers(t, st, o.ID)
	cutoff := time.Date(2025, 11, 1, 5, 0, 0, 0, time.UTC)
	old1 := playResult(t, st, o.ID, 1, cutoff.Add(-48*time.Hour), true, map[int][]string{1: {"1", "2"}, 2: {"1", "2"}})
	old2 := playResult(t, st, o.ID, 2, cutoff.Add(-24*time.Hour), true, map[int][]string{1: {"1"}})
	boundary := playResult(t, st, o.ID, 3, cutoff.Add(-time.Hour), true, map[int][]string{1: {"1"}}) // ends at the cutoff exactly
	running := playResult(t, st, o.ID, 4, cutoff.Add(-72*time.Hour), false, map[int][]string{1: {"1"}})
	recent := playResult(t, st, o.ID, 5, cutoff.Add(24*time.Hour), true, map[int][]string{1: {"1"}})

	// The periods must hold every retiring series' start, or nothing happens: their extensions
	// would be lost (ADR-0033).
	if got, err := st.RetireResults(ctx, o.ID, cutoff, "UTC", nil); err == nil || got != (store.Retired{}) {
		t.Fatalf("retired without periods = %+v, %v", got, err)
	}
	oldest, err := st.OldestRetirableResult(ctx, o.ID, cutoff)
	if err != nil || !oldest.Equal(cutoff.Add(-72*time.Hour)) && !oldest.Equal(cutoff.Add(-48*time.Hour)) {
		t.Fatalf("oldest retirable = %v, %v", oldest, err)
	}
	october := []store.Period{{From: time.Date(2025, 10, 1, 0, 0, 0, 0, time.UTC), To: time.Date(2025, 11, 1, 0, 0, 0, 0, time.UTC)}}
	got, err := st.RetireResults(ctx, o.ID, cutoff, "UTC", october)
	if err != nil {
		t.Fatal(err)
	}
	// Players 1 and 2, damage and mvps each: player 1's maps in both series add up in one row.
	if got != (store.Retired{Results: 2, Rows: 5, Rolled: 4}) {
		t.Errorf("retired = %+v, want 2 results, 5 rows and 4 rolled", got)
	}
	players := func(org, id uuid.UUID) []store.MatchResultPlayer {
		r, err := st.GetMatchResult(ctx, org, id)
		if err != nil {
			t.Fatal(err)
		}
		return r.Players
	}
	tokens := map[string]map[int]string{} // series → kills → token (kills tell the players apart)
	for name, id := range map[string]uuid.UUID{"old1": old1, "old2": old2} {
		tokens[name] = map[int]string{}
		for _, p := range players(o.ID, id) {
			if p.Provider != store.ErasedProvider || p.Name != "" || len(p.Subject) != 32 || p.Subject == "1" || p.Subject == "2" {
				t.Errorf("%s: a player was not anonymised: %+v", name, p)
			}
			if prev, ok := tokens[name][p.Kills]; ok && prev != p.Subject {
				t.Errorf("%s: one player has two tokens across its maps: %s, %s", name, prev, p.Subject)
			}
			tokens[name][p.Kills] = p.Subject
		}
	}
	if tokens["old1"][10] == tokens["old1"][20] || tokens["old1"][10] == tokens["old2"][10] {
		t.Errorf("tokens link players: %v", tokens)
	}
	for name, id := range map[string]uuid.UUID{"boundary": boundary, "running": running, "recent": recent} {
		for _, p := range players(o.ID, id) {
			if p.Provider != "steam" || p.Name == "" {
				t.Errorf("%s was retired: %+v", name, p)
			}
		}
	}
	if again, err := st.RetireResults(ctx, o.ID, cutoff, "UTC", october); err != nil || again != (store.Retired{}) {
		t.Errorf("a second run = %+v, %v", again, err)
	}
	if _, err := st.OldestRetirableResult(ctx, o.ID, cutoff); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("oldest retirable after retiring: %v", err)
	}
	if none, err := st.RetireResults(ctx, uuid.New(), cutoff.Add(1000*time.Hour), "UTC", nil); err != nil || none != (store.Retired{}) {
		t.Errorf("another organization = %+v, %v", none, err)
	}

	// A board's extension totals add the rolled-up periods to the raw results (ADR-0033): player 1
	// had damage 1000 on each of 6 maps (3 rolled, 3 raw: boundary, running, recent), player 2
	// 2000 on 2 rolled maps.
	p1, p2 := store.PlayerKey{Provider: "steam", Subject: "1"}, store.PlayerKey{Provider: "steam", Subject: "2"}
	q := store.BoardQuery{OrganizationID: o.ID, ServerID: "wd-1", Trusts: []string{"official", "community"}}
	ext, err := st.BoardExtensions(ctx, q, []store.PlayerKey{p1, p2})
	if err != nil {
		t.Fatal(err)
	}
	if ext[p1]["damage"] != (store.ExtensionTotal{Total: 6000, Maps: 6, Max: 1000}) || ext[p1]["mvps"] != (store.ExtensionTotal{Total: 6, Maps: 6, Max: 1}) ||
		ext[p2]["damage"] != (store.ExtensionTotal{Total: 4000, Maps: 2, Max: 2000}) {
		t.Errorf("all time = %+v", ext)
	}
	// A window from the cutoff holds only the recent series.
	q.From = cutoff
	if ext, err = st.BoardExtensions(ctx, q, []store.PlayerKey{p1, p2}); err != nil || ext[p1]["damage"] != (store.ExtensionTotal{Total: 1000, Maps: 1, Max: 1000}) || len(ext[p2]) != 0 {
		t.Errorf("from the cutoff = %+v, %v", ext, err)
	}
	// Community trust only: wd-1 is Official, so nothing.
	q.From, q.Trusts = time.Time{}, []string{"community"}
	if ext, err = st.BoardExtensions(ctx, q, []store.PlayerKey{p1}); err != nil || len(ext) != 0 {
		t.Errorf("community = %+v, %v", ext, err)
	}
	// Erasing player 2 re-keys their rolled-up rows too.
	erased, err := st.EraseIdentity(ctx, o.ID, p2, "0123456789abcdef0123456789abcdef", store.Actor{Via: store.ViaCLI}, time.Now())
	if err != nil || erased.ResultMonthlyRows != 2 {
		t.Errorf("erase = %+v, %v", erased, err)
	}
	q.Trusts = []string{"official"}
	if ext, err = st.BoardExtensions(ctx, q, []store.PlayerKey{p2}); err != nil || len(ext) != 0 {
		t.Errorf("erased player's extensions = %+v, %v", ext, err)
	}
}

// A series takes its server's trust when it starts (README principle 11), and a player's
// extensions come back as stored: an absent key stays absent, a 0 is a 0.
func TestMatchResultTrustAndExtensions(t *testing.T) {
	st := storetest.Open(t)
	storetest.Reset(t, st)
	ctx := context.Background()
	o := newOrg(t, st)
	seedServers(t, st, o.ID)
	for server, want := range map[string]string{"wd-1": "official", "wd-2": "community"} {
		r, err := st.StartMatchResult(ctx, store.MatchResult{ID: uuid.New(), OrganizationID: o.ID, ServerID: server, Source: "matchzy", ExternalID: 7, StartedAt: time.Now()})
		if err != nil || r.Trust != want {
			t.Errorf("%s: trust %q, %v", server, r.Trust, err)
		}
		if err := st.ReplaceMatchResultPlayers(ctx, r.ID, 1, []store.MatchResultPlayer{
			{Provider: "steam", Subject: "1", Team: "team1", Extensions: map[string]float64{"mvps": 0, "damage": 12.5}},
			{Provider: "steam", Subject: "2", Team: "team2"},
		}); err != nil {
			t.Fatal(err)
		}
		got, err := st.GetMatchResult(ctx, o.ID, r.ID)
		if err != nil || len(got.Players) != 2 {
			t.Fatalf("%s: %+v, %v", server, got, err)
		}
		for _, p := range got.Players {
			switch p.Subject {
			case "1":
				if v, ok := p.Extensions["mvps"]; !ok || v != 0 || p.Extensions["damage"] != 12.5 || len(p.Extensions) != 2 {
					t.Errorf("%s: player 1's extensions %v", server, p.Extensions)
				}
			case "2":
				if len(p.Extensions) != 0 {
					t.Errorf("%s: player 2's extensions %v", server, p.Extensions)
				}
			}
		}
	}
}

// playResult records a series on wd-1 that started at start and, when ended, ended an hour later:
// its maps by number, each with players by subject; a player's kills are 10 × their subject.
func playResult(t *testing.T, st *store.Store, orgID uuid.UUID, externalID int64, start time.Time, ended bool, maps map[int][]string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	r, err := st.StartMatchResult(ctx, store.MatchResult{ID: uuid.New(), OrganizationID: orgID, ServerID: "wd-1", Source: "matchzy", ExternalID: externalID,
		NumMaps: len(maps), StartedAt: start})
	if err != nil {
		t.Fatal(err)
	}
	for m, subjects := range maps {
		var ps []store.MatchResultPlayer
		for i, s := range subjects {
			kills := 10
			if s != "1" && s != "76561190000000001" {
				kills = 20
			}
			if s == "76561190000000001" {
				kills = 5
			}
			// Stat extensions (ADR-0033): damage 100 × kills and one MVP on each map.
			ps = append(ps, store.MatchResultPlayer{Provider: "steam", Subject: s, Name: "Player " + s, Team: []string{"team1", "team2"}[i%2], Kills: kills,
				Extensions: map[string]float64{"damage": float64(kills * 100), "mvps": 1}})
		}
		if err := st.ReplaceMatchResultPlayers(ctx, r.ID, m, ps); err != nil {
			t.Fatal(err)
		}
	}
	if ended {
		if err := st.EndMatchResult(ctx, r.ID, 1, 0, "team1", start.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	return r.ID
}

func seedServers(t *testing.T, st *store.Store, orgID uuid.UUID) {
	t.Helper()
	srv := func(id, trust string) store.ManagedServer {
		return store.ManagedServer{ID: id, GameID: "wardogs", Name: id, Driver: "wardogs", Location: "slc", Endpoint: "http://203.0.113.10:7789",
			CredentialFile: "/run/secrets/wd", PollInterval: 15 * time.Second, Trust: trust}
	}
	if err := st.ApplyServers(context.Background(), orgID, []store.Game{{ID: "wardogs"}}, nil,
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
