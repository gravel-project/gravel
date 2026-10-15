package stats

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/gravel-project/gravel/internal/org"
	"github.com/gravel-project/gravel/internal/store"
)

func TestCutoffKeepsNoRowOlderThanTheWindow(t *testing.T) {
	chicago, _ := time.LoadLocation("America/Chicago")
	for _, c := range []struct {
		now    time.Time
		months int
		want   string
	}{
		{time.Date(2026, 10, 10, 12, 0, 0, 0, chicago), 13, "2025-10-01"},
		{time.Date(2026, 10, 31, 23, 0, 0, 0, chicago), 13, "2025-10-01"},
		{time.Date(2026, 11, 1, 0, 30, 0, 0, chicago), 13, "2025-11-01"},
		{time.Date(2026, 1, 15, 0, 0, 0, 0, chicago), 1, "2026-01-01"},
		// 03:00Z on November 1st is still October in Chicago.
		{time.Date(2026, 11, 1, 3, 0, 0, 0, time.UTC), 13, "2025-10-01"},
	} {
		got := Cutoff(c.now, chicago, c.months)
		if got.Format(time.DateOnly) != c.want || got.Location() != chicago {
			t.Errorf("Cutoff(%v, %d) = %v, want %s", c.now, c.months, got, c.want)
		}
		// The oldest row kept is younger than the window.
		if age := c.now.Sub(got); age > time.Duration(c.months)*31*24*time.Hour {
			t.Errorf("Cutoff(%v, %d) keeps rows %v old", c.now, c.months, age)
		}
	}
}

func TestPeriodsCutMonthsAtSeasonEdges(t *testing.T) {
	chicago, _ := time.LoadLocation("America/Chicago")
	seasons := []org.Season{
		{Name: "Season 01", Game: "wardogs", From: "2025-01-15", To: "2025-03-01"}, // starts mid-month, ends on a month edge
		{Name: "Launch", From: "2025-02-10", To: "2025-02-20"},                     // inside one month
		{Name: "Later", From: "2026-01-01", To: "2026-02-01"},                      // after the cutoff: no cut
	}
	oldest := time.Date(2025, 1, 15, 3, 0, 0, 0, time.UTC) // January 14th in Chicago
	cutoff := time.Date(2025, 4, 1, 0, 0, 0, 0, chicago)
	got, err := Periods(oldest, cutoff, chicago, seasons)
	if err != nil {
		t.Fatal(err)
	}
	var spans []string
	for _, p := range got {
		spans = append(spans, p.From.Format("01-02")+".."+p.To.Format("01-02"))
	}
	want := "01-01..01-15 01-15..02-01 02-01..02-10 02-10..02-20 02-20..03-01 03-01..04-01"
	if strings.Join(spans, " ") != want {
		t.Errorf("periods = %v\nwant      %s", spans, want)
	}
	if _, err := Periods(oldest, cutoff, chicago, []org.Season{{Name: "Bad", From: "soon", To: "later"}}); err == nil {
		t.Error("a season with bad dates was accepted")
	}
	if none, _ := Periods(cutoff, cutoff, chicago, nil); len(none) != 0 {
		t.Errorf("nothing before the cutoff = %v", none)
	}
}

type rollupStore struct {
	oldest  time.Time
	tz      string
	periods []store.Period
	rolled  store.Rollup
	erased  store.PlayerKey
	token   string

	oldestPart  time.Time
	partPeriods []store.Period
	partMoved   int

	retireCutoff time.Time
	retired      store.Retired

	oldestSession  time.Time
	sessionPeriods []store.Period
	sessionsMoved  int
}

func (r *rollupStore) OldestRawSession(context.Context, uuid.UUID) (time.Time, error) {
	if r.oldestSession.IsZero() {
		return time.Time{}, store.ErrNotFound
	}
	return r.oldestSession, nil
}

func (r *rollupStore) RollUpActivity(_ context.Context, _ uuid.UUID, _ string, periods []store.Period) (int, error) {
	r.sessionPeriods = periods
	return r.sessionsMoved, nil
}

func (r *rollupStore) RetireResults(_ context.Context, _ uuid.UUID, cutoff time.Time) (store.Retired, error) {
	r.retireCutoff = cutoff
	return r.retired, nil
}

func (r *rollupStore) OldestRawParticipation(context.Context, uuid.UUID) (time.Time, error) {
	if r.oldestPart.IsZero() {
		return time.Time{}, store.ErrNotFound
	}
	return r.oldestPart, nil
}

func (r *rollupStore) RollUpParticipation(_ context.Context, _ uuid.UUID, tz string, periods []store.Period) (int, error) {
	r.tz, r.partPeriods = tz, periods
	return r.partMoved, nil
}

func (r *rollupStore) OldestRawMatch(context.Context, uuid.UUID) (time.Time, error) {
	if r.oldest.IsZero() {
		return time.Time{}, store.ErrNotFound
	}
	return r.oldest, nil
}

func (r *rollupStore) RollUp(_ context.Context, _ uuid.UUID, tz string, periods []store.Period) (store.Rollup, error) {
	r.tz, r.periods = tz, periods
	return r.rolled, nil
}

func (r *rollupStore) EraseIdentity(_ context.Context, _ uuid.UUID, key store.PlayerKey, token string, _ store.Actor, _ time.Time) (store.Erasure, error) {
	r.erased, r.token = key, token
	return store.Erasure{MatchRows: 1}, nil
}

func TestRetentionRun(t *testing.T) {
	set := org.Stats{Timezone: "America/Chicago", RawRetentionMonths: 6}
	var changes Changes
	newRun := func(st *rollupStore) (*Retention, *prometheus.Registry) {
		reg := prometheus.NewRegistry()
		r := NewRetention(st, uuid.New(), func(context.Context) (org.Stats, error) { return set, nil }, &changes, reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
		r.now = func() time.Time { return time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC) }
		return r, reg
	}

	// Match results past the window are retired on every run, whatever else is due (#216), from
	// the same cutoff: the start of May in Chicago for six months from October.
	st0 := &rollupStore{retired: store.Retired{Results: 2, Rows: 9}}
	r0, _ := newRun(st0)
	if err := r0.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	chicago, _ := time.LoadLocation("America/Chicago")
	if want := time.Date(2026, 5, 1, 0, 0, 0, 0, chicago); !st0.retireCutoff.Equal(want) {
		t.Errorf("retired before %v, want %v", st0.retireCutoff, want)
	}
	if n := testutil.ToFloat64(r0.retired); n != 2 {
		t.Errorf("retired results metric = %v", n)
	}

	// No raw rows, or none past the window: nothing is rolled.
	recent := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	for name, st := range map[string]*rollupStore{"empty": {}, "recent": {oldest: recent, oldestPart: recent}} {
		r, _ := newRun(st)
		if err := r.Run(context.Background()); err != nil || st.periods != nil || st.partPeriods != nil {
			t.Errorf("%s: %v, periods %v and %v", name, err, st.periods, st.partPeriods)
		}
	}

	// Six months from October keeps May on: January to April roll, in the organization's zone.
	st := &rollupStore{oldest: time.Date(2026, 1, 20, 0, 0, 0, 0, time.UTC), rolled: store.Rollup{Matches: 3, Rows: 40}}
	r, reg := newRun(st)
	if err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st.tz != "America/Chicago" || len(st.periods) != 4 || st.periods[0].From.Format(time.DateOnly) != "2026-01-01" ||
		st.periods[3].To.Format(time.DateOnly) != "2026-05-01" {
		t.Errorf("rolled %s %v", st.tz, st.periods)
	}
	if n := testutil.ToFloat64(r.rows); n != 40 {
		t.Errorf("rolled rows metric = %v", n)
	}
	if changes.Seen() != 1 {
		t.Errorf("a rollup that moved rows bumped the change counter %d times", changes.Seen())
	}
	if _, err := reg.Gather(); err != nil {
		t.Fatal(err)
	}
	if st.partPeriods != nil {
		t.Errorf("participation rolled with none to roll: %v", st.partPeriods)
	}

	// Only participation is old: it rolls, from its own oldest month, and the matches do not.
	st = &rollupStore{oldest: recent, oldestPart: time.Date(2026, 2, 3, 0, 0, 0, 0, time.UTC), partMoved: 5}
	r, _ = newRun(st)
	if err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st.periods != nil || len(st.partPeriods) != 3 || st.partPeriods[0].From.Format(time.DateOnly) != "2026-02-01" {
		t.Errorf("participation only: matches %v, participation %v", st.periods, st.partPeriods)
	}
	if changes.Seen() != 2 {
		t.Errorf("moving participation did not bump the change counter: %d", changes.Seen())
	}

	// Only sessions are old (ADR-0035): they roll from their own oldest month, the rest don't.
	st = &rollupStore{oldest: recent, oldestSession: time.Date(2026, 3, 3, 0, 0, 0, 0, time.UTC), sessionsMoved: 8}
	r, _ = newRun(st)
	seen := changes.Seen()
	if err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st.periods != nil || st.partPeriods != nil || len(st.sessionPeriods) != 2 || st.sessionPeriods[0].From.Format(time.DateOnly) != "2026-03-01" {
		t.Errorf("sessions only: matches %v, participation %v, sessions %v", st.periods, st.partPeriods, st.sessionPeriods)
	}
	if changes.Seen() != seen+1 {
		t.Errorf("moving sessions did not bump the change counter")
	}

	// Both: the periods start at the older of the two, and both roll over the same periods.
	st = &rollupStore{oldest: time.Date(2026, 3, 9, 0, 0, 0, 0, time.UTC), oldestPart: time.Date(2026, 1, 9, 0, 0, 0, 0, time.UTC)}
	r, _ = newRun(st)
	if err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(st.periods) != 4 || len(st.partPeriods) != 4 || st.periods[0].From.Format(time.DateOnly) != "2026-01-01" {
		t.Errorf("both: matches %v, participation %v", st.periods, st.partPeriods)
	}
	if changes.Seen() != 3 { // the matches, participation and sessions cases above
		t.Errorf("a rollup that moved nothing bumped the change counter: %d", changes.Seen())
	}
}

func TestErase(t *testing.T) {
	st := &rollupStore{}
	got, err := Erase(context.Background(), st, uuid.New(), store.PlayerKey{Provider: " steam ", Subject: "76561190000000001"}, store.Actor{}, time.Now())
	if err != nil || got.MatchRows != 1 {
		t.Fatalf("erase = %+v, %v", got, err)
	}
	if st.erased != (store.PlayerKey{Provider: "steam", Subject: "76561190000000001"}) || len(st.token) != 32 {
		t.Errorf("erased %+v with token %q", st.erased, st.token)
	}
	first := st.token
	if _, err := Erase(context.Background(), st, uuid.New(), store.PlayerKey{Provider: "steam", Subject: "76561190000000001"}, store.Actor{}, time.Now()); err != nil || st.token == first {
		t.Errorf("a second erasure reused the token: %v", err)
	}
	for _, k := range []store.PlayerKey{{Provider: "steam"}, {Subject: "1"}, {Provider: store.ErasedProvider, Subject: first}} {
		if _, err := Erase(context.Background(), st, uuid.New(), k, store.Actor{}, time.Now()); !errors.Is(err, ErrInvalidErasure) {
			t.Errorf("%+v: %v", k, err)
		}
	}
}

func TestNamerShowsAnErasedPlayerAsDeleted(t *testing.T) {
	st := newMemStore()
	n := NewNamer(st, uuid.New(), []byte(strings.Repeat("k", 32)), slog.New(slog.NewTextHandler(io.Discard, nil)))
	erased := store.PlayerKey{Provider: store.ErasedProvider, Subject: "0123456789abcdef0123456789abcdef"}
	got, err := n.Names(context.Background(), []store.PlayerKey{erased})
	if err != nil || got[erased] != Deleted {
		t.Errorf("names = %v, %v", got, err)
	}
	if len(st.pseudonyms) != 0 {
		t.Errorf("an erased player was given a pseudonym: %v", st.pseudonyms)
	}
}
