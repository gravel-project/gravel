package cloudvm

import (
	"errors"
	"fmt"
	"maps"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// Profile is a Game spec's cloudvm section: how the game runs on a VM the hub makes (principle
// 12: the per-game knowledge is data in the spec, the driver is game-agnostic). The container
// flavor only, for now: an image, the user it runs as, where its install lives, its ports, and
// the environment it reads its settings and secrets from.
type Profile struct {
	// Image is the game's container image, pinned by digest.
	Image string `yaml:"image"`
	// RuntimeUID is the user the image runs as; the VM's runtime user is mapped onto it, so the
	// install on the volume belongs to the VM's user.
	RuntimeUID int `yaml:"runtime_uid"`
	// InstallDir is where the image keeps the game's install; the data volume is mounted there.
	InstallDir string `yaml:"install_dir"`
	// VolumeMinGiB is the smallest data volume the install fits on.
	VolumeMinGiB int `yaml:"volume_min_gib"`
	// LogTimezone is the timezone the game writes its log timestamps in (IANA).
	LogTimezone string `yaml:"log_timezone"`
	// ReadyTimeout bounds a first start: the game's install is downloaded then.
	ReadyTimeout Duration `yaml:"ready_timeout"`
	Ports        []Port   `yaml:"ports"`
	// RCONPort is the TCP port the hub controls the game on (the firewall admits only the hub).
	RCONPort int `yaml:"rcon_port"`
	// JoinPort is the port players connect to, shown with the address; JoinHint says where the
	// address goes, in the game's terms.
	JoinPort int    `yaml:"join_port"`
	JoinHint string `yaml:"join_hint"`
	// Env are fixed environment lines every server of the game gets.
	Env map[string]string `yaml:"env"`
	// Secrets name the environment variables the hub's secrets are passed in.
	Secrets SecretEnv `yaml:"secrets"`
	// Settings are what a server's manifest may set, and how each reaches the game.
	Settings []Setting `yaml:"settings"`
	// Ladder is the sizes a server moves through as its population grows, smallest first: the
	// default a server's scaling policy walks (#37; a manifest may give its own).
	Ladder []Tier `yaml:"ladder,omitempty"`
	// PlayersSetting is the int setting a rung's players are written to, so the game takes as
	// many players as the machine it runs on is sized for ("max_players"); "" writes none.
	PlayersSetting string `yaml:"players_setting,omitempty"`
	// Hooks are files the hub keeps in the game's install on the volume, written with the
	// environment file and checked with it (#170): a script the image runs at start, say. They hold
	// no secret; the environment does.
	Hooks []Hook `yaml:"hooks,omitempty"`
	// Teams are the game's teams as its status shows them, each with the value its players' team
	// carries (#159): the hub counts the players on each. Empty when the game has none.
	Teams []Team `yaml:"teams,omitempty"`
}

// Hook is one file the hub keeps in a game's install: Path is relative to the install directory
// (the data volume's game directory on the host), Content is the whole file.
type Hook struct {
	Path    string `yaml:"path"`
	Content string `yaml:"content"`
}

// hookPath is the characters a hook's path may hold: it is written into a shell script unquoted.
var hookPath = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)

// MaxHookBytes bounds a game's hooks together: they travel inside the command the hub runs over
// SSH, which is one argument on the host (Linux caps one at 128 KiB).
const MaxHookBytes = 32 << 10

// Team is one of a game's teams: the name players see, and the key its players' team carries
// ("TERRORIST" for "T"); an empty key is the name.
type Team struct {
	Name string `yaml:"name"`
	Key  string `yaml:"key,omitempty"`
}

// Port is one port the game listens on.
type Port struct {
	Name     string `yaml:"name"`
	Protocol string `yaml:"protocol"`
	Port     int    `yaml:"port"`
	// Public ports are open to everyone; the others to the hub only.
	Public bool `yaml:"public"`
	// When names a bool setting that must be true for the port to be open.
	When string `yaml:"when,omitempty"`
}

// SecretEnv names the environment variables that carry the hub's secrets.
type SecretEnv struct {
	RCONPassword   string `yaml:"rcon_password"`
	JoinPassword   string `yaml:"join_password,omitempty"`
	PublisherToken string `yaml:"publisher_token,omitempty"`
	// FeedURL carries the URL the game pushes its log to, with the feed token in it.
	FeedURL string `yaml:"feed_url,omitempty"`
}

// Setting is one key a manifest's settings may set.
type Setting struct {
	Key  string `yaml:"key"`
	Env  string `yaml:"env"`
	Type string `yaml:"type"` // string, int, bool or enum
	// Values are an enum's allowed values.
	Values []string `yaml:"values,omitempty"`
	// Min and Max bound an int.
	Min *int64 `yaml:"min,omitempty"`
	Max *int64 `yaml:"max,omitempty"`
	// MaxLength bounds a string, in characters.
	MaxLength int `yaml:"max_length,omitempty"`
	// Pattern is a regular expression a string must match whole.
	Pattern string `yaml:"pattern,omitempty"`
	// Default is the value when the manifest sets none; "" sets nothing.
	Default string `yaml:"default,omitempty"`
	// DefaultFrom takes the default from the server: "name" is its display name.
	DefaultFrom string `yaml:"default_from,omitempty"`
	// Conflicts are settings that may not be set with this one; Requires, ones that must be.
	Conflicts []string `yaml:"conflicts,omitempty"`
	Requires  []string `yaml:"requires,omitempty"`
}

// Setting types.
const (
	TypeString = "string"
	TypeInt    = "int"
	TypeBool   = "bool"
	TypeEnum   = "enum"
)

// Tier is one rung of a ladder: a size and the players it holds.
type Tier struct {
	Size    string `yaml:"size"`
	Players int    `yaml:"players"`
}

// Duration is a time.Duration written as "45m" in YAML.
type Duration time.Duration

// UnmarshalYAML reads "45m".
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("line %d: %q is not a duration (45m, 1h)", n.Line, s)
	}
	*d = Duration(v)
	return nil
}

// MarshalYAML writes "45m0s".
func (d Duration) MarshalYAML() (any, error) { return time.Duration(d).String(), nil }

var (
	envRE   = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)
	keyRE   = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	imageRE = regexp.MustCompile(`^[a-z0-9][a-z0-9./_-]*(:[A-Za-z0-9._-]+)?@sha256:[0-9a-f]{64}$`)
)

// Validate checks a profile as gravel ships it.
func (p Profile) Validate() error {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf("cloudvm: profile: "+format, a...)) }
	if !imageRE.MatchString(p.Image) {
		bad("image %q must be pinned by digest (name@sha256:…)", p.Image)
	}
	if p.RuntimeUID < 0 || p.RuntimeUID > 65535 {
		bad("runtime_uid %d is not a user id", p.RuntimeUID)
	}
	if !strings.HasPrefix(p.InstallDir, "/") || strings.ContainsAny(p.InstallDir, " :\n") {
		bad("install_dir %q must be an absolute path without spaces or colons", p.InstallDir)
	}
	if p.VolumeMinGiB < 1 {
		bad("volume_min_gib must be at least 1")
	}
	if _, err := time.LoadLocation(p.LogTimezone); err != nil || p.LogTimezone == "" {
		bad("log_timezone %q is not an IANA timezone", p.LogTimezone)
	}
	if time.Duration(p.ReadyTimeout) < time.Minute {
		bad("ready_timeout must be at least a minute")
	}
	rcon := false
	for _, port := range p.Ports {
		if port.Protocol != "tcp" && port.Protocol != "udp" {
			bad("port %s: protocol %q is not tcp or udp", port.Name, port.Protocol)
		}
		if port.Port < 1 || port.Port > 65535 {
			bad("port %s: %d is not a port", port.Name, port.Port)
		}
		if port.When != "" {
			if s, ok := p.setting(port.When); !ok || s.Type != TypeBool {
				bad("port %s: when %q is not a bool setting", port.Name, port.When)
			}
		}
		if port.Protocol == "tcp" && port.Port == p.RCONPort {
			rcon = true
			if port.Public {
				bad("port %s: the RCON port is open to the hub only, never public", port.Name)
			}
		}
	}
	if !rcon {
		bad("rcon_port %d is not one of the ports (tcp)", p.RCONPort)
	}
	if !slices.ContainsFunc(p.Ports, func(x Port) bool { return x.Port == p.JoinPort && x.Public }) {
		bad("join_port %d is not one of the public ports", p.JoinPort)
	}
	if strings.TrimSpace(p.JoinHint) == "" || len(p.JoinHint) > 200 {
		bad("join_hint: one line, at most 200 characters")
	}
	for k := range p.Env {
		if !envRE.MatchString(k) {
			bad("env %q is not an environment variable name", k)
		}
	}
	if !envRE.MatchString(p.Secrets.RCONPassword) {
		bad("secrets.rcon_password must name an environment variable")
	}
	for _, e := range []string{p.Secrets.JoinPassword, p.Secrets.PublisherToken, p.Secrets.FeedURL} {
		if e != "" && !envRE.MatchString(e) {
			bad("secrets: %q is not an environment variable name", e)
		}
	}
	seen := map[string]bool{}
	envs := map[string]bool{}
	for k := range p.Env {
		envs[k] = true
	}
	for _, e := range []string{p.Secrets.RCONPassword, p.Secrets.JoinPassword, p.Secrets.PublisherToken, p.Secrets.FeedURL} {
		if e != "" {
			envs[e] = true
		}
	}
	for _, s := range p.Settings {
		if !keyRE.MatchString(s.Key) || seen[s.Key] {
			bad("setting %q: a lower-case key, once", s.Key)
		}
		seen[s.Key] = true
		if !envRE.MatchString(s.Env) || envs[s.Env] {
			bad("setting %s: env %q must be a variable no other line sets", s.Key, s.Env)
		}
		envs[s.Env] = true
		switch s.Type {
		case TypeString, TypeInt, TypeBool:
		case TypeEnum:
			if len(s.Values) == 0 {
				bad("setting %s: an enum needs values", s.Key)
			}
		default:
			bad("setting %s: type %q is not string, int, bool or enum", s.Key, s.Type)
		}
		if s.Pattern != "" {
			if _, err := regexp.Compile(`^(?:` + s.Pattern + `)$`); err != nil {
				bad("setting %s: pattern: %v", s.Key, err)
			}
		}
		if s.DefaultFrom != "" && s.DefaultFrom != "name" {
			bad("setting %s: default_from %q is not name", s.Key, s.DefaultFrom)
		}
		if s.Default != "" {
			if err := s.check(s.Default); err != nil {
				bad("setting %s: the default: %v", s.Key, err)
			}
		}
	}
	for _, s := range p.Settings {
		for _, o := range append(slices.Clone(s.Conflicts), s.Requires...) {
			if !seen[o] {
				bad("setting %s: %q is not a setting", s.Key, o)
			}
		}
	}
	if err := p.ValidateLadder(p.Ladder); err != nil {
		for _, e := range strings.Split(err.Error(), "\n") {
			bad("%s", e)
		}
	}
	if p.PlayersSetting != "" {
		if s, ok := p.setting(p.PlayersSetting); !ok || s.Type != TypeInt {
			bad("players_setting %q is not an int setting", p.PlayersSetting)
		}
	}
	paths, size := map[string]bool{}, 0
	for i, h := range p.Hooks {
		size += len(h.Content)
		switch clean := path.Clean(h.Path); {
		case h.Path == "" || path.IsAbs(h.Path) || clean != h.Path || clean == "." || strings.HasPrefix(clean, "../"):
			bad("hooks[%d]: path %q is not a plain path inside the install", i, h.Path)
		case !hookPath.MatchString(h.Path):
			bad("hooks[%d]: path %q may hold only letters, digits, '.', '_', '-' and '/'", i, h.Path)
		case paths[h.Path]:
			bad("hooks[%d]: %q is listed twice", i, h.Path)
		case strings.TrimSpace(h.Content) == "":
			bad("hooks[%d]: %s has no content", i, h.Path)
		}
		paths[h.Path] = true
	}
	if size > MaxHookBytes {
		bad("hooks: %d bytes together, at most %d", size, MaxHookBytes)
	}
	teams := map[string]bool{}
	for i, t := range p.Teams {
		switch {
		case strings.TrimSpace(t.Name) == "":
			bad("teams[%d]: a name is required", i)
		case teams[strings.ToLower(t.Name)]:
			bad("teams[%d]: %q is listed twice", i, t.Name)
		}
		teams[strings.ToLower(t.Name)] = true
	}
	return errors.Join(errs...)
}

// ValidateLadder checks a ladder for the game: every rung a size and players, the sizes distinct,
// the players growing rung by rung, and each rung's players a value the players setting takes.
func (p Profile) ValidateLadder(ladder []Tier) error {
	var errs []error
	seen := map[string]bool{}
	for i, t := range ladder {
		if t.Size == "" || t.Players < 1 {
			errs = append(errs, fmt.Errorf("ladder[%d]: a size and at least one player", i))
			continue
		}
		if seen[t.Size] {
			errs = append(errs, fmt.Errorf("ladder[%d]: %s is on the ladder twice", i, t.Size))
		}
		seen[t.Size] = true
		if i > 0 && t.Players <= ladder[i-1].Players {
			errs = append(errs, fmt.Errorf("ladder[%d]: %d players is not more than the rung below's %d (smallest first)", i, t.Players, ladder[i-1].Players))
		}
		if p.PlayersSetting != "" {
			if s, ok := p.setting(p.PlayersSetting); ok {
				if err := s.check(strconv.Itoa(t.Players)); err != nil {
					errs = append(errs, fmt.Errorf("ladder[%d]: %d players: %s %w", i, t.Players, p.PlayersSetting, err))
				}
			}
		}
	}
	return errors.Join(errs...)
}

func (p Profile) setting(key string) (Setting, bool) {
	for _, s := range p.Settings {
		if s.Key == key {
			return s, true
		}
	}
	return Setting{}, false
}

// check is a value against the setting's type and limits.
func (s Setting) check(v string) error {
	switch s.Type {
	case TypeInt:
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return fmt.Errorf("%q is not a whole number", v)
		}
		if s.Min != nil && n < *s.Min || s.Max != nil && n > *s.Max {
			return fmt.Errorf("%d is outside %s", n, s.bounds())
		}
	case TypeBool:
		if v != "true" && v != "false" {
			return fmt.Errorf("%q is not true or false", v)
		}
	case TypeEnum:
		if !slices.Contains(s.Values, v) {
			return fmt.Errorf("%q is not one of %s", v, strings.Join(s.Values, ", "))
		}
	case TypeString:
		if strings.ContainsAny(v, "\n\r\x00") {
			return fmt.Errorf("a value is one line")
		}
		if s.MaxLength > 0 && utf8.RuneCountInString(v) > s.MaxLength {
			return fmt.Errorf("over %d characters", s.MaxLength)
		}
	}
	if s.Pattern != "" && !regexp.MustCompile(`^(?:`+s.Pattern+`)$`).MatchString(v) {
		return fmt.Errorf("%q does not match %s", v, s.Pattern)
	}
	return nil
}

func (s Setting) bounds() string {
	lo, hi := "…", "…"
	if s.Min != nil {
		lo = strconv.FormatInt(*s.Min, 10)
	}
	if s.Max != nil {
		hi = strconv.FormatInt(*s.Max, 10)
	}
	return lo + "–" + hi
}

// ValidateSettings checks a manifest's settings for a server of the game: every key known, every
// value of its type and inside its limits, no conflicting pair, every requirement met, and a value
// taken from the server's name (DefaultFrom) as valid as one set directly.
func (p Profile) ValidateSettings(settings map[string]string, name string) error {
	var errs []error
	for _, s := range p.Settings {
		if _, set := settings[s.Key]; !set && s.DefaultFrom == "name" {
			if err := s.check(name); err != nil {
				errs = append(errs, fmt.Errorf("%s (from the server's name): %w", s.Key, err))
			}
		}
	}
	for _, k := range slices.Sorted(maps.Keys(settings)) {
		s, ok := p.setting(k)
		if !ok {
			errs = append(errs, fmt.Errorf("%q is not a setting of this game (%s)", k, strings.Join(p.keys(), ", ")))
			continue
		}
		if err := s.check(settings[k]); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", k, err))
		}
		for _, c := range s.Conflicts {
			if _, set := settings[c]; set {
				errs = append(errs, fmt.Errorf("%s and %s may not both be set", k, c))
			}
		}
		for _, r := range s.Requires {
			if _, set := settings[r]; !set {
				errs = append(errs, fmt.Errorf("%s needs %s", k, r))
			}
		}
	}
	return errors.Join(errs...)
}

func (p Profile) keys() []string {
	out := make([]string, 0, len(p.Settings))
	for _, s := range p.Settings {
		out = append(out, s.Key)
	}
	return out
}

// Values is every setting's value for a server: the manifest's, else the default (DefaultFrom
// "name" is the server's name); a setting with neither is absent. The settings must have passed
// ValidateSettings.
func (p Profile) Values(settings map[string]string, name string) map[string]string {
	out := map[string]string{}
	for _, s := range p.Settings {
		switch v, set := settings[s.Key]; {
		case set:
			out[s.Key] = v
		case s.DefaultFrom == "name":
			out[s.Key] = name
		case s.Default != "":
			// A setting that conflicts with one the manifest set takes no default.
			if !slices.ContainsFunc(s.Conflicts, func(c string) bool { _, ok := settings[c]; return ok }) {
				out[s.Key] = s.Default
			}
		}
	}
	return out
}

// Open is whether a port is open for a server with these values (its When setting true).
func (p Port) Open(values map[string]string) bool {
	return p.When == "" || values[p.When] == "true"
}
