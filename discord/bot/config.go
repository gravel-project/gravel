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
	EnvTwitchSecret    = "GRAVEL_BOT_TWITCH_CLIENT_SECRET"
	EnvKickSecret      = "GRAVEL_BOT_KICK_CLIENT_SECRET"
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
	Seeding     Seeding     `yaml:"seeding"`
	Presence    Presence    `yaml:"presence"`
	Recaps      Recaps      `yaml:"recaps"`
	Streams     Streams     `yaml:"streams"`
	News        News        `yaml:"news"`
	// MatchResults posts each finished match result (discord/modules/matchresults, ADR-0024).
	MatchResults MatchResults `yaml:"match_results"`
	// Events mirrors the hub's events log into a channel, with Restart and Park on a cloud-VM
	// server's trouble (discord/modules/events, gravel#200).
	Events Events `yaml:"events"`
	Server Server `yaml:"server"`
	Log    Log    `yaml:"log"`
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

// Seeding calls the seeder role to a server under its player floor and records seeding minutes
// (discord/modules/seeding, ADR-0016). The rules live in the Organization settings'
// discord.seeding; this is how the process runs them. Off by default: the bot's app needs
// servers:read (who is on) and participation:write (the minutes).
type Seeding struct {
	Enabled bool `yaml:"enabled"`
	// Interval is how often the rules are checked and, during a session, who is on is read.
	Interval time.Duration `yaml:"interval"`
}

// MinSeedingInterval is the seeding module's floor: each pass is two hub calls, and one more per
// open session.
const MinSeedingInterval = 10 * time.Second

// Presence keeps the "who's playing now" card (discord/modules/presence, ADR-0017). Where it goes
// and which games it counts are the Organization settings' discord.presence. Off by default: it
// needs the gateway and Discord's privileged Presence Intent, which the application's owner turns
// on in the Developer Portal (Bot → Privileged Gateway Intents). Without that toggle the bot still
// runs and the card says it can't see who is playing.
type Presence struct {
	Enabled bool `yaml:"enabled"`
	// Interval is how often the card is brought up to date: the longest a changed count waits.
	Interval time.Duration `yaml:"interval"`
}

// MinPresenceInterval is the presence card's floor: each pass is one hub call, and an edit only
// when a count changed.
const MinPresenceInterval = 15 * time.Second

// Recaps posts a session recap after each of a rule's weekly windows (discord/modules/recaps,
// ADR-0018). The rules are the Organization settings' discord.recaps. Off by default: the bot's
// app needs stats:read unless the settings make stats public.
type Recaps struct {
	Enabled bool `yaml:"enabled"`
	// Interval is how often the rules are checked; a recap posts within one interval of the
	// window's end plus the grace.
	Interval time.Duration `yaml:"interval"`
}

// MinRecapsInterval is the recaps module's floor: each pass is one hub call, and a channel read
// per rule whose window ended and isn't recapped yet.
const MinRecapsInterval = 15 * time.Second

// Streams posts when the Organization settings' discord.streams streamers go live
// (discord/modules/streams, ADR-0019). Off by default: each platform needs a developer
// application of the host's, whose credentials are here; a platform without them is skipped.
type Streams struct {
	Enabled bool `yaml:"enabled"`
	// Interval is how often the platforms are asked who is live: the longest a go-live post waits.
	Interval time.Duration `yaml:"interval"`
	// Twitch is the application from dev.twitch.tv; its secret from GRAVEL_BOT_TWITCH_CLIENT_SECRET,
	// then client_secret_file, then client_secret.
	Twitch StreamApp `yaml:"twitch"`
	// Kick is the application from kick.com's developer settings; its secret from
	// GRAVEL_BOT_KICK_CLIENT_SECRET, then client_secret_file, then client_secret.
	Kick StreamApp `yaml:"kick"`
}

// StreamApp is a streaming platform's developer application: the bot asks for an app access token
// with its client credentials, which reads public stream metadata and nothing of a viewer's.
type StreamApp struct {
	ClientID         string `yaml:"client_id"`
	ClientSecret     string `yaml:"client_secret"`
	ClientSecretFile string `yaml:"client_secret_file"`

	secretSource string
}

// Set reports whether the application is configured.
func (a StreamApp) Set() bool { return a.ClientID != "" && a.ClientSecret != "" }

// SecretSource says where the secret came from, never the secret.
func (a StreamApp) SecretSource() string { return a.secretSource }

// MinStreamsInterval is the go-live posts' floor: each pass is one call per platform, well inside
// both platforms' limits.
const MinStreamsInterval = 30 * time.Second

// News posts Steam games' announcements in Discord (discord/modules/news). The feeds are the
// Organization settings' discord.news; this is how the process runs them. Off by default. Steam's
// news API is public and needs no key, and the bot's app needs no scope beyond reading the settings.
type News struct {
	Enabled bool `yaml:"enabled"`
	// Interval is how often Steam is asked: the longest a new announcement waits.
	Interval time.Duration `yaml:"interval"`
}

// MinNewsInterval is the news module's floor: Steam's API is public and shared, and each pass is
// one call per feed.
const MinNewsInterval = 5 * time.Minute

// MatchResults posts each finished match result in Discord (discord/modules/matchresults,
// ADR-0024). The channel and the servers are the Organization settings' discord.match_results;
// this is how the process runs them. Off by default: the bot's app needs stats:read unless the
// settings make stats public.
type MatchResults struct {
	Enabled bool `yaml:"enabled"`
	// Interval is how often the hub is asked for finished results: the longest a result waits.
	Interval time.Duration `yaml:"interval"`
}

// MinMatchResultsInterval is the match results' floor: each pass is a hub call per listed server,
// and a channel read when a result is new.
const MinMatchResultsInterval = 30 * time.Second

// Events mirrors the hub's events log into the channel the Organization settings name
// (discord.events; discord/modules/events, gravel#200), with buttons that restart or park a
// cloud-VM server for an operator. Off by default: the bot's app needs events:read, and
// servers:read and servers:operate for the buttons.
type Events struct {
	Enabled bool `yaml:"enabled"`
	// Interval is how often the log is read: the longest an event waits for its post.
	Interval time.Duration `yaml:"interval"`
}

// MinEventsInterval is the events feed's floor: each pass is one or two hub calls.
const MinEventsInterval = 5 * time.Second

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
	// AvatarFile is a PNG the bot gives its user and the application at start, where they differ
	// from it (gravel#158): gravel's stock bot points it at the kit's bot avatar in the image, a
	// host at its own. Empty leaves both as the Developer Portal set them. Never fatal: a bad
	// file is logged and the bot runs on.
	AvatarFile string `yaml:"avatar_file"`

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
		Version:      CurrentVersion,
		Discord:      Discord{Gateway: true},
		RoleSync:     RoleSync{Enabled: true, Interval: 10 * time.Minute, PollInterval: 30 * time.Second},
		LinkedRoles:  LinkedRoles{Enabled: true},
		ServerCards:  ServerCards{Enabled: true, Interval: 15 * time.Second},
		Moderation:   Moderation{Command: "mod"},
		ModLog:       ModLog{Interval: 15 * time.Second},
		Seeding:      Seeding{Interval: 30 * time.Second},
		Presence:     Presence{Interval: time.Minute},
		Recaps:       Recaps{Interval: time.Minute},
		Streams:      Streams{Interval: time.Minute},
		News:         News{Interval: 15 * time.Minute},
		MatchResults: MatchResults{Interval: time.Minute},
		Events:       Events{Interval: 15 * time.Second},
		Server:       Server{Listen: "127.0.0.1:8081", InternalListen: "127.0.0.1:9091", ShutdownTimeout: 15 * time.Second},
		Log:          Log{Level: "info", Format: "json"},
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
	if c.Streams.Twitch.ClientSecret, c.Streams.Twitch.secretSource, err = resolveSecret(env, EnvTwitchSecret, c.Streams.Twitch.ClientSecretFile, c.Streams.Twitch.ClientSecret, "streams.twitch.client_secret"); err != nil {
		return Config{}, err
	}
	if c.Streams.Kick.ClientSecret, c.Streams.Kick.secretSource, err = resolveSecret(env, EnvKickSecret, c.Streams.Kick.ClientSecretFile, c.Streams.Kick.ClientSecret, "streams.kick.client_secret"); err != nil {
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
	if c.Seeding.Interval < MinSeedingInterval {
		bad("seeding.interval: %s is shorter than %s", c.Seeding.Interval, MinSeedingInterval)
	}
	if c.Presence.Interval < MinPresenceInterval {
		bad("presence.interval: %s is shorter than %s", c.Presence.Interval, MinPresenceInterval)
	}
	if c.Recaps.Interval < MinRecapsInterval {
		bad("recaps.interval: %s is shorter than %s", c.Recaps.Interval, MinRecapsInterval)
	}
	if c.Streams.Interval < MinStreamsInterval {
		bad("streams.interval: %s is shorter than %s", c.Streams.Interval, MinStreamsInterval)
	}
	if c.News.Interval < MinNewsInterval {
		bad("news.interval: %s is shorter than %s", c.News.Interval, MinNewsInterval)
	}
	if c.MatchResults.Interval < MinMatchResultsInterval {
		bad("match_results.interval: %s is shorter than %s", c.MatchResults.Interval, MinMatchResultsInterval)
	}
	if c.Events.Interval < MinEventsInterval {
		bad("events.interval: %s is shorter than %s", c.Events.Interval, MinEventsInterval)
	}
	for _, p := range []struct {
		name string
		app  StreamApp
	}{{"twitch", c.Streams.Twitch}, {"kick", c.Streams.Kick}} {
		if (p.app.ClientID == "") != (p.app.ClientSecret == "") {
			bad("streams.%s: client_id and the client secret go together", p.name)
		}
	}
	if c.Streams.Enabled && !c.Streams.Twitch.Set() && !c.Streams.Kick.Set() {
		bad("streams.enabled: no platform's application is set; set streams.twitch or streams.kick")
	}
	if c.Presence.Enabled && !c.Discord.Gateway {
		bad("presence.enabled: presence comes only over the gateway; set discord.gateway: true")
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
