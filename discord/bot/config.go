package bot

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/disgoorg/snowflake/v2"
	"gopkg.in/yaml.v3"
)

// CurrentVersion is the configuration format this build reads.
const CurrentVersion = 1

// Role sync's floors: a full pass lists every guild member, and the identity log is one hub
// call per poll.
const (
	MinRoleSyncInterval = time.Minute
	MinRoleSyncPoll     = 5 * time.Second
)

// commandName is a slash command name Discord accepts, in lower case.
var commandName = regexp.MustCompile(`^[-_a-z0-9]{1,32}$`)

// MinServerCardsInterval is the server cards' floor: each pass is two hub calls.
const MinServerCardsInterval = 5 * time.Second

// Environment variables that override the secrets.
const (
	EnvDiscordToken    = "GRAVEL_BOT_DISCORD_TOKEN"
	EnvHubClientSecret = "GRAVEL_BOT_HUB_CLIENT_SECRET"
)

// Config is bot.yaml: the Discord application, the hub to call, the listeners and the log. Like
// the hub's, it is strict (an unknown key is an error), versioned, and reads each secret from
// the environment, then a file (a podman secret), then the inline key, which is for development.
type Config struct {
	Version     int         `yaml:"version"`
	Discord     Discord     `yaml:"discord"`
	Hub         Hub         `yaml:"hub"`
	RoleSync    RoleSync    `yaml:"role_sync"`
	LinkedRoles LinkedRoles `yaml:"linked_roles"`
	ServerCards ServerCards `yaml:"server_cards"`
	Moderation  Moderation  `yaml:"moderation"`
	ModLog      ModLog      `yaml:"mod_log"`
	Server      Server      `yaml:"server"`
	Log         Log         `yaml:"log"`
}

// RoleSync runs the role-sync reconciler (discord/modules/rolesync). What it maps lives in the
// Organization settings' discord section; this is how the process runs it.
type RoleSync struct {
	Enabled bool `yaml:"enabled"`
	// Interval is the full pass: every member, every mapped role, whatever changed.
	Interval time.Duration `yaml:"interval"`
	// PollInterval is how often the hub's identity log is read; a link or an unlink in it
	// starts a pass.
	PollInterval time.Duration `yaml:"poll_interval"`
	// DryRun logs the changes a pass would make and makes none: the first run against a guild
	// whose roles were assigned by hand.
	DryRun bool `yaml:"dry_run"`
}

// LinkedRoles registers the Linked Roles metadata schema with the application at start
// (discord/modules/linkedroles); the hub's /auth/discord/roles writes each member's values.
type LinkedRoles struct {
	Enabled bool `yaml:"enabled"`
}

// ServerCards keeps a live status card per server (discord/modules/servercards). Which server's
// card goes in which channel lives in the Organization settings' discord section; this is how the
// process runs it.
type ServerCards struct {
	Enabled bool `yaml:"enabled"`
	// Interval is how often the cards are read from the hub and edited where they changed.
	Interval time.Duration `yaml:"interval"`
}

// Moderation is the moderation slash command (discord/modules/moderation): kick, ban, unban,
// message and broadcast through the hub, each confirmed first. Off by default: the bot's app needs
// servers:read and servers:moderate, and the hub admits only its owner and moderators (ADR-0013).
type Moderation struct {
	Enabled bool `yaml:"enabled"`
	// Command is the slash command's name: lower-case letters, digits, - and _, at most 32.
	Command string `yaml:"command"`
}

// ModLog posts every moderation action, from the web or Discord, to the channel the Organization
// settings name (discord.mod_log; discord/modules/modlog). Off by default: reading the audit log
// needs servers:moderate on the bot's app.
type ModLog struct {
	Enabled bool `yaml:"enabled"`
	// Interval is how often the audit log is read for new entries.
	Interval time.Duration `yaml:"interval"`
}

// MinModLogInterval is the mod log's floor: each pass is one or two hub calls.
const MinModLogInterval = 5 * time.Second

// Discord is the application the bot runs as.
type Discord struct {
	// ApplicationID and PublicKey come from the Developer Portal; the key verifies HTTP
	// interactions.
	ApplicationID string `yaml:"application_id"`
	PublicKey     string `yaml:"public_key"`
	// Token is the bot token: GRAVEL_BOT_DISCORD_TOKEN, then TokenFile, then Token.
	Token     string `yaml:"token"`
	TokenFile string `yaml:"token_file"`
	// GuildIDs are the guilds the commands are registered in (instantly visible there); empty
	// registers them globally (visible everywhere within the hour).
	GuildIDs []string `yaml:"guild_ids"`
	// Gateway opens the gateway connection: member events, and interactions when the
	// application has no endpoint URL. A deployment that serves /interactions and needs no
	// events can turn it off.
	Gateway bool `yaml:"gateway"`

	tokenSource string
}

// Hub is the gravel hub the bot calls, as an app (docs/hub.md, "Service credentials").
type Hub struct {
	// URL is where the API is reached (http://gravel-hub:8080 on the stack's network).
	URL string `yaml:"url"`
	// PublicURL is the origin members use (https://app.example.com), for the links in replies;
	// URL when empty.
	PublicURL string `yaml:"public_url"`
	// ClientID and the secret are the app's credentials (`gravel-hub apps create`): the secret
	// from GRAVEL_BOT_HUB_CLIENT_SECRET, then ClientSecretFile, then ClientSecret.
	ClientID         string `yaml:"client_id"`
	ClientSecret     string `yaml:"client_secret"`
	ClientSecretFile string `yaml:"client_secret_file"`

	secretSource string
}

// Server holds the two listeners.
type Server struct {
	// Listen serves /interactions, /healthz and /readyz; put it behind TLS termination.
	Listen string `yaml:"listen"`
	// InternalListen serves /metrics; keep it off the public network.
	InternalListen  string        `yaml:"internal_listen"`
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout"`
}

// Log is the structured log.
type Log struct {
	Level  string `yaml:"level"`  // debug · info · warn · error
	Format string `yaml:"format"` // json · text
}

// Env is where Parse resolves secrets from. Tests substitute it.
type Env struct {
	LookupEnv func(string) (string, bool)
	ReadFile  func(string) ([]byte, error)
}

// OSEnv resolves secrets from the process environment and the filesystem.
func OSEnv() Env { return Env{LookupEnv: os.LookupEnv, ReadFile: os.ReadFile} }

// Default returns the configuration with every optional field at its default.
func Default() Config {
	return Config{
		Version:     CurrentVersion,
		Discord:     Discord{Gateway: true},
		RoleSync:    RoleSync{Enabled: true, Interval: 10 * time.Minute, PollInterval: 30 * time.Second},
		LinkedRoles: LinkedRoles{Enabled: true},
		ServerCards: ServerCards{Enabled: true, Interval: 15 * time.Second},
		Moderation:  Moderation{Command: "mod"},
		ModLog:      ModLog{Interval: 15 * time.Second},
		Server:      Server{Listen: "127.0.0.1:8081", InternalListen: "127.0.0.1:9091", ShutdownTimeout: 15 * time.Second},
		Log:         Log{Level: "info", Format: "json"},
	}
}

// Load reads and validates the file at path.
func Load(path string) (Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("config: %w", err)
	}
	defer func() { _ = f.Close() }()
	return Parse(f, OSEnv())
}

// Parse decodes a configuration, resolves its secrets and validates it.
func Parse(r io.Reader, env Env) (Config, error) {
	c := Default()
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil && !errors.Is(err, io.EOF) {
		return Config{}, fmt.Errorf("config: %w", err)
	}
	var err error
	if c.Discord.Token, c.Discord.tokenSource, err = resolveSecret(env, EnvDiscordToken, c.Discord.TokenFile, c.Discord.Token, "discord.token"); err != nil {
		return Config{}, err
	}
	if c.Hub.ClientSecret, c.Hub.secretSource, err = resolveSecret(env, EnvHubClientSecret, c.Hub.ClientSecretFile, c.Hub.ClientSecret, "hub.client_secret"); err != nil {
		return Config{}, err
	}
	if c.Hub.PublicURL == "" {
		c.Hub.PublicURL = c.Hub.URL
	}
	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

func resolveSecret(env Env, envVar, file, inline, inlineKey string) (value, source string, err error) {
	if v, ok := env.LookupEnv(envVar); ok && v != "" {
		return v, "env " + envVar, nil
	}
	if file != "" {
		b, err := env.ReadFile(file)
		if err != nil {
			return "", "", fmt.Errorf("config: %s_file: %w", inlineKey, err)
		}
		return strings.TrimSpace(string(b)), "file " + file, nil
	}
	if inline != "" {
		return inline, inlineKey, nil
	}
	return "", "", nil
}

// TokenSource says where the Discord token came from, never the token.
func (d Discord) TokenSource() string { return d.tokenSource }

// AppID is the application id as a snowflake; Validate has checked it parses.
func (d Discord) AppID() snowflake.ID {
	id, _ := snowflake.Parse(d.ApplicationID)
	return id
}

// Guilds are the guild ids as snowflakes; Validate has checked they parse.
func (d Discord) Guilds() []snowflake.ID {
	out := make([]snowflake.ID, 0, len(d.GuildIDs))
	for _, g := range d.GuildIDs {
		id, _ := snowflake.Parse(g)
		out = append(out, id)
	}
	return out
}

// snowflakeOK reports whether s is a Discord id: 17 to 20 digits.
func snowflakeOK(s string) bool {
	if len(s) < 17 || len(s) > 20 {
		return false
	}
	_, err := snowflake.Parse(s)
	return err == nil && strings.Trim(s, "0123456789") == ""
}

// SecretSource says where the hub client secret came from, never the secret.
func (h Hub) SecretSource() string { return h.secretSource }

// Validate reports every problem at once.
func (c Config) Validate() error {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	if c.Version != CurrentVersion {
		bad("version: got %d, this build reads version %d", c.Version, CurrentVersion)
	}
	if !snowflakeOK(c.Discord.ApplicationID) {
		bad("discord.application_id: required, the application's id (a quoted string of digits)")
	}
	for i, g := range c.Discord.GuildIDs {
		if !snowflakeOK(g) {
			bad("discord.guild_ids[%d]: %q is not a guild id", i, g)
		}
	}
	if len(c.Discord.PublicKey) != 64 || strings.Trim(strings.ToLower(c.Discord.PublicKey), "0123456789abcdef") != "" {
		bad("discord.public_key: must be the application's 64-hex-character public key")
	}
	if c.Discord.Token == "" {
		bad("discord: no token; set %s, discord.token_file or discord.token", EnvDiscordToken)
	}
	if c.Hub.URL == "" {
		bad("hub.url: required")
	} else if !originOK(c.Hub.URL) {
		bad("hub.url: %q must be an http(s) origin with no path", c.Hub.URL)
	}
	if c.Hub.PublicURL != "" && !originOK(c.Hub.PublicURL) {
		bad("hub.public_url: %q must be an http(s) origin with no path", c.Hub.PublicURL)
	}
	if strings.TrimSpace(c.Hub.ClientID) == "" {
		bad("hub.client_id: required (gravel-hub apps create)")
	}
	if c.Hub.ClientSecret == "" {
		bad("hub: no client secret; set %s, hub.client_secret_file or hub.client_secret", EnvHubClientSecret)
	}
	if c.RoleSync.Interval < MinRoleSyncInterval {
		bad("role_sync.interval: %s is shorter than %s", c.RoleSync.Interval, MinRoleSyncInterval)
	}
	if c.RoleSync.PollInterval < MinRoleSyncPoll || c.RoleSync.PollInterval > c.RoleSync.Interval {
		bad("role_sync.poll_interval: %s must be at least %s and at most role_sync.interval", c.RoleSync.PollInterval, MinRoleSyncPoll)
	}
	if c.ServerCards.Interval < MinServerCardsInterval {
		bad("server_cards.interval: %s is shorter than %s", c.ServerCards.Interval, MinServerCardsInterval)
	}
	if !commandName.MatchString(c.Moderation.Command) {
		bad("moderation.command: %q must be 1 to 32 lower-case letters, digits, - or _", c.Moderation.Command)
	}
	if c.ModLog.Interval < MinModLogInterval {
		bad("mod_log.interval: %s is shorter than %s", c.ModLog.Interval, MinModLogInterval)
	}
	if c.Server.Listen == "" || c.Server.InternalListen == "" {
		bad("server.listen and server.internal_listen: required")
	}
	if c.Server.ShutdownTimeout <= 0 {
		bad("server.shutdown_timeout: must be positive")
	}
	if _, err := c.Log.SlogLevel(); err != nil {
		bad("log.level: %v", err)
	}
	if c.Log.Format != "json" && c.Log.Format != "text" {
		bad("log.format: %q is not \"json\" or \"text\"", c.Log.Format)
	}
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("config: %w", errors.Join(errs...))
}

func originOK(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" && u.Path == "" && u.RawQuery == "" && u.Fragment == ""
}

// SlogLevel parses the level.
func (l Log) SlogLevel() (slog.Level, error) {
	var lv slog.Level
	if err := lv.UnmarshalText([]byte(l.Level)); err != nil {
		return 0, fmt.Errorf("%q is not debug, info, warn or error", l.Level)
	}
	return lv, nil
}

// NewLogger builds the configured logger writing to w.
func (l Log) NewLogger(w io.Writer) *slog.Logger {
	lv, err := l.SlogLevel()
	if err != nil {
		lv = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: lv}
	if l.Format == "text" {
		return slog.New(slog.NewTextHandler(w, opts))
	}
	return slog.New(slog.NewJSONHandler(w, opts))
}
