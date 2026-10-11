package stats

import (
	"context"
	"fmt"
	"time"

	"github.com/gravel-project/gravel/internal/servers"
	"github.com/gravel-project/gravel/internal/store"
)

// Recap bounds (ADR-0018).
const (
	// MaxRecapWindow is the longest window a recap covers.
	MaxRecapWindow = 24 * time.Hour
	// MaxRecapFloor bounds the floor the minutes "at or above" count against.
	MaxRecapFloor = 1000
	// RecapTop is how many players a recap ranks by kills.
	RecapTop = 5
	// recapSkew is how far in the future a recap's end may be: a clock a little ahead.
	recapSkew = time.Minute
)

// RecapQuery is a session recap's window on one server, and the floor its minutes are counted
// against (0 counts none).
type RecapQuery struct {
	ServerID string
	From, To time.Time
	Floor    int
}

// Recap is what a server's window held: its occupancy, the matches that overlap it and the players
// with the most kills in them, shown as the boards show players.
type Recap struct {
	ServerID    string
	From, To    time.Time
	Floor       int
	PeakPlayers int
	Minutes     int // with anyone on
	AtFloor     int // whose fewest players reached the floor
	Players     int // distinct, seen in the window
	Matches     []store.RecapMatch
	Top         []Entry
}

// Nobody reports whether nobody was on during the window.
func (r Recap) Nobody() bool { return r.PeakPlayers == 0 && r.Players == 0 }

// Recap reads a session recap, from the cache while it is fresh. The window must be a real one:
// from before to, at most MaxRecapWindow, and ending no later than now. Nothing is stored for a
// recap; it is read from the stats store each time it is not cached.
func (b *Boards) Recap(ctx context.Context, q RecapQuery) (Recap, error) {
	if !servers.ValidID(q.ServerID) {
		return Recap{}, fmt.Errorf("%w: a recap needs a server id", ErrInvalidQuery)
	}
	if q.From.IsZero() || q.To.IsZero() || !q.From.Before(q.To) {
		return Recap{}, fmt.Errorf("%w: a recap's window needs a from before its to", ErrInvalidQuery)
	}
	if q.To.Sub(q.From) > MaxRecapWindow {
		return Recap{}, fmt.Errorf("%w: a recap's window is at most %s", ErrInvalidQuery, MaxRecapWindow)
	}
	now := b.now()
	if q.To.After(now.Add(recapSkew)) {
		return Recap{}, fmt.Errorf("%w: a recap's window must have ended", ErrInvalidQuery)
	}
	if q.Floor < 0 || q.Floor > MaxRecapFloor {
		return Recap{}, fmt.Errorf("%w: floor %d is not between 0 and %d", ErrInvalidQuery, q.Floor, MaxRecapFloor)
	}
	q.From, q.To = q.From.UTC(), q.To.UTC()
	seen := b.changes.Seen()
	if c, ok := b.recaps.get(q, now, seen); ok {
		return c, nil
	}
	raw, err := b.st.SessionRecap(ctx, store.RecapQuery{OrganizationID: b.orgID, ServerID: q.ServerID, From: q.From, To: q.To, Floor: q.Floor, Top: RecapTop})
	if err != nil {
		return Recap{}, err
	}
	out := Recap{ServerID: q.ServerID, From: q.From, To: q.To, Floor: q.Floor, PeakPlayers: raw.PeakPlayers, Minutes: raw.Minutes,
		AtFloor: raw.AtFloor, Players: raw.Players, Matches: raw.Matches}
	keys := make([]store.PlayerKey, 0, len(raw.Top))
	for _, r := range raw.Top {
		keys = append(keys, store.PlayerKey{Provider: r.Provider, Subject: r.Subject})
	}
	shown, err := b.show(ctx, keys)
	if err != nil {
		return Recap{}, err
	}
	for i, r := range raw.Top {
		out.Top = append(out.Top, Entry{Rank: i + 1, Name: shown[i].Name, Pseudonymous: shown[i].Pseudonymous, UserID: shown[i].UserID,
			Kills: r.Kills, Deaths: r.Deaths, SecondsOn: r.SecondsOn, Matches: r.Matches, KD: float64(r.Kills) / float64(max(r.Deaths, 1))})
	}
	b.recaps.put(q, out, now, seen)
	return out, nil
}
