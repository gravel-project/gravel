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

	"github.com/a-h/templ"

	"github.com/gravel-project/gravel/internal/web"
	"github.com/gravel-project/gravel/internal/web/templates"
)

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
		// A member's apps (ADR-0015) on the account page, and the consent page.
		"/fixture/account-apps": func() templ.Component {
			p := base
			p.Title, p.User, p.CSRF = "Account", jo, "csrf"
			p.Flash = &templates.Flash{Kind: "ok", Text: "Revoked. The app can no longer act as you, and must ask again."}
			return templates.Account(templates.AccountPage{Page: p, Identities: identities[:1], Apps: []templates.AppGrant{
				{ClientID: "gravel_a", Name: "Muster", GrantedAt: now, Scopes: []templates.Scope{{Name: "stats:read", Description: "Read leaderboards, match lists and members' stats"}}},
				{ClientID: "gravel_b", Name: "Seeding tracker", GrantedAt: now}}})
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
		"/fixture/members": func() templ.Component {
			p := base
			p.Title, p.User, p.CSRF = "Members", owner, "csrf"
			p.Flash = &templates.Flash{Kind: "ok", Text: "Sam is now a moderator."}
			return templates.Members(templates.MembersPage{Page: p, NextURL: "/members?page=x", Members: []templates.MemberRow{
				{ID: "u1", DisplayName: "Jo", Accounts: "Discord, Steam", Joined: now, Owner: true},
				{ID: "u2", DisplayName: "Sam", Accounts: "Discord", Joined: now, Moderator: true},
				{ID: "u3", DisplayName: "Ana", Accounts: "Discord, Steam", Joined: now}}})
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
		"/fixture/servers", "/fixture/servers-demo", "/fixture/server", "/fixture/server-anonymous", "/fixture/server-game-overlay", "/fixture/boards", "/fixture/member-self", "/fixture/member", "/fixture/members",
		"/fixture/participation", "/fixture/participation-empty", "/fixture/member-participation", "/fixture/recap", "/fixture/recap-nobody",
		"/fixture/server-moderator", "/fixture/server-host", "/fixture/server-host-parked", "/fixture/host-confirm", "/fixture/confirm", "/fixture/log", "/fixture/account-apps", "/fixture/consent",
		"/fixture/account-proof", "/fixture/proof", "/fixture/proof-proven", "/fixture/host-expect"}
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
}
