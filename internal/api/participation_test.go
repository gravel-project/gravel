package api_test

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"

	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
	"github.com/gravel-project/gravel/gen/gravel/hub/v1/hubv1connect"
	"github.com/gravel-project/gravel/internal/apps"
	"github.com/gravel-project/gravel/internal/identity"
	"github.com/gravel-project/gravel/internal/org"
	"github.com/gravel-project/gravel/internal/store"
)

// The participation side of fakeStatsStore: one row per app and key, a board and a profile's
// totals summed in memory (windows and games are the store's, tested against Postgres).

func (f *fakeStatsStore) RecordParticipation(_ context.Context, events []store.ParticipationEvent) ([]bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]bool, len(events))
	for i, e := range events {
		if slices.ContainsFunc(f.events, func(x store.ParticipationEvent) bool { return x.AppID == e.AppID && x.Key == e.Key }) {
			continue
		}
		f.events, out[i] = append(f.events, e), true
	}
	return out, nil
}

func (f *fakeStatsStore) ParticipationKinds(_ context.Context, _ uuid.UUID, game string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var kinds []string
	for _, e := range f.events {
		if (game == "" || e.GameID == game) && !slices.Contains(kinds, e.Kind) {
			kinds = append(kinds, e.Kind)
		}
	}
	slices.Sort(kinds)
	return kinds, nil
}

func (f *fakeStatsStore) ParticipationBoard(_ context.Context, q store.ParticipationBoardQuery) ([]store.ParticipationRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var rows []store.ParticipationRow
	for _, e := range f.events {
		if e.Kind != q.Kind {
			continue
		}
		i := slices.IndexFunc(rows, func(r store.ParticipationRow) bool { return r.Provider == e.Provider && r.Subject == e.Subject })
		if i < 0 {
			rows, i = append(rows, store.ParticipationRow{Provider: e.Provider, Subject: e.Subject}), len(rows)
		}
		rows[i].Events++
		rows[i].Seconds += int64(e.DurationSeconds)
	}
	slices.SortFunc(rows, func(a, b store.ParticipationRow) int { return cmp.Compare(b.Seconds, a.Seconds) })
	return rows[min(q.Offset, len(rows)):min(q.Offset+q.Limit, len(rows))], nil
}

func (f *fakeStatsStore) ParticipationTotals(_ context.Context, q store.ParticipationTotalsQuery) ([]store.KindTotals, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.KindTotals
	for _, e := range f.events {
		if !slices.Contains(q.Keys, store.PlayerKey{Provider: e.Provider, Subject: e.Subject}) {
			continue
		}
		i := slices.IndexFunc(out, func(k store.KindTotals) bool { return k.Kind == e.Kind })
		if i < 0 {
			out, i = append(out, store.KindTotals{Kind: e.Kind}), len(out)
		}
		out[i].Events++
		out[i].Seconds += int64(e.DurationSeconds)
	}
	return out, nil
}

func (f *fakeStatsStore) BoardMembers(_ context.Context, keys []store.PlayerKey) (map[store.PlayerKey]store.BoardMember, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[store.PlayerKey]store.BoardMember{}
	for _, k := range keys {
		if m, ok := f.members[k]; ok {
			out[k] = m
		}
	}
	return out, nil
}

func (f *fakeStatsStore) Pseudonyms(context.Context, uuid.UUID, []store.PlayerKey) (map[store.PlayerKey]string, error) {
	return map[store.PlayerKey]string{}, nil
}

func (f *fakeStatsStore) stored() []store.ParticipationEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.events)
}

// attendance is a valid event: an hour at an event, by a Discord identity, an hour ago.
func attendance(key, subject string) *hubv1.ParticipationEvent {
	return &hubv1.ParticipationEvent{Key: key, Kind: "attendance", Identity: &hubv1.PlayerIdentity{Provider: "discord", Subject: subject},
		OccurredAt: timestamppb.New(time.Now().Add(-time.Hour)), DurationSeconds: 3600}
}

func recordAs(c hubv1connect.StatsServiceClient, events ...*hubv1.ParticipationEvent) (*hubv1.RecordParticipationResponse, error) {
	resp, err := c.RecordParticipation(context.Background(), connect.NewRequest(&hubv1.RecordParticipationRequest{Events: events}))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

// Only an app with participation:write records participation: not anonymous callers, not a
// member, not even the owner, and not an app with other scopes.
func TestRecordParticipationAccess(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	member := r.user(t, "Ana", "100")
	owner := r.user(t, "Olive", "300")
	if _, err := r.org.Claim(ctx, r.token, store.Actor{UserID: &owner.ID}); err != nil {
		t.Fatal(err)
	}
	client := func(c *http.Client) hubv1connect.StatsServiceClient {
		return hubv1connect.NewStatsServiceClient(c, r.srv.URL)
	}
	for name, c := range map[string]struct {
		client hubv1connect.StatsServiceClient
		want   connect.Code
	}{
		"anonymous":                   {client(http.DefaultClient), connect.CodeUnauthenticated},
		"a member":                    {client(cookieClient(http.DefaultClient, r.login(t, member.ID))), connect.CodePermissionDenied},
		"the owner":                   {client(cookieClient(http.DefaultClient, r.login(t, owner.ID))), connect.CodePermissionDenied},
		"an app with stats:read":      {client(bearerClient(http.DefaultClient, r.bearer(t, "reader", apps.ScopeStatsRead, apps.ScopeIdentityRead))), connect.CodePermissionDenied},
		"an app with every other one": {client(bearerClient(http.DefaultClient, r.bearer(t, "all", apps.ScopeServersModerate, apps.ScopeServersConfigure, apps.ScopeServersRead))), connect.CodePermissionDenied},
	} {
		if _, err := recordAs(c.client, attendance("k", "1")); connect.CodeOf(err) != c.want {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
	}
	if n := len(r.stats.stored()); n != 0 {
		t.Fatalf("a refused caller stored %d events", n)
	}
	muster := client(bearerClient(http.DefaultClient, r.bearer(t, "muster", apps.ScopeParticipationWrite)))
	if got, err := recordAs(muster, attendance("k", "1")); err != nil || len(got.GetResults()) != 1 || got.GetResults()[0].GetOutcome() != "recorded" {
		t.Errorf("the app: %v %v", got, err)
	}
}

// The API's side of recording: results in order, duplicates per app, a refused request naming
// every bad field by index with nothing stored, the request size, and the metric.
func TestRecordParticipation(t *testing.T) {
	r := newRig(t)
	r.withServer(t)
	appsSvc := r.apps
	muster := hubv1connect.NewStatsServiceClient(bearerClient(http.DefaultClient, r.bearer(t, "muster", apps.ScopeParticipationWrite)), r.srv.URL)
	seeder := hubv1connect.NewStatsServiceClient(bearerClient(http.DefaultClient, r.bearer(t, "seeder", apps.ScopeParticipationWrite)), r.srv.URL)

	onServer := attendance("m-2", "2")
	onServer.Kind, onServer.ServerId, onServer.Session = "seeding", "wd-1", "window 7"
	got, err := recordAs(muster, attendance("m-1", "1"), onServer, attendance("m-1", "3"))
	if err != nil {
		t.Fatal(err)
	}
	outcomes := func(resp *hubv1.RecordParticipationResponse) string {
		var o []string
		for _, x := range resp.GetResults() {
			o = append(o, x.GetKey()+"="+x.GetOutcome())
		}
		return strings.Join(o, " ")
	}
	if o := outcomes(got); o != "m-1=recorded m-2=recorded m-1=duplicate" {
		t.Errorf("outcomes = %s", o)
	}
	rows := r.stats.stored()
	musterApp, err := appsSvc.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var musterID uuid.UUID
	for _, a := range musterApp {
		if a.Name == "muster" {
			musterID = a.ID
		}
	}
	if len(rows) != 2 || rows[0].AppID != musterID || rows[1].GameID != "wardogs" || rows[1].ServerID != "wd-1" || rows[1].Session != "window 7" ||
		rows[0].Source != "first_party" || rows[0].SchemaVersion != 1 || rows[0].Provider != "discord" || rows[0].Subject != "1" || rows[0].DurationSeconds != 3600 {
		t.Errorf("rows = %+v", rows)
	}
	// The same key again from the same app is a duplicate even with other fields; from another
	// app it is that app's own.
	again := attendance("m-1", "9")
	again.Kind = "seeding"
	if got, err := recordAs(muster, again); err != nil || outcomes(got) != "m-1=duplicate" {
		t.Errorf("a repeat: %v %v", outcomes(got), err)
	}
	if got, err := recordAs(seeder, attendance("m-1", "1")); err != nil || outcomes(got) != "m-1=recorded" {
		t.Errorf("another app's key: %v %v", outcomes(got), err)
	}

	// A bad request: every problem named by index, nothing stored.
	noIdentity, noTime, mismatch, unknownServer, unknownGame, future, old := attendance("b-1", "1"), attendance("b-2", "1"), attendance("b-3", "1"),
		attendance("b-4", "1"), attendance("b-5", "1"), attendance("b-6", "1"), attendance("b-7", "1")
	noIdentity.Identity = nil
	noTime.OccurredAt = nil
	mismatch.ServerId, mismatch.GameId = "wd-1", "cs2"
	unknownServer.ServerId = "wd-9"
	unknownGame.GameId = "quake"
	future.OccurredAt = timestamppb.New(time.Now().Add(time.Hour))
	old.OccurredAt = timestamppb.New(time.Now().AddDate(-2, 0, 0))
	badKind := attendance("b-8", "1")
	badKind.Kind = "Event Attendance"
	before := len(r.stats.stored())
	_, err = recordAs(muster, attendance("ok", "1"), noIdentity, noTime, mismatch, unknownServer, unknownGame, future, old, badKind)
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("a bad request: %v", err)
	}
	msg := connectMessage(err)
	for _, want := range []string{"events[1].identity: required", "events[2].occurred_at: required", `events[3].game_id: "cs2" is not the game of server "wd-1"`,
		`events[4].server_id: "wd-9" is not a server this hub knows`, `events[5].game_id: "quake" is not a game this hub ships`,
		"events[6].occurred_at:", "ahead of the hub's clock", "events[7].occurred_at:", "older than the stats store keeps", `events[8].kind: "Event Attendance"`} {
		if !strings.Contains(msg, want) {
			t.Errorf("%q does not say %q", msg, want)
		}
	}
	if strings.Contains(msg, "events[0]") || strings.HasPrefix(msg, "invalid participation events") {
		t.Errorf("message = %q", msg)
	}
	if len(r.stats.stored()) != before {
		t.Error("a refused request stored events")
	}
	for _, n := range []int{0, 101} {
		events := make([]*hubv1.ParticipationEvent, n)
		for i := range events {
			events[i] = attendance("n", "1")
		}
		if _, err := recordAs(muster, events...); connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(connectMessage(err), "send 1 to 100") {
			t.Errorf("%d events: %v", n, err)
		}
	}

	if n := counter(t, r, "attendance", "recorded"); n != 2 {
		t.Errorf("attendance recorded = %v", n)
	}
	if n := counter(t, r, "invalid", "invalid"); n != 1 {
		t.Errorf("a bad kind counted under kind=invalid: %v", n)
	}
}

// counter is gravel_participation_events_total{kind,outcome} from the rig's registry.
func counter(t *testing.T, r *rig, kind, outcome string) float64 {
	t.Helper()
	mfs, err := r.reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() != "gravel_participation_events_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			labels := map[string]string{}
			for _, l := range m.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			if labels["kind"] == kind && labels["outcome"] == outcome {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

// The wire format an app's HTTP-JSON client sends (docs/hub.md has the same example).
func TestRecordParticipationJSON(t *testing.T) {
	r := newRig(t)
	body := `{"events":[{"key":"muster-42-1","kind":"attendance","identity":{"provider":"discord","subject":"1234"},` +
		`"session":"muster-42","occurredAt":"` + time.Now().Add(-time.Hour).UTC().Format(time.RFC3339) + `","durationSeconds":5400}]}`
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, r.srv.URL+"/gravel.hub.v1.StatsService/RecordParticipation", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", r.bearer(t, "muster", apps.ScopeParticipationWrite))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		Results []struct{ Key, Outcome string } `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || resp.StatusCode != http.StatusOK || len(out.Results) != 1 ||
		out.Results[0].Key != "muster-42-1" || out.Results[0].Outcome != "recorded" {
		t.Errorf("%d %+v %v", resp.StatusCode, out, err)
	}
	if rows := r.stats.stored(); len(rows) != 1 || rows[0].DurationSeconds != 5400 || rows[0].Session != "muster-42" {
		t.Errorf("rows = %+v", rows)
	}
}

// A participation board has GetBoard's access, names and paging; a profile carries participation.
func TestParticipationBoardAndProfile(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	ana := r.user(t, "Ana", "100")
	if err := r.idSt.LinkIdentity(ctx, store.Identity{UserID: ana.ID, Provider: "steam", Subject: "76561190000000001", VerificationMethod: identity.MethodOpenID, VerifiedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	owner := r.user(t, "Olive", "300")
	if _, err := r.org.Claim(ctx, r.token, store.Actor{UserID: &owner.ID}); err != nil {
		t.Fatal(err)
	}
	r.stats.members[store.PlayerKey{Provider: "discord", Subject: "100"}] = store.BoardMember{UserID: ana.ID, DisplayName: "Ana", ShowName: true}
	writer := hubv1connect.NewStatsServiceClient(bearerClient(http.DefaultClient, r.bearer(t, "muster", apps.ScopeParticipationWrite)), r.srv.URL)
	seeding := attendance("s-1", "76561190000000001")
	seeding.Kind, seeding.Identity.Provider, seeding.DurationSeconds = "seeding", "steam", 1800
	if _, err := recordAs(writer, attendance("a-1", "100"), attendance("a-2", "200"), seeding); err != nil {
		t.Fatal(err)
	}
	r.stats.events[1].DurationSeconds = 600 // the stranger took part for less

	client := func(c *http.Client) hubv1connect.StatsServiceClient {
		return hubv1connect.NewStatsServiceClient(c, r.srv.URL)
	}
	board := func(c hubv1connect.StatsServiceClient, req *hubv1.GetParticipationBoardRequest) (*hubv1.GetParticipationBoardResponse, connect.Code) {
		resp, err := c.GetParticipationBoard(ctx, connect.NewRequest(req))
		if err != nil {
			return nil, connect.CodeOf(err)
		}
		return resp.Msg, 0
	}
	anon := client(http.DefaultClient)
	asOwner := client(cookieClient(http.DefaultClient, r.login(t, owner.ID)))
	asMember := client(cookieClient(http.DefaultClient, r.login(t, ana.ID)))
	asReader := client(bearerClient(http.DefaultClient, r.bearer(t, "reader", apps.ScopeStatsRead)))

	// Private stats: the owner and stats:read; the writing scope does not read.
	for name, c := range map[string]struct {
		client hubv1connect.StatsServiceClient
		want   connect.Code
	}{"anonymous": {anon, connect.CodeUnauthenticated}, "a member": {asMember, connect.CodePermissionDenied}, "participation:write": {writer, connect.CodePermissionDenied},
		"the owner": {asOwner, 0}, "stats:read": {asReader, 0}} {
		if _, code := board(c.client, &hubv1.GetParticipationBoardRequest{}); code != c.want {
			t.Errorf("%s: %v, want %v", name, code, c.want)
		}
	}
	got, _ := board(asOwner, &hubv1.GetParticipationBoardRequest{PageSize: 1})
	if got.GetKind() != "attendance" || strings.Join(got.GetKinds(), ",") != "attendance,seeding" || len(got.GetEntries()) != 1 || got.GetNextPageToken() == "" {
		t.Fatalf("the first kind, a page of one: %v", got)
	}
	if e := got.GetEntries()[0]; e.GetName() != "Ana" || e.GetUserId() != ana.ID.String() || e.GetPseudonymous() || e.GetEvents() != 1 || e.GetSeconds() != 3600 || e.GetRank() != 1 {
		t.Errorf("Ana by name: %v", e)
	}
	next, _ := board(asOwner, &hubv1.GetParticipationBoardRequest{PageSize: 1, PageToken: got.GetNextPageToken()})
	if len(next.GetEntries()) != 1 || next.GetEntries()[0].GetRank() != 2 || !next.GetEntries()[0].GetPseudonymous() || next.GetEntries()[0].GetUserId() != "" ||
		next.GetEntries()[0].GetSeconds() != 600 || next.GetNextPageToken() != "" {
		t.Errorf("the second page, under a pseudonym: %v", next)
	}
	if _, code := board(asOwner, &hubv1.GetParticipationBoardRequest{Kind: "Not-A-Kind"}); code != connect.CodeInvalidArgument {
		t.Errorf("a bad kind: %v", code)
	}
	if _, code := board(asOwner, &hubv1.GetParticipationBoardRequest{PageToken: "nope"}); code != connect.CodeInvalidArgument {
		t.Errorf("a bad page token: %v", code)
	}
	if _, code := board(asOwner, &hubv1.GetParticipationBoardRequest{PageSize: -1}); code != connect.CodeInvalidArgument {
		t.Errorf("a negative page size: %v", code)
	}
	if _, code := board(asOwner, &hubv1.GetParticipationBoardRequest{Window: "season", Season: "Nope"}); code != connect.CodeInvalidArgument {
		t.Errorf("an unknown season: %v", code)
	}
	if w, code := board(asOwner, &hubv1.GetParticipationBoardRequest{Kind: "seeding", Window: "week"}); code != 0 || w.GetFrom() == nil || w.GetTo() == nil || w.GetKind() != "seeding" {
		t.Errorf("this week: %v %v", w, code)
	}
	// Public stats: anyone.
	if _, err := r.org.UpdateSettings(ctx, org.Settings{Stats: org.Stats{Public: true}}, store.Actor{}); err != nil {
		t.Fatal(err)
	}
	if got, code := board(anon, &hubv1.GetParticipationBoardRequest{Kind: "seeding"}); code != 0 || len(got.GetEntries()) != 1 {
		t.Errorf("public: %v %v", got, code)
	}

	// Ana's profile sums both her identities' participation, per kind and window.
	p, err := asMember.GetMemberProfile(ctx, connect.NewRequest(&hubv1.GetMemberProfileRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	var parts []string
	for _, x := range p.Msg.GetParticipation() {
		parts = append(parts, x.GetWindow()+"/"+x.GetKind())
		if x.GetWindow() == "all" && (x.GetFrom() != nil || x.GetEvents() != 1) || x.GetWindow() == "week" && x.GetFrom() == nil {
			t.Errorf("participation = %v", x)
		}
		if x.GetKind() == "seeding" && x.GetSeconds() != 1800 || x.GetKind() == "attendance" && x.GetSeconds() != 3600 {
			t.Errorf("participation = %v", x)
		}
	}
	if strings.Join(parts, " ") != "all/attendance all/seeding week/attendance week/seeding month/attendance month/seeding" {
		t.Errorf("participation = %v", parts)
	}
}

// connectMessage is an API error's message without its code.
func connectMessage(err error) string {
	var ce *connect.Error
	if errors.As(err, &ce) {
		return ce.Message()
	}
	return err.Error()
}
