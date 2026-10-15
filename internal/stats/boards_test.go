package stats

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gravel-project/gravel/internal/org"
	"github.com/gravel-project/gravel/internal/servers"
	"github.com/gravel-project/gravel/internal/store"
)

func (m *memStore) Pseudonyms(_ context.Context, _ uuid.UUID, keys []store.PlayerKey) (map[store.PlayerKey]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[store.PlayerKey]string{}
	for _, k := range keys {
		if n, ok := m.pseudonyms[k]; ok {
			out[k] = n
		}
	}
	return out, nil
}

func (m *memStore) AddPseudonym(_ context.Context, _ uuid.UUID, k store.PlayerKey, name string, _ time.Time) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if n, ok := m.pseudonyms[k]; ok {
		return n, nil
	}
	for _, n := range m.pseudonyms {
		if n == name {
			return "", store.ErrPseudonymTaken
		}
	}
	m.pseudonyms[k] = name
	return name, nil
}

var key = []byte("0123456789abcdef0123456789abcdef")

func TestPseudonyms(t *testing.T) {
	a := store.PlayerKey{Provider: "steam", Subject: "76561190000000001"}
	b := store.PlayerKey{Provider: "steam", Subject: "76561190000000002"}
	if first, again := Pseudonym(key, a), Pseudonym(append([]byte(nil), key...), a); first != again {
		t.Errorf("not deterministic: %q, then %q", first, again)
	}
	if Pseudonym(key, a) == Pseudonym([]byte("another key, just as long, 32 b."), a) {
		t.Error("the key does not change the name")
	}
	if n := strings.Fields(Pseudonym(key, a)); len(n) != 2 {
		t.Errorf("two words: %q", Pseudonym(key, a))
	}

	st := newMemStore()
	n := NewNamer(st, uuid.New(), key, quiet())
	// b's name is already someone else's: it gets the numbered form, and keeps it.
	st.pseudonyms[store.PlayerKey{Provider: "steam", Subject: "elsewhere"}] = Pseudonym(key, b)
	got, err := n.Names(context.Background(), []store.PlayerKey{a, b})
	if err != nil {
		t.Fatal(err)
	}
	if got[a] != Pseudonym(key, a) || got[b] != Pseudonym(key, b)+" 2" {
		t.Errorf("names = %v", got)
	}
	again, _ := NewNamer(st, uuid.New(), []byte("a different key now, still 32 b"), quiet()).Names(context.Background(), []store.PlayerKey{a})
	if again[a] != got[a] {
		t.Errorf("a stored pseudonym never changes: %q then %q", got[a], again[a])
	}

	unnamed, err := NewNamer(newMemStore(), uuid.New(), nil, quiet()).Names(context.Background(), []store.PlayerKey{a})
	if err != nil || unnamed[a] != Unnamed {
		t.Errorf("no key: %v %v", unnamed, err)
	}
}

// boardStore records the board query and answers canned rows.
type boardStore struct {
	*memStore
	changes Changes
	q       store.BoardQuery
	rows    []store.BoardRow
	members map[store.PlayerKey]store.BoardMember
	shown   map[uuid.UUID]bool
	writes  int // name choices written
	// games are the servers' games; ext the extension totals BoardExtensions answers (ADR-0033).
	games map[string]string
	ext   map[store.PlayerKey]map[string]store.ExtensionTotal
	extQ  *store.BoardQuery
}

func (b *boardStore) ServerGame(_ context.Context, _ uuid.UUID, server string) (string, error) {
	if g, ok := b.games[server]; ok {
		return g, nil
	}
	return "", store.ErrNotFound
}

func (b *boardStore) BoardExtensions(_ context.Context, q store.BoardQuery, keys []store.PlayerKey) (map[store.PlayerKey]map[string]store.ExtensionTotal, error) {
	b.extQ = &q
	out := map[store.PlayerKey]map[string]store.ExtensionTotal{}
	for _, k := range keys {
		if t, ok := b.ext[k]; ok {
			out[k] = t
		}
	}
	return out, nil
}

func (b *boardStore) Board(_ context.Context, q store.BoardQuery) ([]store.BoardRow, error) {
	b.q = q
	rows := b.rows
	if len(rows) > q.Limit {
		rows = rows[:q.Limit]
	}
	return rows, nil
}

func (b *boardStore) BoardMembers(_ context.Context, keys []store.PlayerKey) (map[store.PlayerKey]store.BoardMember, error) {
	out := map[store.PlayerKey]store.BoardMember{}
	for _, k := range keys {
		if m, ok := b.members[k]; ok {
			out[k] = m
		}
	}
	return out, nil
}

func (b *boardStore) ShowNameOnBoards(_ context.Context, id uuid.UUID) (bool, error) {
	return b.shown[id], nil
}

func (b *boardStore) SetShowNameOnBoards(_ context.Context, id uuid.UUID, show bool) error {
	if b.shown == nil {
		b.shown = map[uuid.UUID]bool{}
	}
	b.shown[id] = show
	b.writes++
	return nil
}

func newBoards(set org.Stats, rows ...store.BoardRow) (*Boards, *boardStore) {
	bs := &boardStore{memStore: newMemStore(), rows: rows, members: map[store.PlayerKey]store.BoardMember{}}
	b := NewBoards(bs, uuid.New(), NewNamer(bs, uuid.New(), key, quiet()), func(context.Context) (org.Stats, error) { return set, nil }, &bs.changes)
	// Wednesday 2026-10-14 03:00 UTC is Tuesday evening in Chicago.
	b.now = func() time.Time { return time.Date(2026, 10, 14, 3, 0, 0, 0, time.UTC) }
	return b, bs
}

func TestBoardWindowsAndTrust(t *testing.T) {
	chicago, _ := time.LoadLocation("America/Chicago")
	set := org.Stats{Timezone: "America/Chicago", Seasons: []org.Season{{Name: "Season 02", Game: "wardogs", From: "2026-10-15", To: "2027-01-15"}}}
	b, bs := newBoards(set)
	ctx := context.Background()

	board, err := b.Board(ctx, Query{Window: WindowWeek})
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 10, 12, 0, 0, 0, 0, chicago); !board.From.Equal(want) || !board.To.Equal(want.AddDate(0, 0, 7)) {
		t.Errorf("this week in Chicago runs Monday to Monday: %v – %v", board.From, board.To)
	}
	if bs.q.Metric != store.BoardKills || len(bs.q.Trusts) != 1 || bs.q.Trusts[0] != TrustOfficial || bs.q.MinMatches != 1 {
		t.Errorf("the organization's board counts Official rows, by kills: %+v", bs.q)
	}

	if _, err := b.Board(ctx, Query{Window: WindowMonth, ServerID: "wd-1", Metric: store.BoardKD}); err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 10, 1, 0, 0, 0, 0, chicago); !bs.q.From.Equal(want) || len(bs.q.Trusts) != 2 || bs.q.MinMatches != org.DefaultMinMatches {
		t.Errorf("a server's monthly K/D board: %+v", bs.q)
	}

	if _, err := b.Board(ctx, Query{Window: WindowSeason, Season: "season 02"}); err != nil {
		t.Fatal(err)
	}
	if bs.q.GameID != "wardogs" || !bs.q.From.Equal(time.Date(2026, 10, 15, 0, 0, 0, 0, chicago)) {
		t.Errorf("a season limits the board to its game and dates: %+v", bs.q)
	}

	for _, q := range []Query{
		{Window: WindowSeason, Season: "Season 03"},
		{Window: "fortnight"},
		{Metric: "headshots"},
		{ServerID: "wd-1", GameID: "wardogs"},
		{Window: WindowSeason, Season: "Season 02", GameID: "cs2"},
	} {
		if _, err := b.Board(ctx, q); !errors.Is(err, ErrInvalidQuery) {
			t.Errorf("%+v: %v", q, err)
		}
	}
}

func TestBoardNamesAndPages(t *testing.T) {
	ana := store.PlayerKey{Provider: "steam", Subject: "1"}
	ben := store.PlayerKey{Provider: "steam", Subject: "2"}
	b, bs := newBoards(org.Stats{},
		store.BoardRow{Provider: "steam", Subject: "1", Kills: 30, Deaths: 10, Matches: 4},
		store.BoardRow{Provider: "steam", Subject: "2", Kills: 20, Deaths: 0, Matches: 2},
		store.BoardRow{Provider: "steam", Subject: "3", Kills: 10, Deaths: 5, Matches: 1})
	ana_ := uuid.New()
	bs.members[ana] = store.BoardMember{UserID: ana_, DisplayName: "Ana", ShowName: true}
	bs.members[ben] = store.BoardMember{UserID: uuid.New(), DisplayName: "Ben", ShowName: false} // linked, not opted in

	board, err := b.Board(context.Background(), Query{PageSize: 2})
	if err != nil {
		t.Fatal(err)
	}
	if !board.More || len(board.Entries) != 2 {
		t.Fatalf("a page of 2 of 3: %+v", board)
	}
	if e := board.Entries[0]; e.Name != "Ana" || e.Pseudonymous || e.UserID != ana_ || e.Rank != 1 || e.KD != 3 {
		t.Errorf("an opted-in member by name: %+v", e)
	}
	if e := board.Entries[1]; e.Name != Pseudonym(key, ben) || !e.Pseudonymous || e.UserID != uuid.Nil || e.KD != 20 {
		t.Errorf("a member who did not opt in, under a pseudonym (K/D over max(deaths, 1)): %+v", e)
	}
	next, err := b.Board(context.Background(), Query{PageSize: 2, Offset: 2})
	if err != nil {
		t.Fatal(err)
	}
	if bs.q.Offset != 2 || next.Entries[0].Rank != 3 {
		t.Errorf("the next page ranks from 3: %+v", next.Entries)
	}
}

// A board is served from memory while the stats store is unwritten (up to BoardMaxTTL); after a
// write it is still served for BoardLiveTTL; a member's name choice clears it.
func TestBoardCache(t *testing.T) {
	b, bs := newBoards(org.Stats{}, store.BoardRow{Provider: "steam", Subject: "1", Kills: 3, Matches: 1})
	ctx := context.Background()
	now := time.Date(2026, 10, 14, 3, 0, 0, 0, time.UTC)
	b.now = func() time.Time { return now }
	read := func(q Query) bool { // true when the store was read
		bs.q = store.BoardQuery{}
		if _, err := b.Board(ctx, q); err != nil {
			t.Fatal(err)
		}
		return bs.q.Metric != ""
	}
	if !read(Query{}) || read(Query{}) {
		t.Error("the second read went to the store")
	}
	if !read(Query{Metric: store.BoardDeaths}) {
		t.Error("another query was served from the cache")
	}
	// Idle: nothing written, so the answer outlives BoardLiveTTL.
	now = now.Add(10 * time.Minute)
	if read(Query{}) {
		t.Error("an idle hub re-read an unchanged board")
	}
	// A live match: a write is not seen until BoardLiveTTL has passed since the answer.
	now = now.Add(BoardMaxTTL) // past the cap: re-read
	if !read(Query{}) {
		t.Error("an answer older than BoardMaxTTL was served")
	}
	bs.changes.Bump()
	now = now.Add(BoardLiveTTL / 2)
	if read(Query{}) {
		t.Error("a write re-read the board inside BoardLiveTTL")
	}
	now = now.Add(BoardLiveTTL / 2)
	if !read(Query{}) {
		t.Error("a write was not seen after BoardLiveTTL")
	}
	// A name choice is a change like a recorded poll: seen within BoardLiveTTL, never at once, so
	// toggling it can't keep the boards uncached. An unchanged choice writes and marks nothing.
	member := uuid.New()
	if read(Query{}) {
		t.Fatal("the fresh answer was not kept")
	}
	if err := b.SetShowName(ctx, member, false); err != nil || bs.writes != 0 {
		t.Fatalf("an unchanged choice: %v, %d writes", err, bs.writes)
	}
	seen := bs.changes.Seen()
	if err := b.SetShowName(ctx, member, true); err != nil || bs.writes != 1 || bs.changes.Seen() == seen {
		t.Fatalf("a changed choice is written and marks the stats changed: %v, %d writes", err, bs.writes)
	}
	for range 50 { // a member toggling as fast as they can
		_ = b.SetShowName(ctx, member, false)
		_ = b.SetShowName(ctx, member, true)
	}
	if read(Query{}) {
		t.Error("a name choice re-read the board inside BoardLiveTTL")
	}
	now = now.Add(BoardLiveTTL)
	if !read(Query{}) {
		t.Error("a name choice was not seen after BoardLiveTTL")
	}
	if read(Query{}) {
		t.Error("the re-read answer was not kept")
	}
}

// A board of one game shows that game's own stats (ADR-0033): a CS2 server's board carries CS2's
// extensions as its columns and each player's totals, read over the same window and trusts; a
// stat a player has no result for is absent and a reported 0 stays 0. A War Dogs board, which
// declares none, and the organization's board carry the common stats only.
func TestBoardExtensions(t *testing.T) {
	a := store.PlayerKey{Provider: "steam", Subject: "76561190000000001"}
	c := store.PlayerKey{Provider: "steam", Subject: "76561190000000003"}
	b, bs := newBoards(org.Stats{}, store.BoardRow{Provider: a.Provider, Subject: a.Subject, Kills: 30, Matches: 3},
		store.BoardRow{Provider: c.Provider, Subject: c.Subject, Kills: 4, Matches: 1})
	bs.games = map[string]string{"cs2-1": "cs2", "wd-1": "wardogs"}
	bs.ext = map[store.PlayerKey]map[string]store.ExtensionTotal{a: {"damage": {Total: 4620, Maps: 3, Max: 2310}, "mvps": {Total: 0, Maps: 3}}}
	got, err := b.Board(context.Background(), Query{ServerID: "cs2-1", Window: WindowWeek})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range got.Extensions {
		names = append(names, e.Name)
	}
	if strings.Join(names, ",") != "assists,damage,headshot_kills,mvps,score" {
		t.Errorf("columns = %v", names)
	}
	if bs.extQ == nil || bs.extQ.ServerID != "cs2-1" || !bs.extQ.From.Equal(got.From) || len(bs.extQ.Trusts) != 2 {
		t.Errorf("extension query = %+v", bs.extQ)
	}
	e := got.Entries[0].Extensions
	if v, ok := e["mvps"]; !ok || v != 0 || e["damage"] != 4620 || len(e) != 2 {
		t.Errorf("a's extensions = %v", e)
	}
	if got.Entries[1].Extensions != nil {
		t.Errorf("c has no results, yet %v", got.Entries[1].Extensions)
	}
	for name, q := range map[string]Query{"wardogs server": {ServerID: "wd-1"}, "organization": {}, "unknown server": {ServerID: "gone"}} {
		bs.extQ = nil
		got, err := b.Board(context.Background(), q)
		if err != nil || len(got.Extensions) != 0 || got.Entries[0].Extensions != nil || bs.extQ != nil {
			t.Errorf("%s: columns %v, entry %v, queried %v, %v", name, got.Extensions, got.Entries[0].Extensions, bs.extQ != nil, err)
		}
	}
	// A game's board reads the game's.
	if got, err := b.Board(context.Background(), Query{GameID: "cs2"}); err != nil || len(got.Extensions) != 5 || bs.extQ.GameID != "cs2" {
		t.Errorf("game board: %v, %+v", err, bs.extQ)
	}
}

// Each aggregate totals a player's maps its own way.
func TestExtensionValue(t *testing.T) {
	tot := store.ExtensionTotal{Total: 30, Maps: 4, Max: 12}
	for agg, want := range map[string]float64{servers.StatSum: 30, servers.StatAvg: 7.5, servers.StatMax: 12} {
		if got := extensionValue(servers.StatExtension{Aggregate: agg}, tot); got != want {
			t.Errorf("%s = %v, want %v", agg, got, want)
		}
	}
}
