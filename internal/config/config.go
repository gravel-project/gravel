// Package config loads and validates the hub's single configuration file.
//
// One YAML file, strict (unknown keys are errors) and versioned. Secrets never live in it: each
// one (the database URL, the Discord client secret, the Steam Web API key) comes from an
// environment variable, from a *_file key (a podman secret mount) or, for development only,
// from the inline key, in that order of precedence.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// CurrentVersion is the config file format this build understands.
const CurrentVersion = 1

// Environment variables that override a secret's *_file and inline keys when set.
const (
	EnvDatabaseURL         = "GRAVEL_DATABASE_URL"
	EnvDiscordClientSecret = "GRAVEL_DISCORD_CLIENT_SECRET"
	EnvSteamAPIKey         = "GRAVEL_STEAM_API_KEY"
)

// Migration policies.
const (
	MigrateAuto   = "auto"   // apply pending migrations at start
	MigrateManual = "manual" // refuse to serve until `gravel-hub migrate` has run
)

// Config is the whole hub configuration.
type Config struct {
	Version      int          `yaml:"version"`
	Organization Organization `yaml:"organization"`
	Server       Server       `yaml:"server"`
	Database     Database     `yaml:"database"`
	Log          Log          `yaml:"log"`
	Claim        Claim        `yaml:"claim"`
	Auth         Auth         `yaml:"auth"`
	RateLimit    RateLimit    `yaml:"rate_limit"`
	Backup       Backup       `yaml:"backup"`
	Apps         Apps         `yaml:"apps"`
}

// Apps tunes first-party app credentials (ADR-0008).
type Apps struct {
	// TokenTTL is how long a bearer token issued at /oauth/token lives.
	TokenTTL time.Duration `yaml:"token_ttl"`
}

// Organization names the built-in organization.
type Organization struct {
	Name string `yaml:"name"`
}

// Server holds the two listeners and the drain timeout.
type Server struct {
	// Listen serves the API, /healthz and /readyz. Put it behind TLS termination (cloudflared).
	Listen string `yaml:"listen"`
	// InternalListen serves /metrics and pprof. Keep it on loopback or the pod network.
	InternalListen  string        `yaml:"internal_listen"`
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout"`
	// ClientIPHeader names the header a trusted reverse proxy puts the client's address in
	// (CF-Connecting-IP behind cloudflared). Empty means the connection's own address. Set it
	// only when nothing but that proxy can reach the listener: the header is trusted as given.
	ClientIPHeader string `yaml:"client_ip_header"`
}

// Database is the Postgres connection and migration policy.
type Database struct {
	// URL is the connection URL. Development only: production sets GRAVEL_DATABASE_URL or URLFile.
	URL string `yaml:"url"`
	// URLFile is a file holding the URL, typically a podman secret under /run/secrets.
	URLFile        string        `yaml:"url_file"`
	Migrate        string        `yaml:"migrate"`
	MaxConns       int32         `yaml:"max_conns"`
	ConnectTimeout time.Duration `yaml:"connect_timeout"`

	source string
}

// Log sets the slog handler.
type Log struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

// Claim tunes the owner-claim token.
type Claim struct {
	TokenTTL time.Duration `yaml:"token_ttl"`
}

// Auth is login: the public origin, session lifetimes and the providers.
type Auth struct {
	// BaseURL is the origin members reach the hub at (https://app.example.com). Provider
	// callback URLs are built on it and cookies are Secure when it is https. Required once a
	// provider is enabled.
	BaseURL string `yaml:"base_url"`
	// SessionTTL is how long a login lasts; there is no idle timeout.
	SessionTTL time.Duration `yaml:"session_ttl"`
	// AttemptTTL is how long a member has to come back from a provider.
	AttemptTTL time.Duration `yaml:"attempt_ttl"`
	Discord    Discord       `yaml:"discord"`
	Steam      Steam         `yaml:"steam"`
}

// Discord is the Discord OAuth2 application: scope identify for login and linking, plus
// role_connections.write in the Linked Roles flow (/auth/discord/roles, which needs Login).
type Discord struct {
	Enabled  bool   `yaml:"enabled"`
	ClientID string `yaml:"client_id"`
	// ClientSecret is development only: production sets GRAVEL_DISCORD_CLIENT_SECRET or
	// ClientSecretFile (a podman secret).
	ClientSecret     string `yaml:"client_secret"`
	ClientSecretFile string `yaml:"client_secret_file"`
	// Login says whether members sign in with Discord (the primary login) or only link it.
	Login bool `yaml:"login"`

	secretSource string
}

// Steam is Steam's OpenID login. It needs no credential to prove a SteamID64; the Web API key
// is optional and only fetches the persona name and avatar.
type Steam struct {
	Enabled bool `yaml:"enabled"`
	// APIKey is development only: production sets GRAVEL_STEAM_API_KEY or APIKeyFile.
	APIKey     string `yaml:"api_key"`
	APIKeyFile string `yaml:"api_key_file"`
	// Login says whether members may sign in with Steam alone; by default it is link-only.
	Login bool `yaml:"login"`

	keySource string
}

// Backup is what the hub watches of its own backups (ADR-0006): the status file the backup job
// writes after every base backup. The WAL archiver's statistics come from Postgres itself.
type Backup struct {
	// StatusFile is where gravel-backup writes its status (the gravel-backup volume, mounted
	// read-only in the hub). Empty means no backup job is configured; only the archiver metrics
	// are exposed.
	StatusFile string `yaml:"status_file"`
}

// RateLimit bounds request rates: per client IP for anonymous requests, per user for
// authenticated ones. Health probes are never limited.
type RateLimit struct {
	PerIP   Limit `yaml:"per_ip"`
	PerUser Limit `yaml:"per_user"`
	// PerApp applies to requests carrying an app's bearer token, per app.
	PerApp Limit `yaml:"per_app"`
}

// Limit is a sustained rate with a burst allowance.
type Limit struct {
	RequestsPerMinute int `yaml:"requests_per_minute"`
	Burst             int `yaml:"burst"`
}

// Default returns the configuration with every optional field at its default.
func Default() Config {
	return Config{
		Version: CurrentVersion,
		Server: Server{
			Listen:          "127.0.0.1:8080",
			InternalListen:  "127.0.0.1:9090",
			ShutdownTimeout: 15 * time.Second,
		},
		Database: Database{
			Migrate:        MigrateAuto,
			MaxConns:       8,
			ConnectTimeout: 60 * time.Second,
		},
		Log:   Log{Level: "info", Format: "json"},
		Claim: Claim{TokenTTL: 15 * time.Minute},
		Auth: Auth{
			SessionTTL: 30 * 24 * time.Hour,
			AttemptTTL: 10 * time.Minute,
			Discord:    Discord{Login: true},
			Steam:      Steam{Login: false},
		},
		RateLimit: RateLimit{
			PerIP:   Limit{RequestsPerMinute: 120, Burst: 40},
			PerUser: Limit{RequestsPerMinute: 600, Burst: 100},
			PerApp:  Limit{RequestsPerMinute: 600, Burst: 100},
		},
		Apps: Apps{TokenTTL: time.Hour},
	}
}

// Env is where Parse resolves secrets from. Tests substitute it.
type Env struct {
	LookupEnv func(string) (string, bool)
	ReadFile  func(string) ([]byte, error)
}

// OSEnv resolves secrets from the process environment and the filesystem.
func OSEnv() Env {
	return Env{LookupEnv: os.LookupEnv, ReadFile: os.ReadFile}
}

// Load reads and validates the file at path.
func Load(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("config: %w", err)
	}
	return Parse(bytes.NewReader(b), OSEnv())
}

// Parse decodes a config document strictly, resolves secrets and validates the result.
func Parse(r io.Reader, env Env) (Config, error) {
	cfg := Default()
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		if errors.Is(err, io.EOF) {
			return Config{}, errors.New("config: the file is empty")
		}
		return Config{}, fmt.Errorf("config: %w", err)
	}
	if err := cfg.resolveSecrets(env); err != nil {
		return Config{}, err
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c *Config) resolveSecrets(env Env) error {
	var err error
	if c.Database.URL, c.Database.source, err = resolveSecret(env, EnvDatabaseURL, c.Database.URLFile, c.Database.URL, "database.url"); err != nil {
		return err
	}
	// A disabled provider's secret is not read: the file need not exist (the development
	// stack keeps the keys in hub.yaml with the providers off).
	if c.Auth.Discord.Enabled {
		if c.Auth.Discord.ClientSecret, c.Auth.Discord.secretSource, err = resolveSecret(env, EnvDiscordClientSecret, c.Auth.Discord.ClientSecretFile, c.Auth.Discord.ClientSecret, "auth.discord.client_secret"); err != nil {
			return err
		}
	}
	if c.Auth.Steam.Enabled {
		if c.Auth.Steam.APIKey, c.Auth.Steam.keySource, err = resolveSecret(env, EnvSteamAPIKey, c.Auth.Steam.APIKeyFile, c.Auth.Steam.APIKey, "auth.steam.api_key"); err != nil {
			return err
		}
	}
	return nil
}

// resolveSecret applies the precedence every secret shares: the environment variable, then
// the file, then the inline value. It returns the value and where it came from.
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

// Validate reports every problem at once.
func (c Config) Validate() error {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if c.Version != CurrentVersion {
		bad("version: got %d, this hub reads version %d", c.Version, CurrentVersion)
	}
	if strings.TrimSpace(c.Organization.Name) == "" {
		bad("organization.name: required")
	}
	if err := checkAddr(c.Server.Listen); err != nil {
		bad("server.listen: %v", err)
	}
	if err := checkAddr(c.Server.InternalListen); err != nil {
		bad("server.internal_listen: %v", err)
	}
	if c.Server.ShutdownTimeout <= 0 {
		bad("server.shutdown_timeout: must be positive")
	}
	if c.Database.URL == "" {
		bad("database: no URL; set %s, database.url_file or database.url", EnvDatabaseURL)
	} else if u, err := url.Parse(c.Database.URL); err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		bad("database.url (%s): must be a postgres:// URL", c.Database.source)
	}
	if c.Database.Migrate != MigrateAuto && c.Database.Migrate != MigrateManual {
		bad("database.migrate: %q is not %q or %q", c.Database.Migrate, MigrateAuto, MigrateManual)
	}
	if c.Database.MaxConns < 1 {
		bad("database.max_conns: must be at least 1")
	}
	if c.Database.ConnectTimeout <= 0 {
		bad("database.connect_timeout: must be positive")
	}
	if _, err := c.Log.SlogLevel(); err != nil {
		bad("log.level: %v", err)
	}
	if c.Log.Format != "json" && c.Log.Format != "text" {
		bad("log.format: %q is not \"json\" or \"text\"", c.Log.Format)
	}
	if c.Claim.TokenTTL <= 0 {
		bad("claim.token_ttl: must be positive")
	}
	if c.Server.ClientIPHeader != "" && strings.ContainsAny(c.Server.ClientIPHeader, " :\t") {
		bad("server.client_ip_header: %q is not a header name", c.Server.ClientIPHeader)
	}
	if c.Auth.SessionTTL <= 0 {
		bad("auth.session_ttl: must be positive")
	}
	if c.Auth.AttemptTTL <= 0 {
		bad("auth.attempt_ttl: must be positive")
	}
	if c.Auth.BaseURL != "" {
		if u, err := url.Parse(c.Auth.BaseURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
			bad("auth.base_url: %q must be an http(s) origin with no path, like https://app.example.com", c.Auth.BaseURL)
		}
	} else if c.Auth.Discord.Enabled || c.Auth.Steam.Enabled {
		bad("auth.base_url: required when a provider is enabled (the callback URLs are built on it)")
	}
	if c.Auth.Discord.Enabled {
		if strings.TrimSpace(c.Auth.Discord.ClientID) == "" {
			bad("auth.discord.client_id: required when discord is enabled")
		}
		if c.Auth.Discord.ClientSecret == "" {
			bad("auth.discord: no client secret; set %s, auth.discord.client_secret_file or auth.discord.client_secret", EnvDiscordClientSecret)
		}
	}
	if (c.Auth.Discord.Enabled && c.Auth.Discord.Login) || (c.Auth.Steam.Enabled && c.Auth.Steam.Login) {
		// fine: somebody can log in
	} else if c.Auth.Discord.Enabled || c.Auth.Steam.Enabled {
		bad("auth: every enabled provider is link-only, so nobody can log in; set login: true on one")
	}
	if c.Apps.TokenTTL <= 0 {
		bad("apps.token_ttl: must be positive")
	}
	for name, l := range map[string]Limit{"rate_limit.per_ip": c.RateLimit.PerIP, "rate_limit.per_user": c.RateLimit.PerUser, "rate_limit.per_app": c.RateLimit.PerApp} {
		if l.RequestsPerMinute < 1 {
			bad("%s.requests_per_minute: must be at least 1", name)
		}
		if l.Burst < 1 {
			bad("%s.burst: must be at least 1", name)
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("config: invalid:\n%w", errors.Join(errs...))
}

func checkAddr(addr string) error {
	if addr == "" {
		return errors.New("required")
	}
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return fmt.Errorf("want host:port: %w", err)
	}
	return nil
}

// Source says where the database URL came from, for the startup log. Never the URL itself.
func (d Database) Source() string { return d.source }

// SecretSource says where the client secret came from, for the startup log.
func (d Discord) SecretSource() string { return d.secretSource }

// KeySource says where the API key came from ("" when there is none), for the startup log.
func (s Steam) KeySource() string { return s.keySource }

// Secure reports whether the hub is reached over HTTPS, which decides whether cookies carry the
// Secure attribute and the __Host- prefix.
func (a Auth) Secure() bool { return strings.HasPrefix(strings.ToLower(a.BaseURL), "https://") }

// CallbackURL is the provider callback members return to after authenticating.
func (a Auth) CallbackURL(provider string) string {
	return strings.TrimSuffix(a.BaseURL, "/") + "/auth/" + provider + "/callback"
}

// RolesURL is the Linked Roles verification URL to set on a provider's application (Discord's
// "Linked Roles Verification URL").
func (a Auth) RolesURL(provider string) string {
	return strings.TrimSuffix(a.BaseURL, "/") + "/auth/" + provider + "/roles"
}

// RedactedURL is the database URL with any password replaced, safe for logs.
func (d Database) RedactedURL() string {
	u, err := url.Parse(d.URL)
	if err != nil {
		return "(unparseable)"
	}
	return u.Redacted()
}

// SlogLevel maps the configured level name to slog.
func (l Log) SlogLevel() (slog.Level, error) {
	switch strings.ToLower(l.Level) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return 0, fmt.Errorf("%q is not debug, info, warn or error", l.Level)
}

// NewLogger builds the process logger from the Log section.
func (l Log) NewLogger(w io.Writer) *slog.Logger {
	level, err := l.SlogLevel()
	if err != nil {
		level = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: level}
	if l.Format == "text" {
		return slog.New(slog.NewTextHandler(w, opts))
	}
	return slog.New(slog.NewJSONHandler(w, opts))
}
