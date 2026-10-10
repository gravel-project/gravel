package org

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// SettingsVersion is the settings document format this build reads and writes.
const SettingsVersion = 1

// Settings is the Organization settings resource: every host-configurable knob, one document,
// managed through the API or applied from a committed manifest (`gravel-hub settings apply`).
// Theme tokens and navigation links first (gravel#7), the Discord role mapping (ADR-0008), the
// per-game overlays (gravel#66); token lifetimes and layout overrides join it with their
// features. Zero values mean "the default".
type Settings struct {
	Version int       `json:"version" yaml:"version"`
	Theme   Theme     `json:"theme" yaml:"theme"`
	Nav     []NavLink `json:"nav,omitempty" yaml:"nav,omitempty"`
	Discord Discord   `json:"discord,omitzero" yaml:"discord,omitempty"`
	Stats   Stats     `json:"stats,omitzero" yaml:"stats,omitempty"`
	// Games are per-game settings by game id ("wardogs"), each a game this hub ships.
	Games map[string]GameSettings `json:"games,omitempty" yaml:"games,omitempty"`
}

// GameSettings are one game's settings: its overlay on the theme and its links, on the game's
// pages (a server of the game, its moderation pages, a board scoped to the game or one of its
// servers), and its map images. The site's header and footer keep the organization's theme.
type GameSettings struct {
	Theme GameTheme `json:"theme,omitzero" yaml:"theme,omitempty"`
	// Nav are links in the game's own navigation on its pages; Placement must be empty.
	Nav []NavLink `json:"nav,omitempty" yaml:"nav,omitempty"`
	// Maps are image URLs by map name, as the game names a map in play or in its rotation (War
	// Dogs: a level, "Ozeti", or a theatre, "Europe"): the server card's picture (#97). A key
	// "map/lighting" ("Europe/DayClear") is the map in that lighting, tried before the plain name.
	Maps map[string]string `json:"maps,omitempty" yaml:"maps,omitempty"`
	// MapFallback is the image for a map Maps doesn't name (https). Empty is gravel's generic
	// placeholder, served by the hub, once the game has map images at all.
	MapFallback string `json:"map_fallback,omitempty" yaml:"map_fallback,omitempty"`
}

// GameTheme is a game's overlay: its accent in each scheme and a banner. Only the accent of
// Light and Dark may be set; empty keeps the organization's.
type GameTheme struct {
	Light     Tokens `json:"light,omitzero" yaml:"light,omitempty"`
	Dark      Tokens `json:"dark,omitzero" yaml:"dark,omitempty"`
	BannerURL string `json:"banner_url,omitempty" yaml:"banner_url,omitempty"`
	// BannerAlt is the banner's alternative text; empty when it is decorative.
	BannerAlt string `json:"banner_alt,omitempty" yaml:"banner_alt,omitempty"`
}

// IsZero reports whether the game sets nothing.
func (g GameSettings) IsZero() bool {
	return g.Theme == GameTheme{} && len(g.Nav) == 0 && len(g.Maps) == 0 && g.MapFallback == ""
}

// Stats is how the stats boards behave (ADR-0012). Zero values are the defaults: boards for the
// owner only, K/D from 3 matches, weeks and seasons in UTC, no named season, raw rows kept 13
// months.
type Stats struct {
	// Public shows the boards and match lists to everyone; until the host's privacy policy covers
	// stats, leave it off (the owner and apps with stats:read still see them).
	Public bool `json:"public,omitempty" yaml:"public,omitempty"`
	// MinMatches is how many matches a player needs before a K/D board shows them.
	MinMatches int `json:"min_matches,omitempty" yaml:"min_matches,omitempty"`
	// Timezone is the IANA zone weeks, months and season dates are in.
	Timezone string `json:"timezone,omitempty" yaml:"timezone,omitempty"`
	// Seasons are named windows; ending one deletes nothing.
	Seasons []Season `json:"seasons,omitempty" yaml:"seasons,omitempty"`
	// RawRetentionMonths is how long a player's per-match rows are kept before they roll up into
	// monthly totals (ADR-0012 §6): no raw row is older. 0 is the default, 13, a placeholder
	// pending counsel.
	RawRetentionMonths int `json:"raw_retention_months,omitempty" yaml:"raw_retention_months,omitempty"`
}

// Season is a named window of matches: From (included) to To (excluded), dates in Stats.Timezone.
// Game limits it to one game's matches; empty is every game.
type Season struct {
	Name string `json:"name" yaml:"name"`
	Game string `json:"game,omitempty" yaml:"game,omitempty"`
	From string `json:"from" yaml:"from"`
	To   string `json:"to" yaml:"to"`
}

// DefaultMinMatches is MinMatches when it is 0.
const DefaultMinMatches = 3

// DefaultRawRetentionMonths is RawRetentionMonths when it is 0.
const DefaultRawRetentionMonths = 13

// RawRetentionMonthsOrDefault is RawRetentionMonths, or DefaultRawRetentionMonths when it is 0.
func (s Stats) RawRetentionMonthsOrDefault() int {
	if s.RawRetentionMonths == 0 {
		return DefaultRawRetentionMonths
	}
	return s.RawRetentionMonths
}

// IsZero reports whether every stats setting is its default.
func (s Stats) IsZero() bool {
	return !s.Public && s.MinMatches == 0 && s.Timezone == "" && len(s.Seasons) == 0 && s.RawRetentionMonths == 0
}

// Location is the timezone, UTC when unset; Validate checked it loads.
func (s Stats) Location() *time.Location {
	if s.Timezone == "" {
		return time.UTC
	}
	loc, err := time.LoadLocation(s.Timezone)
	if err != nil {
		return time.UTC
	}
	return loc
}

// MinMatchesOrDefault is MinMatches, or DefaultMinMatches when it is 0.
func (s Stats) MinMatchesOrDefault() int {
	if s.MinMatches == 0 {
		return DefaultMinMatches
	}
	return s.MinMatches
}

// Window is a season's start and end as instants.
func (se Season) Window(loc *time.Location) (from, to time.Time, err error) {
	if from, err = time.ParseInLocation(time.DateOnly, se.From, loc); err != nil {
		return time.Time{}, time.Time{}, err
	}
	if to, err = time.ParseInLocation(time.DateOnly, se.To, loc); err != nil {
		return time.Time{}, time.Time{}, err
	}
	return from, to, nil
}

// Theme is how the organization's pages look.
type Theme struct {
	Light      Tokens `json:"light" yaml:"light"`
	Dark       Tokens `json:"dark" yaml:"dark"`
	Font       string `json:"font,omitempty" yaml:"font,omitempty"`
	LogoURL    string `json:"logo_url,omitempty" yaml:"logo_url,omitempty"`
	FaviconURL string `json:"favicon_url,omitempty" yaml:"favicon_url,omitempty"`
}

// Tokens are one colour scheme's colours, as CSS colours; empty means the default.
type Tokens struct {
	Accent     string `json:"accent,omitempty" yaml:"accent,omitempty"`
	Background string `json:"background,omitempty" yaml:"background,omitempty"`
	Foreground string `json:"foreground,omitempty" yaml:"foreground,omitempty"`
	Muted      string `json:"muted,omitempty" yaml:"muted,omitempty"`
	Line       string `json:"line,omitempty" yaml:"line,omitempty"`
	OK         string `json:"ok,omitempty" yaml:"ok,omitempty"`
	Err        string `json:"err,omitempty" yaml:"err,omitempty"`
}

// NavLink is a navigation extension: in the header or the footer, for everyone or the owner.
type NavLink struct {
	Label     string `json:"label" yaml:"label"`
	URL       string `json:"url" yaml:"url"`
	Placement string `json:"placement,omitempty" yaml:"placement,omitempty"` // "header" (default) or "footer"
	Role      string `json:"role,omitempty" yaml:"role,omitempty"`           // "" (everyone) or "owner"
}

// Discord is the guild the bot works in (ADR-0008 §3): the roles role sync maps and the
// channels the server cards live in. Ids are Discord snowflakes as strings. Everything here is
// public: ids, never a credential. Empty means the bot has nothing to do.
type Discord struct {
	GuildID     string        `json:"guild_id,omitempty" yaml:"guild_id,omitempty"`
	Roles       DiscordRoles  `json:"roles,omitzero" yaml:"roles,omitempty"`
	Recognition []Recognition `json:"recognition,omitempty" yaml:"recognition,omitempty"`
	// ServerCards are the live status cards the bot keeps (discord/modules/servercards): one
	// message per server, edited in place.
	ServerCards []ServerCard `json:"server_cards,omitempty" yaml:"server_cards,omitempty"`
	// ModLog is the channel the bot posts every moderation action to (discord/modules/modlog):
	// the hub's audit log, from the web and from Discord alike. Empty is none.
	ModLog string `json:"mod_log,omitempty" yaml:"mod_log,omitempty"`
}

// ServerCard places one server's status card in a channel of the guild.
type ServerCard struct {
	// Server is the server's id in the hub: "htg-wardogs-1".
	Server  string `json:"server" yaml:"server"`
	Channel string `json:"channel" yaml:"channel"`
	// Note is a line the host writes under the numbers: "Matches start at 20 players."
	Note string `json:"note,omitempty" yaml:"note,omitempty"`
}

// DiscordRoles are the roles that follow a member's linked identities. Role sync adds and
// removes them as identities are linked and unlinked.
type DiscordRoles struct {
	// Linked is for a member with any identity besides Discord.
	Linked string `json:"linked,omitempty" yaml:"linked,omitempty"`
	// Providers maps a provider ("steam") to the role for a member who linked it.
	Providers map[string]string `json:"providers,omitempty" yaml:"providers,omitempty"`
}

// Recognition is a role earned once and kept: role sync adds it and never removes it.
type Recognition struct {
	Role string `json:"role" yaml:"role"`
	// Rule is RuleFirstMembers: the first Count members by registration.
	Rule  string `json:"rule" yaml:"rule"`
	Count int    `json:"count" yaml:"count"`
}

// Recognition rules.
const RuleFirstMembers = "first_members"

// MappedProviders are the providers a Discord role may follow: the hub's login and link
// providers (internal/identity/providers).
var MappedProviders = []string{"discord", "steam"}

// IsZero reports whether nothing is set.
func (d Discord) IsZero() bool {
	return d.GuildID == "" && d.Roles.IsZero() && len(d.Recognition) == 0 && len(d.ServerCards) == 0 && d.ModLog == ""
}

// IsZero reports whether no role is mapped.
func (r DiscordRoles) IsZero() bool { return r.Linked == "" && len(r.Providers) == 0 }

// RoleIDs are every role the mapping names, in a stable order: linked, the providers' by
// provider name, then the recognition roles. Role sync manages these and no other.
func (d Discord) RoleIDs() []string {
	var out []string
	if d.Roles.Linked != "" {
		out = append(out, d.Roles.Linked)
	}
	for _, p := range slices.Sorted(maps.Keys(d.Roles.Providers)) {
		out = append(out, d.Roles.Providers[p])
	}
	for _, r := range d.Recognition {
		out = append(out, r.Role)
	}
	return out
}

// Placements and roles a NavLink may carry.
const (
	PlacementHeader = "header"
	PlacementFooter = "footer"
	RoleEveryone    = ""
	RoleOwner       = "owner"
)

// Limits.
const (
	MaxNavLinks     = 12
	MaxNavLabel     = 40
	MaxFontFamily   = 120
	MaxURLLength    = 512
	maxNavURLLength = MaxURLLength
	MaxRecognition  = 10
	MaxFirstMembers = 100000
	MaxServerCards  = 10
	MaxCardNote     = 200
	MaxMinMatches   = 100
	MaxSeasons      = 50
	MaxSeasonName   = 40
	// MaxRawRetentionMonths bounds stats.raw_retention_months: ten years.
	MaxRawRetentionMonths = 120
	// MaxGames bounds the games section, MaxGameNav each game's links, MaxBannerAlt the
	// banner's alternative text.
	MaxGames     = 20
	MaxGameNav   = 6
	MaxBannerAlt = 120
	// MaxGameMaps bounds a game's map images, MaxMapName a map's name.
	MaxGameMaps = 50
	MaxMapName  = 64
)

// ErrInvalidSettings wraps every validation failure; the message says which field and why.
var ErrInvalidSettings = errors.New("invalid settings")

var (
	cssColor = regexp.MustCompile(`^(#[0-9a-fA-F]{3}|#[0-9a-fA-F]{6}|#[0-9a-fA-F]{8}|[a-zA-Z]{3,20}|rgba?\([0-9.,% ]+\)|hsla?\([0-9.,% deg]+\))$`)
	cssFont  = regexp.MustCompile(`^[A-Za-z0-9 ,'"-]{1,120}$`)
	// serverID is the servers manifest's id rule (internal/servers), which this package cannot
	// import: the server need not exist yet, the bot logs a card whose server is unknown.
	serverID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
)

// Validate reports every problem at once, each wrapping ErrInvalidSettings.
func (s Settings) Validate() error {
	var errs []error
	bad := func(format string, a ...any) {
		errs = append(errs, fmt.Errorf("%w: "+format, append([]any{ErrInvalidSettings}, a...)...))
	}
	if s.Version != SettingsVersion {
		bad("version: got %d, this hub reads version %d", s.Version, SettingsVersion)
	}
	for scheme, t := range map[string]Tokens{"light": s.Theme.Light, "dark": s.Theme.Dark} {
		for name, v := range map[string]string{"accent": t.Accent, "background": t.Background, "foreground": t.Foreground, "muted": t.Muted, "line": t.Line, "ok": t.OK, "err": t.Err} {
			if v != "" && !cssColor.MatchString(v) {
				bad("theme.%s.%s: %q is not a plain CSS colour", scheme, name, v)
			}
		}
	}
	if s.Theme.Font != "" && !cssFont.MatchString(s.Theme.Font) {
		bad("theme.font: %q is not a font-family list (letters, digits, spaces, commas, quotes, hyphens; at most %d characters)", s.Theme.Font, MaxFontFamily)
	}
	for name, v := range map[string]string{"logo_url": s.Theme.LogoURL, "favicon_url": s.Theme.FaviconURL} {
		if v != "" && !assetURLOK(v) {
			bad("theme.%s: %q must be an https URL or a site-relative path", name, v)
		}
	}
	if len(s.Nav) > MaxNavLinks {
		bad("nav: %d links, at most %d", len(s.Nav), MaxNavLinks)
	}
	for i, l := range s.Nav {
		validateNavLink(fmt.Sprintf("nav[%d]", i), l, bad)
		if l.Placement != "" && l.Placement != PlacementHeader && l.Placement != PlacementFooter {
			bad("nav[%d].placement: %q is not %q or %q", i, l.Placement, PlacementHeader, PlacementFooter)
		}
	}
	validateDiscord(s.Discord, bad)
	validateStats(s.Stats, bad)
	validateGames(s.Games, bad)
	if len(errs) == 0 {
		return nil
	}
	return errors.Join(errs...)
}

func validateNavLink(field string, l NavLink, bad func(string, ...any)) {
	if strings.TrimSpace(l.Label) == "" || len(l.Label) > MaxNavLabel {
		bad("%s.label: required, at most %d characters", field, MaxNavLabel)
	}
	if !navURLOK(l.URL) {
		bad("%s.url: %q must be an http(s) URL or a site-relative path", field, l.URL)
	}
	if l.Role != RoleEveryone && l.Role != RoleOwner {
		bad("%s.role: %q is not empty or %q", field, l.Role, RoleOwner)
	}
}

func validateGames(games map[string]GameSettings, bad func(string, ...any)) {
	if len(games) > MaxGames {
		bad("games: %d games, at most %d", len(games), MaxGames)
	}
	for _, id := range slices.Sorted(maps.Keys(games)) {
		g, field := games[id], "games."+id
		if !serverID.MatchString(id) {
			bad("games: %q is not a game id (lower-case letters, digits and hyphens)", id)
			continue
		}
		for scheme, t := range map[string]Tokens{"light": g.Theme.Light, "dark": g.Theme.Dark} {
			if t.Accent != "" && !cssColor.MatchString(t.Accent) {
				bad("%s.theme.%s.accent: %q is not a plain CSS colour", field, scheme, t.Accent)
			}
			if (Tokens{Accent: t.Accent}) != t {
				bad("%s.theme.%s: a game overlays only the accent; the page around it keeps the organization's colours", field, scheme)
			}
		}
		if g.Theme.BannerURL != "" && !assetURLOK(g.Theme.BannerURL) {
			bad("%s.theme.banner_url: %q must be an https URL or a site-relative path", field, g.Theme.BannerURL)
		}
		if alt := g.Theme.BannerAlt; utf8.RuneCountInString(alt) > MaxBannerAlt || strings.ContainsFunc(alt, unicode.IsControl) {
			bad("%s.theme.banner_alt: one line of at most %d characters", field, MaxBannerAlt)
		} else if alt != "" && g.Theme.BannerURL == "" {
			bad("%s.theme.banner_alt: set without a banner_url", field)
		}
		if len(g.Nav) > MaxGameNav {
			bad("%s.nav: %d links, at most %d", field, len(g.Nav), MaxGameNav)
		}
		for i, l := range g.Nav {
			f := fmt.Sprintf("%s.nav[%d]", field, i)
			validateNavLink(f, l, bad)
			if l.Placement != "" {
				bad("%s.placement: a game's links are shown on its pages; leave placement out", f)
			}
		}
		if len(g.Maps) > MaxGameMaps {
			bad("%s.maps: %d maps, at most %d", field, len(g.Maps), MaxGameMaps)
		}
		for _, name := range slices.Sorted(maps.Keys(g.Maps)) {
			if !mapKeyOK(name) {
				bad("%s.maps: %q is not a map name or map/lighting (as the game spells them, at most %d characters)", field, name, MaxMapName)
				continue
			}
			if !imageURLOK(g.Maps[name]) {
				bad("%s.maps.%s: %q must be an https URL", field, name, g.Maps[name])
			}
		}
		if g.MapFallback != "" && !imageURLOK(g.MapFallback) {
			bad("%s.map_fallback: %q must be an https URL", field, g.MapFallback)
		}
	}
}

// CheckGames reports a games key or a season's game that is not one of known, the games this
// hub ships (internal/servers), each wrapping ErrInvalidSettings. Validate cannot do this, as
// this package does not know the games.
func (s Settings) CheckGames(known []string) error {
	var errs []error
	unknown := func(field, id string) {
		errs = append(errs, fmt.Errorf("%w: %s: %q is not a game this hub ships (%s)", ErrInvalidSettings, field, id, strings.Join(known, ", ")))
	}
	for _, id := range slices.Sorted(maps.Keys(s.Games)) {
		if !slices.Contains(known, id) {
			unknown("games", id)
		}
	}
	for i, se := range s.Stats.Seasons {
		if se.Game != "" && !slices.Contains(known, se.Game) {
			unknown(fmt.Sprintf("stats.seasons[%d].game", i), se.Game)
		}
	}
	return errors.Join(errs...)
}

func validateDiscord(d Discord, bad func(string, ...any)) {
	if d.IsZero() {
		return
	}
	if d.GuildID == "" {
		bad("discord.guild_id: required when roles are mapped or server cards placed")
	} else if !snowflakeOK(d.GuildID) {
		bad("discord.guild_id: %q is not a Discord id", d.GuildID)
	}
	seen := map[string]string{}
	role := func(field, id string) {
		if !snowflakeOK(id) {
			bad("%s: %q is not a Discord id", field, id)
			return
		}
		if id == d.GuildID {
			bad("%s: %q is the guild's @everyone role, which every member has", field, id)
		}
		if other, dup := seen[id]; dup {
			bad("%s: role %s is already mapped by %s", field, id, other)
			return
		}
		seen[id] = field
	}
	if d.Roles.Linked != "" {
		role("discord.roles.linked", d.Roles.Linked)
	}
	for _, p := range slices.Sorted(maps.Keys(d.Roles.Providers)) {
		field := "discord.roles.providers." + p
		if !slices.Contains(MappedProviders, p) {
			bad("%s: %q is not a provider (%s)", field, p, strings.Join(MappedProviders, ", "))
			continue
		}
		role(field, d.Roles.Providers[p])
	}
	if len(d.Recognition) > MaxRecognition {
		bad("discord.recognition: %d entries, at most %d", len(d.Recognition), MaxRecognition)
	}
	for i, r := range d.Recognition {
		field := fmt.Sprintf("discord.recognition[%d]", i)
		role(field+".role", r.Role)
		if r.Rule != RuleFirstMembers {
			bad("%s.rule: %q is not %q", field, r.Rule, RuleFirstMembers)
		}
		if r.Count < 1 || r.Count > MaxFirstMembers {
			bad("%s.count: %d is not between 1 and %d", field, r.Count, MaxFirstMembers)
		}
	}
	if d.ModLog != "" && !snowflakeOK(d.ModLog) {
		bad("discord.mod_log: %q is not a Discord channel id", d.ModLog)
	}
	if len(d.ServerCards) > MaxServerCards {
		bad("discord.server_cards: %d cards, at most %d", len(d.ServerCards), MaxServerCards)
	}
	servers := map[string]int{}
	for i, c := range d.ServerCards {
		field := fmt.Sprintf("discord.server_cards[%d]", i)
		if !serverID.MatchString(c.Server) {
			bad("%s.server: %q is not a server id (lower-case letters, digits and hyphens)", field, c.Server)
		} else if j, dup := servers[c.Server]; dup {
			bad("%s.server: %q already has a card (discord.server_cards[%d])", field, c.Server, j)
		} else {
			servers[c.Server] = i
		}
		if !snowflakeOK(c.Channel) {
			bad("%s.channel: %q is not a Discord id", field, c.Channel)
		}
		note := strings.TrimSpace(c.Note)
		if len([]rune(note)) > MaxCardNote || strings.ContainsFunc(note, unicode.IsControl) {
			bad("%s.note: one line of at most %d characters", field, MaxCardNote)
		}
	}
}

func validateStats(st Stats, bad func(string, ...any)) {
	if st.MinMatches < 0 || st.MinMatches > MaxMinMatches {
		bad("stats.min_matches: %d is not between 0 (the default, %d) and %d", st.MinMatches, DefaultMinMatches, MaxMinMatches)
	}
	if st.RawRetentionMonths < 0 || st.RawRetentionMonths > MaxRawRetentionMonths {
		bad("stats.raw_retention_months: %d is not between 0 (the default, %d) and %d", st.RawRetentionMonths, DefaultRawRetentionMonths, MaxRawRetentionMonths)
	}
	loc := time.UTC
	if st.Timezone != "" {
		l, err := time.LoadLocation(st.Timezone)
		if err != nil || st.Timezone == "Local" {
			bad("stats.timezone: %q is not an IANA zone (America/Chicago)", st.Timezone)
		} else {
			loc = l
		}
	}
	if len(st.Seasons) > MaxSeasons {
		bad("stats.seasons: %d seasons, at most %d", len(st.Seasons), MaxSeasons)
	}
	names := map[string]int{}
	for i, se := range st.Seasons {
		field := fmt.Sprintf("stats.seasons[%d]", i)
		name := strings.TrimSpace(se.Name)
		if name == "" || utf8.RuneCountInString(name) > MaxSeasonName || strings.ContainsFunc(name, unicode.IsControl) {
			bad("%s.name: required, one line of at most %d characters", field, MaxSeasonName)
		} else if j, dup := names[strings.ToLower(name)]; dup {
			bad("%s.name: %q is also stats.seasons[%d]", field, name, j)
		} else {
			names[strings.ToLower(name)] = i
		}
		if se.Game != "" && !serverID.MatchString(se.Game) {
			bad("%s.game: %q is not a game id", field, se.Game)
		}
		from, to, err := se.Window(loc)
		if err != nil {
			bad("%s: from and to must be dates (2026-10-15)", field)
		} else if !from.Before(to) {
			bad("%s: from %s is not before to %s", field, se.From, se.To)
		}
	}
}

// snowflakeOK reports whether s is a Discord id: 17 to 20 digits.
func snowflakeOK(s string) bool {
	if len(s) < 17 || len(s) > 20 || strings.Trim(s, "0123456789") != "" {
		return false
	}
	_, err := strconv.ParseUint(s, 10, 64)
	return err == nil
}

func assetURLOK(u string) bool {
	if len(u) > MaxURLLength {
		return false
	}
	if strings.HasPrefix(u, "/") && !strings.HasPrefix(u, "//") {
		return true
	}
	p, err := url.Parse(u)
	return err == nil && p.Scheme == "https" && p.Host != ""
}

// mapKeyOK is a map name ("Europe") or a map and a lighting ("Europe/DayClear"): each part
// trimmed and non-empty, one slash at most, no control characters, MaxMapName in all.
func mapKeyOK(k string) bool {
	if k == "" || utf8.RuneCountInString(k) > MaxMapName || strings.ContainsFunc(k, unicode.IsControl) || strings.Count(k, "/") > 1 {
		return false
	}
	for _, part := range strings.Split(k, "/") {
		if part == "" || strings.TrimSpace(part) != part {
			return false
		}
	}
	return true
}

// imageURLOK is an image Discord fetches itself (a server card's), so a site-relative path can't
// work: https only.
func imageURLOK(u string) bool {
	p, err := url.Parse(u)
	return err == nil && p.Scheme == "https" && p.Host != "" && len(u) <= MaxURLLength
}

func navURLOK(u string) bool {
	if len(u) > maxNavURLLength {
		return false
	}
	if strings.HasPrefix(u, "/") && !strings.HasPrefix(u, "//") {
		return true
	}
	p, err := url.Parse(u)
	return err == nil && (p.Scheme == "https" || p.Scheme == "http") && p.Host != ""
}

// Normalized returns s with the version set and the nav placements made explicit.
func (s Settings) Normalized() Settings {
	s.Version = SettingsVersion
	for i := range s.Stats.Seasons {
		s.Stats.Seasons[i].Name = strings.TrimSpace(s.Stats.Seasons[i].Name)
	}
	if len(s.Stats.Seasons) == 0 {
		s.Stats.Seasons = nil
	}
	for i := range s.Nav {
		if s.Nav[i].Placement == "" {
			s.Nav[i].Placement = PlacementHeader
		}
		s.Nav[i].Label = strings.TrimSpace(s.Nav[i].Label)
	}
	if len(s.Discord.Roles.Providers) == 0 {
		s.Discord.Roles.Providers = nil
	}
	if len(s.Discord.Recognition) == 0 {
		s.Discord.Recognition = nil
	}
	for i := range s.Discord.ServerCards {
		s.Discord.ServerCards[i].Note = strings.TrimSpace(s.Discord.ServerCards[i].Note)
	}
	if len(s.Discord.ServerCards) == 0 {
		s.Discord.ServerCards = nil
	}
	if len(s.Games) == 0 {
		s.Games = nil
	} else {
		games := make(map[string]GameSettings, len(s.Games))
		for id, g := range s.Games {
			if g.IsZero() {
				continue
			}
			g.Theme.BannerAlt = strings.TrimSpace(g.Theme.BannerAlt)
			for i := range g.Nav {
				g.Nav[i].Label = strings.TrimSpace(g.Nav[i].Label)
			}
			if len(g.Nav) == 0 {
				g.Nav = nil
			}
			if len(g.Maps) == 0 {
				g.Maps = nil
			}
			games[id] = g
		}
		s.Games = games
		if len(games) == 0 {
			s.Games = nil
		}
	}
	return s
}

// DefaultSettings is an empty document: every token at its default, no links.
func DefaultSettings() Settings { return Settings{Version: SettingsVersion} }

// ParseSettings decodes a stored document. "{}" (never written) is the default.
func ParseSettings(raw json.RawMessage) (Settings, error) {
	if len(raw) == 0 || string(raw) == "{}" {
		return DefaultSettings(), nil
	}
	var s Settings
	if err := json.Unmarshal(raw, &s); err != nil {
		return Settings{}, fmt.Errorf("org: settings: %w", err)
	}
	return s.Normalized(), nil
}

// ParseManifest decodes a YAML manifest (`gravel-hub settings apply`), strictly.
func ParseManifest(b []byte) (Settings, error) {
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	var s Settings
	if err := dec.Decode(&s); err != nil {
		return Settings{}, fmt.Errorf("org: manifest: %w", err)
	}
	if s.Version == 0 {
		s.Version = SettingsVersion
	}
	return s.Normalized(), nil
}

// Manifest encodes settings as the YAML manifest `settings export` prints.
func (s Settings) Manifest() ([]byte, error) {
	return yaml.Marshal(s.Normalized())
}

// Settings returns the built-in organization's settings.
func (s *Service) Settings(ctx context.Context) (Settings, time.Time, error) {
	o, err := s.st.GetBuiltinOrganization(ctx)
	if err != nil {
		return Settings{}, time.Time{}, err
	}
	set, err := ParseSettings(o.Settings)
	if err != nil {
		return Settings{}, time.Time{}, err
	}
	var at time.Time
	if o.SettingsUpdatedAt != nil {
		at = *o.SettingsUpdatedAt
	}
	return set, at, nil
}

// UpdateSettings validates and stores a whole document; the caller checks who may.
func (s *Service) UpdateSettings(ctx context.Context, set Settings) (Settings, error) {
	set = set.Normalized()
	if err := set.Validate(); err != nil {
		return Settings{}, err
	}
	if s.games != nil {
		if err := set.CheckGames(s.games); err != nil {
			return Settings{}, err
		}
	}
	o, err := s.st.GetBuiltinOrganization(ctx)
	if err != nil {
		return Settings{}, err
	}
	raw, err := json.Marshal(set)
	if err != nil {
		return Settings{}, fmt.Errorf("org: settings: %w", err)
	}
	if _, err := s.st.UpdateOrganizationSettings(ctx, o.ID, raw, s.now()); err != nil {
		return Settings{}, err
	}
	s.logger.Info("organization settings updated", "organization_id", o.ID, "nav_links", len(set.Nav), "discord_roles", len(set.Discord.RoleIDs()))
	return set, nil
}
