// Package config loads and validates the hub's single configuration file.
//
// One YAML file, strict (unknown keys are errors) and versioned. Secrets never live in it: the
// database URL comes from the GRAVEL_DATABASE_URL environment variable, from database.url_file (a
// podman secret mount) or, for development only, from database.url, in that order of precedence.
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

// EnvDatabaseURL overrides database.url and database.url_file when set.
const EnvDatabaseURL = "GRAVEL_DATABASE_URL"

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
	if v, ok := env.LookupEnv(EnvDatabaseURL); ok && v != "" {
		c.Database.URL = v
		c.Database.source = "env " + EnvDatabaseURL
		return nil
	}
	if c.Database.URLFile != "" {
		b, err := env.ReadFile(c.Database.URLFile)
		if err != nil {
			return fmt.Errorf("config: database.url_file: %w", err)
		}
		c.Database.URL = strings.TrimSpace(string(b))
		c.Database.source = "file " + c.Database.URLFile
		return nil
	}
	if c.Database.URL != "" {
		c.Database.source = "database.url"
	}
	return nil
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
