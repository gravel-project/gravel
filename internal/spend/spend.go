// Package spend is what the cloud costs (gravel#207, ADR-0034): a snapshot a day of each cloud
// location's own bill, where its token may read it (cloud.Coster), and the hub's own estimate
// from the host events beside it, with a cause for each line and a month-end projection.
package spend

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/gravel-project/gravel/cloud"
	"github.com/gravel-project/gravel/drivers/cloudvm"
	"github.com/gravel-project/gravel/internal/store"
)

// SnapshotEvery is how often the cloud_spend job reads the bills. A cloud brings its bill up to
// date once a night; a day's snapshot is replaced each time, so reading more often only catches
// the night's update sooner.
const SnapshotEvery = 6 * time.Hour

// Location is a cloud location as the job reads its bill: the provider built from its token, or
// why it could not be.
type Location struct {
	ID       string
	Provider string
	Cloud    cloud.Provider
	Err      error
}

// Store is what spend needs from the database.
type Store interface {
	SaveCloudBill(ctx context.Context, b store.CloudBill) error
	CloudBills(ctx context.Context, orgID uuid.UUID, month time.Time) ([]store.CloudBill, error)
	RunningCostByDay(ctx context.Context, orgID uuid.UUID, from, to time.Time, timezone string) ([]store.DayCost, error)
	ListServerHosts(ctx context.Context, orgID uuid.UUID) ([]store.ServerHost, error)
}

// LocationStatus is what the job last found of a location's bill.
type LocationStatus struct {
	ID       string
	Provider string
	// Billed: the bill was read. Otherwise Reason says why not, in words for the page.
	Billed bool
	Reason string
	ReadAt time.Time
}

// Service reads the bills and reports the spend.
type Service struct {
	st        Store
	orgID     uuid.UUID
	locations func(ctx context.Context) ([]Location, error)
	zone      func(ctx context.Context) (*time.Location, error)
	logger    *slog.Logger
	now       func() time.Time

	mu     sync.Mutex
	status map[string]LocationStatus
}

// New builds the service: locations are the cloud locations (none: no cloud spend), zone the
// organization's timezone (stats.timezone).
func New(st Store, orgID uuid.UUID, locations func(context.Context) ([]Location, error), zone func(context.Context) (*time.Location, error),
	logger *slog.Logger) *Service {
	return &Service{st: st, orgID: orgID, locations: locations, zone: zone, logger: logger, now: time.Now, status: map[string]LocationStatus{}}
}

// WithClock replaces the clock (tests).
func (s *Service) WithClock(now func() time.Time) *Service {
	s.now = now
	return s
}

// Snapshot reads each cloud location's bill and stores it as today's (the job). A location whose
// cloud has no cost reader, or whose token may not read bills, is costed by the estimate only and
// says why; one that fails otherwise is retried at the next run. The first error is returned
// after every location was tried.
func (s *Service) Snapshot(ctx context.Context) error {
	locs, err := s.locations(ctx)
	if err != nil {
		return err
	}
	loc, err := s.zone(ctx)
	if err != nil {
		return err
	}
	hosts, err := s.st.ListServerHosts(ctx, s.orgID)
	if err != nil {
		return err
	}
	now := s.now()
	var first error
	seen := map[string]bool{}
	for _, l := range locs {
		seen[l.ID] = true
		st := LocationStatus{ID: l.ID, Provider: l.Provider, ReadAt: now}
		coster, ok := l.Cloud.(cloud.Coster)
		switch {
		case l.Err != nil:
			st.Reason = "the hub could not reach the cloud: " + l.Err.Error()
		case !ok:
			st.Reason = l.Provider + " has no bill the hub can read"
		default:
			bill, err := coster.MonthToDate(ctx)
			switch {
			case errors.Is(err, cloud.ErrForbidden):
				st.Reason = "the location's token may not read the bill (it needs billing:read)"
			case err != nil:
				st.Reason = "reading the bill failed; the hub tries again"
				s.logger.WarnContext(ctx, "spend: reading a cloud bill failed", "location", l.ID, "error", err.Error())
				if first == nil {
					first = fmt.Errorf("spend: location %s: %w", l.ID, err)
				}
			default:
				if err := s.st.SaveCloudBill(ctx, s.snapshot(l.ID, bill, hosts, now, loc)); err != nil {
					return err
				}
				st.Billed = true
			}
		}
		s.mu.Lock()
		s.status[l.ID] = st
		s.mu.Unlock()
	}
	s.mu.Lock()
	for id := range s.status {
		if !seen[id] {
			delete(s.status, id)
		}
	}
	s.mu.Unlock()
	return first
}

// snapshot is a bill as stored: today's in loc, each line with its cause.
func (s *Service) snapshot(locationID string, bill cloud.Bill, hosts []store.ServerHost, now time.Time, loc *time.Location) store.CloudBill {
	today := now.In(loc)
	month := bill.Month
	if month.IsZero() {
		month = time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, time.UTC)
	}
	out := store.CloudBill{OrganizationID: s.orgID, LocationID: locationID, TakenOn: time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, time.UTC),
		Month: month, TakenAt: now}
	if !bill.GeneratedAt.IsZero() {
		g := bill.GeneratedAt
		out.GeneratedAt = &g
	}
	for _, l := range bill.Lines {
		kind := l.Kind
		if !slices.Contains(cloud.CostKinds, kind) {
			kind = cloud.CostOther
		}
		out.Lines = append(out.Lines, store.CloudBillLine{ResourceID: l.ResourceID, Name: l.Name, Product: l.Product, Kind: kind,
			Description: l.Description, USD: l.USD, ServerID: Cause(l, hosts)})
	}
	return out
}

// Cause is the server a bill line is for (ADR-0034 §3): the host whose instance or volume the
// line's resource is, or, since parking makes a new instance each time, the server whose names
// the line's name is (the instance is the server's id, the volume <id>-data, a snapshot
// <id>-data-…). "" for none: the hub's own host, anything made by hand.
func Cause(l cloud.BillLine, hosts []store.ServerHost) string {
	if l.ResourceID != "" {
		for _, h := range hosts {
			if l.ResourceID == h.InstanceID || l.ResourceID == h.VolumeID {
				return h.ServerID
			}
		}
	}
	if l.Name != "" {
		for _, h := range hosts {
			vol := cloudvm.VolumeName(h.ServerID)
			if l.Name == cloudvm.InstanceName(h.ServerID) || l.Name == vol || strings.HasPrefix(l.Name, vol+"-") {
				return h.ServerID
			}
		}
	}
	return ""
}

// Statuses are the locations as the job last found them, by id.
func (s *Service) Statuses() []LocationStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]LocationStatus, 0, len(s.status))
	for _, st := range s.status {
		out = append(out, st)
	}
	slices.SortFunc(out, func(a, b LocationStatus) int { return strings.Compare(a.ID, b.ID) })
	return out
}

// Day is one day of a month: what the bills say it cost (when two days' snapshots tell it) and
// the hub's estimate of its compute.
type Day struct {
	Date         time.Time // the date, in the organization's timezone (only the date counts)
	BilledUSD    float64
	HasBilled    bool
	EstimatedUSD float64
}

// CauseSpend is one server's (or one uncaused resource's) part of the month.
type CauseSpend struct {
	ServerID string // "" for an uncaused line
	Name     string // the line's name for an uncaused one
	// Kinds are the billed USD by cost kind; EstimatedUSD the hub's compute estimate.
	Kinds        map[string]float64
	BilledUSD    float64
	EstimatedUSD float64
}

// Report is a month's spend.
type Report struct {
	Month     time.Time // the first day, in Zone
	Zone      string
	Current   bool // the month holds now: it has a projection
	Locations []LocationStatus
	// BilledUSD is the month so far by the bills (the newest snapshot of each location that has
	// one), as of BilledAsOf; Billed is whether any location has one.
	Billed       bool
	BilledUSD    float64
	BilledAsOf   time.Time
	EstimatedUSD float64
	// ProjectedUSD is where the month ends (ADR-0034 §5), from the bills when there are any,
	// else the estimate (ProjectedFrom "billed" or "estimated"); the month's total once it ended.
	ProjectedUSD  float64
	ProjectedFrom string
	Days          []Day
	Causes        []CauseSpend
}

// ErrInvalidMonth is a month that isn't "YYYY-MM".
var ErrInvalidMonth = errors.New("spend: a month is YYYY-MM")

// Report is a month's spend: month is "YYYY-MM", "" for this month.
func (s *Service) Report(ctx context.Context, month string) (Report, error) {
	loc, err := s.zone(ctx)
	if err != nil {
		return Report{}, err
	}
	now := s.now().In(loc)
	start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, loc)
	if month != "" {
		m, err := time.Parse("2006-01", month)
		if err != nil {
			return Report{}, ErrInvalidMonth
		}
		start = time.Date(m.Year(), m.Month(), 1, 0, 0, 0, 0, loc)
	}
	end := start.AddDate(0, 1, 0)
	out := Report{Month: start, Zone: loc.String(), Current: !now.Before(start) && now.Before(end), Locations: s.Statuses()}
	if start.After(now) {
		return out, nil
	}
	upTo := end
	if out.Current {
		upTo = now
	}
	days := make([]Day, int(math.Round(end.Sub(start).Hours()/24)))
	for i := range days {
		days[i].Date = time.Date(start.Year(), start.Month(), start.Day()+i, 0, 0, 0, 0, time.UTC)
	}
	causes := map[string]*CauseSpend{}
	cause := func(server, name string) *CauseSpend {
		k := server
		if server == "" {
			k = "\x00" + name
		}
		c := causes[k]
		if c == nil {
			c = &CauseSpend{ServerID: server, Kinds: map[string]float64{}}
			if server == "" {
				c.Name = name
			}
			causes[k] = c
		}
		return c
	}

	// The estimate: each day's compute, by server.
	est, err := s.st.RunningCostByDay(ctx, s.orgID, start, upTo, loc.String())
	if err != nil {
		return Report{}, err
	}
	for _, d := range est {
		i := dayIndex(start, d.Day)
		if i < 0 || i >= len(days) {
			continue
		}
		days[i].EstimatedUSD += d.USD
		out.EstimatedUSD += d.USD
		cause(d.ServerID, "").EstimatedUSD += d.USD
	}

	// The bills: each location's newest snapshot is its month so far; consecutive snapshots'
	// difference is the spend of the day before the later one (a bill lags a day).
	bills, err := s.st.CloudBills(ctx, s.orgID, time.Date(start.Year(), start.Month(), 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		return Report{}, err
	}
	byLoc := map[string][]store.CloudBill{}
	for _, b := range bills {
		byLoc[b.LocationID] = append(byLoc[b.LocationID], b)
	}
	for _, snaps := range byLoc {
		out.Billed = true
		latest := snaps[len(snaps)-1]
		for _, l := range latest.Lines {
			c := cause(l.ServerID, l.Name)
			c.Kinds[l.Kind] += l.USD
			c.BilledUSD += l.USD
			out.BilledUSD += l.USD
		}
		asOf := latest.TakenAt
		if latest.GeneratedAt != nil {
			asOf = *latest.GeneratedAt
		}
		if asOf.After(out.BilledAsOf) {
			out.BilledAsOf = asOf
		}
		for i := 1; i < len(snaps); i++ {
			gap := int(math.Round(snaps[i].TakenOn.Sub(snaps[i-1].TakenOn).Hours() / 24))
			if gap < 1 {
				continue
			}
			diff := total(snaps[i]) - total(snaps[i-1])
			for g := range gap { // a gap's spend is spread over its days
				j := dayIndex(start, snaps[i].TakenOn) - 1 - g
				if j >= 0 && j < len(days) {
					days[j].BilledUSD += diff / float64(gap)
					days[j].HasBilled = true
				}
			}
		}
	}

	// The projection.
	elapsed := upTo.Sub(start).Hours() / 24
	length := end.Sub(start).Hours() / 24
	today := dayIndex(start, time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC))
	switch {
	case !out.Current && out.Billed:
		out.ProjectedUSD, out.ProjectedFrom = out.BilledUSD, "billed"
	case !out.Current:
		out.ProjectedUSD, out.ProjectedFrom = out.EstimatedUSD, "estimated"
	case out.Billed:
		var recent []float64
		for i := today - 1; i >= 0 && len(recent) < 7; i-- {
			if days[i].HasBilled {
				recent = append(recent, days[i].BilledUSD)
			}
		}
		billedElapsed := out.BilledAsOf.Sub(start).Hours() / 24
		out.ProjectedUSD, out.ProjectedFrom = Project(out.BilledUSD, recent, min(max(billedElapsed, 0), length), length), "billed"
	default:
		var recent []float64
		for i := today - 1; i >= 0 && len(recent) < 7; i-- {
			recent = append(recent, days[i].EstimatedUSD)
		}
		out.ProjectedUSD, out.ProjectedFrom = Project(out.EstimatedUSD, recent, elapsed, length), "estimated"
	}
	if out.Current {
		out.Days = days[:min(today+1, len(days))]
	} else {
		out.Days = days
	}
	for _, c := range causes {
		out.Causes = append(out.Causes, *c)
	}
	slices.SortFunc(out.Causes, func(a, b CauseSpend) int {
		if x, y := a.BilledUSD+a.EstimatedUSD, b.BilledUSD+b.EstimatedUSD; x != y {
			if x > y {
				return -1
			}
			return 1
		}
		return strings.Compare(a.ServerID+a.Name, b.ServerID+b.Name)
	})
	return out, nil
}

// Project is where a month ends (ADR-0034 §5): the month to date plus a day's rate times the
// days left. The rate is the average of the recent whole days (newest first, up to seven); with
// none (the month's first day, or one snapshot), it is the month to date over the days elapsed.
func Project(toDate float64, recent []float64, elapsedDays, monthDays float64) float64 {
	left := max(monthDays-elapsedDays, 0)
	var rate float64
	switch {
	case len(recent) > 0:
		for _, v := range recent {
			rate += v
		}
		rate /= float64(len(recent))
	case elapsedDays > 0:
		rate = toDate / elapsedDays
	}
	return toDate + rate*left
}

func total(b store.CloudBill) float64 {
	var t float64
	for _, l := range b.Lines {
		t += l.USD
	}
	return t
}

// dayIndex is a date's index in the month starting at start (dates compared by year, month, day).
func dayIndex(start, day time.Time) int {
	a := time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, time.UTC)
	b := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.UTC)
	return int(math.Round(b.Sub(a).Hours() / 24))
}
