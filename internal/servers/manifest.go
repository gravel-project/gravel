package servers

import (
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	_ "time/tzdata" // seeding time zones resolve in a container with no zoneinfo

	"gopkg.in/yaml.v3"
)

// ManifestVersion is the servers.yaml format this hub reads.
const ManifestVersion = 1

// Manifest is servers.yaml: the games a deployment enables and the servers it registers
// (ADR-0010). `gravel-hub servers apply` makes the hub match it; `export` prints it back.
type Manifest struct {
	Version int       `yaml:"version"`
	Games   []GameRef `yaml:"games"`
	Servers []Server  `yaml:"servers"`
}

// GameRef enables a shipped Game spec, with optional tightenings of its bands.
type GameRef struct {
	ID    string `yaml:"id"`
	Bands []Band `yaml:"bands,omitempty"`
}

// Server is one ManagedServer: a server that exists somewhere and a driver controls. It holds no
// credential: CredentialFile names a file the hub reads at start and on change, and never stores.
type Server struct {
	ID             string   `yaml:"id"`
	Name           string   `yaml:"name"`
	Game           string   `yaml:"game"`
	Driver         string   `yaml:"driver"`
	Location       string   `yaml:"location"`
	Endpoint       string   `yaml:"endpoint"`
	CredentialFile string   `yaml:"credential_file"`
	PollInterval   Duration `yaml:"poll_interval,omitempty"`
	Trust          string   `yaml:"trust"`
	Seeding        *Seeding `yaml:"seeding,omitempty"`
	Feed           *Feed    `yaml:"feed,omitempty"`
}

// Feed is where a server that pushes events posts them, and the token it posts with (ADR-0011).
// The driver writes both into the server's configuration (War Dogs: [WDServerFeed]); the hub's
// ingest route takes the token as the server's. Like the credential, the token is a file the hub
// reads and never stores: its first line is the token, an optional second line the previous one,
// accepted for RotationGrace while the server still has it.
type Feed struct {
	// URL is the origin the server posts to; the game appends its own path (/api/ingest/events).
	URL       string `yaml:"url" json:"url"`
	TokenFile string `yaml:"token_file" json:"token_file"`
}

// Seeding is when a server wants players (hidden-token-gaming/htg#32 reads it): below Threshold
// during Hours, at most once per Cooldown, never during Quiet.
type Seeding struct {
	Threshold int      `yaml:"threshold" json:"threshold"`
	Hours     string   `yaml:"hours" json:"hours"`
	Quiet     string   `yaml:"quiet,omitempty" json:"quiet,omitempty"`
	Timezone  string   `yaml:"timezone" json:"timezone"`
	Cooldown  Duration `yaml:"cooldown,omitempty" json:"cooldown,omitempty"`
}

// Trust levels.
const (
	TrustOfficial  = "official"
	TrustCommunity = "community"
)

// Defaults and limits.
const (
	DefaultPollInterval = 15 * time.Second
	MinPollInterval     = 5 * time.Second
	MaxPollInterval     = 10 * time.Minute
	DefaultCooldown     = time.Hour
	MaxNameLength       = 64
	MaxServers          = 100
)

// ErrInvalidManifest wraps every validation failure; the message says which field and why.
var ErrInvalidManifest = errors.New("invalid servers manifest")

var (
	idPattern     = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	windowPattern = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]-([01][0-9]|2[0-3]):[0-5][0-9]$`)
)

// ParseManifest reads a manifest strictly (an unknown key is an error) and fills the defaults.
func ParseManifest(b []byte) (Manifest, error) {
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return Manifest{}, fmt.Errorf("servers: manifest: %w", err)
	}
	return m.Normalized(), nil
}

// Normalized fills the defaults and sorts games and servers by id, so two manifests that mean
// the same thing compare equal.
func (m Manifest) Normalized() Manifest {
	if m.Version == 0 {
		m.Version = ManifestVersion
	}
	m.Games = slices.Clone(m.Games)
	m.Servers = slices.Clone(m.Servers)
	for i := range m.Games {
		m.Games[i].Bands = slices.Clone(m.Games[i].Bands)
		slices.SortFunc(m.Games[i].Bands, func(a, b Band) int {
			return strings.Compare(a.Section+"\x00"+a.Key, b.Section+"\x00"+b.Key)
		})
		if len(m.Games[i].Bands) == 0 {
			m.Games[i].Bands = nil
		}
	}
	for i := range m.Servers {
		s := &m.Servers[i]
		if s.PollInterval == 0 {
			s.PollInterval = Duration(DefaultPollInterval)
		}
		if s.Seeding != nil {
			sd := *s.Seeding
			if sd.Cooldown == 0 {
				sd.Cooldown = Duration(DefaultCooldown)
			}
			s.Seeding = &sd
		}
	}
	slices.SortFunc(m.Games, func(a, b GameRef) int { return strings.Compare(a.ID, b.ID) })
	slices.SortFunc(m.Servers, func(a, b Server) int { return strings.Compare(a.ID, b.ID) })
	return m
}

// Marshal writes the manifest as YAML, normalized.
func (m Manifest) Marshal() ([]byte, error) { return yaml.Marshal(m.Normalized()) }

// Validate reports every problem at once, each wrapping ErrInvalidManifest. specs are the
// shipped Game specs and drivers the drivers this hub has.
func (m Manifest) Validate(specs map[string]Spec, drivers []string) error {
	var errs []error
	bad := func(format string, a ...any) {
		errs = append(errs, fmt.Errorf("%w: "+format, append([]any{ErrInvalidManifest}, a...)...))
	}
	if m.Version != ManifestVersion {
		bad("version: got %d, this hub reads version %d", m.Version, ManifestVersion)
	}
	games := map[string]bool{}
	for i, g := range m.Games {
		spec, ok := specs[g.ID]
		switch {
		case !ok:
			bad("games[%d].id: %q is not a game this hub ships (%s)", i, g.ID, strings.Join(sortedKeys(specs), ", "))
		case games[g.ID]:
			bad("games[%d].id: %q is listed twice", i, g.ID)
		}
		games[g.ID] = true
		if ok {
			validateBands(fmt.Sprintf("games[%d]", i), spec, g.Bands, bad)
		}
	}
	if len(m.Servers) > MaxServers {
		bad("servers: %d servers, at most %d", len(m.Servers), MaxServers)
	}
	seen := map[string]bool{}
	for i, s := range m.Servers {
		at := fmt.Sprintf("servers[%d]", i)
		if !idPattern.MatchString(s.ID) {
			bad("%s.id: %q must be lower-case letters, digits and hyphens, starting with a letter or digit, at most 63", at, s.ID)
		} else if seen[s.ID] {
			bad("%s.id: %q is listed twice", at, s.ID)
		}
		seen[s.ID] = true
		if strings.TrimSpace(s.Name) == "" || len(s.Name) > MaxNameLength {
			bad("%s.name: required, at most %d characters", at, MaxNameLength)
		}
		spec, gameOK := specs[s.Game]
		if !games[s.Game] {
			bad("%s.game: %q is not in games", at, s.Game)
		}
		switch {
		case !slices.Contains(drivers, s.Driver):
			bad("%s.driver: %q is not a driver this hub has (%s)", at, s.Driver, strings.Join(drivers, ", "))
		case gameOK && len(spec.Drivers) == 0:
			bad("%s.game: %s is a catalog entry; gravel has no driver for its servers yet", at, spec.Name)
		case gameOK && !slices.Contains(spec.Drivers, s.Driver):
			bad("%s.driver: %q does not control %s (%s)", at, s.Driver, spec.Name, strings.Join(spec.Drivers, ", "))
		}
		if !idPattern.MatchString(s.Location) {
			bad("%s.location: %q must be lower-case letters, digits and hyphens", at, s.Location)
		}
		if u, err := url.Parse(s.Endpoint); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.User != nil {
			bad("%s.endpoint: %q must be an http(s) origin with no path, query or credentials", at, s.Endpoint)
		}
		if !filepath.IsAbs(s.CredentialFile) || filepath.Clean(s.CredentialFile) != s.CredentialFile {
			bad("%s.credential_file: %q must be a clean absolute path (a podman secret: /run/secrets/<name>)", at, s.CredentialFile)
		}
		if d := time.Duration(s.PollInterval); d < MinPollInterval || d > MaxPollInterval {
			bad("%s.poll_interval: %s is outside %s–%s", at, d, MinPollInterval, MaxPollInterval)
		}
		if s.Trust != TrustOfficial && s.Trust != TrustCommunity {
			bad("%s.trust: %q is not %q or %q", at, s.Trust, TrustOfficial, TrustCommunity)
		}
		if s.Seeding != nil {
			validateSeeding(at+".seeding", *s.Seeding, bad)
		}
		if f := s.Feed; f != nil {
			if u, err := url.Parse(f.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.User != nil {
				bad("%s.feed.url: %q must be an http(s) origin with no path, query or credentials (the game adds its path)", at, f.URL)
			}
			if !filepath.IsAbs(f.TokenFile) || filepath.Clean(f.TokenFile) != f.TokenFile {
				bad("%s.feed.token_file: %q must be a clean absolute path (a podman secret: /run/secrets/<name>)", at, f.TokenFile)
			} else if f.TokenFile == s.CredentialFile {
				bad("%s.feed.token_file: the feed token must not be the RCON credential", at)
			}
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return errors.Join(errs...)
}

func validateBands(at string, spec Spec, over []Band, bad func(string, ...any)) {
	seen := map[string]bool{}
	for i, o := range over {
		where := fmt.Sprintf("%s.bands[%d]", at, i)
		base, ok := spec.Band(o.Section, o.Key)
		if !ok {
			bad("%s: %s has no band for %s %s; a manifest may only tighten the spec's bands", where, spec.Name, o.Section, o.Key)
			continue
		}
		if seen[o.Section+"\x00"+o.Key] {
			bad("%s: %s %s is listed twice", where, o.Section, o.Key)
		}
		seen[o.Section+"\x00"+o.Key] = true
		if o.UnlessSet != "" {
			bad("%s.unless_set: the spec's condition can't be changed by a manifest", where)
		}
		kind := o.Kind()
		switch {
		case kind == "" && o.Min == nil && o.Max == nil && o.MaxLength == nil && o.Hosts == nil:
			bad("%s: neither min nor max is set", where)
			continue
		case kind == "":
			bad("%s: set only one of min/max, max_length or hosts", where)
			continue
		case kind != base.Kind():
			bad("%s: the spec's band for %s is a %s band, not a %s band", where, o.Key, base.Kind(), kind)
			continue
		}
		switch kind {
		case BandRange:
			if o.Min != nil && base.Min != nil && *o.Min < *base.Min {
				bad("%s.min: %d loosens the spec's %d", where, *o.Min, *base.Min)
			}
			if o.Max != nil && base.Max != nil && *o.Max > *base.Max {
				bad("%s.max: %d loosens the spec's %d", where, *o.Max, *base.Max)
			}
			lo, hi := o.Min, o.Max
			if lo == nil {
				lo = base.Min
			}
			if hi == nil {
				hi = base.Max
			}
			if lo != nil && hi != nil && *lo > *hi {
				bad("%s: min %d is above max %d", where, *lo, *hi)
			}
		case BandLength:
			if *o.MaxLength < 1 || *o.MaxLength > *base.MaxLength {
				bad("%s.max_length: %d must be from 1 to the spec's %d", where, *o.MaxLength, *base.MaxLength)
			}
		case BandHosts:
			if len(o.Hosts) == 0 {
				bad("%s.hosts: an empty list allows no URL; leave the value empty in the configuration instead", where)
			}
			for _, h := range o.Hosts {
				if !slices.ContainsFunc(base.Hosts, func(b string) bool { return h == b || strings.HasSuffix(h, "."+b) }) {
					bad("%s.hosts: %q loosens the spec's hosts (%s)", where, h, strings.Join(base.Hosts, ", "))
				}
			}
		}
	}
}

func validateSeeding(at string, s Seeding, bad func(string, ...any)) {
	if s.Threshold < 1 || s.Threshold > 1000 {
		bad("%s.threshold: %d is outside 1–1000", at, s.Threshold)
	}
	if !windowPattern.MatchString(s.Hours) {
		bad("%s.hours: %q is not HH:MM-HH:MM", at, s.Hours)
	}
	if s.Quiet != "" && !windowPattern.MatchString(s.Quiet) {
		bad("%s.quiet: %q is not HH:MM-HH:MM", at, s.Quiet)
	}
	if s.Timezone == "" {
		bad("%s.timezone: required (an IANA zone such as America/Chicago)", at)
	} else if _, err := time.LoadLocation(s.Timezone); err != nil {
		bad("%s.timezone: %q is not an IANA time zone", at, s.Timezone)
	}
	if d := time.Duration(s.Cooldown); d < time.Minute || d > 24*time.Hour {
		bad("%s.cooldown: %s is outside 1m–24h", at, d)
	}
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// Duration is a time.Duration written as "15s" in YAML and JSON.
type Duration time.Duration

// String is the duration as Go writes it.
func (d Duration) String() string { return time.Duration(d).String() }

// MarshalYAML writes "15s".
func (d Duration) MarshalYAML() (any, error) { return d.String(), nil }

// UnmarshalYAML reads "15s".
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("line %d: %q is not a duration (15s, 2m, 1h)", n.Line, s)
	}
	*d = Duration(v)
	return nil
}

// MarshalJSON writes "15s".
func (d Duration) MarshalJSON() ([]byte, error) { return []byte(`"` + d.String() + `"`), nil }

// UnmarshalJSON reads "15s".
func (d *Duration) UnmarshalJSON(b []byte) error {
	v, err := time.ParseDuration(strings.Trim(string(b), `"`))
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}
