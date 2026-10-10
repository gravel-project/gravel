package web_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
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
		"/fixture/servers": func() templ.Component {
			p := base
			p.Title = "Servers"
			return templates.Servers(templates.ServersPage{Page: p, Servers: []templates.ServerRow{online, {ID: "friends-1", Name: "Friends' server", Game: "War Dogs", Location: "home", State: "unreachable", StateLabel: "Offline"}}})
		},
		"/fixture/server": func() templ.Component {
			p := base
			p.Title, p.User, p.CSRF = online.Name, jo, "csrf"
			ended := now.Add(42 * time.Minute)
			return templates.Server(templates.ServerPage{Page: p, Server: online, ObservedAt: &now, Teams: []templates.Team{{Name: "Lonestar", Score: 412}, {Name: "Boreal", Score: 388}},
				Players:  []templates.PlayerRow{{Name: "Jo", Team: "Lonestar", Kills: 7, Deaths: 2, PingMs: 41, Member: true}, {Name: "Stranger", Team: "Boreal", Kills: 1, Deaths: 5, PingMs: 90}},
				Matches:  []templates.MatchRow{{Started: now, Map: "Ozeti", Players: 24, Kills: 310}, {Started: now, Ended: &ended, Map: "Bakurani", Players: 31, Kills: 402}},
				BoardURL: "/boards?scope=server%3Ahtg-wardogs-1"})
		},
		"/fixture/server-anonymous": func() templ.Component {
			p := base
			p.Title = online.Name
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
		"/fixture/members": func() templ.Component {
			p := base
			p.Title, p.User, p.CSRF = "Members", owner, "csrf"
			p.Flash = &templates.Flash{Kind: "ok", Text: "Sam is now a moderator."}
			return templates.Members(templates.MembersPage{Page: p, NextURL: "/members?page=x", Members: []templates.MemberRow{
				{ID: "u1", DisplayName: "Jo", Accounts: "Discord, Steam", Joined: now, Owner: true},
				{ID: "u2", DisplayName: "Sam", Accounts: "Discord", Joined: now, Moderator: true},
				{ID: "u3", DisplayName: "Ana", Accounts: "Discord, Steam", Joined: now}}})
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

	for _, path := range []string{"/login", "/fixture/account-unowned", "/fixture/account-owner", "/fixture/error",
		"/fixture/servers", "/fixture/server", "/fixture/server-anonymous", "/fixture/boards", "/fixture/member-self", "/fixture/member", "/fixture/members"} {
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
