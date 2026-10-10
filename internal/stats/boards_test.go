package stats

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gravel-project/gravel/internal/org"
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

func (b *boardStore) SetShowNameOnBoards(context.Context, uuid.UUID, bool) error { return nil }

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
	if err := b.SetShowName(ctx, uuid.New(), true); err != nil {
		t.Fatal(err)
	}
	if !read(Query{}) {
		t.Error("a name choice left the cached board")
	}
}
