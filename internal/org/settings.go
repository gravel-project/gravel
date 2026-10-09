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

	"gopkg.in/yaml.v3"
)

// SettingsVersion is the settings document format this build reads and writes.
const SettingsVersion = 1

// Settings is the Organization settings resource: every host-configurable knob, one document,
// managed through the API or applied from a committed manifest (`gravel-hub settings apply`).
// Theme tokens and navigation links first (gravel#7), the Discord role mapping (ADR-0008);
// token lifetimes and layout overrides join it with their features. Zero values mean "the
// default".
type Settings struct {
	Version int       `json:"version" yaml:"version"`
	Theme   Theme     `json:"theme" yaml:"theme"`
	Nav     []NavLink `json:"nav,omitempty" yaml:"nav,omitempty"`
	Discord Discord   `json:"discord,omitzero" yaml:"discord,omitempty"`
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

// Discord is the guild role sync manages and the roles it maps (ADR-0008 §3). Ids are Discord
// snowflakes as strings. Everything here is public: ids, never a credential. Empty means role
// sync has nothing to do.
type Discord struct {
	GuildID     string        `json:"guild_id,omitempty" yaml:"guild_id,omitempty"`
	Roles       DiscordRoles  `json:"roles,omitzero" yaml:"roles,omitempty"`
	Recognition []Recognition `json:"recognition,omitempty" yaml:"recognition,omitempty"`
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

// IsZero reports whether no mapping is set.
func (d Discord) IsZero() bool {
	return d.GuildID == "" && d.Roles.IsZero() && len(d.Recognition) == 0
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
)

// ErrInvalidSettings wraps every validation failure; the message says which field and why.
var ErrInvalidSettings = errors.New("invalid settings")

var (
	cssColor = regexp.MustCompile(`^(#[0-9a-fA-F]{3}|#[0-9a-fA-F]{6}|#[0-9a-fA-F]{8}|[a-zA-Z]{3,20}|rgba?\([0-9.,% ]+\)|hsla?\([0-9.,% deg]+\))$`)
	cssFont  = regexp.MustCompile(`^[A-Za-z0-9 ,'"-]{1,120}$`)
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
		if strings.TrimSpace(l.Label) == "" || len(l.Label) > MaxNavLabel {
			bad("nav[%d].label: required, at most %d characters", i, MaxNavLabel)
		}
		if !navURLOK(l.URL) {
			bad("nav[%d].url: %q must be an http(s) URL or a site-relative path", i, l.URL)
		}
		if l.Placement != "" && l.Placement != PlacementHeader && l.Placement != PlacementFooter {
			bad("nav[%d].placement: %q is not %q or %q", i, l.Placement, PlacementHeader, PlacementFooter)
		}
		if l.Role != RoleEveryone && l.Role != RoleOwner {
			bad("nav[%d].role: %q is not empty or %q", i, l.Role, RoleOwner)
		}
	}
	validateDiscord(s.Discord, bad)
	if len(errs) == 0 {
		return nil
	}
	return errors.Join(errs...)
}

func validateDiscord(d Discord, bad func(string, ...any)) {
	if d.IsZero() {
		return
	}
	if d.GuildID == "" {
		bad("discord.guild_id: required when roles are mapped")
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
