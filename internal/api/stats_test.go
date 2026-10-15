package api_test

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
	"github.com/gravel-project/gravel/gen/gravel/hub/v1/hubv1connect"
	"github.com/gravel-project/gravel/internal/apps"
	"github.com/gravel-project/gravel/internal/authz"
	"github.com/gravel-project/gravel/internal/identity"
	"github.com/gravel-project/gravel/internal/org"
	"github.com/gravel-project/gravel/internal/stats"
	"github.com/gravel-project/gravel/internal/store"
)

// fakeStatsStore is the little of the stats store a profile and the participation API use: the
// name choice, a player's totals and recent matches, members shown by name, and participation
// kept in memory. The SQL is tested against Postgres in internal/store.
type fakeStatsStore struct {
	stats.Store // what a test does not use panics
	mu          sync.Mutex
	showName    map[uuid.UUID]bool
	keys        []store.PlayerKey
	games       []string
	members     map[store.PlayerKey]store.BoardMember
	events      []store.ParticipationEvent
	activity    store.Activity // PlayerActivity's answer
	activityQ   store.ActivityQuery
}

func (f *fakeStatsStore) PlayerActivity(_ context.Context, q store.ActivityQuery) (store.Activity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.activityQ = q
	return f.activity, nil
}

func (f *fakeStatsStore) ShowNameOnBoards(_ context.Context, id uuid.UUID) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.showName[id], nil
}

func (f *fakeStatsStore) SetShowNameOnBoards(_ context.Context, id uuid.UUID, show bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.showName[id] = show
	return nil
}

func (f *fakeStatsStore) PlayerTotals(_ context.Context, q store.PlayerTotalsQuery) (store.BoardRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keys, f.games = q.Keys, append(f.games, q.GameID)
	if q.From.IsZero() {
		return store.BoardRow{Kills: 40, Deaths: 10, SecondsOn: 7200, Matches: 6}, nil
	}
	return store.BoardRow{Kills: 4, Deaths: 2, SecondsOn: 900, Matches: 1}, nil
}

func (f *fakeStatsStore) PlayerMatches(_ context.Context, _ uuid.UUID, _ []store.PlayerKey, _ int) ([]store.PlayerMatch, error) {
	at := time.Date(2026, 10, 10, 20, 0, 0, 0, time.UTC)
	return []store.PlayerMatch{{Match: store.Match{ID: 7, ServerID: "wd-1", StartedAt: at, Map: "Ozeti"}, ServerName: "War Dogs #1", Kills: 4, Deaths: 2, SecondsOn: 900}}, nil
}

// Who may read a member's profile: the member, the owner, an app with stats:read, or anyone when
// the member shows their name and stats are public. Everyone else gets not_found, whatever the
// reason, so a profile never says who plays under a pseudonym.
func TestGetMemberProfileAccess(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	ana := r.user(t, "Ana", "100")
	if err := r.idSt.LinkIdentity(ctx, store.Identity{UserID: ana.ID, Provider: "steam", Subject: "76561190000000001", VerificationMethod: identity.MethodOpenID, VerifiedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	ben := r.user(t, "Ben", "200")
	owner := r.user(t, "Olive", "300")
	if _, err := r.org.Claim(ctx, r.token, store.Actor{UserID: &owner.ID}); err != nil {
		t.Fatal(err)
	}
	clientOf := func(c *http.Client) hubv1connect.StatsServiceClient {
		return hubv1connect.NewStatsServiceClient(c, r.srv.URL)
	}
	anon := clientOf(http.DefaultClient)
	asAna := clientOf(cookieClient(http.DefaultClient, r.login(t, ana.ID)))
	asBen := clientOf(cookieClient(http.DefaultClient, r.login(t, ben.ID)))
	asOwner := clientOf(cookieClient(http.DefaultClient, r.login(t, owner.ID)))
	asBot := clientOf(bearerClient(http.DefaultClient, r.bearer(t, "bot", apps.ScopeStatsRead)))
	asOther := clientOf(bearerClient(http.DefaultClient, r.bearer(t, "other", apps.ScopeIdentityRead)))
	get := func(c hubv1connect.StatsServiceClient, id string) (*hubv1.GetMemberProfileResponse, connect.Code) {
		resp, err := c.GetMemberProfile(ctx, connect.NewRequest(&hubv1.GetMemberProfileRequest{UserId: id}))
		if err != nil {
			return nil, connect.CodeOf(err)
		}
		return resp.Msg, 0
	}
	anaID := ana.ID.String()

	// Ana's own profile, by her id or by none: every linked identity counts.
	p, code := get(asAna, "")
	if code != 0 || p.GetUserId() != anaID || !p.GetSelf() || p.GetDisplayName() != "Ana" || p.GetShowName() {
		t.Fatalf("own profile: %v %v", p, code)
	}
	if len(r.stats.keys) != 2 {
		t.Errorf("the profile summed %v, want both of Ana's identities", r.stats.keys)
	}
	if len(p.GetTotals()) != 3 || p.GetTotals()[0].GetWindow() != "all" || p.GetTotals()[0].GetKills() != 40 || p.GetTotals()[0].GetKd() != 4 ||
		p.GetTotals()[0].GetFrom() != nil || p.GetTotals()[1].GetFrom() == nil {
		t.Errorf("totals = %v", p.GetTotals())
	}
	if len(p.GetRecent()) != 1 || p.GetRecent()[0].GetServerName() != "War Dogs #1" || p.GetRecent()[0].GetEndedAt() != nil {
		t.Errorf("recent = %v", p.GetRecent())
	}

	// Private stats, Ana under a pseudonym: only the owner and an app with stats:read.
	for name, c := range map[string]hubv1connect.StatsServiceClient{"owner": asOwner, "the bot": asBot} {
		if p, code := get(c, anaID); code != 0 || p.GetSelf() {
			t.Errorf("%s: %v %v", name, p, code)
		}
	}
	for name, c := range map[string]hubv1connect.StatsServiceClient{"anonymous": anon, "another member": asBen, "an app without the scope": asOther} {
		if _, code := get(c, anaID); code != connect.CodeNotFound {
			t.Errorf("%s: %v, want not_found", name, code)
		}
	}

	// Ana shows her name, but stats are private: still not_found for strangers.
	if _, err := asAna.SetBoardName(ctx, connect.NewRequest(&hubv1.SetBoardNameRequest{ShowName: true})); err != nil {
		t.Fatal(err)
	}
	if _, code := get(anon, anaID); code != connect.CodeNotFound {
		t.Errorf("a shown name with private stats: %v", code)
	}
	// Public stats and a shown name: anyone.
	if _, err := r.org.UpdateSettings(ctx, org.Settings{Stats: org.Stats{Public: true, Seasons: []org.Season{
		{Name: "Now", Game: "wardogs", From: time.Now().AddDate(0, 0, -1).Format(time.DateOnly), To: time.Now().AddDate(0, 0, 2).Format(time.DateOnly)}}}}, store.Actor{}); err != nil {
		t.Fatal(err)
	}
	r.stats.games = nil
	if p, code := get(anon, anaID); code != 0 || !p.GetShowName() || len(p.GetTotals()) != 4 || p.GetTotals()[3].GetSeason() != "Now" {
		t.Errorf("public profile: %v %v", p, code)
	} else if r.stats.games[3] != "wardogs" {
		t.Errorf("a game's season did not filter to the game: %v", r.stats.games)
	}
	// Public stats, a pseudonymous member: not_found.
	if _, code := get(anon, ben.ID.String()); code != connect.CodeNotFound {
		t.Errorf("a pseudonymous member's profile: %v", code)
	}

	if _, code := get(anon, uuid.New().String()); code != connect.CodeNotFound {
		t.Errorf("an unknown member: %v", code)
	}
	if _, code := get(anon, "not-a-uuid"); code != connect.CodeInvalidArgument {
		t.Errorf("a bad id: %v", code)
	}
	if _, code := get(anon, ""); code != connect.CodeUnauthenticated {
		t.Errorf("no id and no session: %v", code)
	}
}

// GetActivity is staff's, in person, with stats:read everywhere: a member, a game's moderator and
// an app are refused; the hours come as the whole week, averaged over the window's weeks; a
// regular gone quiet is shown as a board shows them.
func TestGetActivity(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	owner := r.user(t, "Olive", "300")
	if _, err := r.org.Claim(ctx, r.token, store.Actor{UserID: &owner.ID}); err != nil {
		t.Fatal(err)
	}
	mod := r.user(t, "Mo", "400")
	r.grant(t, owner.ID, mod.ID, "moderator")
	gameMod := r.user(t, "Gil", "500")
	if _, err := r.ids.SetRole(ctx, authz.ForMember(owner.ID, true, nil), gameMod.ID, "moderator", "wardogs", true); err != nil {
		t.Fatal(err)
	}
	member := r.user(t, "Ana", "100")
	shown := store.PlayerKey{Provider: "steam", Subject: "76561190000000001"}
	r.stats.members[shown] = store.BoardMember{UserID: member.ID, DisplayName: "Ana", ShowName: true}
	last := time.Date(2026, 8, 30, 21, 0, 0, 0, time.UTC)
	r.stats.activity = store.Activity{
		Hours:    []store.ActivityHour{{Day: 3, Hour: 20, PlayerHours: 12, Peak: 5}},
		Sessions: 40, Players: 9, MedianSeconds: 1830.4, Returning: 4, ActiveDays: 18, New: 3, Lost: 2,
		Quiet: []store.QuietRegular{{Key: shown, DaysBefore: 6, LastSeen: last, LastServer: "wd-1", Seconds: 7200},
			{Key: store.PlayerKey{Provider: "steam", Subject: "76561190000000002"}, DaysBefore: 4, LastSeen: last, LastServer: "wd-1", Seconds: 3600}},
	}
	clientOf := func(c *http.Client) hubv1connect.StatsServiceClient {
		return hubv1connect.NewStatsServiceClient(c, r.srv.URL)
	}
	call := func(c hubv1connect.StatsServiceClient, window string) (*hubv1.GetActivityResponse, connect.Code) {
		resp, err := c.GetActivity(ctx, connect.NewRequest(&hubv1.GetActivityRequest{Window: window}))
		if err != nil {
			return nil, connect.CodeOf(err)
		}
		return resp.Msg, 0
	}
	for name, c := range map[string]hubv1connect.StatsServiceClient{
		"anonymous":      clientOf(http.DefaultClient),
		"a member":       clientOf(cookieClient(http.DefaultClient, r.login(t, member.ID))),
		"a game's staff": clientOf(cookieClient(http.DefaultClient, r.login(t, gameMod.ID))),
		"an app":         clientOf(bearerClient(http.DefaultClient, r.bearer(t, "bot", apps.ScopeStatsRead))),
	} {
		if _, code := call(c, ""); code != connect.CodeUnauthenticated && code != connect.CodePermissionDenied {
			t.Errorf("%s was answered: %v", name, code)
		}
	}
	asMod := clientOf(cookieClient(http.DefaultClient, r.login(t, mod.ID)))
	got, code := call(asMod, "")
	if code != 0 {
		t.Fatalf("a moderator: %v", code)
	}
	if got.GetWindow() != "30d" || len(got.GetHours()) != 168 || got.GetSessions() != 40 || got.GetPlayers() != 9 || got.GetMedianSessionSeconds() != 1830 ||
		got.GetReturningPlayers() != 4 || got.GetMeanActiveDays() != 2 || got.GetNewPlayers() != 3 || got.GetLostPlayers() != 2 || got.GetRegularDays() != 4 {
		t.Errorf("activity = %v", got)
	}
	if q := r.stats.activityQ; q.To.Sub(q.From) != 30*24*time.Hour || q.From.Sub(q.RegularFrom) != 8*7*24*time.Hour || q.RegularDays != 4 {
		t.Errorf("query = %+v", q)
	}
	// Wednesday 20:00 is the 3rd day's 21st hour; a 30-day window holds it four or five times.
	wed := got.GetHours()[2*24+20]
	if wed.GetDay() != 3 || wed.GetHour() != 20 || wed.GetPeak() != 5 || (wed.GetAverage() != 3 && wed.GetAverage() != 2.4) {
		t.Errorf("Wednesday 20:00 = %v", wed)
	}
	q := got.GetQuiet()
	if len(q) != 2 || q[0].GetName() != "Ana" || q[0].GetPseudonymous() || q[0].GetUserId() != member.ID.String() || q[0].GetHours() != 2 ||
		!q[0].GetLastSeen().AsTime().Equal(last) || q[0].GetLastServerId() != "wd-1" || !q[1].GetPseudonymous() || q[1].GetName() == "" || q[1].GetUserId() != "" {
		t.Errorf("quiet = %v", q)
	}
	if _, code := call(asMod, "1y"); code != connect.CodeInvalidArgument {
		t.Errorf("an unknown window: %v", code)
	}
	if got, code := call(asMod, "7d"); code != 0 || r.stats.activityQ.To.Sub(r.stats.activityQ.From) != 7*24*time.Hour || got.GetWindow() != "7d" {
		t.Errorf("7d: %v %v", got, code)
	}
}
