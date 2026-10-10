package stats

import (
	"context"
	"time"

	"github.com/gravel-project/gravel/internal/store"
)

// ProfileMatches is how many recent matches a profile lists.
const ProfileMatches = 20

// WindowTotals is a player's totals over one window: all time, this week, this month, or a
// season running now (Season names it). From and To are zero for all time.
type WindowTotals struct {
	Window   string
	Season   string
	From, To time.Time
	Totals   store.BoardRow
	KD       float64
	// Participation is their participation from first-party apps over the window, by kind; a
	// kind they never took part in is absent (ADR-0014).
	Participation []KindTotals
}

// KindTotals is a player's participation of one kind over a window.
type KindTotals struct {
	Kind    string
	Events  int
	Seconds int
}

// Profile is one player's stats across their identities.
type Profile struct {
	Totals []WindowTotals
	Recent []store.PlayerMatch
}

// Profile sums a player's identities (a member's linked accounts) over all time, this week, this
// month and each season running now, counting Official servers' rows as the organization's board
// does (a game's season counts that game's matches), with their participation per kind over the
// same windows, and lists their recent matches on any server.
func (b *Boards) Profile(ctx context.Context, keys []store.PlayerKey) (Profile, error) {
	set, err := b.settings(ctx)
	if err != nil {
		return Profile{}, err
	}
	queries := []Query{{Window: WindowAll}, {Window: WindowWeek}, {Window: WindowMonth}}
	now := b.now()
	for _, se := range set.Seasons {
		if from, to, err := se.Window(set.Location()); err == nil && !now.Before(from) && now.Before(to) {
			queries = append(queries, Query{Window: WindowSeason, Season: se.Name})
		}
	}
	var out Profile
	for _, q := range queries {
		from, to, game, err := b.window(set, q)
		if err != nil {
			return Profile{}, err
		}
		row, err := b.st.PlayerTotals(ctx, store.PlayerTotalsQuery{OrganizationID: b.orgID, Keys: keys, GameID: game, From: from, To: to,
			Timezone: set.Location().String(), Trusts: []string{TrustOfficial}})
		if err != nil {
			return Profile{}, err
		}
		kinds, err := b.st.ParticipationTotals(ctx, store.ParticipationTotalsQuery{OrganizationID: b.orgID, Keys: keys, GameID: game, From: from, To: to,
			Timezone: set.Location().String()})
		if err != nil {
			return Profile{}, err
		}
		wt := WindowTotals{Window: q.Window, Season: q.Season, From: from, To: to, Totals: row, KD: float64(row.Kills) / float64(max(row.Deaths, 1))}
		for _, k := range kinds {
			wt.Participation = append(wt.Participation, KindTotals{Kind: k.Kind, Events: clamp(k.Events), Seconds: clamp(k.Seconds)})
		}
		out.Totals = append(out.Totals, wt)
	}
	if out.Recent, err = b.st.PlayerMatches(ctx, b.orgID, keys, ProfileMatches); err != nil {
		return Profile{}, err
	}
	return out, nil
}
