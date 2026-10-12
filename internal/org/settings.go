package org

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // stats.timezone resolves in a container with no zoneinfo
	"unicode"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"github.com/gravel-project/gravel/discord/seedhours"
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
	// Seeding are the seeding rules (discord/modules/seeding, ADR-0016): when the bot calls the
	// seeder role to a server under its player floor, one rule per server.
	Seeding []SeedingRule `json:"seeding,omitempty" yaml:"seeding,omitempty"`
	// Presence is "who's playing now" (discord/modules/presence, ADR-0017): a card with how many
	// members are playing each game, counted from Discord presence and never stored.
	Presence Presence `json:"presence,omitzero" yaml:"presence,omitempty"`
	// Recaps are the session recaps (discord/modules/recaps, ADR-0018): after each of a rule's
	// weekly windows, the bot posts what the server's window held. One rule per server.
	Recaps []RecapRule `json:"recaps,omitempty" yaml:"recaps,omitempty"`
	// Streams are the go-live posts (discord/modules/streams, ADR-0019): one post per stream of
	// each listed streamer, edited when the stream ends.
	Streams Streams `json:"streams,omitzero" yaml:"streams,omitempty"`
	// News are the Steam news feeds (discord/modules/news): each game's announcements posted in a
	// channel.
	News News `json:"news,omitzero" yaml:"news,omitempty"`
}

// News places Steam games' announcements: each feed is one game's news into one channel. Empty is
// none.
type News struct {
	// Feeds are read in this order, at most MaxNewsFeeds; each game once per channel.
	Feeds []NewsFeed `json:"feeds,omitempty" yaml:"feeds,omitempty"`
}

// NewsFeed is one game's Steam announcements into one channel.
type NewsFeed struct {
	// AppID is the game's Steam app id, as in its store URL: 730 for Counter-Strike 2.
	AppID   int    `json:"app_id" yaml:"app_id"`
	Channel string `json:"channel" yaml:"channel"`
	// Label names the game in each post's footer: "<label> · Steam announcement".
	Label string `json:"label" yaml:"label"`
	// Feeds are Steam's feed names to read; none is NewsDefaultFeed, the game's own announcements.
	Feeds []string `json:"feeds,omitempty" yaml:"feeds,omitempty"`
	// Color is the posts' embed color as 0xRRGGBB; 0 is the bot's default (Steam's blue).
	Color int `json:"color,omitempty" yaml:"color,omitempty"`
}

// IsZero reports whether no feed is listed.
func (n News) IsZero() bool { return len(n.Feeds) == 0 }

// The news feeds' bounds.
const (
	MaxNewsFeeds = 20
	MaxNewsLabel = 64
	// NewsDefaultFeed is what a feed reads when it names none.
	NewsDefaultFeed = "steam_community_announcements"
)

// newsFeedName is one of Steam's feed names: steam_community_announcements, steam_updates.
var newsFeedName = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// Streams places the go-live posts: when a listed streamer goes live on Twitch or Kick the bot
// posts once in Channel, pinging Role when one is set, and edits the post when the stream ends.
// Empty is no posts. The bot needs each platform's app credentials in bot.yaml.
type Streams struct {
	Channel string `json:"channel,omitempty" yaml:"channel,omitempty"`
	// Role is pinged by each go-live post: an opt-in role members choose. Empty pings no one.
	Role string `json:"role,omitempty" yaml:"role,omitempty"`
	// Note is a line the host writes under each post.
	Note string `json:"note,omitempty" yaml:"note,omitempty"`
	// Streamers are watched in this order, at most MaxStreamers.
	Streamers []Streamer `json:"streamers,omitempty" yaml:"streamers,omitempty"`
}

// Streamer is one channel on a streaming platform.
type Streamer struct {
	// Platform is StreamTwitch or StreamKick.
	Platform string `json:"platform" yaml:"platform"`
	// Login is the channel's name in its URL: "hiddentoken" for twitch.tv/hiddentoken. Lower case.
	Login string `json:"login" yaml:"login"`
	// Label is the name the post shows; the login when empty.
	Label string `json:"label,omitempty" yaml:"label,omitempty"`
}

// IsZero reports whether no post is placed.
func (s Streams) IsZero() bool {
	return s.Channel == "" && s.Role == "" && s.Note == "" && len(s.Streamers) == 0
}

// The streaming platforms and the go-live posts' bounds.
const (
	StreamTwitch     = "twitch"
	StreamKick       = "kick"
	MaxStreamers     = 10
	MaxStreamerLabel = 40
	MaxStreamsNote   = 200
)

// StreamPlatforms are the platforms a streamer may be on.
var StreamPlatforms = []string{StreamTwitch, StreamKick}

// streamLogins are each platform's channel names: Twitch's 4 to 25 letters, digits and
// underscores; Kick's slugs, which also take hyphens.
var streamLogins = map[string]*regexp.Regexp{
	StreamTwitch: regexp.MustCompile(`^[a-z0-9][a-z0-9_]{3,24}$`),
	StreamKick:   regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{2,24}$`),
}

// RecapRule is when and where the bot posts a server's session recap: a few minutes after each of
// its weekly windows ends, unless nobody was on.
type RecapRule struct {
	Server  string `json:"server" yaml:"server"`
	Channel string `json:"channel" yaml:"channel"`
	// Title names the session in the post: "Wardogs Wednesday".
	Title string `json:"title" yaml:"title"`
	// Hours are the sessions, as seeding's (in stats.timezone). None is a rule that never posts.
	Hours []SeedingHours `json:"hours,omitempty" yaml:"hours,omitempty"`
	// Floor is the player count "minutes at or above" counts against (War Dogs, 20); 0 leaves
	// the line out.
	Floor int `json:"floor,omitempty" yaml:"floor,omitempty"`
	// Note is a line appended to the post.
	Note string `json:"note,omitempty" yaml:"note,omitempty"`
}

// Windows are the rule's hours as discord/seedhours reads them.
func (r RecapRule) Windows() []seedhours.Window {
	out := make([]seedhours.Window, len(r.Hours))
	for i, h := range r.Hours {
		out[i] = seedhours.Window{Days: h.Days, From: h.From, To: h.To}
	}
	return out
}

// Recap rules' bounds.
const (
	MaxRecapRules = 10
	MaxRecapTitle = 60
	MaxRecapFloor = 1000
	MaxRecapNote  = 200
)

// Presence places the "who's playing now" card: one message in Channel, edited when a count
// changes. A member counts for a game while Discord shows them playing it, which only members who
// share their activity on Discord do. Empty is no card.
type Presence struct {
	Channel string `json:"channel,omitempty" yaml:"channel,omitempty"`
	// Note is a line the host writes under the counts.
	Note string `json:"note,omitempty" yaml:"note,omitempty"`
	// Games are counted in this order, at most MaxPresenceGames.
	Games []PresenceGame `json:"games,omitempty" yaml:"games,omitempty"`
}

// PresenceGame is one game on the card and how its Discord activity is recognized: by the name
// Discord shows ("WARDOGS", compared without regard to case) or by the activity's Discord
// application id. At least one of the two is required.
type PresenceGame struct {
	// Game is the line's key, in the metrics: "wardogs". The hub's game id where there is one.
	Game string `json:"game" yaml:"game"`
	// Label is what the card shows: "War Dogs".
	Label          string   `json:"label" yaml:"label"`
	Activities     []string `json:"activities,omitempty" yaml:"activities,omitempty"`
	ApplicationIDs []string `json:"application_ids,omitempty" yaml:"application_ids,omitempty"`
}

// IsZero reports whether no card is placed.
func (p Presence) IsZero() bool { return p.Channel == "" && p.Note == "" && len(p.Games) == 0 }

// Presence card bounds.
const (
	MaxPresenceGames      = 20
	MaxPresenceLabel      = 40
	MaxPresenceActivities = 10
	MaxPresenceActivity   = 128
	MaxPresenceNote       = 200
)

// SeedingRule is when and where the bot calls for seeders for one server: inside one of its
// weekly windows, while the server is live and under Below, at most once per Cooldown. Outside
// every window nothing is posted, so a rule without hours never calls.
type SeedingRule struct {
	// Server is the server's id in the hub: "htg-wardogs-1".
	Server string `json:"server" yaml:"server"`
	// Channel is where the call is posted; Role is the opt-in seeder role it pings, and nothing
	// else.
	Channel string `json:"channel" yaml:"channel"`
	Role    string `json:"role" yaml:"role"`
	// Below is the player count under which a call is due: the game's own floor (War Dogs, 20).
	Below int `json:"below" yaml:"below"`
	// Hours are the weekly windows, in stats.timezone (UTC when unset).
	Hours []SeedingHours `json:"hours,omitempty" yaml:"hours,omitempty"`
	// Cooldown is the least time between two calls, a Go duration ("2h"); empty is
	// DefaultSeedingCooldown.
	Cooldown string `json:"cooldown,omitempty" yaml:"cooldown,omitempty"`
	// CallAtStart makes each window's first call without waiting out a cooldown left from
	// before the window: at the window's start, while the server is under Below.
	CallAtStart bool `json:"call_at_start,omitempty" yaml:"call_at_start,omitempty"`
	// Note is a line the host appends to the call: how to join.
	Note string `json:"note,omitempty" yaml:"note,omitempty"`
}

// SeedingHours is one weekly window: the days it starts on (mon … sun) and the time of day from
// and to ("18:00", "23:00"); a to before from ends the next day (discord/seedhours).
type SeedingHours struct {
	Days []string `json:"days" yaml:"days,flow"`
	From string   `json:"from" yaml:"from"`
	To   string   `json:"to" yaml:"to"`
}

// DefaultSeedingCooldown is a seeding rule's cooldown when it sets none.
const DefaultSeedingCooldown = 2 * time.Hour

// Seeding rules' bounds.
const (
	MaxSeedingRules    = 10
	MinSeedingBelow    = 2
	MaxSeedingBelow    = 1000
	MinSeedingCooldown = 15 * time.Minute
	MaxSeedingCooldown = 24 * time.Hour
	MaxSeedingNote     = 200
)

// CooldownOrDefault is the rule's cooldown, DefaultSeedingCooldown when it sets none.
func (r SeedingRule) CooldownOrDefault() time.Duration {
	if d, err := time.ParseDuration(r.Cooldown); err == nil && r.Cooldown != "" {
		return d
	}
	return DefaultSeedingCooldown
}

// Windows are the rule's hours as discord/seedhours reads them.
func (r SeedingRule) Windows() []seedhours.Window {
	out := make([]seedhours.Window, 0, len(r.Hours))
	for _, h := range r.Hours {
		out = append(out, seedhours.Window{Days: h.Days, From: h.From, To: h.To})
	}
	return out
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
	return d.GuildID == "" && d.Roles.IsZero() && len(d.Recognition) == 0 && len(d.ServerCards) == 0 && d.ModLog == "" && len(d.Seeding) == 0 &&
		d.Presence.IsZero() && len(d.Recaps) == 0 && d.Streams.IsZero() && d.News.IsZero()
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
		bad("discord.guild_id: required when roles are mapped, server cards placed, seeding set, presence placed, recaps set, streams placed or news listed")
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
	validateSeeding(d, bad)
	validatePresence(d.Presence, bad)
	validateRecaps(d, bad)
	validateStreams(d, bad)
	validateNews(d.News, bad)
}

func validateNews(n News, bad func(string, ...any)) {
	if len(n.Feeds) > MaxNewsFeeds {
		bad("discord.news.feeds: %d feeds, at most %d", len(n.Feeds), MaxNewsFeeds)
	}
	seen := map[string]int{}
	for i, f := range n.Feeds {
		field := fmt.Sprintf("discord.news.feeds[%d]", i)
		if f.AppID <= 0 || f.AppID > math.MaxInt32 {
			bad("%s.app_id: %d is not a Steam app id (730 for Counter-Strike 2)", field, f.AppID)
		}
		if !snowflakeOK(f.Channel) {
			bad("%s.channel: %q is not a Discord id", field, f.Channel)
		} else if j, dup := seen[strconv.Itoa(f.AppID)+"/"+f.Channel]; dup {
			bad("%s: app %d already posts to channel %s (discord.news.feeds[%d])", field, f.AppID, f.Channel, j)
		} else {
			seen[strconv.Itoa(f.AppID)+"/"+f.Channel] = i
		}
		label := strings.TrimSpace(f.Label)
		switch {
		case label == "":
			bad("%s.label: required, the game's name for the posts' footer", field)
		case utf8.RuneCountInString(label) > MaxNewsLabel || strings.ContainsFunc(label, unicode.IsControl):
			bad("%s.label: one line of at most %d characters", field, MaxNewsLabel)
		}
		names := map[string]bool{}
		for j, name := range f.Feeds {
			switch {
			case !newsFeedName.MatchString(name):
				bad("%s.feeds[%d]: %q is not a Steam feed name (%s)", field, j, name, NewsDefaultFeed)
			case names[name]:
				bad("%s.feeds[%d]: %q is listed twice", field, j, name)
			}
			names[name] = true
		}
		if f.Color < 0 || f.Color > 0xffffff {
			bad("%s.color: %#x is not a color from 0x000000 to 0xffffff", field, f.Color)
		}
	}
}

func validateStreams(d Discord, bad func(string, ...any)) {
	s := d.Streams
	if s.IsZero() {
		return
	}
	if !snowflakeOK(s.Channel) {
		bad("discord.streams.channel: %q is not a Discord id", s.Channel)
	}
	if s.Role != "" {
		if !snowflakeOK(s.Role) {
			bad("discord.streams.role: %q is not a Discord id", s.Role)
		} else if s.Role == d.GuildID {
			bad("discord.streams.role: %q is the guild's @everyone role; name an opt-in role or none", s.Role)
		}
	}
	note := strings.TrimSpace(s.Note)
	if utf8.RuneCountInString(note) > MaxStreamsNote || strings.ContainsFunc(note, unicode.IsControl) {
		bad("discord.streams.note: one line of at most %d characters", MaxStreamsNote)
	}
	switch n := len(s.Streamers); {
	case n == 0:
		bad("discord.streams.streamers: at least one streamer")
	case n > MaxStreamers:
		bad("discord.streams.streamers: %d streamers, at most %d", n, MaxStreamers)
	}
	seen := map[string]int{}
	for i, st := range s.Streamers {
		field := fmt.Sprintf("discord.streams.streamers[%d]", i)
		login, ok := streamLogins[st.Platform]
		if !ok {
			bad("%s.platform: %q is not a platform (%s)", field, st.Platform, strings.Join(StreamPlatforms, ", "))
		} else if !login.MatchString(st.Login) {
			bad("%s.login: %q is not a %s channel name (lower case, as in its URL)", field, st.Login, st.Platform)
		} else if j, dup := seen[st.Platform+"/"+st.Login]; dup {
			bad("%s: %s %q is already listed (discord.streams.streamers[%d])", field, st.Platform, st.Login, j)
		} else {
			seen[st.Platform+"/"+st.Login] = i
		}
		label := strings.TrimSpace(st.Label)
		if utf8.RuneCountInString(label) > MaxStreamerLabel || strings.ContainsFunc(label, unicode.IsControl) {
			bad("%s.label: one line of at most %d characters", field, MaxStreamerLabel)
		}
	}
}

func validateRecaps(d Discord, bad func(string, ...any)) {
	if len(d.Recaps) > MaxRecapRules {
		bad("discord.recaps: %d rules, at most %d", len(d.Recaps), MaxRecapRules)
	}
	servers := map[string]int{}
	for i, r := range d.Recaps {
		field := fmt.Sprintf("discord.recaps[%d]", i)
		if !serverID.MatchString(r.Server) {
			bad("%s.server: %q is not a server id (lower-case letters, digits and hyphens)", field, r.Server)
		} else if j, dup := servers[r.Server]; dup {
			bad("%s.server: %q already has a rule (discord.recaps[%d])", field, r.Server, j)
		} else {
			servers[r.Server] = i
		}
		if !snowflakeOK(r.Channel) {
			bad("%s.channel: %q is not a Discord id", field, r.Channel)
		}
		title := strings.TrimSpace(r.Title)
		if title == "" || utf8.RuneCountInString(title) > MaxRecapTitle || strings.ContainsFunc(title, unicode.IsControl) {
			bad("%s.title: one line of 1 to %d characters", field, MaxRecapTitle)
		}
		if r.Floor < 0 || r.Floor > MaxRecapFloor {
			bad("%s.floor: %d is not between 0 and %d", field, r.Floor, MaxRecapFloor)
		}
		_, probs := seedhours.Parse(r.Windows())
		for _, p := range probs {
			bad("%s.hours%s: %s", field, p.Field, p.Reason)
		}
		note := strings.TrimSpace(r.Note)
		if utf8.RuneCountInString(note) > MaxRecapNote || strings.ContainsFunc(note, unicode.IsControl) {
			bad("%s.note: one line of at most %d characters", field, MaxRecapNote)
		}
	}
}

func validatePresence(p Presence, bad func(string, ...any)) {
	if p.IsZero() {
		return
	}
	if !snowflakeOK(p.Channel) {
		bad("discord.presence.channel: %q is not a Discord id", p.Channel)
	}
	note := strings.TrimSpace(p.Note)
	if utf8.RuneCountInString(note) > MaxPresenceNote || strings.ContainsFunc(note, unicode.IsControl) {
		bad("discord.presence.note: one line of at most %d characters", MaxPresenceNote)
	}
	switch n := len(p.Games); {
	case n == 0:
		bad("discord.presence.games: at least one game")
	case n > MaxPresenceGames:
		bad("discord.presence.games: %d games, at most %d", n, MaxPresenceGames)
	}
	games := map[string]int{}
	names := map[string]string{}
	apps := map[string]string{}
	for i, g := range p.Games {
		field := fmt.Sprintf("discord.presence.games[%d]", i)
		if !serverID.MatchString(g.Game) {
			bad("%s.game: %q is not a key (lower-case letters, digits and hyphens)", field, g.Game)
		} else if j, dup := games[g.Game]; dup {
			bad("%s.game: %q is already on the card (discord.presence.games[%d])", field, g.Game, j)
		} else {
			games[g.Game] = i
		}
		label := strings.TrimSpace(g.Label)
		if label == "" || utf8.RuneCountInString(label) > MaxPresenceLabel || strings.ContainsFunc(label, unicode.IsControl) {
			bad("%s.label: one line of 1 to %d characters", field, MaxPresenceLabel)
		}
		if len(g.Activities)+len(g.ApplicationIDs) == 0 {
			bad("%s: names no activity and no application id, so nothing would count", field)
		}
		if len(g.Activities) > MaxPresenceActivities {
			bad("%s.activities: %d names, at most %d", field, len(g.Activities), MaxPresenceActivities)
		}
		if len(g.ApplicationIDs) > MaxPresenceActivities {
			bad("%s.application_ids: %d ids, at most %d", field, len(g.ApplicationIDs), MaxPresenceActivities)
		}
		for j, a := range g.Activities {
			key := strings.ToLower(strings.TrimSpace(a))
			switch {
			case key == "" || utf8.RuneCountInString(key) > MaxPresenceActivity || strings.ContainsFunc(key, unicode.IsControl):
				bad("%s.activities[%d]: one line of 1 to %d characters", field, j, MaxPresenceActivity)
			case names[key] != "":
				bad("%s.activities[%d]: %q already counts for %s", field, j, a, names[key])
			default:
				names[key] = g.Game
			}
		}
		for j, id := range g.ApplicationIDs {
			switch {
			case !snowflakeOK(id):
				bad("%s.application_ids[%d]: %q is not a Discord id", field, j, id)
			case apps[id] != "":
				bad("%s.application_ids[%d]: %s already counts for %s", field, j, id, apps[id])
			default:
				apps[id] = g.Game
			}
		}
	}
}

func validateSeeding(d Discord, bad func(string, ...any)) {
	if len(d.Seeding) > MaxSeedingRules {
		bad("discord.seeding: %d rules, at most %d", len(d.Seeding), MaxSeedingRules)
	}
	servers := map[string]int{}
	for i, r := range d.Seeding {
		field := fmt.Sprintf("discord.seeding[%d]", i)
		if !serverID.MatchString(r.Server) {
			bad("%s.server: %q is not a server id (lower-case letters, digits and hyphens)", field, r.Server)
		} else if j, dup := servers[r.Server]; dup {
			bad("%s.server: %q already has a rule (discord.seeding[%d])", field, r.Server, j)
		} else {
			servers[r.Server] = i
		}
		if !snowflakeOK(r.Channel) {
			bad("%s.channel: %q is not a Discord id", field, r.Channel)
		}
		switch {
		case !snowflakeOK(r.Role):
			bad("%s.role: %q is not a Discord id", field, r.Role)
		case r.Role == d.GuildID:
			bad("%s.role: %q is the guild's @everyone role; a call pings only an opt-in role", field, r.Role)
		}
		if r.Below < MinSeedingBelow || r.Below > MaxSeedingBelow {
			bad("%s.below: %d is not between %d and %d", field, r.Below, MinSeedingBelow, MaxSeedingBelow)
		}
		if r.Cooldown != "" {
			cd, err := time.ParseDuration(r.Cooldown)
			switch {
			case err != nil:
				bad("%s.cooldown: %q is not a duration (2h, 90m)", field, r.Cooldown)
			case cd < MinSeedingCooldown || cd > MaxSeedingCooldown:
				bad("%s.cooldown: %s is not between %s and %s", field, r.Cooldown, MinSeedingCooldown, MaxSeedingCooldown)
			}
		}
		_, probs := seedhours.Parse(r.Windows())
		for _, p := range probs {
			bad("%s.hours%s: %s", field, p.Field, p.Reason)
		}
		note := strings.TrimSpace(r.Note)
		if utf8.RuneCountInString(note) > MaxSeedingNote || strings.ContainsFunc(note, unicode.IsControl) {
			bad("%s.note: one line of at most %d characters", field, MaxSeedingNote)
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
	for i := range s.Discord.Seeding {
		s.Discord.Seeding[i].Note = strings.TrimSpace(s.Discord.Seeding[i].Note)
		if len(s.Discord.Seeding[i].Hours) == 0 {
			s.Discord.Seeding[i].Hours = nil
		}
	}
	if len(s.Discord.Seeding) == 0 {
		s.Discord.Seeding = nil
	}
	s.Discord.Presence.Note = strings.TrimSpace(s.Discord.Presence.Note)
	for i := range s.Discord.Presence.Games {
		g := &s.Discord.Presence.Games[i]
		g.Label = strings.TrimSpace(g.Label)
		for j := range g.Activities {
			g.Activities[j] = strings.TrimSpace(g.Activities[j])
		}
		if len(g.Activities) == 0 {
			g.Activities = nil
		}
		if len(g.ApplicationIDs) == 0 {
			g.ApplicationIDs = nil
		}
	}
	if len(s.Discord.Presence.Games) == 0 {
		s.Discord.Presence.Games = nil
	}
	for i := range s.Discord.Recaps {
		r := &s.Discord.Recaps[i]
		r.Title, r.Note = strings.TrimSpace(r.Title), strings.TrimSpace(r.Note)
		if len(r.Hours) == 0 {
			r.Hours = nil
		}
	}
	if len(s.Discord.Recaps) == 0 {
		s.Discord.Recaps = nil
	}
	s.Discord.Streams.Note = strings.TrimSpace(s.Discord.Streams.Note)
	for i := range s.Discord.Streams.Streamers {
		s.Discord.Streams.Streamers[i].Label = strings.TrimSpace(s.Discord.Streams.Streamers[i].Label)
	}
	if len(s.Discord.Streams.Streamers) == 0 {
		s.Discord.Streams.Streamers = nil
	}
	for i := range s.Discord.News.Feeds {
		f := &s.Discord.News.Feeds[i]
		f.Label = strings.TrimSpace(f.Label)
		if len(f.Feeds) == 0 {
			f.Feeds = nil
		}
	}
	if len(s.Discord.News.Feeds) == 0 {
		s.Discord.News.Feeds = nil
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
