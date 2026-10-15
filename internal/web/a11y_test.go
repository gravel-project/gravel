package web_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/a-h/templ"
	"google.golang.org/protobuf/types/known/timestamppb"

	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
	"github.com/gravel-project/gravel/gen/gravel/hub/v1/hubv1connect"
	"github.com/gravel-project/gravel/internal/events"
	"github.com/gravel-project/gravel/internal/web"
	"github.com/gravel-project/gravel/internal/web/admin"
	"github.com/gravel-project/gravel/internal/web/templates"
)

// The admin dashboard (ADR-0029) under axe: the built bundle, signed in as a staff member by a
// fake GetMe, since axe's browser carries no session.
type a11yMe struct {
	hubv1connect.UnimplementedIdentityServiceHandler
}

// ListUsers is People's list (gravel#203): the owner, a member with a game's grant and a local
// account (the reset link), and one with no role, so every row's controls are scanned.
func (a11yMe) ListUsers(context.Context, *connect.Request[hubv1.ListUsersRequest]) (*connect.Response[hubv1.ListUsersResponse], error) {
	now := timestamppb.Now()
	return connect.NewResponse(&hubv1.ListUsersResponse{Users: []*hubv1.User{
		{Id: "u1", DisplayName: "Jo", Owner: true, CreatedAt: now, LastLoginAt: now, Identities: []*hubv1.Identity{{Provider: "discord", Subject: "1", DisplayName: "jo"}}},
		{Id: "u2", DisplayName: "Sam", CreatedAt: now, Grants: []*hubv1.RoleGrant{{UserId: "u2", Role: "operator", GameId: "cs2"}, {UserId: "u2", Role: "moderator"}},
			Identities: []*hubv1.Identity{{Provider: "local", Subject: "sam"}}},
		{Id: "u3", DisplayName: "Ana", CreatedAt: now, Identities: []*hubv1.Identity{{Provider: "steam", Subject: "76561190000000003"}}},
	}}), nil
}

func (a11yMe) GetMe(context.Context, *connect.Request[hubv1.GetMeRequest]) (*connect.Response[hubv1.GetMeResponse], error) {
	return connect.NewResponse(&hubv1.GetMeResponse{User: &hubv1.User{Id: "u1", DisplayName: "Jo", Owner: true, Roles: []string{"admin"},
		Capabilities: []string{"members:manage", "servers:operate", "servers:read", "events:read"},
		Grants:       []*hubv1.RoleGrant{{UserId: "u1", Role: "moderator", GameId: "cs2"}}}}), nil
}

type a11yOrg struct {
	hubv1connect.UnimplementedOrganizationServiceHandler
}

func (a11yOrg) GetOrganization(context.Context, *connect.Request[hubv1.GetOrganizationRequest]) (*connect.Response[hubv1.GetOrganizationResponse], error) {
	return connect.NewResponse(&hubv1.GetOrganizationResponse{Organization: &hubv1.Organization{Id: "o1", Name: "Hidden Token Gaming", Owned: true, Demo: true}}), nil
}

// a11yEvents is an events log with every severity, a server's, a member's and an app's events and
// a detail, so the feed (gravel#193) is scanned with everything a row can show.
func (a11yOrg) ListEvents(context.Context, *connect.Request[hubv1.ListEventsRequest]) (*connect.Response[hubv1.ListEventsResponse], error) {
	now := time.Now()
	ago := func(d time.Duration) *timestamppb.Timestamp { return timestamppb.New(now.Add(-d)) }
	return connect.NewResponse(&hubv1.ListEventsResponse{HeadId: 4, NextAfterId: 4, NextBeforeId: 1, Events: []*hubv1.Event{
		{Id: 4, At: ago(time.Minute), Kind: "server.down", Severity: "critical", ServerId: "htg-wardogs-2", GameId: "wardogs",
			Summary: "HTG WARDOGS | NA WEST | #2 is down", Detail: map[string]string{"misses": "6"}},
		{Id: 3, At: ago(5 * time.Minute), Kind: "server.unreachable", Severity: "warning", ServerId: "htg-wardogs-2", GameId: "wardogs", Summary: "HTG WARDOGS | NA WEST | #2 stopped answering its polls"},
		{Id: 2, At: ago(time.Hour), Kind: "moderation.kick", Severity: "notice", ServerId: "htg-cs2-1", GameId: "cs2", ActorUserId: "u1", ActorName: "Jo", Summary: "kick on htg-cs2-1: ok"},
		{Id: 1, At: ago(2 * time.Hour), Kind: "lease.ready", Severity: "info", ServerId: "htg-cs2-1", GameId: "cs2", ActorAppId: "a1", ActorName: "HTG Bot", Summary: "a lease on htg-cs2-1 is ready"},
	}}), nil
}

func (a11yOrg) ListEventKinds(context.Context, *connect.Request[hubv1.ListEventKindsRequest]) (*connect.Response[hubv1.ListEventKindsResponse], error) {
	out := &hubv1.ListEventKindsResponse{}
	for _, k := range events.Kinds() {
		out.Kinds = append(out.Kinds, &hubv1.EventKind{Name: k.Name, Group: events.Group(k.Name), Severity: string(k.Severity), About: k.About})
	}
	return connect.NewResponse(out), nil
}

func (a11yFleet) ListServers(context.Context, *connect.Request[hubv1.ListServersRequest]) (*connect.Response[hubv1.ListServersResponse], error) {
	return connect.NewResponse(&hubv1.ListServersResponse{Servers: []*hubv1.Server{
		{Id: "htg-wardogs-2", Name: "HTG WARDOGS | NA WEST | #2", GameId: "wardogs"}, {Id: "htg-cs2-1", Name: "HTG CS2 #1", GameId: "cs2"}}}), nil
}

func (a11yFleet) ListGames(context.Context, *connect.Request[hubv1.ListGamesRequest]) (*connect.Response[hubv1.ListGamesResponse], error) {
	return connect.NewResponse(&hubv1.ListGamesResponse{Games: []*hubv1.Game{{Id: "wardogs", Name: "War Dogs"}, {Id: "cs2", Name: "Counter-Strike 2"}}}), nil
}

// a11yFleet is a fleet with every health and both alarm severities, so the fleet page (gravel#192)
// is scanned with everything it can show.
type a11yFleet struct {
	hubv1connect.UnimplementedServerServiceHandler
}

// GetServerStatus, GetServerHost and ListServerLeases are a server's controls (gravel#202): a
// running cloud-VM host with a pinned size and an expectation, and a live lease, so every section
// of /admin/servers/{id} is scanned.
func (a11yFleet) GetServerStatus(_ context.Context, req *connect.Request[hubv1.GetServerStatusRequest]) (*connect.Response[hubv1.GetServerStatusResponse], error) {
	return connect.NewResponse(&hubv1.GetServerStatusResponse{Server: &hubv1.Server{Id: req.Msg.GetServerId(), Name: "HTG CS2 #1", GameId: "cs2", Driver: "cloudvm",
		Status: &hubv1.ServerStatus{State: "online"}}}), nil
}

type a11yHosts struct {
	hubv1connect.UnimplementedServerHostServiceHandler
}

func (a11yHosts) GetServerHost(_ context.Context, req *connect.Request[hubv1.GetServerHostRequest]) (*connect.Response[hubv1.GetServerHostResponse], error) {
	return connect.NewResponse(&hubv1.GetServerHostResponse{Host: &hubv1.ServerHost{ServerId: req.Msg.GetServerId(), State: "running", Desired: "running",
		Size: "c-4", PinnedSize: "c-4", Address: "198.51.100.1", HourlyPriceUsd: 0.125, MonthCostUsd: 31.5,
		ExpectedPlayers: 10, ExpectedUntil: timestamppb.New(time.Now().Add(2 * time.Hour)), ExpectedReason: "PUG night",
		Sizes: []*hubv1.HostSize{{Slug: "c-2", Vcpus: 2, MemoryMib: 4096, PriceHourlyUsd: 0.063}, {Slug: "c-4", Vcpus: 4, MemoryMib: 8192, PriceHourlyUsd: 0.125}}}}), nil
}

type a11yLeases struct {
	hubv1connect.UnimplementedServerLeaseServiceHandler
}

func (a11yLeases) ListServerLeases(_ context.Context, req *connect.Request[hubv1.ListServerLeasesRequest]) (*connect.Response[hubv1.ListServerLeasesResponse], error) {
	return connect.NewResponse(&hubv1.ListServerLeasesResponse{Leases: []*hubv1.ServerLease{{Id: "l-1", ServerId: req.Msg.GetServerId(), Holder: "muster",
		Pool: "pug", State: "ready", CreatedAt: timestamppb.Now()}}}), nil
}

func (a11yFleet) GetFleetSummary(context.Context, *connect.Request[hubv1.GetFleetSummaryRequest]) (*connect.Response[hubv1.GetFleetSummaryResponse], error) {
	now := time.Now()
	ago := func(d time.Duration) *timestamppb.Timestamp { return timestamppb.New(now.Add(-d)) }
	return connect.NewResponse(&hubv1.GetFleetSummaryResponse{AsOf: timestamppb.New(now),
		Totals: &hubv1.FleetTotals{Servers: 5, Up: 1, Degraded: 1, Down: 1, Idle: 1, Unknown: 1, Players: 31, Capacity: 190},
		Alarms: []*hubv1.FleetAlarm{
			{Kind: "server.down", Severity: "critical", ServerId: "htg-wardogs-2", Since: ago(3 * time.Minute), Detail: "not answering"},
			{Kind: "backup.stale", Severity: "warning", Since: ago(27 * time.Hour), Detail: "no base backup has succeeded since yesterday"},
		},
		Servers: []*hubv1.FleetServer{
			{Id: "htg-wardogs-2", Name: "HTG WARDOGS | NA WEST | #2", GameId: "wardogs", GameName: "War Dogs", Health: "down", Reason: "not answering", HealthSince: ago(5 * time.Minute), Misses: 6, MaxPlayers: 80},
			{Id: "htg-cs2-1", Name: "HTG CS2 #1", GameId: "cs2", GameName: "Counter-Strike 2", Health: "degraded", Reason: "the host is booting", HostState: "booting", LeaseState: "starting", MaxPlayers: 10},
			{Id: "htg-wardogs-3", Name: "HTG WARDOGS #3", GameId: "wardogs", GameName: "War Dogs", Health: "unknown", Reason: "not polled yet"},
			{Id: "htg-wardogs-1", Name: "HTG WARDOGS | NA WEST | #1", GameId: "wardogs", GameName: "War Dogs", Health: "up", Reason: "answering", Players: 31, MaxPlayers: 100, Map: "Ozeti", ObservedAt: ago(8 * time.Second), PollMs: 120},
			{Id: "htg-cs2-2", Name: "HTG CS2 #2", GameId: "cs2", GameName: "Counter-Strike 2", Health: "idle", Reason: "parked, as asked", HostState: "parked"},
		}}), nil
}

// TestAccessibility runs axe (@axe-core/cli through npx, driving Chrome) over the rendered
// pages: the login page from the real handler, and the account and error pages from fixtures
// so no session is needed. It runs only with A11Y=1 (`make a11y`; CI does), because it needs
// node and a browser. AXE_CHROME_PATH and AXE_CHROMEDRIVER_PATH, when set, name a matching
// Chrome and ChromeDriver pair (CI installs one with browser-driver-manager, because the
// driver axe bundles and the runner's Chrome drift apart); unset, axe uses its own driver and
// the Chrome it finds.
func TestAccessibility(t *testing.T) {
	if os.Getenv("A11Y") == "" {
		t.Skip("set A11Y=1 (make a11y) to run axe over the pages")
	}
	version := os.Getenv("AXE_CLI_VERSION")
	if version == "" {
		version = "4.13.0"
	}
	var browserArgs []string
	if p := os.Getenv("AXE_CHROME_PATH"); p != "" {
		browserArgs = append(browserArgs, "--chrome-path", p)
	}
	if p := os.Getenv("AXE_CHROMEDRIVER_PATH"); p != "" {
		browserArgs = append(browserArgs, "--chromedriver-path", p)
	}
	r := newRig(t)
	theme := web.DefaultTheme()
	theme.Nav = []templates.NavLink{{Label: "Muster", URL: "https://muster.example", Placement: "header"}, {Label: "Rules", URL: "https://example.com/rules", Placement: "footer"}}
	base := templates.Page{Org: templates.Org{Name: "Hidden Token Gaming"}, Theme: theme, HeaderNav: theme.Nav[:1], FooterNav: theme.Nav[1:]}
	jo := &templates.User{ID: "u1", DisplayName: "Jo"}
	owner := &templates.User{ID: "u1", DisplayName: "Jo", Owner: true}
	now := time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)
	identities := []templates.Identity{
		{Provider: "discord", ProviderDisplay: "Discord", Subject: "1", DisplayName: "Jo", AvatarURL: "https://cdn.example/jo.png", Method: "oauth2", VerifiedAt: now, LastLoginAt: &now, CanUnlink: true},
		{Provider: "steam", ProviderDisplay: "Steam", Subject: "76561198000000001", Method: "openid", VerifiedAt: now, CanUnlink: true},
	}
	online := templates.ServerRow{ID: "htg-wardogs-1", Name: "HTG WARDOGS | NA WEST | #1", Game: "War Dogs", Location: "qonzer-slc", Official: true,
		State: "ok", StateLabel: "Online", Players: 24, MaxPlayers: 80, Map: "Ozeti"}
	profileTotals := []templates.TotalsRow{{Label: "All time", Kills: 40, Deaths: 10, KD: "4.00", Time: "2h 00m", Matches: 6},
		{Label: "This week", Kills: 4, Deaths: 2, KD: "2.00", Time: "15m", Matches: 1}, {Label: "Season 02", Kills: 4, Deaths: 2, KD: "2.00", Time: "15m", Matches: 1}}
	fixtures := map[string]func() templ.Component{
		"/fixture/account-unowned": func() templ.Component {
			p := base
			p.Title, p.User, p.CSRF = "Account", jo, "csrf"
			p.Flash = &templates.Flash{Kind: "ok", Text: "Welcome, Jo. Your account is ready."}
			return templates.Account(templates.AccountPage{Page: p, Identities: identities[:1], LinkProviders: []templates.Provider{{Name: "steam", DisplayName: "Steam"}}})
		},
		"/fixture/account-owner": func() templ.Component {
			p := base
			p.Title, p.User, p.CSRF = "Account", owner, "csrf"
			p.Org.Owned = true
			p.Flash = &templates.Flash{Kind: "err", Text: "You cannot unlink your only account."}
			return templates.Account(templates.AccountPage{Page: p, Identities: identities, RoleProviders: []templates.Provider{{Name: "discord", DisplayName: "Discord"}}})
		},
		// The ownership handover (#197): the nominee's prompt, and the owner's pending nomination.
		"/fixture/account-nominated": func() templ.Component {
			p := base
			p.Title, p.User, p.CSRF = "Account", jo, "csrf"
			p.Org.Owned = true
			p.Org.Pending = &templates.PendingOwner{UserID: jo.ID, Name: "Jo", ExpiresAt: now.Add(7 * 24 * time.Hour)}
			return templates.Account(templates.AccountPage{Page: p, Identities: identities})
		},
		"/fixture/account-handover": func() templ.Component {
			p := base
			p.Title, p.User, p.CSRF = "Account", owner, "csrf"
			p.Org.Owned = true
			p.Org.Pending = &templates.PendingOwner{UserID: "u2", Name: "Sam", ExpiresAt: now.Add(-time.Hour), Expired: true}
			return templates.Account(templates.AccountPage{Page: p, Identities: identities})
		},
		// A member's apps (ADR-0015) on the account page, and the consent page.
		"/fixture/account-apps": func() templ.Component {
			p := base
			p.Title, p.User, p.CSRF = "Account", jo, "csrf"
			p.Flash = &templates.Flash{Kind: "ok", Text: "Revoked. The app can no longer act as you, and must ask again."}
			return templates.Account(templates.AccountPage{Page: p, Identities: identities[:1], Apps: []templates.AppGrant{
				{ClientID: "gravel_a", Name: "Muster", GrantedAt: now, Scopes: []templates.Scope{{Name: "stats:read", Description: "Read leaderboards, match lists and members' stats"}}},
				{ClientID: "gravel_b", Name: "Seeding tracker", GrantedAt: now}}})
		},
		// Local accounts (ADR-0026): the login form after a refusal, registration, and the
		// account page offering a username and password.
		"/fixture/login-local": func() templ.Component {
			p := base
			p.Title, p.CanLogIn = "Log in", true
			p.Flash = &templates.Flash{Kind: "err", Text: "That username and password don't match an account that may log in now."}
			return templates.Login(templates.LoginPage{Page: p, Local: true, LocalFirst: true, Register: true, Username: "neckbeard",
				Providers: []templates.Provider{{Name: "discord", DisplayName: "Discord"}}})
		},
		"/fixture/register": func() templ.Component {
			p := base
			p.Title, p.CanLogIn = "Make an account", true
			p.Flash = &templates.Flash{Kind: "err", Text: "A password has at least 12 characters."}
			return templates.Register(templates.RegisterPage{Page: p, Username: "neckbeard", MinPasswordLength: 12})
		},
		"/fixture/account-local": func() templ.Component {
			p := base
			p.Title, p.User, p.CSRF = "Account", jo, "csrf"
			return templates.Account(templates.AccountPage{Page: p, Identities: identities[:1], Local: &templates.LocalForm{MinPasswordLength: 12}})
		},
		"/fixture/reset": func() templ.Component {
			p := base
			p.Title, p.CanLogIn = "Choose a password", true
			p.Flash = &templates.Flash{Kind: "err", Text: "A password has at least 12 characters."}
			return templates.Reset(templates.ResetPage{Page: p, Username: "vps.dad", Set: true, MinPasswordLength: 12, Action: "/auth/local/reset/fixture"})
		},
		"/fixture/consent": func() templ.Component {
			p := base
			p.Title, p.User, p.CSRF = "Sign in to Muster", jo, "csrf"
			return templates.Consent(templates.ConsentPage{Page: p, AppName: "Muster", Wider: true, ReturnHost: "muster.hiddentoken.com",
				Scopes: []templates.Scope{{Name: "stats:read", Description: "Read leaderboards, match lists and members' stats"},
					{Name: "identity:read", Description: "See the members of this hub and the accounts they linked"}},
				Params: templates.AuthorizationParams{ResponseType: "code", ClientID: "gravel_a", RedirectURI: "https://muster.hiddentoken.com/auth/callback",
					Scope: "stats:read identity:read", State: "s", CodeChallenge: "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM", CodeChallengeMethod: "S256"}})
		},
		"/fixture/servers": func() templ.Component {
			p := base
			p.Title = "Servers"
			return templates.Servers(templates.ServersPage{Page: p, Servers: []templates.ServerRow{online, {ID: "friends-1", Name: "Friends' server", Game: "War Dogs", Location: "home", State: "unreachable", StateLabel: "Offline"}}})
		},
		// A demo hub's banner (gravel#112) above the header.
		"/fixture/servers-demo": func() templ.Component {
			p := base
			p.Title = "Servers"
			p.Org.Demo = true
			return templates.Servers(templates.ServersPage{Page: p, Servers: []templates.ServerRow{online}})
		},
		"/fixture/server": func() templ.Component {
			p := base
			p.Title, p.User, p.CSRF = online.Name, jo, "csrf"
			ended := now.Add(42 * time.Minute)
			eight, seven := int32(8), int32(7)
			return templates.Server(templates.ServerPage{Page: p, Server: online, ObservedAt: &now, Scored: true,
				Teams:    []templates.Team{{Name: "Lonestar", Score: 412, Players: &eight}, {Name: "Boreal", Score: 388, Players: &seven}},
				Players:  []templates.PlayerRow{{Name: "Jo", Team: "Lonestar", Kills: 7, Deaths: 2, PingMs: 41, Member: true}, {Name: "Stranger", Team: "Boreal", Kills: 1, Deaths: 5, PingMs: 90}},
				Matches:  []templates.MatchRow{{Started: now, Map: "Ozeti", Players: 24, Kills: 310}, {Started: now, Ended: &ended, Map: "Bakurani", Players: 31, Kills: 402}},
				BoardURL: "/boards?scope=server%3Ahtg-wardogs-1", NextMap: "NorthAmerica"})
		},
		"/fixture/server-anonymous": func() templ.Component {
			p := base
			p.Title = online.Name
			return templates.Server(templates.ServerPage{Page: p, Server: online, PlayersNote: "Log in to see who's on.", MatchesNote: "Match history is private on this hub.", BoardURL: "/boards",
				Join: &templates.Join{Label: "Join by ID", Value: "a968511e-a098-48ec-9bdb-909255478f15", Hint: "Paste it into Join by ID at the bottom left of the Community browser."}})
		},
		// A game's page with its overlay (gravel#66): banner, game links, and the rig's War Dogs
		// accent from /theme.css on the content.
		"/fixture/server-game-overlay": func() templ.Component {
			p := base
			p.Title = online.Name
			p.Game = &templates.Game{ID: "wardogs", Name: "War Dogs", BannerURL: "/fixture/wardogs-banner.png", BannerAlt: "War Dogs",
				Nav: []templates.NavLink{{Label: "Playbook", URL: "https://example.com/wardogs/"}, {Label: "Rules", URL: "https://example.com/wardogs/rules"}}}
			return templates.Server(templates.ServerPage{Page: p, Server: online, PlayersNote: "Log in to see who's on.", MatchesNote: "Match history is private on this hub.", BoardURL: "/boards"})
		},
		"/fixture/boards": func() templ.Component {
			p := base
			p.Title = "Leaderboards"
			cols := []templates.BoardColumn{{Metric: "kills", Label: "Kills", Sorted: true, URL: "/boards?metric=kills"}, {Metric: "deaths", Label: "Deaths", URL: "/boards?metric=deaths"},
				{Metric: "kd", Label: "K/D", URL: "/boards?metric=kd"}, {Metric: "time", Label: "Time on", URL: "/boards?metric=time"}, {Metric: "matches", Label: "Matches", URL: "/boards?metric=matches"}}
			return templates.Boards(templates.BoardsPage{Page: p, Heading: "Leaderboards", Metric: "kills", Columns: cols,
				Scopes:  []templates.Option{{Value: "", Label: "Everyone (official servers)", Selected: true}, {Value: "game:wardogs", Label: "War Dogs"}},
				Windows: []templates.Option{{Value: "all", Label: "All time"}, {Value: "week", Label: "This week", Selected: true}},
				Range:   "Oct 5, 2026 to Oct 11, 2026", NextURL: "/boards?page=x",
				Rows: []templates.BoardRow{{Rank: 1, Name: "Brave Falcon", Pseudonymous: true, Cells: []string{"42", "10", "4.20", "2h 05m", "5"}},
					{Rank: 2, Name: "Jo", Cells: []string{"30", "12", "2.50", "45m", "4"}}}})
		},
		"/fixture/member-self": func() templ.Component {
			p := base
			p.Title, p.User, p.CSRF = "Jo", jo, "csrf"
			p.Flash = &templates.Flash{Kind: "ok", Text: "Leaderboards now show your name."}
			return templates.Member(templates.MemberPage{Page: p, UserID: "u1", DisplayName: "Jo", Self: true, ShowName: true, Totals: profileTotals,
				Recent: []templates.PlayedMatch{{Started: now, Live: true, ServerID: "htg-wardogs-1", Server: online.Name, Map: "Ozeti", Kills: 4, Deaths: 2, Time: "15m"}}})
		},
		"/fixture/member": func() templ.Component {
			p := base
			p.Title = "Sam"
			return templates.Member(templates.MemberPage{Page: p, UserID: "u2", DisplayName: "Sam", ShowName: true, Totals: profileTotals})
		},
		// The participation board (ADR-0014): a picker of kinds, a window, a ranked page.
		"/fixture/participation": func() templ.Component {
			p := base
			p.Title = "Participation"
			return templates.Participation(templates.ParticipationPage{Page: p, Kind: "Seeding", Range: "Oct 5, 2026 to Oct 11, 2026", NextURL: "/participation?page=x",
				Kinds:   []templates.Option{{Value: "attendance", Label: "Attendance"}, {Value: "seeding", Label: "Seeding", Selected: true}},
				Windows: []templates.Option{{Value: "all", Label: "All time"}, {Value: "week", Label: "This week", Selected: true}},
				Rows: []templates.ParticipationRow{{Rank: 1, Name: "Brave Falcon", Pseudonymous: true, Time: "2h 05m", Times: 5},
					{Rank: 2, Name: "Jo", UserID: "u1", Time: "45m", Times: 2}}})
		},
		// A session recap (ADR-0018) in its game's overlay: the numbers, the matches, the top five.
		"/fixture/recap": func() templ.Component {
			p := base
			p.Title = online.Name + " recap"
			p.Game = &templates.Game{ID: "wardogs", Name: "War Dogs"}
			return templates.Recap(templates.RecapPage{Page: p, ServerID: online.ID, ServerName: online.Name, BoardURL: "/boards?scope=server%3Ahtg-wardogs-1",
				Window: "Wed Oct 7, 2026, 18:00 to 22:00 CDT", Floor: 20, PeakPlayers: 31, Minutes: 236, MinutesAtFloor: 174, Players: 44,
				Matches: []templates.RecapMatch{{Started: "18:02", Ended: "18:44", Map: "Ozeti", Players: 28, Teams: []templates.Team{{Name: "Lonestar", Score: 412}, {Name: "Boreal", Score: 388}}},
					{Started: "21:30", Map: "Bakurani", Players: 19}},
				Top: []templates.BoardRow{{Rank: 1, Name: "Brave Falcon", Pseudonymous: true, Cells: []string{"42", "10", "4.20", "2h 05m", "2"}},
					{Rank: 2, Name: "Jo", UserID: "u1", Cells: []string{"30", "12", "2.50", "45m", "1"}}}})
		},
		"/fixture/recap-nobody": func() templ.Component {
			p := base
			p.Title = online.Name + " recap"
			return templates.Recap(templates.RecapPage{Page: p, ServerID: online.ID, ServerName: online.Name, BoardURL: "/boards", Window: "Wed Oct 7, 2026, 18:00 to 22:00 CDT"})
		},
		"/fixture/participation-empty": func() templ.Component {
			p := base
			p.Title = "Participation"
			return templates.Participation(templates.ParticipationPage{Page: p, Empty: true})
		},
		"/fixture/member-participation": func() templ.Component {
			p := base
			p.Title = "Sam"
			return templates.Member(templates.MemberPage{Page: p, UserID: "u2", DisplayName: "Sam", ShowName: true, Totals: profileTotals,
				Participation: []templates.KindTable{
					{Label: "Event attendance", Rows: []templates.KindTotalsRow{{Label: "All time", Times: 2, Time: "2h 00m"}, {Label: "This week", Times: 0, Time: "0m"}}},
					{Label: "Seeding", Rows: []templates.KindTotalsRow{{Label: "All time", Times: 3, Time: "1h 30m"}, {Label: "This week", Times: 1, Time: "10m"}}},
				}})
		},
		"/fixture/server-moderator": func() templ.Component {
			p := base
			p.Title, p.User, p.CSRF = online.Name, owner, "csrf"
			p.Flash = &templates.Flash{Kind: "ok", Text: "Kicked Stranger."}
			return templates.Server(templates.ServerPage{Page: p, Server: online, ObservedAt: &now, BoardURL: "/boards",
				Players: []templates.PlayerRow{{Name: "Jo", Subject: "76561190000000001", Team: "Lonestar", Kills: 7, Deaths: 2, PingMs: 41, Member: true},
					{Name: "Stranger", Subject: "76561190000000002", Team: "Boreal", Kills: 1, Deaths: 5, PingMs: 90}},
				Mod: &templates.ModControls{Kick: true, Ban: true, Unban: true, Message: true, Broadcast: true,
					Bans: []templates.BanRow{{Subject: "76561190000000009", Name: "76561190000000009", Reason: "cheating", By: "Jo", At: now}}}})
		},
		"/fixture/server-host": func() templ.Component {
			p := base
			p.Title, p.User, p.CSRF = "HTG CS2", owner, "csrf"
			return templates.Server(templates.ServerPage{Page: p, Server: online, ObservedAt: &now, BoardURL: "/boards",
				Mod: &templates.ModControls{Kick: true, Ban: true, Unban: true, Broadcast: true},
				Host: &templates.HostPanel{State: "running", StateLabel: "Running", Since: now, Size: "c-2", Address: "203.0.113.50", Reserved: true,
					HourlyUSD: 0.0625, MonthUSD: 3.5, HoursMonth: 56, BootSeconds: 70, ReadySeconds: 1500, ParkAfter: 30 * time.Minute,
					ExpectedPlayers: 10, ExpectedUntil: now.Add(3 * time.Hour), ExpectedReason: "PUG 3 filled", ExpectMax: 20, CanExpect: true,
					Leases: []templates.LeaseRow{{ID: "l2", State: "ready", Ref: "PUG 3", Holder: "an app", Plugins: "matchzy loaded", Created: now, Live: true},
						{ID: "l1", State: "released", Ref: "test", Holder: "a moderator", Reason: "nobody on for 15m0s", Created: now}},
					CanPark: true, CanResize: true, Sizes: []templates.HostSizeOption{{Slug: "c-4", Label: "c-4 · 4 vCPU · 8 GB · $0.125/h"}}}})
		},
		"/fixture/server-host-parked": func() templ.Component {
			p := base
			p.Title, p.User, p.CSRF = "HTG CS2", owner, "csrf"
			return templates.Server(templates.ServerPage{Page: p, Server: online, BoardURL: "/boards", Mod: &templates.ModControls{},
				Host: &templates.HostPanel{State: "parked", StateLabel: "Parked", Since: now, Address: "203.0.113.50", Reserved: true,
					ErrorCode: "cloud_rate_limited", CanUnpark: true, CanDestroy: true}})
		},
		"/fixture/host-confirm": func() templ.Component {
			p := base
			p.Title, p.User, p.CSRF = "Confirm", owner, "csrf"
			return templates.HostConfirm(templates.HostConfirmPage{Page: p, ServerID: "htg-cs2-1", ServerName: "HTG CS2", Action: "resize", Size: "c-4",
				Question: "Resize HTG CS2 to c-4?", Warning: "Everyone on is disconnected for a few minutes.", OfferForce: true})
		},
		// Proof in game (gravel#5): the account page's offer, and a code's page open and proven.
		"/fixture/account-proof": func() templ.Component {
			p := base
			p.Title, p.User, p.CSRF = "Account", jo, "csrf"
			return templates.Account(templates.AccountPage{Page: p, Identities: identities, ProofServers: []templates.ProofServer{{ID: "htg-cs2-1", Name: "HTG CS2"}},
				ProofAccounts: identities[1:]})
		},
		"/fixture/proof": func() templ.Component {
			p := base
			p.Title, p.User = "Your code", jo
			return templates.Proof(templates.ProofPage{Page: p, Code: "7K3M-9QXA", Instructions: "Join the server, then type in chat: !link 7K3M-9QXA",
				ServerName: "HTG CS2", Join: &templates.Join{Label: "Connect", Value: "203.0.113.50:27015", Hint: "In CS2's console: connect 203.0.113.50:27015"},
				ExpiresAt: now.Add(10 * time.Minute), Status: templates.ProofStatus{ID: "p1", State: "open", ProviderDisplay: "Steam"}})
		},
		"/fixture/proof-proven": func() templ.Component {
			p := base
			p.Title, p.User = "Your code", jo
			return templates.Proof(templates.ProofPage{Page: p, Code: "7K3M-9QXA", Instructions: "Join the server, then type in chat: !link 7K3M-9QXA",
				ServerName: "HTG CS2", ExpiresAt: now.Add(10 * time.Minute), Status: templates.ProofStatus{ID: "p1", State: "proven", ProviderDisplay: "Steam", Subject: "76561198000000001"}})
		},
		"/fixture/host-expect": func() templ.Component {
			p := base
			p.Title, p.User, p.CSRF = "Confirm", owner, "csrf"
			return templates.HostConfirm(templates.HostConfirmPage{Page: p, ServerID: "htg-cs2-1", ServerName: "HTG CS2", Action: "expect", Players: "10", For: "3h",
				Reason: "PUG 3 filled", Question: "Expect 10 players on HTG CS2 for the next 3h?", Warning: "Until then the machine is sized for them."})
		},
		"/fixture/result": func() templ.Component {
			p := base
			p.Title, p.User = "Alpha vs Bravo", jo
			return templates.Result(templates.ResultPage{Page: p, ID: "r1", ServerID: "htg-cs2-1", ServerName: "HTG CS2", Team1: "Alpha", Team2: "Bravo",
				Score1: 1, Winner: "team1", Started: "Oct 14, 01:00 UTC", Ended: true, Maps: []templates.ResultMap{{Number: 1, Name: "de_nuke", Score1: 13, Score2: 9,
					Winner: "team1", DemoURL: "/results/r1/demo/0", Players1: []templates.ResultPlayer{{Name: "Jo", UserID: "u1", Kills: 21, Deaths: 12}},
					Players2: []templates.ResultPlayer{{Name: "Brave Falcon", Pseudonymous: true, Kills: 15, Deaths: 18}}}}})
		},
		"/fixture/confirm": func() templ.Component {
			p := base
			p.Title, p.User, p.CSRF = "Confirm", owner, "csrf"
			return templates.Confirm(templates.ConfirmPage{Page: p, ServerID: "htg-wardogs-1", ServerName: online.Name, Action: "ban",
				Question: "Ban Stranger from " + online.Name + "?", Subject: "76561190000000002", Name: "Stranger", Reason: "cheating"})
		},
		"/fixture/log": func() templ.Component {
			p := base
			p.Title, p.User = "Moderation log", owner
			return templates.Audit(templates.AuditPage{Page: p, ServerID: "htg-wardogs-1", ServerName: online.Name, NextURL: "/servers/htg-wardogs-1/log?page=x",
				Entries: []templates.AuditRow{{At: now, Actor: "Jo", Action: "ban", Target: "76561190000000009", Detail: "cheating", Outcome: "ok", OK: true},
					{At: now, Actor: "htg-bot for Discord 42", Action: "kick", Target: "76561190000000002", Detail: "afk", Outcome: "player not found"}}})
		},
		"/fixture/error": func() templ.Component {
			p := base
			p.Title = "Login attempt expired"
			return templates.Error(templates.ErrorPage{Page: p, Message: "This login attempt expired or was already used. Start again."})
		},
	}
	mux := http.NewServeMux()
	// `make a11y` builds the bundle first; without it /admin would scan a 503.
	dashboard := admin.New(admin.Bundle())
	if !dashboard.Built() {
		t.Fatal("no admin bundle in this build: make a11y builds it (make admin)")
	}
	dashboard.Register(mux)
	mux.Handle(hubv1connect.NewIdentityServiceHandler(a11yMe{}))
	mux.Handle(hubv1connect.NewOrganizationServiceHandler(a11yOrg{}))
	mux.Handle(hubv1connect.NewServerServiceHandler(a11yFleet{}))
	mux.Handle(hubv1connect.NewServerHostServiceHandler(a11yHosts{}))
	mux.Handle(hubv1connect.NewServerLeaseServiceHandler(a11yLeases{}))
	for path, build := range fixtures {
		mux.HandleFunc("GET "+path, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_ = build().Render(r.Context(), w)
		})
	}
	// Everything else (the login page, /theme.css, /static/) comes from the real rig.
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		resp, err := http.Get(r.srv.URL + req.URL.RequestURI()) //nolint:noctx // test proxy
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer func() { _ = resp.Body.Close() }()
		for k, v := range resp.Header {
			w.Header()[k] = v
		}
		w.WriteHeader(resp.StatusCode)
		buf := make([]byte, 64<<10)
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				_, _ = w.Write(buf[:n])
			}
			if err != nil {
				break
			}
		}
	}))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	paths := []string{"/login", "/fixture/account-unowned", "/fixture/account-owner", "/fixture/error",
		"/fixture/account-nominated", "/fixture/account-handover",
		"/fixture/servers", "/fixture/servers-demo", "/fixture/server", "/fixture/server-anonymous", "/fixture/server-game-overlay", "/fixture/boards", "/fixture/member-self", "/fixture/member",
		"/fixture/participation", "/fixture/participation-empty", "/fixture/member-participation", "/fixture/recap", "/fixture/recap-nobody",
		"/fixture/server-moderator", "/fixture/server-host", "/fixture/server-host-parked", "/fixture/host-confirm", "/fixture/confirm", "/fixture/log", "/fixture/account-apps", "/fixture/consent",
		"/fixture/account-proof", "/fixture/proof", "/fixture/proof-proven", "/fixture/host-expect", "/fixture/result",
		"/fixture/login-local", "/fixture/register", "/fixture/account-local", "/fixture/reset",
		"/admin", "/admin/events", "/admin/people", "/admin/servers/htg-cs2-1", "/admin/access", "/admin/nowhere"}
	// What each dashboard route shows once rendered in the browser.
	adminRenders := map[string]string{"/admin": ">HTG WARDOGS | NA WEST | #1</a>", "/admin/events": ">HTG WARDOGS | NA WEST | #2 is down</p>", "/admin/people": ">Sam</a>", "/admin/servers/htg-cs2-1": ">Release the lease</button>", "/admin/access": ">Your access</h1>", "/admin/nowhere": ">Not found</h1>"}
	// The same, as text on the page, for the viewport script.
	adminRenderText := map[string]string{"/admin": "HTG WARDOGS | NA WEST | #1", "/admin/events": "HTG WARDOGS | NA WEST | #2 is down", "/admin/people": "Add a local account", "/admin/servers/htg-cs2-1": "Release the lease", "/admin/access": "Your access"}
	// A fixture left out of the list would never be scanned and the run would still pass.
	for path := range fixtures {
		if !slices.Contains(paths, path) {
			t.Errorf("fixture %s is not in the list axe scans", path)
		}
	}
	for _, path := range paths {
		t.Run(strings.TrimPrefix(path, "/"), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			args := append([]string{"--yes", "@axe-core/cli@" + version, srv.URL + path, "--exit"}, browserArgs...)
			if want, ok := adminRenders[path]; ok {
				// The dashboard renders after its first calls answer; axe over a page still
				// loading (or one the CSP blanked) would pass, so the DOM must show the page.
				args = append(args, "--load-delay", "2000")
				renderedDOM(ctx, t, srv.URL+path, want)
			}
			cmd := exec.CommandContext(ctx, "npx", args...)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("axe found violations on %s:\n%s", path, out)
			}
			if !strings.Contains(string(out), "0 violations found") && !strings.Contains(string(out), "Violations: 0") && !strings.Contains(strings.ToLower(string(out)), "no violations") {
				t.Logf("axe output for %s:\n%s", path, out)
			}
		})
	}
	// The dashboard at a phone's and an iPad's sizes (gravel#192): no sideways scroll and no axe
	// violation at either, which @axe-core/cli above cannot check (it scans at Chrome's default).
	t.Run("admin-viewports", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		chrome := os.Getenv("AXE_CHROME_PATH")
		if chrome == "" {
			var err error
			if chrome, err = exec.LookPath("google-chrome"); err != nil {
				t.Fatal("no Chrome: set AXE_CHROME_PATH or put google-chrome on the PATH")
			}
		}
		// The script resolves puppeteer-core and axe-core from web/admin's node_modules (make admin).
		cmd := exec.CommandContext(ctx, "node", "../../web/admin/scripts/viewport.mjs", chrome,
			srv.URL+"/admin", adminRenderText["/admin"], srv.URL+"/admin/events", adminRenderText["/admin/events"],
			srv.URL+"/admin/people", adminRenderText["/admin/people"], srv.URL+"/admin/servers/htg-cs2-1", adminRenderText["/admin/servers/htg-cs2-1"], srv.URL+"/admin/access", adminRenderText["/admin/access"])
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("the dashboard at phone and iPad sizes:\n%s", out)
		}
		t.Logf("%s", out)
	})

}

// renderedDOM loads url in headless Chrome (AXE_CHROME_PATH, else google-chrome on the PATH) and
// fails unless the DOM it settles on contains want, and the demo banner: the dashboard ran its
// script under the CSP and drew the page from its calls.
func renderedDOM(ctx context.Context, t *testing.T, url, want string) {
	t.Helper()
	chrome := os.Getenv("AXE_CHROME_PATH")
	if chrome == "" {
		chrome = "google-chrome"
	}
	out, err := exec.CommandContext(ctx, chrome, "--headless=new", "--disable-gpu", "--no-sandbox", "--virtual-time-budget=5000", "--dump-dom", url).Output()
	if err != nil {
		t.Fatalf("chrome --dump-dom %s: %v", url, err)
	}
	dom := string(out)
	for _, s := range []string{want, templates.DemoNotice} {
		if !strings.Contains(dom, s) {
			t.Fatalf("%s did not render %q in the browser; the DOM:\n%s", url, s, dom)
		}
	}
}
