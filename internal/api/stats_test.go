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
	"github.com/gravel-project/gravel/internal/identity"
	"github.com/gravel-project/gravel/internal/org"
	"github.com/gravel-project/gravel/internal/stats"
	"github.com/gravel-project/gravel/internal/store"
)

// fakeStatsStore is the little of the stats store a profile reads: the name choice, a player's
// totals and recent matches. The SQL is tested against Postgres in internal/store.
type fakeStatsStore struct {
	stats.Store // what a test does not use panics
	mu          sync.Mutex
	showName    map[uuid.UUID]bool
	keys        []store.PlayerKey
	games       []string
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
	if _, err := r.org.Claim(ctx, r.token, owner.ID); err != nil {
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
		{Name: "Now", Game: "wardogs", From: time.Now().AddDate(0, 0, -1).Format(time.DateOnly), To: time.Now().AddDate(0, 0, 2).Format(time.DateOnly)}}}}); err != nil {
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
