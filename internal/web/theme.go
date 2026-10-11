package web

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"connectrpc.com/connect"

	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
	"github.com/gravel-project/gravel/gen/gravel/hub/v1/hubv1connect"
	"github.com/gravel-project/gravel/internal/web/templates"
)

// ThemeSource says how the pages look for an organization: tokens, logo, favicon and
// navigation links. gravel#7 stores a theme on the Organization settings and implements this;
// until then StaticTheme serves the defaults.
type ThemeSource interface {
	Theme(ctx context.Context) (templates.Theme, error)
}

// StaticTheme is a ThemeSource with one fixed theme.
type StaticTheme struct{ T templates.Theme }

// Theme returns the fixed theme.
func (s StaticTheme) Theme(context.Context) (templates.Theme, error) { return s.T, nil }

// apiTheme reads the organization's settings through the hub's own API, in process, like
// every other page read (ADR-0005); an empty token means the default (normalizeTheme fills it).
type apiTheme struct {
	client hubv1connect.OrganizationServiceClient
}

func (t apiTheme) Theme(ctx context.Context) (templates.Theme, error) {
	resp, err := t.client.GetOrganizationSettings(ctx, connect.NewRequest(&hubv1.GetOrganizationSettingsRequest{}))
	if err != nil {
		return templates.Theme{}, fmt.Errorf("GetOrganizationSettings: %w", err)
	}
	set := resp.Msg.GetSettings()
	tokens := func(p *hubv1.ThemeTokens) templates.Tokens {
		return templates.Tokens{Accent: p.GetAccent(), Background: p.GetBackground(), Foreground: p.GetForeground(), Muted: p.GetMuted(), Line: p.GetLine(), OK: p.GetOk(), Err: p.GetErr()}
	}
	theme := templates.Theme{
		Light: tokens(set.GetTheme().GetLight()), Dark: tokens(set.GetTheme().GetDark()),
		Font: set.GetTheme().GetFont(), LogoURL: set.GetTheme().GetLogoUrl(), FaviconURL: set.GetTheme().GetFaviconUrl(),
	}
	for _, l := range set.GetNav() {
		theme.Nav = append(theme.Nav, templates.NavLink{Label: l.GetLabel(), URL: l.GetUrl(), Placement: l.GetPlacement(), Role: l.GetRole()})
	}
	for id, g := range set.GetGames() {
		if theme.Games == nil {
			theme.Games = map[string]templates.GameTheme{}
		}
		gt := templates.GameTheme{LightAccent: g.GetTheme().GetLight().GetAccent(), DarkAccent: g.GetTheme().GetDark().GetAccent(),
			BannerURL: g.GetTheme().GetBannerUrl(), BannerAlt: g.GetTheme().GetBannerAlt()}
		for _, l := range g.GetNav() {
			gt.Nav = append(gt.Nav, templates.NavLink{Label: l.GetLabel(), URL: l.GetUrl(), Role: l.GetRole()})
		}
		theme.Games[id] = gt
	}
	return theme, nil
}

// DefaultTheme is gravel's own look, from the brand kit vendored under static/brand (built in
// mkzsystems/brand, never edited here): the amber accent on graphite, Overpass where the viewer
// has it, the mark as the logo and the favicon. Every colour pair meets WCAG AA contrast on its
// scheme's background; the kit's gate checks the same pairs.
func DefaultTheme() templates.Theme {
	return templates.Theme{
		Light:      templates.Tokens{Accent: "#9f6209", Background: "#f5f4f0", Foreground: "#171615", Muted: "#56544f", Line: "#dcdad4", OK: "#dcf3e7", Err: "#fbe1e0"},
		Dark:       templates.Tokens{Accent: "#f29a1a", Background: "#111214", Foreground: "#ecebe8", Muted: "#a19f9b", Line: "#2a2a2f", OK: "#17322a", Err: "#3b1f20"},
		Font:       "Overpass, system-ui, sans-serif",
		LogoURL:    "/static/brand/logo/gravel-mark-auto.svg",
		FaviconURL: "/static/brand/favicon/favicon.svg",
	}
}

var (
	// gameID is a game's id as the Game specs spell it, safe inside a CSS attribute selector.
	gameID   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	cssColor = regexp.MustCompile(`^(#[0-9a-fA-F]{3}|#[0-9a-fA-F]{6}|#[0-9a-fA-F]{8}|[a-zA-Z]{3,20}|rgba?\([0-9.,% ]+\)|hsla?\([0-9.,% deg]+\))$`)
	cssFont  = regexp.MustCompile(`^[A-Za-z0-9 ,'"-]{1,120}$`)
)

// normalizeTheme returns t with every token that is not a plain CSS colour (or font list)
// replaced by the default, so a stored theme can never inject into the stylesheet, and with
// only https or site-relative logo and favicon URLs kept (the pages' CSP allows nothing else).
func normalizeTheme(t templates.Theme) templates.Theme {
	d := DefaultTheme()
	t.Light = normalizeTokens(t.Light, d.Light)
	t.Dark = normalizeTokens(t.Dark, d.Dark)
	if !cssFont.MatchString(t.Font) {
		t.Font = d.Font
	}
	t.LogoURL = safeAssetURL(t.LogoURL)
	t.FaviconURL = safeAssetURL(t.FaviconURL)
	games := map[string]templates.GameTheme{}
	for id, g := range t.Games {
		if !gameID.MatchString(id) {
			continue
		}
		if !cssColor.MatchString(g.LightAccent) {
			g.LightAccent = ""
		}
		if !cssColor.MatchString(g.DarkAccent) {
			g.DarkAccent = ""
		}
		g.BannerURL = safeAssetURL(g.BannerURL)
		if g.BannerURL == "" {
			g.BannerAlt = ""
		}
		games[id] = g
	}
	t.Games = nil
	if len(games) > 0 {
		t.Games = games
	}
	return t
}

func normalizeTokens(t, d templates.Tokens) templates.Tokens {
	pick := func(v, def string) string {
		if cssColor.MatchString(v) {
			return v
		}
		return def
	}
	return templates.Tokens{
		Accent: pick(t.Accent, d.Accent), Background: pick(t.Background, d.Background), Foreground: pick(t.Foreground, d.Foreground),
		Muted: pick(t.Muted, d.Muted), Line: pick(t.Line, d.Line), OK: pick(t.OK, d.OK), Err: pick(t.Err, d.Err),
	}
}

func safeAssetURL(u string) string {
	if strings.HasPrefix(u, "https://") || (strings.HasPrefix(u, "/") && !strings.HasPrefix(u, "//")) {
		return u
	}
	return ""
}

// themeCSS renders the tokens as custom properties the stylesheet reads, light first and dark
// under the scheme media query.
func themeCSS(t templates.Theme) string {
	block := func(k templates.Tokens) string {
		return fmt.Sprintf("--accent:%s;--bg:%s;--fg:%s;--muted:%s;--line:%s;--ok:%s;--err:%s", k.Accent, k.Background, k.Foreground, k.Muted, k.Line, k.OK, k.Err)
	}
	css := fmt.Sprintf(":root{color-scheme:light dark;--font:%s;%s}\n@media (prefers-color-scheme: dark){:root{%s}}\n", t.Font, block(t.Light), block(t.Dark))
	// A game's overlay: its accent inside the game's pages' content (data-game). Both schemes
	// always get a rule, the organization's accent standing in for one the game leaves empty,
	// so a light-only overlay never shows its light accent on the dark background.
	for _, id := range slices.Sorted(maps.Keys(t.Games)) {
		g := t.Games[id]
		if g.LightAccent == "" && g.DarkAccent == "" {
			continue
		}
		light, dark := cmp.Or(g.LightAccent, t.Light.Accent), cmp.Or(g.DarkAccent, t.Dark.Accent)
		css += fmt.Sprintf("[data-game=%q]{--accent:%s}\n@media (prefers-color-scheme: dark){[data-game=%q]{--accent:%s}}\n", id, light, id, dark)
	}
	return css
}

// themeStylesheet serves /theme.css from the theme source, with an ETag so browsers revalidate
// cheaply when a host changes a token.
func (h *Handler) themeStylesheet(w http.ResponseWriter, r *http.Request) {
	t, err := h.theme.Theme(r.Context())
	if err != nil {
		h.logger.WarnContext(r.Context(), "theme unavailable, serving the defaults", "error", err.Error())
		t = DefaultTheme()
	}
	css := themeCSS(normalizeTheme(t))
	sum := sha256.Sum256([]byte(css))
	etag := `"` + hex.EncodeToString(sum[:8]) + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	_, _ = w.Write([]byte(css))
}
