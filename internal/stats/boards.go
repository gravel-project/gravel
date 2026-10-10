package stats

import (
	"context"
	"errors"
	"fmt"
	"strings"
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

// SettingsSource is where the boards read the Organization settings' stats section.
type SettingsSource func(ctx context.Context) (org.Stats, error)

// Boards answers board and match queries.
type Boards struct {
	st       Store
	orgID    uuid.UUID
	names    *Namer
	settings SettingsSource
	now      func() time.Time
}

// NewBoards builds the board reader.
func NewBoards(st Store, orgID uuid.UUID, names *Namer, settings SettingsSource) *Boards {
	return &Boards{st: st, orgID: orgID, names: names, settings: settings, now: time.Now}
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

// Board ranks the players a query selects.
func (b *Boards) Board(ctx context.Context, q Query) (Board, error) {
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
	members, err := b.st.BoardMembers(ctx, keys)
	if err != nil {
		return Board{}, err
	}
	var anon []store.PlayerKey
	for _, k := range keys {
		if m, ok := members[k]; !ok || !m.ShowName {
			anon = append(anon, k)
		}
	}
	names, err := b.names.Names(ctx, anon)
	if err != nil {
		return Board{}, err
	}
	for i, r := range rows {
		k := keys[i]
		e := Entry{Rank: q.Offset + i + 1, Kills: r.Kills, Deaths: r.Deaths, SecondsOn: r.SecondsOn, Matches: r.Matches,
			KD: float64(r.Kills) / float64(max(r.Deaths, 1))}
		if m, ok := members[k]; ok && m.ShowName {
			e.Name, e.UserID = m.DisplayName, m.UserID
		} else {
			e.Name, e.Pseudonymous = names[k], true
		}
		out.Entries = append(out.Entries, e)
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

// SetShowName records it.
func (b *Boards) SetShowName(ctx context.Context, userID uuid.UUID, show bool) error {
	return b.st.SetShowNameOnBoards(ctx, userID, show)
}
