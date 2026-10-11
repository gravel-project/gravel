package stats

import (
	"context"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/gravel-project/gravel/internal/org"
	"github.com/gravel-project/gravel/internal/servers"
	"github.com/gravel-project/gravel/internal/store"
)

// partStore keeps participation in memory with the store's duplicate rule: one row per app and key.
type partStore struct {
	rows []store.ParticipationEvent
	keys map[uuid.UUID]map[string]bool
	fail error
}

func (p *partStore) RecordParticipation(_ context.Context, events []store.ParticipationEvent) ([]bool, error) {
	if p.fail != nil {
		return nil, p.fail
	}
	if p.keys == nil {
		p.keys = map[uuid.UUID]map[string]bool{}
	}
	out := make([]bool, len(events))
	for i, e := range events {
		if p.keys[e.AppID] == nil {
			p.keys[e.AppID] = map[string]bool{}
		}
		if p.keys[e.AppID][e.Key] {
			continue
		}
		p.keys[e.AppID][e.Key], out[i] = true, true
		p.rows = append(p.rows, e)
	}
	return out, nil
}

// knownServers is the hub's servers by id, with their games.
type knownServers struct {
	games map[string]string
	err   error
	reads int
}

func (k *knownServers) Server(_ context.Context, id string) (servers.Server, error) {
	k.reads++
	if k.err != nil {
		return servers.Server{}, k.err
	}
	g, ok := k.games[id]
	if !ok {
		return servers.Server{}, store.ErrNotFound
	}
	return servers.Server{ID: id, Game: g}, nil
}

// The participation rig: Wednesday 2026-10-14 03:00 UTC, an organization in Chicago keeping raw
// rows 13 months (from 2025-10-01).
var partNow = time.Date(2026, 10, 14, 3, 0, 0, 0, time.UTC)

func newParticipation(t *testing.T) (*Participation, *partStore, *knownServers, *Changes) {
	t.Helper()
	st, srv, changes := &partStore{}, &knownServers{games: map[string]string{"wd-1": "wardogs"}}, &Changes{}
	p := NewParticipation(st, uuid.New(), srv, []string{"cs2", "wardogs"}, func(context.Context) (org.Stats, error) {
		return org.Stats{Timezone: "America/Chicago"}, nil
	}, changes, prometheus.NewRegistry())
	p.now = func() time.Time { return partNow }
	return p, st, srv, changes
}

// seeding is a valid event: ten minutes of seeding by a Steam identity an hour ago.
func seeding(key string) ParticipationEvent {
	return ParticipationEvent{Key: key, Kind: "seeding", Identity: &store.PlayerKey{Provider: "steam", Subject: "76561190000000001"},
		OccurredAt: partNow.Add(-time.Hour), DurationSeconds: 600}
}

// Every field is checked, and each problem is named by the event's index and the field.
func TestRecordParticipationValidation(t *testing.T) {
	cutoff := time.Date(2025, 10, 1, 5, 0, 0, 0, time.UTC) // midnight in Chicago
	cases := []struct {
		name  string
		edit  func(e *ParticipationEvent)
		field string // "" when the event is valid
	}{
		{"valid", func(*ParticipationEvent) {}, ""},
		{"no key", func(e *ParticipationEvent) { e.Key = "" }, "key: required"},
		{"a long key", func(e *ParticipationEvent) { e.Key = strings.Repeat("k", 129) }, "key: 129 characters, at most 128"},
		{"a key at the limit", func(e *ParticipationEvent) { e.Key = strings.Repeat("k", 128) }, ""},
		{"a key with a space and punctuation", func(e *ParticipationEvent) { e.Key = "seeding 2026-10-14/wd-1#3~" }, ""},
		{"a key with a newline", func(e *ParticipationEvent) { e.Key = "a\nb" }, "key: must be printable ASCII"},
		{"a key that is not ASCII", func(e *ParticipationEvent) { e.Key = "clé" }, "key: must be printable ASCII"},
		{"no kind", func(e *ParticipationEvent) { e.Kind = "" }, `kind: "" is not a lower-case letter`},
		{"an upper-case kind", func(e *ParticipationEvent) { e.Kind = "Seeding" }, `kind: "Seeding"`},
		{"a kind starting with a digit", func(e *ParticipationEvent) { e.Kind = "1st" }, `kind: "1st"`},
		{"a kind with a hyphen", func(e *ParticipationEvent) { e.Kind = "event-attendance" }, `kind: "event-attendance"`},
		{"a long kind", func(e *ParticipationEvent) { e.Kind = "a" + strings.Repeat("b", 32) }, "kind: "},
		{"a kind at the limit", func(e *ParticipationEvent) { e.Kind = "a" + strings.Repeat("b_9", 10) + "z" }, ""},
		{"no identity", func(e *ParticipationEvent) { e.Identity = nil }, "identity: required"},
		{"an unknown provider", func(e *ParticipationEvent) { e.Identity.Provider = "xbox" }, `identity.provider: "xbox" is not one of discord, steam`},
		{"the erased provider", func(e *ParticipationEvent) { e.Identity.Provider = store.ErasedProvider }, "identity.provider: "},
		{"a Discord identity", func(e *ParticipationEvent) { e.Identity.Provider, e.Identity.Subject = "discord", "1234" }, ""},
		{"no subject", func(e *ParticipationEvent) { e.Identity.Subject = "" }, "identity.subject: required"},
		{"a long subject", func(e *ParticipationEvent) { e.Identity.Subject = strings.Repeat("1", 65) }, "identity.subject: 65 characters, at most 64"},
		{"a subject with a control character", func(e *ParticipationEvent) { e.Identity.Subject = "1\x002" }, "identity.subject: must not hold control characters"},
		{"an unknown game", func(e *ParticipationEvent) { e.GameID = "quake" }, `game_id: "quake" is not a game this hub ships`},
		{"a game without a server", func(e *ParticipationEvent) { e.GameID = "cs2" }, ""},
		{"an unknown server", func(e *ParticipationEvent) { e.ServerID = "nope" }, `server_id: "nope" is not a server this hub knows`},
		{"a server and another game", func(e *ParticipationEvent) { e.ServerID, e.GameID = "wd-1", "cs2" }, `game_id: "cs2" is not the game of server "wd-1" (wardogs)`},
		{"a server and its game", func(e *ParticipationEvent) { e.ServerID, e.GameID = "wd-1", "wardogs" }, ""},
		{"a long session", func(e *ParticipationEvent) { e.Session = strings.Repeat("s", 129) }, "session: 129 characters, at most 128"},
		{"a session with a control character", func(e *ParticipationEvent) { e.Session = "muster\t42" }, "session: must not hold control characters"},
		{"a session at the limit", func(e *ParticipationEvent) { e.Session = strings.Repeat("é", 128) }, ""},
		{"no time", func(e *ParticipationEvent) { e.OccurredAt = time.Time{} }, "occurred_at: required"},
		{"six minutes ahead", func(e *ParticipationEvent) { e.OccurredAt = partNow.Add(6 * time.Minute) }, "occurred_at: 2026-10-14T03:06:00Z is more than 5m0s ahead"},
		{"four minutes ahead", func(e *ParticipationEvent) { e.OccurredAt = partNow.Add(4 * time.Minute) }, ""},
		{"before the retention window", func(e *ParticipationEvent) { e.OccurredAt = cutoff.Add(-time.Second) }, "occurred_at: 2025-10-01T04:59:59Z is before 2025-10-01, older than"},
		{"the window's first instant", func(e *ParticipationEvent) { e.OccurredAt = cutoff }, ""},
		{"a negative duration", func(e *ParticipationEvent) { e.DurationSeconds = -1 }, "duration_seconds: -1 is not between 0 and 86400"},
		{"a duration over a day", func(e *ParticipationEvent) { e.DurationSeconds = 86401 }, "duration_seconds: 86401 is not between 0 and 86400"},
		{"no duration", func(e *ParticipationEvent) { e.DurationSeconds = 0 }, ""},
		{"a whole day", func(e *ParticipationEvent) { e.DurationSeconds = 86400 }, ""},
		{"a future schema", func(e *ParticipationEvent) { e.SchemaVersion = 2 }, "schema_version: 2 is not 1"},
		{"schema 1", func(e *ParticipationEvent) { e.SchemaVersion = 1 }, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, st, _, _ := newParticipation(t)
			e := seeding("k-1")
			c.edit(&e)
			got, err := p.Record(context.Background(), uuid.New(), []ParticipationEvent{seeding("k-0"), e})
			if c.field == "" {
				if err != nil || len(got) != 2 || got[1].Outcome != OutcomeRecorded {
					t.Fatalf("a valid event: %v %v", got, err)
				}
				return
			}
			if !errors.Is(err, ErrInvalidParticipation) || !strings.Contains(err.Error(), "events[1]."+c.field) || strings.Contains(err.Error(), "events[0]") {
				t.Fatalf("got %v, want events[1].%s", err, c.field)
			}
			if got != nil || len(st.rows) != 0 {
				t.Errorf("a refused request stored %v (%v)", st.rows, got)
			}
		})
	}
}

// One request with several bad events names every problem, stores nothing, and counts every event
// as invalid, a bad kind under "invalid".
func TestRecordParticipationRefusesTheWholeRequest(t *testing.T) {
	p, st, _, changes := newParticipation(t)
	bad1, bad2 := seeding("b-1"), seeding("")
	bad1.Kind, bad1.DurationSeconds = "Not A Kind", -5
	bad2.Identity = nil
	_, err := p.Record(context.Background(), uuid.New(), []ParticipationEvent{seeding("ok-1"), bad1, bad2})
	if !errors.Is(err, ErrInvalidParticipation) {
		t.Fatalf("err = %v", err)
	}
	for _, want := range []string{"events[1].kind", "events[1].duration_seconds", "events[2].key: required", "events[2].identity: required"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%q does not name %s", err, want)
		}
	}
	if strings.Contains(err.Error(), "events[0]") || len(st.rows) != 0 || changes.Seen() != 0 {
		t.Errorf("rows %v, changes %d", st.rows, changes.Seen())
	}
	if n := testutil.ToFloat64(p.events.WithLabelValues("seeding", OutcomeInvalid)); n != 2 {
		t.Errorf("seeding invalid = %v, want 2", n)
	}
	if n := testutil.ToFloat64(p.events.WithLabelValues(OutcomeInvalid, OutcomeInvalid)); n != 1 {
		t.Errorf("a bad kind is counted under kind=invalid: %v", n)
	}

	for _, n := range []int{0, 101} {
		events := make([]ParticipationEvent, n)
		for i := range events {
			events[i] = seeding("many")
		}
		if _, err := p.Record(context.Background(), uuid.New(), events); !errors.Is(err, ErrInvalidParticipation) || !strings.Contains(err.Error(), "send 1 to 100") {
			t.Errorf("%d events: %v", n, err)
		}
	}
	hundred := make([]ParticipationEvent, 100)
	for i := range hundred {
		hundred[i] = seeding("h-" + string(rune('a'+i%26)) + string(rune('a'+i/26)))
	}
	if got, err := p.Record(context.Background(), uuid.New(), hundred); err != nil || len(got) != 100 {
		t.Errorf("100 events: %d, %v", len(got), err)
	}
	if _, err := p.Record(context.Background(), uuid.Nil, []ParticipationEvent{seeding("x")}); err == nil || errors.Is(err, ErrInvalidParticipation) {
		t.Errorf("no app: %v", err)
	}
}

// Results come back in order: a key the app used before is a duplicate, even with other fields;
// another app's key is its own. The rows carry the app, first_party, the schema version, the
// receipt time, and a server's game; the change counter moves only when something was stored.
func TestRecordParticipationResults(t *testing.T) {
	p, st, srv, changes := newParticipation(t)
	ctx := context.Background()
	seeder, muster := uuid.New(), uuid.New()
	onServer := seeding("s-1")
	onServer.ServerID, onServer.Session = "wd-1", "seed-window-42"
	got, err := p.Record(ctx, seeder, []ParticipationEvent{onServer, seeding("s-2"), seeding("s-2")})
	if err != nil {
		t.Fatal(err)
	}
	if want := []ParticipationResult{{"s-1", OutcomeRecorded}, {"s-2", OutcomeRecorded}, {"s-2", OutcomeDuplicate}}; !reflect.DeepEqual(got, want) {
		t.Errorf("first = %v", got)
	}
	r := st.rows[0]
	if r.AppID != seeder || r.GameID != "wardogs" || r.ServerID != "wd-1" || r.Session != "seed-window-42" || r.Source != SourceFirstParty ||
		r.SchemaVersion != ParticipationSchemaVersion || !r.ReceivedAt.Equal(partNow) || r.Provider != "steam" || r.Subject != "76561190000000001" || r.DurationSeconds != 600 {
		t.Errorf("row = %+v", r)
	}
	if changes.Seen() != 1 {
		t.Errorf("changes = %d", changes.Seen())
	}

	changed := seeding("s-1")
	changed.Kind, changed.DurationSeconds = "attendance", 0
	got, err = p.Record(ctx, seeder, []ParticipationEvent{changed})
	if err != nil || got[0].Outcome != OutcomeDuplicate || changes.Seen() != 1 {
		t.Errorf("a repeat with other fields: %v %v (changes %d)", got, err, changes.Seen())
	}
	if got, err = p.Record(ctx, muster, []ParticipationEvent{seeding("s-1")}); err != nil || got[0].Outcome != OutcomeRecorded || changes.Seen() != 2 {
		t.Errorf("another app's s-1: %v %v", got, err)
	}
	if n := testutil.ToFloat64(p.events.WithLabelValues("seeding", OutcomeRecorded)); n != 3 {
		t.Errorf("recorded = %v", n)
	}
	if n := testutil.ToFloat64(p.events.WithLabelValues("seeding", OutcomeDuplicate)); n != 1 {
		t.Errorf("seeding duplicates = %v", n)
	}
	if n := testutil.ToFloat64(p.events.WithLabelValues("attendance", OutcomeDuplicate)); n != 1 {
		t.Errorf("attendance duplicates = %v", n)
	}

	// A server named twice in one request is looked up once.
	srv.reads = 0
	a, b := seeding("t-1"), seeding("t-2")
	a.ServerID, b.ServerID = "wd-1", "wd-1"
	if _, err := p.Record(ctx, seeder, []ParticipationEvent{a, b}); err != nil || srv.reads != 1 {
		t.Errorf("server reads = %d, %v", srv.reads, err)
	}
	// The servers or the store failing is an error, not an invalid request.
	srv.err = errors.New("database away")
	if _, err := p.Record(ctx, seeder, []ParticipationEvent{a}); err == nil || errors.Is(err, ErrInvalidParticipation) {
		t.Errorf("a failed server read: %v", err)
	}
	srv.err, st.fail = nil, errors.New("database away")
	if _, err := p.Record(ctx, seeder, []ParticipationEvent{seeding("u-1")}); err == nil || errors.Is(err, ErrInvalidParticipation) {
		t.Errorf("a failed write: %v", err)
	}
}

// partBoardStore answers a participation board from canned rows and remembers the query.
type partBoardStore struct {
	*boardStore
	kinds  []string
	prows  []store.ParticipationRow
	pq     *store.ParticipationBoardQuery
	totals map[string][]store.KindTotals // by game ("" for every game)
	tq     []store.ParticipationTotalsQuery
}

func (p *partBoardStore) ParticipationKinds(context.Context, uuid.UUID) ([]string, error) {
	return p.kinds, nil
}

func (p *partBoardStore) ParticipationBoard(_ context.Context, q store.ParticipationBoardQuery) ([]store.ParticipationRow, error) {
	p.pq = &q
	rows := p.prows
	if len(rows) > q.Limit {
		rows = rows[:q.Limit]
	}
	return rows, nil
}

func (p *partBoardStore) ParticipationTotals(_ context.Context, q store.ParticipationTotalsQuery) ([]store.KindTotals, error) {
	p.tq = append(p.tq, q)
	return p.totals[q.GameID], nil
}

func (p *partBoardStore) PlayerTotals(context.Context, store.PlayerTotalsQuery) (store.BoardRow, error) {
	return store.BoardRow{Kills: 1}, nil
}

func (p *partBoardStore) PlayerMatches(context.Context, uuid.UUID, []store.PlayerKey, int) ([]store.PlayerMatch, error) {
	return nil, nil
}

func newParticipationBoards(set org.Stats) (*Boards, *partBoardStore) {
	bs := &boardStore{memStore: newMemStore(), members: map[store.PlayerKey]store.BoardMember{}}
	ps := &partBoardStore{boardStore: bs}
	b := NewBoards(ps, uuid.New(), NewNamer(ps, uuid.New(), key, quiet()), func(context.Context) (org.Stats, error) { return set, nil }, &bs.changes)
	b.now = func() time.Time { return partNow }
	return b, ps
}

// A participation board picks its kind, ranks a page under the combat boards' names and windows,
// and refuses a malformed kind.
func TestParticipationBoard(t *testing.T) {
	chicago, _ := time.LoadLocation("America/Chicago")
	set := org.Stats{Timezone: "America/Chicago", Seasons: []org.Season{{Name: "Season 02", Game: "wardogs", From: "2026-10-15", To: "2027-01-15"}}}
	b, ps := newParticipationBoards(set)
	ctx := context.Background()

	// Nothing recorded yet: no kind, no entries, and the store's board is not read.
	got, err := b.ParticipationBoard(ctx, ParticipationQuery{})
	if err != nil || got.Kind != "" || len(got.Kinds) != 0 || len(got.Entries) != 0 || ps.pq != nil {
		t.Fatalf("empty: %+v %v", got, err)
	}

	b, ps = newParticipationBoards(set)
	ps.kinds = []string{"attendance", "seeding"}
	ana, ben := store.PlayerKey{Provider: "discord", Subject: "1"}, store.PlayerKey{Provider: "steam", Subject: "2"}
	anaID := uuid.New()
	ps.members[ana] = store.BoardMember{UserID: anaID, DisplayName: "Ana", ShowName: true}
	ps.members[ben] = store.BoardMember{UserID: uuid.New(), DisplayName: "Ben"} // linked, not opted in
	ps.prows = []store.ParticipationRow{{Provider: "discord", Subject: "1", Events: 3, Seconds: 5400}, {Provider: "steam", Subject: "2", Events: 1, Seconds: 600},
		{Provider: "steam", Subject: "3", Events: math.MaxInt32 + 1, Seconds: -1}}

	got, err = b.ParticipationBoard(ctx, ParticipationQuery{PageSize: 2})
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != "attendance" || !reflect.DeepEqual(got.Kinds, ps.kinds) || ps.pq.Kind != "attendance" || ps.pq.Limit != 3 || !got.More || len(got.Entries) != 2 {
		t.Fatalf("the first kind, a page of 2: %+v (query %+v)", got, ps.pq)
	}
	if e := got.Entries[0]; e.Name != "Ana" || e.Pseudonymous || e.UserID != anaID || e.Rank != 1 || e.Events != 3 || e.Seconds != 5400 {
		t.Errorf("an opted-in member by name: %+v", e)
	}
	if e := got.Entries[1]; e.Name != Pseudonym(key, ben) || !e.Pseudonymous || e.UserID != uuid.Nil || e.Rank != 2 {
		t.Errorf("a member who did not opt in, under a pseudonym: %+v", e)
	}
	got, err = b.ParticipationBoard(ctx, ParticipationQuery{Kind: "seeding", Offset: 2})
	if err != nil || ps.pq.Offset != 2 || ps.pq.Kind != "seeding" || got.Entries[0].Rank != 3 || got.Entries[2].Events != math.MaxInt32 || got.Entries[2].Seconds != 0 {
		t.Errorf("the next page ranks from 3 (the fake ignores the offset), the 64-bit sums clamped: %+v %v", got.Entries, err)
	}

	// The windows are a combat board's: this week in Chicago; a game's season limits the game.
	if _, err := b.ParticipationBoard(ctx, ParticipationQuery{Kind: "seeding", Window: WindowWeek}); err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 10, 12, 0, 0, 0, 0, chicago); !ps.pq.From.Equal(want) || !ps.pq.To.Equal(want.AddDate(0, 0, 7)) || ps.pq.Timezone != "America/Chicago" || ps.pq.GameID != "" {
		t.Errorf("this week: %+v", ps.pq)
	}
	if _, err := b.ParticipationBoard(ctx, ParticipationQuery{Kind: "seeding", Window: WindowSeason, Season: "season 02"}); err != nil || ps.pq.GameID != "wardogs" {
		t.Errorf("a game's season: %+v %v", ps.pq, err)
	}
	if _, err := b.ParticipationBoard(ctx, ParticipationQuery{Kind: "seeding", GameID: "cs2"}); err != nil || ps.pq.GameID != "cs2" {
		t.Errorf("a game: %+v %v", ps.pq, err)
	}

	// A well-formed kind nobody recorded is an empty board, not an error, and reads no rows.
	ps.pq = nil
	if got, err := b.ParticipationBoard(ctx, ParticipationQuery{Kind: "streaming"}); err != nil || got.Kind != "streaming" || len(got.Entries) != 0 || ps.pq != nil {
		t.Errorf("an unrecorded kind: %+v %v", got, err)
	}
	for _, q := range []ParticipationQuery{
		{Kind: "Seeding"},
		{Kind: "seed-ing"},
		{Kind: "seeding", Window: "fortnight"},
		{Kind: "seeding", Window: WindowSeason, Season: "Season 03"},
		{Kind: "seeding", Window: WindowSeason, Season: "Season 02", GameID: "cs2"},
	} {
		if _, err := b.ParticipationBoard(ctx, q); !errors.Is(err, ErrInvalidQuery) {
			t.Errorf("%+v: %v", q, err)
		}
	}
}

// A participation board is cached like a combat board, and a member's name choice clears it.
func TestParticipationBoardCache(t *testing.T) {
	b, ps := newParticipationBoards(org.Stats{})
	ps.kinds = []string{"seeding"}
	ps.prows = []store.ParticipationRow{{Provider: "steam", Subject: "1", Events: 1, Seconds: 60}}
	now := partNow
	b.now = func() time.Time { return now }
	ctx := context.Background()
	read := func() bool {
		ps.pq = nil
		if _, err := b.ParticipationBoard(ctx, ParticipationQuery{Kind: "seeding"}); err != nil {
			t.Fatal(err)
		}
		return ps.pq != nil
	}
	if !read() || read() {
		t.Error("the second read went to the store")
	}
	now = now.Add(10 * time.Minute)
	if read() {
		t.Error("an idle hub re-read an unchanged board")
	}
	ps.changes.Bump() // an event recorded
	now = now.Add(BoardLiveTTL / 2)
	if !read() {
		t.Error("a write was not seen once BoardLiveTTL had passed since the answer")
	}
	if read() {
		t.Error("the fresh answer was not kept")
	}
	if err := b.SetShowName(ctx, uuid.New(), true); err != nil {
		t.Fatal(err)
	}
	if read() {
		t.Error("a name choice re-read the participation board inside BoardLiveTTL")
	}
	now = now.Add(BoardLiveTTL)
	if !read() {
		t.Error("a name choice was not seen on the participation board after BoardLiveTTL")
	}
}

// A profile carries participation per kind over each of its windows, filtered like the totals.
func TestProfileParticipation(t *testing.T) {
	set := org.Stats{Timezone: "America/Chicago", Seasons: []org.Season{{Name: "Now", Game: "wardogs", From: "2026-10-01", To: "2026-11-01"}}}
	b, ps := newParticipationBoards(set)
	ps.totals = map[string][]store.KindTotals{
		"":        {{Kind: "attendance", Events: 2}, {Kind: "seeding", Events: 3, Seconds: 1800}},
		"wardogs": {{Kind: "seeding", Events: 1, Seconds: 600}},
	}
	keys := []store.PlayerKey{{Provider: "discord", Subject: "1"}, {Provider: "steam", Subject: "2"}}
	p, err := b.Profile(context.Background(), keys)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Totals) != 4 || p.Totals[3].Season != "Now" {
		t.Fatalf("windows = %+v", p.Totals)
	}
	if want := []KindTotals{{"attendance", 2, 0}, {"seeding", 3, 1800}}; !reflect.DeepEqual(p.Totals[0].Participation, want) {
		t.Errorf("all time = %+v", p.Totals[0].Participation)
	}
	if want := []KindTotals{{"seeding", 1, 600}}; !reflect.DeepEqual(p.Totals[3].Participation, want) {
		t.Errorf("a game's season counts the game's events: %+v", p.Totals[3].Participation)
	}
	if q := ps.tq[1]; !reflect.DeepEqual(q.Keys, keys) || q.From.IsZero() || q.Timezone != "America/Chicago" {
		t.Errorf("this week's query = %+v", q)
	}
}
