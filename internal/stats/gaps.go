package stats

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/gravel-project/gravel/internal/ingest"
	"github.com/gravel-project/gravel/internal/servers"
)

// Gap windows (ADR-0011 §5, gravel#3).
const (
	// DefaultGapWindow is how far back a gap list reads when the query names no start.
	DefaultGapWindow = 30 * 24 * time.Hour
	// MaxGapWindow is the longest window a gap list covers.
	MaxGapWindow = 90 * 24 * time.Hour
	// MaxGaps is how many gaps one answer holds.
	MaxGaps = 100
)

// GapQuery is one server's gaps that began in a window. A zero To is now; a zero From is
// DefaultGapWindow before To.
type GapQuery struct {
	ServerID string
	From, To time.Time
}

// Gap is a span of a server's feed the hub knows it is missing, as the ingest gap detectors
// recorded it. Only counts: a gap names no player.
type Gap struct {
	Reason   string
	From, To time.Time
	Events   int    // dropped: events the server gave up on
	MatchID  string // reconcile: the match compared
	Kills    int    // reconcile: kills the feed lacked
	Deaths   int    // reconcile: deaths the feed lacked
	Players  int    // reconcile: players it lacked them for
}

// Gaps is a gap list and the window it read.
type Gaps struct {
	From, To time.Time
	Gaps     []Gap
	More     bool // more began in the window than MaxGaps
}

// gapFields are a gap row's extensions, as ingest.Gaps writes them.
type gapFields struct {
	Reason  string    `json:"reason"`
	To      time.Time `json:"to"`
	Events  int       `json:"events"`
	Match   int64     `json:"match"`
	Kills   int       `json:"kills"`
	Deaths  int       `json:"deaths"`
	Players int       `json:"players"`
}

// Gaps reads a server's gaps, oldest first. The boards count from the poll, not the feed, so a
// gap says what the feed lacks; it never changes a board. Not cached: it is one indexed read of
// a few rows, and a server page asks once per view.
func (b *Boards) Gaps(ctx context.Context, q GapQuery) (Gaps, error) {
	if !servers.ValidID(q.ServerID) {
		return Gaps{}, fmt.Errorf("%w: a gap list needs a server id", ErrInvalidQuery)
	}
	if q.To.IsZero() {
		q.To = b.now()
	}
	if q.From.IsZero() {
		q.From = q.To.Add(-DefaultGapWindow)
	}
	if !q.From.Before(q.To) {
		return Gaps{}, fmt.Errorf("%w: a gap list's window needs a from before its to", ErrInvalidQuery)
	}
	if q.To.Sub(q.From) > MaxGapWindow {
		return Gaps{}, fmt.Errorf("%w: a gap list's window is at most %d days", ErrInvalidQuery, int(MaxGapWindow.Hours()/24))
	}
	// An unknown server is not_found (store.ErrNotFound), not an empty list.
	if _, err := b.st.ServerGame(ctx, b.orgID, q.ServerID); err != nil {
		return Gaps{}, err
	}
	out := Gaps{From: q.From.UTC(), To: q.To.UTC()}
	rows, err := b.st.StatsEvents(ctx, b.orgID, q.ServerID, ingest.KindGap, out.From, out.To, MaxGaps+1)
	if err != nil {
		return Gaps{}, err
	}
	if len(rows) > MaxGaps {
		rows, out.More = rows[:MaxGaps], true
	}
	for _, r := range rows {
		var f gapFields
		if err := json.Unmarshal(r.Extensions, &f); err != nil {
			return Gaps{}, fmt.Errorf("stats: gap %d: %w", r.ID, err)
		}
		g := Gap{Reason: f.Reason, From: r.ObservedAt.UTC(), To: f.To.UTC(), Events: f.Events, Kills: f.Kills, Deaths: f.Deaths, Players: f.Players}
		if f.Match != 0 {
			g.MatchID = fmt.Sprint(f.Match)
		}
		if g.To.Before(g.From) {
			g.To = g.From // a gap's end is never before its start; an old row without one reads as a point
		}
		out.Gaps = append(out.Gaps, g)
	}
	return out, nil
}
