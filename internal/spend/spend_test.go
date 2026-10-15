package spend_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gravel-project/gravel/cloud"
	"github.com/gravel-project/gravel/cloud/fake"
	"github.com/gravel-project/gravel/internal/spend"
	"github.com/gravel-project/gravel/internal/store"
)

// memStore keeps the snapshots in memory and answers the estimate it is given.
type memStore struct {
	mu    sync.Mutex
	bills []store.CloudBill
	est   []store.DayCost
	hosts []store.ServerHost
}

func (m *memStore) SaveCloudBill(_ context.Context, b store.CloudBill) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.bills = slices.DeleteFunc(m.bills, func(x store.CloudBill) bool { return x.LocationID == b.LocationID && x.TakenOn.Equal(b.TakenOn) })
	m.bills = append(m.bills, b)
	slices.SortFunc(m.bills, func(a, b store.CloudBill) int {
		if c := strings.Compare(a.LocationID, b.LocationID); c != 0 {
			return c
		}
		return a.TakenOn.Compare(b.TakenOn)
	})
	return nil
}

func (m *memStore) CloudBills(_ context.Context, _ uuid.UUID, month time.Time) ([]store.CloudBill, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []store.CloudBill
	for _, b := range m.bills {
		if b.Month.Equal(month) {
			out = append(out, b)
		}
	}
	return out, nil
}

func (m *memStore) RunningCostByDay(_ context.Context, _ uuid.UUID, from, to time.Time, _ string) ([]store.DayCost, error) {
	var out []store.DayCost
	for _, d := range m.est {
		if !d.Day.Before(time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, time.UTC)) && d.Day.Before(to) {
			out = append(out, d)
		}
	}
	return out, nil
}

func (m *memStore) ListServerHosts(context.Context, uuid.UUID) ([]store.ServerHost, error) {
	return m.hosts, nil
}

var chicago, _ = time.LoadLocation("America/Chicago")

func zone(context.Context) (*time.Location, error) { return chicago, nil }

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// The projection at the month's edges: the first day with no whole day behind it runs at the
// rate so far; later, at the recent days' average; at the end, nothing is left to add.
func TestProject(t *testing.T) {
	for name, c := range map[string]struct {
		toDate        float64
		recent        []float64
		elapsed, days float64
		want          float64
	}{
		"first morning":       {toDate: 1, elapsed: 0.25, days: 31, want: 1 + 4*30.75},
		"the first instant":   {toDate: 0, elapsed: 0, days: 31, want: 0},
		"a week in":           {toDate: 70, recent: []float64{10, 10, 10, 10, 10, 10, 10}, elapsed: 7.5, days: 30, want: 70 + 10*22.5},
		"a quiet recent week": {toDate: 100, recent: []float64{0, 0, 2}, elapsed: 20, days: 30, want: 100 + (2.0/3)*10},
		"the month's end":     {toDate: 300, recent: []float64{50}, elapsed: 30, days: 30, want: 300},
		"past the end":        {toDate: 300, recent: []float64{50}, elapsed: 31, days: 30, want: 300},
	} {
		if got := spend.Project(c.toDate, c.recent, c.elapsed, c.days); !near(got, c.want) {
			t.Errorf("%s: %v, want %v", name, got, c.want)
		}
	}
}

// coster-less is a cloud without a cost reader.
type plain struct{ cloud.Provider }

// The job stores each readable location's bill as today's, each line with its cause, and says
// why a location has none.
func TestSnapshot(t *testing.T) {
	st := &memStore{hosts: []store.ServerHost{{ServerID: "htg-cs2-1", InstanceID: "4101", VolumeID: "v-1"}}}
	billed := fake.New()
	billed.SetBill(cloud.Bill{Month: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), GeneratedAt: time.Date(2026, 10, 15, 6, 0, 0, 0, time.UTC), Lines: []cloud.BillLine{
		{ResourceID: "4101", Name: "htg-cs2-1", Kind: cloud.CostCompute, USD: 12.4}, // the live instance, by id
		{ResourceID: "3999", Name: "htg-cs2-1", Kind: cloud.CostCompute, USD: 3},    // an instance parked away, by name
		{Name: "htg-cs2-1-data", Kind: cloud.CostStorage, USD: 6.67},                // the volume
		{Name: "htg-cs2-1-data-cs2-2026-10-12", Kind: cloud.CostStorage, USD: 0.81}, // its snapshot
		{ResourceID: "4000", Name: "htg-hub-1", Kind: cloud.CostCompute, USD: 11.2}, // the hub's own host: uncaused
		{Name: "htg-cs2-10", Kind: "mystery", USD: 0.1},                             // another server's name, not this one's; an unknown kind
	}})
	forbidden := fake.New()
	forbidden.FailNext("MonthToDate", cloud.ErrForbidden)
	broken := fake.New()
	broken.FailNext("MonthToDate", errors.New("the cloud is down"))
	locs := []spend.Location{
		{ID: "do-sfo3", Provider: "digitalocean", Cloud: billed},
		{ID: "do-nyc3", Provider: "digitalocean", Cloud: forbidden},
		{ID: "other", Provider: "acme", Cloud: plain{billed}},
		{ID: "gone", Provider: "digitalocean", Err: errors.New("no token file")},
		{ID: "down", Provider: "digitalocean", Cloud: broken},
	}
	s := spend.New(st, uuid.New(), func(context.Context) ([]spend.Location, error) { return locs, nil }, zone, quiet()).
		WithClock(func() time.Time { return time.Date(2026, 10, 15, 4, 0, 0, 0, time.UTC) }) // the 14th in Chicago
	if err := s.Snapshot(context.Background()); err == nil || !strings.Contains(err.Error(), "down") {
		t.Errorf("snapshot: %v", err)
	}
	if len(st.bills) != 1 || st.bills[0].LocationID != "do-sfo3" || st.bills[0].TakenOn.Day() != 14 || st.bills[0].GeneratedAt == nil {
		t.Fatalf("bills = %+v", st.bills)
	}
	var causes []string
	for _, l := range st.bills[0].Lines {
		causes = append(causes, l.ServerID+"/"+l.Kind)
	}
	if strings.Join(causes, " ") != "htg-cs2-1/compute htg-cs2-1/compute htg-cs2-1/storage htg-cs2-1/storage /compute /other" {
		t.Errorf("causes = %v", causes)
	}
	reasons := map[string]string{}
	for _, st := range s.Statuses() {
		reasons[st.ID] = st.Reason
		if st.Billed != (st.ID == "do-sfo3") {
			t.Errorf("%s billed %t", st.ID, st.Billed)
		}
	}
	for id, want := range map[string]string{"do-nyc3": "billing:read", "other": "no bill", "gone": "could not reach", "down": "tries again"} {
		if !strings.Contains(reasons[id], want) {
			t.Errorf("%s: %q, want %q", id, reasons[id], want)
		}
	}
	// A location gone from the list is forgotten.
	locs = locs[:1]
	_ = s.Snapshot(context.Background())
	if n := len(s.Statuses()); n != 1 {
		t.Errorf("%d statuses after a location went", n)
	}
}

// A month's report: the newest snapshot is the month so far, consecutive snapshots' difference
// the day before the later one's spend, the estimate day by day, causes merged, and the
// projection from the bills when there are any.
func TestReport(t *testing.T) {
	oct := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	snap := func(day int, cs2, hub float64) store.CloudBill {
		gen := time.Date(2026, 10, day, 11, 0, 0, 0, time.UTC) // 06:00 in Chicago
		return store.CloudBill{LocationID: "do-sfo3", TakenOn: time.Date(2026, 10, day, 0, 0, 0, 0, time.UTC), Month: oct, GeneratedAt: &gen, TakenAt: gen,
			Lines: []store.CloudBillLine{{Name: "htg-cs2-1", Kind: "compute", USD: cs2, ServerID: "htg-cs2-1"}, {Name: "htg-hub-1", Kind: "compute", USD: hub}}}
	}
	st := &memStore{bills: []store.CloudBill{snap(12, 30, 9), snap(13, 32, 9.8), snap(15, 38, 11.4)}}
	for d := 1; d <= 15; d++ {
		st.est = append(st.est, store.DayCost{ServerID: "htg-cs2-1", Day: time.Date(2026, 10, d, 0, 0, 0, 0, time.UTC), USD: 2})
	}
	s := spend.New(st, uuid.New(), func(context.Context) ([]spend.Location, error) { return nil, nil }, zone, quiet()).
		WithClock(func() time.Time { return time.Date(2026, 10, 15, 17, 0, 0, 0, time.UTC) }) // noon on the 15th in Chicago
	r, err := s.Report(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if !r.Current || !r.Billed || !near(r.BilledUSD, 49.4) || !near(r.EstimatedUSD, 30) || r.ProjectedFrom != "billed" || len(r.Days) != 15 {
		t.Fatalf("report = %+v", r)
	}
	// 12→13: 2.8 on the 12th; 13→15: 7.6 over two days, 3.8 on the 13th and the 14th.
	for i, want := range map[int]float64{11: 2.8, 12: 3.8, 13: 3.8} {
		if d := r.Days[i]; !d.HasBilled || !near(d.BilledUSD, want) {
			t.Errorf("day %d: %+v, want %v", i+1, d, want)
		}
	}
	if r.Days[10].HasBilled || r.Days[14].HasBilled {
		t.Errorf("days without two snapshots are billed: %+v %+v", r.Days[10], r.Days[14])
	}
	// The bill is as of 06:00 Chicago on the 15th: 14.25 days elapsed of 31, at (2.8+3.8+3.8)/3 a day.
	if want := 49.4 + (10.4/3)*(31-14.25); !near(r.ProjectedUSD, want) {
		t.Errorf("projected %v, want %v", r.ProjectedUSD, want)
	}
	if len(r.Causes) != 2 || r.Causes[0].ServerID != "htg-cs2-1" || !near(r.Causes[0].BilledUSD, 38) || !near(r.Causes[0].EstimatedUSD, 30) ||
		r.Causes[1].ServerID != "" || r.Causes[1].Name != "htg-hub-1" || !near(r.Causes[1].Kinds["compute"], 11.4) {
		t.Errorf("causes = %+v", r.Causes)
	}

	// Without bills, the estimate projects: the last seven whole days at 2 a day.
	st.bills = nil
	if r, err = s.Report(context.Background(), "2026-10"); err != nil || r.Billed || r.ProjectedFrom != "estimated" || !near(r.ProjectedUSD, 30+2*(31-14.5)) {
		t.Errorf("estimated: %+v, %v", r, err)
	}
	// A past month is its total; a future one is empty; a bad month is refused.
	if r, err = s.Report(context.Background(), "2026-09"); err != nil || r.Current || r.ProjectedUSD != 0 || len(r.Days) != 30 {
		t.Errorf("september: %+v, %v", r, err)
	}
	if r, err = s.Report(context.Background(), "2026-11"); err != nil || len(r.Days) != 0 || r.ProjectedUSD != 0 {
		t.Errorf("november: %+v, %v", r, err)
	}
	if _, err = s.Report(context.Background(), "October"); !errors.Is(err, spend.ErrInvalidMonth) {
		t.Errorf("a bad month: %v", err)
	}
}
