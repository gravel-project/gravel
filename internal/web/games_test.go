package web_test

import (
	"context"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gravel-project/gravel/discord/modules/servercards"
	"github.com/gravel-project/gravel/gen/gravel/hub/v1/hubv1connect"
	"github.com/gravel-project/gravel/internal/api"
	"github.com/gravel-project/gravel/internal/identity"
	"github.com/gravel-project/gravel/internal/identity/identitytest"
	"github.com/gravel-project/gravel/internal/org"
	"github.com/gravel-project/gravel/internal/org/orgtest"
	"github.com/gravel-project/gravel/internal/session"
	"github.com/gravel-project/gravel/internal/store"
	"github.com/gravel-project/gravel/internal/web"
)

// The game overlay's rules in /theme.css (gravel#66): the game's accent inside its pages, a
// rule for both schemes with the organization's accent standing in, and nothing from a game id
// or a value that could escape the stylesheet.
func TestGameOverlayStylesheet(t *testing.T) {
	r := newRig(t)
	_, css := get(t, browser(t), r.srv.URL+"/theme.css")
	for _, want := range []string{
		"[data-game=\"wardogs\"]{--accent:#8a5a00}\n",
		// No dark accent: the organization's dark accent (the default) keeps dark mode readable.
		"@media (prefers-color-scheme: dark){[data-game=\"wardogs\"]{--accent:#f29a1a}}\n",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("theme.css lacks %q:\n%s", want, css)
		}
	}
	for _, never := range []string{"cs2", "red;", "url(x)", "x:y", `a"]`} {
		if strings.Contains(css, never) {
			t.Errorf("theme.css carries %q:\n%s", never, css)
		}
	}
}

func TestGameOverlayPages(t *testing.T) {
	r := newRig(t)
	scope := `<div class="game" data-game="wardogs">`
	overlay := []string{scope, `<img class="game-banner" src="https://cdn.example/wardogs.png" alt="War Dogs">`,
		`<nav class="game-nav" aria-label="War Dogs">`, `href="https://example.com/wardogs/">Playbook`}

	// A server of the game: the overlay wraps the content, after the site's header.
	_, body := get(t, browser(t), r.srv.URL+"/servers/htg-wardogs-1")
	for _, want := range overlay {
		if !strings.Contains(body, want) {
			t.Errorf("the server page lacks %q", want)
		}
	}
	if strings.Contains(body, "/servers/htg-wardogs-1/config") {
		t.Error("an owner-only game link shows to a visitor")
	}
	if strings.Index(body, scope) < strings.Index(body, `<main id="page"`) || strings.Index(body, scope) > strings.Index(body, "<h1>") {
		t.Error("the overlay must open inside main, before the content")
	}
	if strings.Count(body, `href="https://muster.example">Muster`) != 1 {
		t.Error("the organization's own links stay in the header, once")
	}

	// The owner sees the owner's game links, on the server's moderation log too.
	c := moderator(t, r)
	_, body = get(t, c, r.srv.URL+"/servers/htg-wardogs-1")
	if !strings.Contains(body, `href="/servers/htg-wardogs-1/config">Config`) {
		t.Error("the owner's game link is missing")
	}
	_, body = get(t, c, r.srv.URL+"/servers/htg-wardogs-1/log")
	if !strings.Contains(body, scope) || !strings.Contains(body, "Moderation log") {
		t.Errorf("the moderation log is one of the game's pages: %s", body)
	}
	_, page := get(t, c, r.srv.URL+"/servers/htg-wardogs-1")
	form := url.Values{"_csrf": {csrfOf(t, page)}, "action": {"kick"}, "subject": {"76561190000000002"}, "name": {"Stranger"}, "text": {"afk"}}
	if _, body = post(t, c, r.srv.URL+"/servers/htg-wardogs-1/moderate", form, nil); !strings.Contains(body, scope) || !strings.Contains(body, "Kick Stranger") {
		t.Errorf("the confirmation is one of the game's pages: %s", body)
	}

	// Boards: scoped to the game or one of its servers, with the overlay; everyone's, without.
	for _, q := range []string{"scope=game%3Awardogs", "scope=server%3Ahtg-wardogs-1"} {
		if _, body = get(t, browser(t), r.srv.URL+"/boards?"+q); !strings.Contains(body, scope) {
			t.Errorf("boards?%s lacks the overlay", q)
		}
	}
	// htmx swaps #page only, so the fragment carries the overlay, or drops it with the scope.
	if _, frag := get(t, browser(t), r.srv.URL+"/boards?scope=game%3Awardogs", "HX-Request", "true"); strings.Contains(frag, "<html") || !strings.Contains(frag, scope) {
		t.Errorf("the game board's fragment lacks the overlay: %s", frag)
	}
	for _, path := range []string{"/boards", "/servers", "/login"} {
		if _, body = get(t, browser(t), r.srv.URL+path); strings.Contains(body, "data-game") || strings.Contains(body, "game-banner") {
			t.Errorf("%s is no game's page but carries the overlay", path)
		}
	}
	if _, frag := get(t, browser(t), r.srv.URL+"/boards", "HX-Request", "true"); strings.Contains(frag, "data-game") {
		t.Error("everyone's board fragment carries the overlay")
	}
}

// A game with no overlay in the settings still marks its pages, plainly: no banner, no links.
func TestGamePageWithoutOverlay(t *testing.T) {
	theme := testTheme()
	theme.Games = nil
	r := newRigWithTheme(t, theme)
	_, body := get(t, browser(t), r.srv.URL+"/servers/htg-wardogs-1")
	if !strings.Contains(body, `data-game="wardogs"`) || strings.Contains(body, "game-banner") || strings.Contains(body, "game-nav") {
		t.Errorf("a game without an overlay: %s", body)
	}
}

// TestGameOverlayFromSettings follows a game's overlay from the Organization settings through the
// API to the stylesheet (the hub's wiring), not the rig's static theme.
func TestGameOverlayFromSettings(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	idSt := identitytest.NewFakeStore()
	orgSvc := org.New(&orgtest.FakeStore{}, 15*time.Minute, logger)
	o, _, err := orgSvc.EnsureBuiltin(context.Background(), "Hidden Token Gaming")
	if err != nil {
		t.Fatal(err)
	}
	ids := identity.New(idSt, o.ID, nil, 10*time.Minute, logger)
	sess := session.New(idSt, time.Hour, false, logger)
	apiMux := http.NewServeMux()
	access := api.NewAccess(orgSvc, ids, logger)
	apiMux.Handle(hubv1connect.NewOrganizationServiceHandler(api.NewOrganizationServer(orgSvc, access, logger)))
	apiMux.Handle(hubv1connect.NewIdentityServiceHandler(api.NewIdentityServer(ids, sess, orgSvc, access, logger)))
	h, err := web.New(ids, sess, sess.Middleware(apiMux), nil, logger)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	h.Register(mux)
	srv := httptest.NewServer(sess.Middleware(mux))
	defer srv.Close()

	if _, err := orgSvc.UpdateSettings(context.Background(), org.Settings{Theme: org.Theme{Dark: org.Tokens{Accent: "#3ee07a"}},
		Games: map[string]org.GameSettings{"wardogs": {Theme: org.GameTheme{Light: org.Tokens{Accent: "#8a5a00"}, Dark: org.Tokens{Accent: "#f0b429"}}}}}, store.Actor{}); err != nil {
		t.Fatal(err)
	}
	_, css := get(t, browser(t), srv.URL+"/theme.css")
	if !strings.Contains(css, "[data-game=\"wardogs\"]{--accent:#8a5a00}") || !strings.Contains(css, "{[data-game=\"wardogs\"]{--accent:#f0b429}}") {
		t.Errorf("theme.css from the settings:\n%s", css)
	}
}

// The hub serves gravel's generic map image where the bot's server cards look for it (#97).
func TestMapPlaceholder(t *testing.T) {
	if web.MapPlaceholderPath != servercards.PlaceholderPath {
		t.Fatalf("the hub serves the placeholder at %s, the cards look at %s", web.MapPlaceholderPath, servercards.PlaceholderPath)
	}
	r := newRig(t)
	resp, body := get(t, browser(t), r.srv.URL+web.MapPlaceholderPath)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "image/png" || resp.Header.Get("Cache-Control") == "" {
		t.Fatalf("placeholder: %d %q %q", resp.StatusCode, resp.Header.Get("Content-Type"), resp.Header.Get("Cache-Control"))
	}
	img, err := png.Decode(strings.NewReader(body))
	if err != nil || img.Bounds().Dx() != 1280 || img.Bounds().Dy() != 720 {
		t.Errorf("a 1280x720 PNG: %v %v", img.Bounds(), err)
	}
	if len(body) > 100_000 {
		t.Errorf("the placeholder is %d bytes; keep it small", len(body))
	}
	_, again := get(t, browser(t), r.srv.URL+web.MapPlaceholderPath)
	if again != body {
		t.Error("the image is the same on every request")
	}
}
