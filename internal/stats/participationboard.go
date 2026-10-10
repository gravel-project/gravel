package stats

import (
	"context"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/gravel-project/gravel/internal/store"
)

// ParticipationQuery is a participation board request: one kind (empty is the first the
// organization has), optionally one game, over a window like a combat board's.
type ParticipationQuery struct {
	Kind     string
	GameID   string
	Window   string
	Season   string
	PageSize int
	Offset   int
}

// ParticipationEntry is one player on a participation board, shown as on a combat board.
type ParticipationEntry struct {
	Rank         int
	Name         string
	Pseudonymous bool
	UserID       uuid.UUID
	Events       int
	Seconds      int
}

// ParticipationBoard is a page of a participation board: the kind it ranks (empty when nothing
// has been recorded), every kind the organization has, the window (zero for all time) and
// whether a next page exists.
type ParticipationBoard struct {
	Entries  []ParticipationEntry
	Kind     string
	Kinds    []string
	From, To time.Time
	More     bool
}

// ParticipationBoard ranks the players who took part in a kind by time, then times, from the
// cache while it is fresh. It counts first_party events only: they are the only rows in the
// participation tables (ADR-0011's trust mapping).
func (b *Boards) ParticipationBoard(ctx context.Context, q ParticipationQuery) (ParticipationBoard, error) {
	now, seen := b.now(), b.changes.Seen()
	if c, ok := b.participation.get(q, now, seen); ok {
		return c, nil
	}
	out, err := b.participationBoard(ctx, q)
	if err != nil {
		return ParticipationBoard{}, err
	}
	b.participation.put(q, out, now, seen)
	return out, nil
}

func (b *Boards) participationBoard(ctx context.Context, q ParticipationQuery) (ParticipationBoard, error) {
	if q.Kind != "" && !ValidKind(q.Kind) {
		return ParticipationBoard{}, fmt.Errorf("%w: kind %s is not a lower-case letter followed by up to %d lower-case letters, digits or underscores",
			ErrInvalidQuery, quote(q.Kind), MaxParticipationKind-1)
	}
	set, err := b.settings(ctx)
	if err != nil {
		return ParticipationBoard{}, err
	}
	from, to, game, err := b.window(set, Query{GameID: q.GameID, Window: q.Window, Season: q.Season})
	if err != nil {
		return ParticipationBoard{}, err
	}
	kinds, err := b.st.ParticipationKinds(ctx, b.orgID)
	if err != nil {
		return ParticipationBoard{}, err
	}
	out := ParticipationBoard{Kind: q.Kind, Kinds: kinds, From: from, To: to}
	if out.Kind == "" {
		if len(kinds) == 0 {
			return out, nil
		}
		out.Kind = kinds[0]
	}
	if !slices.Contains(kinds, out.Kind) {
		return out, nil // nothing of that kind yet: an empty board, not an error
	}
	size := q.PageSize
	if size <= 0 || size > MaxPage {
		size = MaxPage
	}
	rows, err := b.st.ParticipationBoard(ctx, store.ParticipationBoardQuery{OrganizationID: b.orgID, Kind: out.Kind, GameID: game,
		From: from, To: to, Timezone: set.Location().String(), Limit: size + 1, Offset: q.Offset})
	if err != nil {
		return ParticipationBoard{}, err
	}
	if len(rows) > size {
		rows, out.More = rows[:size], true
	}
	keys := make([]store.PlayerKey, 0, len(rows))
	for _, r := range rows {
		keys = append(keys, store.PlayerKey{Provider: r.Provider, Subject: r.Subject})
	}
	shown, err := b.show(ctx, keys)
	if err != nil {
		return ParticipationBoard{}, err
	}
	for i, r := range rows {
		out.Entries = append(out.Entries, ParticipationEntry{Rank: q.Offset + i + 1, Name: shown[i].Name, Pseudonymous: shown[i].Pseudonymous,
			UserID: shown[i].UserID, Events: clamp(r.Events), Seconds: clamp(r.Seconds)})
	}
	return out, nil
}

// clamp is a sum the store keeps as a 64-bit number, as the API's 32-bit fields can carry it.
func clamp(n int64) int {
	return int(min(max(n, 0), math.MaxInt32))
}
