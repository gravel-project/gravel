package stats

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/gravel-project/gravel/internal/store"
)

// Player activity (gravel#208, ADR-0035): when people play and who stopped, read from the player
// sessions the recorder stitches from the poll. Identities are resolved at read time, as a board
// resolves them.

// The windows an activity read covers, ending now.
const (
	ActivityWeek    = "7d"
	ActivityMonth   = "30d"
	ActivityQuarter = "90d"
)

// activityWindows are the windows by name, in days.
var activityWindows = map[string]int{ActivityWeek: 7, ActivityMonth: 30, ActivityQuarter: 90}

// The regulars a window looks for: active on RegularDays days or more in the RegularWeeks before
// the window.
const (
	RegularDays  = 4
	RegularWeeks = 8
	// QuietLimit bounds the regulars gone quiet listed.
	QuietLimit = 50
)

// Activity is a window's activity as a page shows it.
type Activity struct {
	Window   string
	From, To time.Time
	Timezone string
	// Hours is the week, Monday first, hour by hour in the timezone: the players on in that hour
	// on average over the window's weeks, and the most in any one.
	Hours [7][24]ActivityCell
	// Sessions started in the window, by how many players; the median session.
	Sessions, Players int
	MedianSession     time.Duration
	// Returning players came on two days or more; MeanDays is the players' mean active days.
	Returning int
	MeanDays  float64
	// New players were never seen before the window; Lost were on in the window of the same
	// length before it and not in this one.
	New, Lost int
	Weeks     []store.ActivityWeek
	// Quiet are regulars before the window with no session in it, the most recently seen first.
	Quiet        []Quiet
	RegularDays  int
	RegularWeeks int
	// Rolled says the window reaches back past the raw retention window, where only monthly
	// totals are left: the numbers before it are missing.
	Rolled bool
}

// ActivityCell is one hour of the week.
type ActivityCell struct {
	Average float64
	Peak    int
}

// Quiet is a regular gone quiet, shown as a board shows a player.
type Quiet struct {
	Name         string
	Pseudonymous bool
	UserID       uuid.UUID
	DaysBefore   int
	LastSeen     time.Time
	LastServer   string
	Hours        float64 // on in the regulars' window
}

// Activity reads a window's player activity. An unknown window is ErrInvalidQuery.
func (b *Boards) Activity(ctx context.Context, window string) (Activity, error) {
	if window == "" {
		window = ActivityMonth
	}
	days, ok := activityWindows[window]
	if !ok {
		return Activity{}, fmt.Errorf("%w: window %q is not 7d, 30d or 90d", ErrInvalidQuery, window)
	}
	set, err := b.settings(ctx)
	if err != nil {
		return Activity{}, err
	}
	loc := set.Location()
	to := b.now().In(loc)
	from := to.AddDate(0, 0, -days)
	regularFrom := from.AddDate(0, 0, -7*RegularWeeks)
	got, err := b.st.PlayerActivity(ctx, store.ActivityQuery{OrganizationID: b.orgID, Timezone: loc.String(), From: from, To: to,
		RegularFrom: regularFrom, RegularDays: RegularDays, Limit: QuietLimit})
	if err != nil {
		return Activity{}, err
	}
	out := Activity{Window: window, From: from, To: to, Timezone: loc.String(), Sessions: got.Sessions, Players: got.Players,
		MedianSession: time.Duration(got.MedianSeconds * float64(time.Second)).Round(time.Second), Returning: got.Returning,
		New: got.New, Lost: got.Lost, Weeks: got.Weeks, RegularDays: RegularDays, RegularWeeks: RegularWeeks,
		Rolled: regularFrom.Before(Cutoff(to, loc, set.RawRetentionMonthsOrDefault()))}
	if got.Players > 0 {
		out.MeanDays = float64(got.ActiveDays) / float64(got.Players)
	}
	counts := HourCounts(from, to, loc)
	for _, h := range got.Hours {
		if h.Day < 1 || h.Day > 7 || h.Hour < 0 || h.Hour > 23 {
			continue
		}
		c := &out.Hours[h.Day-1][h.Hour]
		if n := counts[h.Day-1][h.Hour]; n > 0 {
			c.Average = float64(h.PlayerHours) / float64(n)
		}
		c.Peak = h.Peak
	}
	keys := make([]store.PlayerKey, len(got.Quiet))
	for i, q := range got.Quiet {
		keys[i] = q.Key
	}
	shown, err := b.show(ctx, keys)
	if err != nil {
		return Activity{}, err
	}
	for i, q := range got.Quiet {
		out.Quiet = append(out.Quiet, Quiet{Name: shown[i].Name, Pseudonymous: shown[i].Pseudonymous, UserID: shown[i].UserID,
			DaysBefore: q.DaysBefore, LastSeen: q.LastSeen, LastServer: q.LastServer, Hours: float64(q.Seconds) / 3600})
	}
	return out, nil
}

// HourCounts is how many times each hour of the week (Monday first, in loc) starts in [from, to):
// what an hour's players are averaged over. An hour a daylight-saving change skips isn't counted;
// one it repeats is counted once.
func HourCounts(from, to time.Time, loc *time.Location) [7][24]int {
	var out [7][24]int
	from, to = from.In(loc), to.In(loc)
	seen := map[time.Time]bool{}
	start := time.Date(from.Year(), from.Month(), from.Day(), from.Hour(), 0, 0, 0, loc)
	for h := start; h.Before(to); h = h.Add(time.Hour) {
		local := time.Date(h.Year(), h.Month(), h.Day(), h.Hour(), 0, 0, 0, time.UTC)
		if seen[local] {
			continue
		}
		seen[local] = true
		day := (int(h.Weekday()) + 6) % 7
		out[day][h.Hour()]++
	}
	return out
}

// ActivityWindows are the windows an activity read takes, shortest first.
func ActivityWindows() []string {
	ws := []string{ActivityWeek, ActivityMonth, ActivityQuarter}
	slices.SortFunc(ws, func(a, b string) int { return activityWindows[a] - activityWindows[b] })
	return ws
}
