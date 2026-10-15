package store_test

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/gravel-project/gravel/internal/store"
	"github.com/gravel-project/gravel/internal/store/storetest"
)

// A location's bill is kept a snapshot a day (ADR-0034): a day's later read replaces its earlier
// one, lines and all, and a month reads back by location and day.
func TestCloudBills(t *testing.T) {
	st := storetest.Open(t)
	storetest.Reset(t, st)
	ctx := context.Background()
	o := newOrg(t, st)
	oct := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	gen := time.Date(2026, 10, 14, 6, 0, 0, 0, time.UTC)
	save := func(day int, usd float64, lines int) {
		t.Helper()
		b := store.CloudBill{OrganizationID: o.ID, LocationID: "do-sfo3", TakenOn: time.Date(2026, 10, day, 0, 0, 0, 0, time.UTC), Month: oct, GeneratedAt: &gen,
			TakenAt: gen.Add(time.Hour)}
		for i := range lines {
			b.Lines = append(b.Lines, store.CloudBillLine{ResourceID: "4101", Name: "htg-cs2-1", Product: "Droplets", Kind: "compute", USD: usd + float64(i), ServerID: "cs2-1"})
		}
		if err := st.SaveCloudBill(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	save(14, 10, 2)
	save(14, 12.5, 1) // the same day again: replaces
	save(15, 14.25, 1)
	// An empty bill is a snapshot too.
	if err := st.SaveCloudBill(ctx, store.CloudBill{OrganizationID: o.ID, LocationID: "do-nyc3", TakenOn: time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC), Month: oct, TakenAt: gen}); err != nil {
		t.Fatal(err)
	}
	got, err := st.CloudBills(ctx, o.ID, oct)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].LocationID != "do-nyc3" || len(got[0].Lines) != 0 || got[0].GeneratedAt != nil ||
		got[1].TakenOn.Day() != 14 || len(got[1].Lines) != 1 || got[1].Lines[0].USD != 12.5 || got[1].Lines[0].ServerID != "cs2-1" ||
		got[2].TakenOn.Day() != 15 || got[2].Lines[0].USD != 14.25 || !got[2].GeneratedAt.Equal(gen) {
		t.Fatalf("bills = %+v", got)
	}
	if none, err := st.CloudBills(ctx, o.ID, oct.AddDate(0, 1, 0)); err != nil || len(none) != 0 {
		t.Errorf("november = %+v, %v", none, err)
	}
	if err := st.SaveCloudBill(ctx, store.CloudBill{OrganizationID: o.ID, LocationID: "x", TakenOn: oct, Month: oct, TakenAt: gen,
		Lines: []store.CloudBillLine{{Kind: "fuel", USD: 1}}}); err == nil {
		t.Error("a kind the table does not know was saved")
	}
}

// The estimate is each run at its recorded price, cut at midnight in the timezone asked for.
func TestRunningCostByDay(t *testing.T) {
	st := storetest.Open(t)
	storetest.Reset(t, st)
	ctx := context.Background()
	o := newOrg(t, st)
	// Midnight in Chicago is 05:00 UTC in October.
	t0 := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
	if err := st.ApplyServers(ctx, o.ID, []store.Game{{ID: "cs2"}}, nil, []store.ManagedServer{cloudServer()}, t0); err != nil {
		t.Fatal(err)
	}
	price, cheap := 0.0625, 0.03571
	ev := func(at time.Duration, from, to string, p *float64) {
		t.Helper()
		if _, err := st.InsertServerHostEvent(ctx, store.ServerHostEvent{OrganizationID: o.ID, ServerID: "cs2-1", At: t0.Add(at),
			FromState: from, ToState: to, Size: "c-2", PriceHourlyUSD: p}); err != nil {
			t.Fatal(err)
		}
	}
	// Running 03:00–07:00 UTC (two hours on each side of Chicago's midnight), parked, then from
	// 10:00 at the cheaper rung, and a run with no price recorded from 20:00.
	ev(0, "absent", "creating", nil)
	ev(3*time.Hour, "starting", "running", &price)
	ev(7*time.Hour, "running", "parking", nil)
	ev(7*time.Hour+time.Minute, "parking", "parked", nil)
	ev(10*time.Hour, "starting", "running", &cheap)
	ev(20*time.Hour, "running", "resizing", nil)
	ev(20*time.Hour+time.Minute, "resizing", "running", nil)
	got, err := st.RunningCostByDay(ctx, o.ID, t0, t0.Add(22*time.Hour), "America/Chicago")
	if err != nil {
		t.Fatal(err)
	}
	near := func(a, b float64) bool { return math.Abs(a-b) < 1e-6 }
	// The 11th (Chicago): 03:00–05:00 UTC at 0.0625. The 12th: 05:00–07:00 at 0.0625, 10:00–20:00
	// at 0.03571; the unpriced run from 20:01 counts nothing.
	if len(got) != 2 || got[0].Day.Day() != 11 || !near(got[0].USD, 2*price) || got[1].Day.Day() != 12 || !near(got[1].USD, 2*price+10*cheap) {
		t.Fatalf("by day = %+v", got)
	}
	// The same span in UTC is one day.
	if utc, err := st.RunningCostByDay(ctx, o.ID, t0, t0.Add(22*time.Hour), "UTC"); err != nil || len(utc) != 1 || !near(utc[0].USD, 4*price+10*cheap) {
		t.Errorf("utc = %+v, %v", utc, err)
	}
	if empty, err := st.RunningCostByDay(ctx, o.ID, t0, t0, "UTC"); err != nil || len(empty) != 0 {
		t.Errorf("an empty span = %+v, %v", empty, err)
	}
}
