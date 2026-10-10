package servers

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"

	"gopkg.in/yaml.v3"

	"github.com/gravel-project/gravel/drivers"
	"github.com/gravel-project/gravel/games/cs2"
	"github.com/gravel-project/gravel/games/pubg"
	"github.com/gravel-project/gravel/games/seaofthieves"
	"github.com/gravel-project/gravel/games/starcitizen"
	"github.com/gravel-project/gravel/games/wardogs"
	"github.com/gravel-project/gravel/internal/store"
)

// Spec is a Game spec gravel ships (ADR-0010): what a game is, who its players are keyed by,
// which drivers control it and the publisher's rules (gravel#8). A deployment enables a game by
// naming it in servers.yaml and may tighten a band, never loosen one. A spec with no drivers is a
// catalog entry: its rules are known before gravel can run a server for it.
type Spec struct {
	ID               string   `yaml:"id"`
	Name             string   `yaml:"name"`
	IdentityProvider string   `yaml:"identity_provider"`
	Drivers          []string `yaml:"drivers"`
	Bands            []Band   `yaml:"bands"`
	// Plugins are the server plugins the publisher's rules forbid (or, with an allow list, the
	// only ones permitted); checked wherever a server image or its plugin set is built or approved.
	Plugins PluginPolicy `yaml:"plugins"`
	// NoPaidPerks is a publisher's rule that no paid benefit (a supporter perk, a crowdfunding
	// reward) may depend on the game's data or features; the benefit logic honours it.
	NoPaidPerks bool `yaml:"no_paid_perks"`
	// Layout is how the pages show the game: declarative data, so the core stays game-agnostic.
	Layout Layout `yaml:"layout"`
}

// Layout is a game's page layout (README, Leaderboards & UI).
type Layout struct {
	// Board is the columns a board of the game shows, in order; the first is what it ranks by
	// unless the reader picks another. Each is a board metric: kills, deaths, kd, time, matches.
	Board []string `yaml:"board,omitempty"`
}

// BoardMetrics are the metrics a board can rank by, which Layout.Board picks from.
var BoardMetrics = []string{store.BoardKills, store.BoardDeaths, store.BoardKD, store.BoardTime, store.BoardMatches}

// Band is a publisher guardrail on one configuration key (gravel#8), of one kind: a numeric
// range (Min, Max; a nil bound is open), a longest value (MaxLength, in characters) or the hosts
// a URL value may point at (Hosts: https, the host or a subdomain of one; empty is allowed).
// UnlessSet makes the band apply only while that key of the same section is empty.
type Band struct {
	Section   string   `yaml:"section" json:"section"`
	Key       string   `yaml:"key" json:"key"`
	Min       *int64   `yaml:"min,omitempty" json:"min,omitempty"`
	Max       *int64   `yaml:"max,omitempty" json:"max,omitempty"`
	MaxLength *int64   `yaml:"max_length,omitempty" json:"max_length,omitempty"`
	Hosts     []string `yaml:"hosts,omitempty" json:"hosts,omitempty"`
	UnlessSet string   `yaml:"unless_set,omitempty" json:"unless_set,omitempty"`
}

// Band kinds.
const (
	BandRange  = "range"
	BandLength = "length"
	BandHosts  = "hosts"
)

// Kind is the band's kind by the fields it sets, or "" when it sets none or more than one kind.
func (b Band) Kind() string {
	var kinds []string
	if b.Min != nil || b.Max != nil {
		kinds = append(kinds, BandRange)
	}
	if b.MaxLength != nil {
		kinds = append(kinds, BandLength)
	}
	if b.Hosts != nil {
		kinds = append(kinds, BandHosts)
	}
	if len(kinds) != 1 {
		return ""
	}
	return kinds[0]
}

// Driver is the band as a driver checks it.
func (b Band) Driver() drivers.Band {
	return drivers.Band{Section: b.Section, Key: b.Key, Min: b.Min, Max: b.Max, MaxLength: b.MaxLength,
		Hosts: slices.Clone(b.Hosts), UnlessSet: b.UnlessSet}
}

// Band finds the spec's band for a key.
func (s Spec) Band(section, key string) (Band, bool) {
	for _, b := range s.Bands {
		if b.Section == section && b.Key == key {
			return b, true
		}
	}
	return Band{}, false
}

// Tighten is the spec's bands with a manifest's tightenings applied; Validate has checked that
// each one tightens.
func (s Spec) Tighten(over []Band) []Band {
	out := slices.Clone(s.Bands)
	for _, o := range over {
		for i, b := range out {
			if b.Section == o.Section && b.Key == o.Key {
				if o.Min != nil {
					out[i].Min = o.Min
				}
				if o.Max != nil {
					out[i].Max = o.Max
				}
				if o.MaxLength != nil {
					out[i].MaxLength = o.MaxLength
				}
				if o.Hosts != nil {
					out[i].Hosts = slices.Clone(o.Hosts)
				}
			}
		}
	}
	return out
}

// PluginPolicy is a publisher's rule on server plugins. Names compare by their letters and digits
// only, case-insensitively, so "CS2-WeaponPaints" and "cs2_weapon_paints" are one name.
type PluginPolicy struct {
	// Deny are the forbidden plugins: a plugin whose name contains a rule's Match is refused.
	Deny []PluginRule `yaml:"deny,omitempty"`
	// Allow, when set, is the only plugins permitted, by name.
	Allow []string `yaml:"allow,omitempty"`
}

// PluginRule is one forbidden kind of plugin and the publisher's reason.
type PluginRule struct {
	Match  string `yaml:"match"`
	Reason string `yaml:"reason"`
}

// ErrPluginRefused is a plugin set the game's policy forbids; PluginError names each plugin.
var ErrPluginRefused = errors.New("servers: a plugin is not allowed")

// PluginError is ErrPluginRefused with every refused plugin and why.
type PluginError struct {
	Game    string
	Refused []RefusedPlugin
}

// RefusedPlugin is one plugin the policy refuses.
type RefusedPlugin struct {
	Name   string
	Reason string
}

func (e *PluginError) Error() string {
	parts := make([]string, 0, len(e.Refused))
	for _, r := range e.Refused {
		parts = append(parts, fmt.Sprintf("%s (%s)", r.Name, r.Reason))
	}
	return fmt.Sprintf("servers: %s does not allow these plugins: %s", e.Game, strings.Join(parts, "; "))
}

// Unwrap makes the error ErrPluginRefused.
func (e *PluginError) Unwrap() error { return ErrPluginRefused }

// CheckPlugins refuses a plugin set the spec's policy forbids, naming every refused plugin; nil
// when all are allowed.
func (s Spec) CheckPlugins(names []string) error {
	var refused []RefusedPlugin
	for _, name := range names {
		n := pluginKey(name)
		if i := slices.IndexFunc(s.Plugins.Deny, func(r PluginRule) bool { return strings.Contains(n, pluginKey(r.Match)) }); i >= 0 {
			refused = append(refused, RefusedPlugin{Name: name, Reason: s.Plugins.Deny[i].Reason})
			continue
		}
		if s.Plugins.Allow != nil && !slices.ContainsFunc(s.Plugins.Allow, func(a string) bool { return pluginKey(a) == n }) {
			refused = append(refused, RefusedPlugin{Name: name, Reason: "not on the allow list"})
		}
	}
	if len(refused) == 0 {
		return nil
	}
	return &PluginError{Game: s.Name, Refused: refused}
}

func pluginKey(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return unicode.ToLower(r)
		}
		return -1
	}, s)
}

// ParseSpec reads a spec strictly and checks its rules are well formed.
func ParseSpec(b []byte) (Spec, error) {
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	var s Spec
	if err := dec.Decode(&s); err != nil {
		return Spec{}, fmt.Errorf("servers: game spec: %w", err)
	}
	if s.ID == "" || s.Name == "" || s.IdentityProvider == "" {
		return Spec{}, fmt.Errorf("servers: game spec %q: id, name and identity_provider are required", s.ID)
	}
	var problems []string
	seen := map[string]bool{}
	for i, band := range s.Bands {
		at := fmt.Sprintf("bands[%d] (%s %s)", i, band.Section, band.Key)
		if band.Section == "" || band.Key == "" {
			problems = append(problems, at+": section and key are required")
		}
		if seen[band.Section+"\x00"+band.Key] {
			problems = append(problems, at+": listed twice")
		}
		seen[band.Section+"\x00"+band.Key] = true
		switch band.Kind() {
		case "":
			problems = append(problems, at+": set exactly one of min/max, max_length or hosts")
		case BandRange:
			if band.Min != nil && band.Max != nil && *band.Min > *band.Max {
				problems = append(problems, at+": min is above max")
			}
		case BandLength:
			if *band.MaxLength < 1 {
				problems = append(problems, at+": max_length must be at least 1")
			}
		case BandHosts:
			for _, h := range band.Hosts {
				if !validHost(h) {
					problems = append(problems, fmt.Sprintf("%s: host %q must be a lower-case domain name", at, h))
				}
			}
		}
	}
	for i, r := range s.Plugins.Deny {
		if pluginKey(r.Match) == "" || strings.TrimSpace(r.Reason) == "" {
			problems = append(problems, fmt.Sprintf("plugins.deny[%d]: match (with a letter or digit) and reason are required", i))
		}
	}
	for i, m := range s.Layout.Board {
		if !slices.Contains(BoardMetrics, m) || slices.Index(s.Layout.Board, m) != i {
			problems = append(problems, fmt.Sprintf("layout.board[%d]: %q is not one of %s, or is listed twice", i, m, strings.Join(BoardMetrics, ", ")))
		}
	}
	for i, a := range s.Plugins.Allow {
		if pluginKey(a) == "" {
			problems = append(problems, fmt.Sprintf("plugins.allow[%d]: %q has no letter or digit", i, a))
		}
	}
	if len(problems) > 0 {
		return Spec{}, fmt.Errorf("servers: game spec %q: %s", s.ID, strings.Join(problems, "; "))
	}
	return s, nil
}

func validHost(h string) bool {
	if h == "" || h != strings.ToLower(h) || !strings.Contains(h, ".") || strings.HasPrefix(h, ".") || strings.HasSuffix(h, ".") {
		return false
	}
	return !strings.ContainsFunc(h, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' && r != '.'
	})
}

// Specs are the Game specs this hub ships, by id.
func Specs() map[string]Spec {
	out := map[string]Spec{}
	for _, raw := range [][]byte{wardogs.GameSpec, cs2.GameSpec, seaofthieves.GameSpec, starcitizen.GameSpec, pubg.GameSpec} {
		s, err := ParseSpec(raw)
		if err != nil {
			panic(err) // a shipped spec is tested; a broken one is a build error
		}
		out[s.ID] = s
	}
	return out
}
