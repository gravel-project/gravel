package stats

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/gravel-project/gravel/internal/org"
	"github.com/gravel-project/gravel/internal/store"
)

// ErrInvalidQuery is a board or match query the hub cannot answer; the message says why.
var ErrInvalidQuery = errors.New("invalid stats query")

// Windows a board can cover.
const (
	WindowAll    = "all"
	WindowWeek   = "week"
	WindowMonth  = "month"
	WindowSeason = "season"
)

// Trust levels a row is written under (ADR-0011's trust mapping reads them).
const (
	TrustOfficial  = "official"
	TrustCommunity = "community"
)

// MaxPage is the largest page of a board or a match list.
const MaxPage = 100

// Board answers are cached: a public board can be read by anyone, as often as they like, and
// every read is an aggregate over the stats tables, while the numbers change only when someone
// plays. An answer is kept while the stats store has not been written (Changes), up to
// BoardMaxTTL (a new week, an edited season); once it has, it is still kept for BoardLiveTTL, so
// a live match costs one read a minute per board however many watch.
const (
	BoardLiveTTL      = time.Minute
	BoardMaxTTL       = 15 * time.Minute
	boardCacheEntries = 512
)

// SettingsSource is where the boards read the Organization settings' stats section.
type SettingsSource func(ctx context.Context) (org.Stats, error)

// Boards answers board and match queries.
type Boards struct {
	st       Store
	orgID    uuid.UUID
	names    *Namer
	settings SettingsSource
	now      func() time.Time

	changes       *Changes
	boards        answers[Query, Board]
	participation answers[ParticipationQuery, ParticipationBoard]
	recaps        answers[RecapQuery, Recap]
}

// answers is a cache of board answers by query, each kept while it is fresh.
type answers[Q comparable, A any] struct {
	mu sync.Mutex
	m  map[Q]cached[A]
}

type cached[A any] struct {
	answer  A
	at      time.Time
	changes uint64
}

// fresh reports whether a cached answer may still be served.
func (c cached[A]) fresh(now time.Time, changes uint64) bool {
	age := now.Sub(c.at)
	return age < BoardLiveTTL || (c.changes == changes && age < BoardMaxTTL)
}

// get is q's answer when it is fresh.
func (a *answers[Q, A]) get(q Q, now time.Time, changes uint64) (A, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	c, ok := a.m[q]
	if !ok || !c.fresh(now, changes) {
		var zero A
		return zero, false
	}
	return c.answer, true
}

// put keeps q's answer, read at now when the store had seen changes writes.
func (a *answers[Q, A]) put(q Q, answer A, now time.Time, changes uint64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.m == nil || len(a.m) >= boardCacheEntries {
		a.m = map[Q]cached[A]{} // bounded: a scraper walking every page only refills it
	}
	a.m[q] = cached[A]{answer: answer, at: now, changes: changes}
}

// NewBoards builds the board reader; changes is the stats store's write counter the recorder
// bumps (nil caches for BoardLiveTTL only).
func NewBoards(st Store, orgID uuid.UUID, names *Namer, settings SettingsSource, changes *Changes) *Boards {
	if changes == nil {
		changes = &Changes{}
		changes.Bump()
	}
	return &Boards{st: st, orgID: orgID, names: names, settings: settings, now: time.Now, changes: changes}
}

// Query is a board request. At most one of ServerID and GameID; neither is the organization.
type Query struct {
	ServerID string
	GameID   string
	Window   string
	Season   string
	Metric   string
	PageSize int
	Offset   int
}

// Entry is one player on a board. UserID is set only when the member chose to be shown by name.
type Entry struct {
	Rank         int
	Name         string
	Pseudonymous bool
	UserID       uuid.UUID
	Kills        int
	Deaths       int
	KD           float64
	SecondsOn    int
	Matches      int
}

// Board is a page of a board, with its window (zero for all time) and whether a next page exists.
type Board struct {
	Entries  []Entry
	From, To time.Time
	More     bool
}

// Public reports whether the Organization settings make stats public.
func (b *Boards) Public(ctx context.Context) (bool, error) {
	set, err := b.settings(ctx)
	if err != nil {
		return false, err
	}
	return set.Public, nil
}

// Board ranks the players a query selects, from the cache while it is fresh.
func (b *Boards) Board(ctx context.Context, q Query) (Board, error) {
	now, seen := b.now(), b.changes.Seen()
	if c, ok := b.boards.get(q, now, seen); ok {
		return c, nil
	}
	out, err := b.board(ctx, q)
	if err != nil {
		return Board{}, err
	}
	b.boards.put(q, out, now, seen)
	return out, nil
}

func (b *Boards) board(ctx context.Context, q Query) (Board, error) {
	set, err := b.settings(ctx)
	if err != nil {
		return Board{}, err
	}
	if q.ServerID != "" && q.GameID != "" {
		return Board{}, fmt.Errorf("%w: a board is of a server or a game, not both", ErrInvalidQuery)
	}
	metric := q.Metric
	if metric == "" {
		metric = store.BoardKills
	}
	switch metric {
	case store.BoardKills, store.BoardDeaths, store.BoardKD, store.BoardTime, store.BoardMatches:
	default:
		return Board{}, fmt.Errorf("%w: metric %q is not kills, deaths, kd, time or matches", ErrInvalidQuery, q.Metric)
	}
	from, to, game, err := b.window(set, q)
	if err != nil {
		return Board{}, err
	}
	size := q.PageSize
	if size <= 0 || size > MaxPage {
		size = MaxPage
	}
	// ADR-0011's trust mapping: a server's board counts its own rows whatever its trust; a game's
	// or the organization's counts Official servers' only.
	trusts := []string{TrustOfficial}
	if q.ServerID != "" {
		trusts = append(trusts, TrustCommunity)
	}
	minMatches := 1
	if metric == store.BoardKD {
		minMatches = set.MinMatchesOrDefault()
	}
	rows, err := b.st.Board(ctx, store.BoardQuery{OrganizationID: b.orgID, ServerID: q.ServerID, GameID: game, From: from, To: to,
		Timezone: set.Location().String(), Trusts: trusts, Metric: metric, MinMatches: minMatches, Limit: size + 1, Offset: q.Offset})
	if err != nil {
		return Board{}, err
	}
	out := Board{From: from, To: to}
	if len(rows) > size {
		rows, out.More = rows[:size], true
	}
	keys := make([]store.PlayerKey, 0, len(rows))
	for _, r := range rows {
		keys = append(keys, store.PlayerKey{Provider: r.Provider, Subject: r.Subject})
	}
	shown, err := b.show(ctx, keys)
	if err != nil {
		return Board{}, err
	}
	for i, r := range rows {
		out.Entries = append(out.Entries, Entry{Rank: q.Offset + i + 1, Name: shown[i].Name, Pseudonymous: shown[i].Pseudonymous, UserID: shown[i].UserID,
			Kills: r.Kills, Deaths: r.Deaths, SecondsOn: r.SecondsOn, Matches: r.Matches, KD: float64(r.Kills) / float64(max(r.Deaths, 1))})
	}
	return out, nil
}

// shownAs is how a board shows a player: by name when they linked the identity and chose to be
// shown (UserID is then the member), else under their pseudonym.
type shownAs struct {
	Name         string
	Pseudonymous bool
	UserID       uuid.UUID
}

// show resolves a page of identities to how a board shows them, in order (principle 5: the
// identity becomes a member only here, when the board is read).
func (b *Boards) show(ctx context.Context, keys []store.PlayerKey) ([]shownAs, error) {
	members, err := b.st.BoardMembers(ctx, keys)
	if err != nil {
		return nil, err
	}
	var anon []store.PlayerKey
	for _, k := range keys {
		if m, ok := members[k]; !ok || !m.ShowName {
			anon = append(anon, k)
		}
	}
	names, err := b.names.Names(ctx, anon)
	if err != nil {
		return nil, err
	}
	out := make([]shownAs, len(keys))
	for i, k := range keys {
		if m, ok := members[k]; ok && m.ShowName {
			out[i] = shownAs{Name: m.DisplayName, UserID: m.UserID}
		} else {
			out[i] = shownAs{Name: names[k], Pseudonymous: true}
		}
	}
	return out, nil
}

// ShownAs is how a page shows a player: the name a member chose to be shown by, or a pseudonym.
type ShownAs struct {
	Name         string
	Pseudonymous bool
	UserID       uuid.UUID // the member, when shown by name
}

// ShowAs resolves identities to how a page shows them, in order: a match result's players
// (ADR-0024), the same way a board does.
func (b *Boards) ShowAs(ctx context.Context, keys []store.PlayerKey) ([]ShownAs, error) {
	shown, err := b.show(ctx, keys)
	if err != nil {
		return nil, err
	}
	out := make([]ShownAs, len(shown))
	for i, s := range shown {
		out[i] = ShownAs(s)
	}
	return out, nil
}

// window turns a query's window into instants in the organization's timezone, and the game a
// season limits the board to.
func (b *Boards) window(set org.Stats, q Query) (from, to time.Time, game string, err error) {
	loc, now, game := set.Location(), b.now().In(set.Location()), q.GameID
	switch q.Window {
	case "", WindowAll:
		return time.Time{}, time.Time{}, game, nil
	case WindowWeek:
		day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
		from = day.AddDate(0, 0, -((int(day.Weekday()) + 6) % 7)) // ISO: weeks start on Monday
		return from, from.AddDate(0, 0, 7), game, nil
	case WindowMonth:
		from = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, loc)
		return from, from.AddDate(0, 1, 0), game, nil
	case WindowSeason:
		for _, se := range set.Seasons {
			if !strings.EqualFold(se.Name, strings.TrimSpace(q.Season)) {
				continue
			}
			if from, to, err = se.Window(loc); err != nil {
				return time.Time{}, time.Time{}, "", fmt.Errorf("%w: season %q has bad dates", ErrInvalidQuery, se.Name)
			}
			if se.Game != "" {
				if game != "" && game != se.Game {
					return time.Time{}, time.Time{}, "", fmt.Errorf("%w: season %q is %s's, not %s's", ErrInvalidQuery, se.Name, se.Game, game)
				}
				if q.ServerID == "" {
					game = se.Game
				}
			}
			return from, to, game, nil
		}
		return time.Time{}, time.Time{}, "", fmt.Errorf("%w: no season named %q", ErrInvalidQuery, q.Season)
	}
	return time.Time{}, time.Time{}, "", fmt.Errorf("%w: window %q is not all, week, month or season", ErrInvalidQuery, q.Window)
}

// Matches is a page of a server's matches, newest first, below beforeID (0 for the first page).
func (b *Boards) Matches(ctx context.Context, serverID string, beforeID int64, pageSize int) ([]store.MatchSummary, bool, error) {
	if serverID == "" {
		return nil, false, fmt.Errorf("%w: server_id is required", ErrInvalidQuery)
	}
	if pageSize <= 0 || pageSize > MaxPage {
		pageSize = MaxPage
	}
	ms, err := b.st.ListMatches(ctx, b.orgID, serverID, beforeID, pageSize+1)
	if err != nil {
		return nil, false, err
	}
	if len(ms) > pageSize {
		return ms[:pageSize], true, nil
	}
	return ms, false, nil
}

// ShowName is a member's choice to be shown by name on boards.
func (b *Boards) ShowName(ctx context.Context, userID uuid.UUID) (bool, error) {
	return b.st.ShowNameOnBoards(ctx, userID)
}

// SetShowName records a member's choice. An unchanged choice writes nothing. A change marks the
// stats as changed, as a recorded poll does, so every cached board, participation board and recap
// shows it within BoardLiveTTL. Clearing the caches instead would let any member, toggling at
// their rate limit, keep every public board uncached.
func (b *Boards) SetShowName(ctx context.Context, userID uuid.UUID, show bool) error {
	cur, err := b.st.ShowNameOnBoards(ctx, userID)
	if err != nil {
		return err
	}
	if cur == show {
		return nil
	}
	if err := b.st.SetShowNameOnBoards(ctx, userID, show); err != nil {
		return err
	}
	b.changes.Bump()
	return nil
}
