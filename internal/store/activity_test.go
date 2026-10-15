package store_test

import (
	"context"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/gravel-project/gravel/internal/store"
	"github.com/gravel-project/gravel/internal/store/storetest"
)

// A poll extends each identity's open session on the server when the session's last sighting is
// at since or later, starts one otherwise, and keeps servers apart (ADR-0035).
func TestRecordSessions(t *testing.T) {
	st := storetest.Open(t)
	storetest.Reset(t, st)
	ctx := context.Background()
	o := newOrg(t, st)
	seedServers(t, st, o.ID)
	t0 := time.Date(2026, 10, 10, 20, 0, 0, 0, time.UTC)
	gap := 52 * time.Second
	a, b := store.PlayerKey{Provider: "steam", Subject: "a"}, store.PlayerKey{Provider: "steam", Subject: "b"}
	poll := func(server string, at time.Time, keys ...store.PlayerKey) int {
		t.Helper()
		n, err := st.RecordSessions(ctx, o.ID, server, "wardogs", at, at.Add(-gap), keys)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := poll("wd-1", t0, a, a); n != 1 {
		t.Errorf("a first poll started %d sessions, want 1 (a listed twice)", n)
	}
	if n := poll("wd-1", t0.Add(15*time.Second), a, b); n != 1 {
		t.Errorf("the second poll started %d, want b's", n)
	}
	if n := poll("wd-1", t0.Add(60*time.Second), a); n != 0 { // 45 s since a's last sighting: carried on
		t.Errorf("a within the gap started %d", n)
	}
	if n := poll("wd-1", t0.Add(120*time.Second), a, b); n != 2 { // a 60 s and b 105 s since: both past the gap
		t.Errorf("past the gap started %d, want 2", n)
	}
	if n := poll("wd-2", t0.Add(120*time.Second), a); n != 1 {
		t.Errorf("another server's session started %d", n)
	}
	if n, err := st.RecordSessions(ctx, o.ID, "wd-1", "wardogs", t0, t0, nil); err != nil || n != 0 {
		t.Errorf("nobody on: %d %v", n, err)
	}
	if oldest, err := st.OldestRawSession(ctx, o.ID); err != nil || !oldest.Equal(t0) {
		t.Errorf("oldest = %v %v", oldest, err)
	}
}

// PlayerActivity over a hand-built month in Chicago: the hours of the week, the sessions and
// their median, who came back, who is new and who was lost, the weeks, and the regulars gone
// quiet (an erased one never listed); after August rolls up, a player seen only in it is still
// not new.
func TestPlayerActivity(t *testing.T) {
	st := storetest.Open(t)
	storetest.Reset(t, st)
	ctx := context.Background()
	o := newOrg(t, st)
	seedServers(t, st, o.ID)
	chi, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatal(err)
	}
	at := func(m time.Month, d, h, min int) time.Time { return time.Date(2026, m, d, h, min, 0, 0, chi) }
	session := func(provider, subject string, from, to time.Time) {
		t.Helper()
		k := []store.PlayerKey{{Provider: provider, Subject: subject}}
		if _, err := st.RecordSessions(ctx, o.ID, "wd-1", "wardogs", from, from.Add(-time.Minute), k); err != nil {
			t.Fatal(err)
		}
		if _, err := st.RecordSessions(ctx, o.ID, "wd-1", "wardogs", to, from, k); err != nil {
			t.Fatal(err)
		}
	}
	// A: three days in September (two Wednesday evenings) and one in August.
	session("steam", "a", at(9, 2, 20, 10), at(9, 2, 21, 20))
	session("steam", "a", at(9, 5, 19, 0), at(9, 5, 19, 30))
	session("steam", "a", at(9, 9, 20, 0), at(9, 9, 20, 40))
	session("steam", "a", at(8, 20, 18, 0), at(8, 20, 18, 10))
	// B: new, on the 20th.
	session("steam", "b", at(9, 20, 21, 0), at(9, 20, 22, 0))
	// C: a regular in August, gone quiet; D: twice in August; E: a regular, erased.
	for _, d := range []int{3, 5, 10, 12, 17} {
		session("steam", "c", at(8, d, 20, 0), at(8, d, 21, 0))
		session("erased", "0123456789abcdef0123456789abcdef", at(8, d, 19, 0), at(8, d, 19, 30))
	}
	session("steam", "d", at(8, 24, 20, 0), at(8, 24, 20, 30))
	session("steam", "d", at(8, 25, 20, 0), at(8, 25, 20, 30))

	from, to := at(9, 1, 0, 0), at(10, 1, 0, 0)
	q := store.ActivityQuery{OrganizationID: o.ID, Timezone: "America/Chicago", From: from, To: to, RegularFrom: from.AddDate(0, 0, -56), RegularDays: 4}
	got, err := st.PlayerActivity(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	if got.Sessions != 4 || got.Players != 2 || got.MedianSeconds != 3000 || got.Returning != 1 || got.ActiveDays != 4 || got.New != 1 || got.Lost != 3 {
		t.Errorf("activity = %+v", got)
	}
	hours := map[[2]int][2]int{}
	for _, h := range got.Hours {
		hours[[2]int{h.Day, h.Hour}] = [2]int{h.PlayerHours, h.Peak}
	}
	want := map[[2]int][2]int{{3, 20}: {2, 1}, {3, 21}: {1, 1}, {6, 19}: {1, 1}, {7, 21}: {1, 1}, {7, 22}: {1, 1}}
	if !reflect.DeepEqual(hours, want) {
		t.Errorf("hours = %v\nwant    %v", hours, want)
	}
	var weeks []string
	for _, w := range got.Weeks {
		weeks = append(weeks, w.Start.Format("01-02")+":"+strconv.Itoa(w.Players)+"/"+strconv.Itoa(w.New))
	}
	if !reflect.DeepEqual(weeks, []string{"08-31:1/0", "09-07:1/0", "09-14:1/1"}) {
		t.Errorf("weeks = %v", weeks)
	}
	if len(got.Quiet) != 1 || got.Quiet[0].Key.Subject != "c" || got.Quiet[0].DaysBefore != 5 || got.Quiet[0].Seconds != 5*3600 ||
		!got.Quiet[0].LastSeen.Equal(at(8, 17, 21, 0)) || got.Quiet[0].LastServer != "wd-1" {
		t.Errorf("quiet = %+v", got.Quiet)
	}

	// August rolls up: A's August session leaves the raw rows, and A is still not new.
	moved, err := st.RollUpActivity(ctx, o.ID, "America/Chicago", []store.Period{{From: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), To: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}})
	if err != nil || moved != 13 {
		t.Fatalf("rolled %d, %v; want 13", moved, err)
	}
	if again, err := st.PlayerActivity(ctx, q); err != nil || again.New != 1 || again.Players != 2 {
		t.Errorf("after the rollup: %+v %v", again, err)
	}
	conn, err := pgx.Connect(ctx, storetest.DatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	var sessions, days int
	if err := conn.QueryRow(ctx, `SELECT sessions, active_days FROM player_activity_monthly WHERE organization_id = $1 AND subject = 'c'`, o.ID).Scan(&sessions, &days); err != nil || sessions != 5 || days != 5 {
		t.Errorf("C's August: %d sessions on %d days, %v", sessions, days, err)
	}
	if oldest, err := st.OldestRawSession(ctx, o.ID); err != nil || !oldest.Equal(at(9, 2, 20, 10)) {
		t.Errorf("oldest after the rollup = %v %v", oldest, err)
	}
	if _, err := st.OldestRawSession(ctx, uuid.New()); err == nil {
		t.Error("an organization with no sessions has an oldest")
	}
}

// An erasure re-keys an identity's sessions and monthly rows and keeps their numbers (ADR-0035,
// ADR-0012 §7).
func TestEraseIdentityKeysSessions(t *testing.T) {
	st := storetest.Open(t)
	storetest.Reset(t, st)
	ctx := context.Background()
	o := newOrg(t, st)
	seedServers(t, st, o.ID)
	victim := store.PlayerKey{Provider: "steam", Subject: "76561190000000001"}
	other := store.PlayerKey{Provider: "steam", Subject: "76561190000000002"}
	for _, at := range []time.Time{time.Date(2025, 1, 20, 20, 0, 0, 0, time.UTC), time.Date(2026, 10, 5, 20, 0, 0, 0, time.UTC)} {
		if _, err := st.RecordSessions(ctx, o.ID, "wd-1", "wardogs", at, at.Add(-time.Minute), []store.PlayerKey{victim, other}); err != nil {
			t.Fatal(err)
		}
	}
	if moved, err := st.RollUpActivity(ctx, o.ID, "UTC", []store.Period{{From: day2025(1, 1), To: day2025(2, 1)}}); err != nil || moved != 2 {
		t.Fatalf("rolled %d, %v", moved, err)
	}
	const token = "0123456789abcdef0123456789abcdef"
	got, err := st.EraseIdentity(ctx, o.ID, victim, token, store.Actor{Via: store.ViaCLI}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if got.SessionRows != 1 || got.ActivityMonthlyRows != 1 {
		t.Errorf("erasure = %+v", got)
	}
	conn, err := pgx.Connect(ctx, storetest.DatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	var left, keyed, otherRows int
	if err := conn.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM player_sessions WHERE subject = $1) + (SELECT count(*) FROM player_activity_monthly WHERE subject = $1),
		(SELECT count(*) FROM player_sessions WHERE provider = 'erased' AND subject = $2) + (SELECT count(*) FROM player_activity_monthly WHERE provider = 'erased' AND subject = $2),
		(SELECT count(*) FROM player_sessions WHERE subject = $3) + (SELECT count(*) FROM player_activity_monthly WHERE subject = $3)`,
		victim.Subject, token, other.Subject).Scan(&left, &keyed, &otherRows); err != nil {
		t.Fatal(err)
	}
	if left != 0 || keyed != 2 || otherRows != 2 {
		t.Errorf("rows naming the player %d, re-keyed %d, the other player's %d", left, keyed, otherRows)
	}
}
