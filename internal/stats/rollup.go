package stats

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/gravel-project/gravel/internal/org"
	"github.com/gravel-project/gravel/internal/store"
)

// RollupEvery is how often the rollup job looks for raw rows past the retention window. Each run
// moves whole periods only, so running often costs one query when there is nothing to do.
const RollupEvery = 6 * time.Hour

// RollupStore is what the rollup and erasure need from the store.
type RollupStore interface {
	OldestRawMatch(ctx context.Context, orgID uuid.UUID) (time.Time, error)
	RollUp(ctx context.Context, orgID uuid.UUID, timezone string, periods []store.Period) (store.Rollup, error)
	EraseIdentity(ctx context.Context, orgID uuid.UUID, key store.PlayerKey, token string) (store.Erasure, error)
}

// Retention rolls a player's per-match rows into monthly totals once they pass the Organization
// settings' stats.raw_retention_months (ADR-0012 §6), so no raw row is older than the window and
// the boards keep their numbers.
type Retention struct {
	st       RollupStore
	orgID    uuid.UUID
	settings SettingsSource
	changes  *Changes
	logger   *slog.Logger
	now      func() time.Time
	rows     prometheus.Counter
}

// NewRetention builds the rollup and registers its metric; changes is bumped when it moves rows,
// for the boards' cache (nil for none).
func NewRetention(st RollupStore, orgID uuid.UUID, settings SettingsSource, changes *Changes, reg prometheus.Registerer, logger *slog.Logger) *Retention {
	r := &Retention{st: st, orgID: orgID, settings: settings, changes: changes, logger: logger, now: time.Now,
		rows: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "gravel_stats_rolled_rows_total", Help: "Per-match player rows rolled up into monthly totals past the raw retention window (ADR-0012).",
		})}
	reg.MustRegister(r.rows)
	return r
}

// Cutoff is the first day the raw rows are kept from: the start of the month retention-1 months
// before now's, in loc. A row is rolled up once its match started before it, so none is older
// than the window (13 months keeps between 12 and 13).
func Cutoff(now time.Time, loc *time.Location, retentionMonths int) time.Time {
	now = now.In(loc)
	return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, loc).AddDate(0, -(retentionMonths - 1), 0)
}

// Periods are the rollup periods from the month holding oldest up to cutoff: calendar months in
// loc, each cut wherever a season starts or ends inside it, so a season's board adds whole
// periods. Dates are midnights in loc.
func Periods(oldest, cutoff time.Time, loc *time.Location, seasons []org.Season) ([]store.Period, error) {
	oldest = oldest.In(loc)
	start := time.Date(oldest.Year(), oldest.Month(), 1, 0, 0, 0, 0, loc)
	var cuts []time.Time
	for _, se := range seasons {
		from, to, err := se.Window(loc)
		if err != nil {
			return nil, fmt.Errorf("stats: season %q: %w", se.Name, err)
		}
		cuts = append(cuts, from, to)
	}
	var out []store.Period
	for m := start; m.Before(cutoff); m = m.AddDate(0, 1, 0) {
		end := m.AddDate(0, 1, 0)
		edges := []time.Time{m}
		for _, c := range cuts {
			if c.After(m) && c.Before(end) {
				edges = append(edges, c)
			}
		}
		slices.SortFunc(edges, func(a, b time.Time) int { return a.Compare(b) })
		edges = slices.CompactFunc(edges, func(a, b time.Time) bool { return a.Equal(b) })
		edges = append(edges, end)
		for i := 0; i+1 < len(edges); i++ {
			out = append(out, store.Period{From: edges[i], To: edges[i+1]})
		}
	}
	return out, nil
}

// Run rolls up every whole period before the cutoff; a run with nothing to do reads one row.
func (r *Retention) Run(ctx context.Context) error {
	set, err := r.settings(ctx)
	if err != nil {
		return err
	}
	oldest, err := r.st.OldestRawMatch(ctx, r.orgID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	loc := set.Location()
	cutoff := Cutoff(r.now(), loc, set.RawRetentionMonthsOrDefault())
	if !oldest.Before(cutoff) {
		return nil
	}
	periods, err := Periods(oldest, cutoff, loc, set.Seasons)
	if err != nil {
		return err
	}
	got, err := r.st.RollUp(ctx, r.orgID, loc.String(), periods)
	if err != nil {
		return err
	}
	r.rows.Add(float64(got.Rows))
	if got.Rows > 0 {
		r.changes.Bump()
		r.logger.InfoContext(ctx, "stats: rolled raw rows up into monthly totals", "matches", got.Matches, "rows", got.Rows,
			"before", cutoff.Format(time.DateOnly), "retention_months", set.RawRetentionMonthsOrDefault())
	}
	return nil
}

// ErrInvalidErasure is an erasure request without an identity.
var ErrInvalidErasure = errors.New("stats: an erasure needs a provider and a subject")

// Erase replaces a player's identity in the stats tables (and the stored ingest batches) with a
// random token, and deletes their pseudonym (ADR-0012 §7). The numbers stay, shown as a deleted
// player; nothing maps the token back to the identity. A player still on a server is recorded
// under their identity again from the next poll.
func Erase(ctx context.Context, st RollupStore, orgID uuid.UUID, key store.PlayerKey) (store.Erasure, error) {
	key.Provider, key.Subject = strings.TrimSpace(key.Provider), strings.TrimSpace(key.Subject)
	if key.Provider == "" || key.Subject == "" || key.Provider == store.ErasedProvider {
		return store.Erasure{}, ErrInvalidErasure
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return store.Erasure{}, fmt.Errorf("stats: erase: %w", err)
	}
	return st.EraseIdentity(ctx, orgID, key, hex.EncodeToString(b))
}
