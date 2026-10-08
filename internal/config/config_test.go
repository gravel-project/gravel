package config

import (
	"errors"
	"strings"
	"testing"
	"time"
)

const minimal = `
version: 1
organization:
  name: Test Org
database:
  url: postgres://gravel:pw@localhost:5432/gravel
`

func noEnv() Env {
	return Env{
		LookupEnv: func(string) (string, bool) { return "", false },
		ReadFile:  func(string) ([]byte, error) { return nil, errors.New("no files") },
	}
}

func TestParseDefaults(t *testing.T) {
	cfg, err := Parse(strings.NewReader(minimal), noEnv())
	if err != nil {
		t.Fatal(err)
	}
	want := Default()
	if cfg.Server != want.Server {
		t.Errorf("server defaults: got %+v want %+v", cfg.Server, want.Server)
	}
	if cfg.Database.Migrate != MigrateAuto || cfg.Database.MaxConns != 8 || cfg.Database.ConnectTimeout != time.Minute {
		t.Errorf("database defaults: got %+v", cfg.Database)
	}
	if cfg.Log != want.Log || cfg.Claim != want.Claim {
		t.Errorf("log/claim defaults: got %+v %+v", cfg.Log, cfg.Claim)
	}
	if cfg.Database.Source() != "database.url" {
		t.Errorf("source: got %q", cfg.Database.Source())
	}
}

func TestParseOverrides(t *testing.T) {
	doc := minimal + `
server:
  listen: 0.0.0.0:8443
  internal_listen: 127.0.0.1:9191
  shutdown_timeout: 3s
log:
  level: debug
  format: text
claim:
  token_ttl: 1h
`
	cfg, err := Parse(strings.NewReader(doc), noEnv())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Listen != "0.0.0.0:8443" || cfg.Server.InternalListen != "127.0.0.1:9191" || cfg.Server.ShutdownTimeout != 3*time.Second {
		t.Errorf("server: %+v", cfg.Server)
	}
	if cfg.Log.Level != "debug" || cfg.Log.Format != "text" || cfg.Claim.TokenTTL != time.Hour {
		t.Errorf("log/claim: %+v %+v", cfg.Log, cfg.Claim)
	}
}

func TestParseRejectsUnknownKeys(t *testing.T) {
	_, err := Parse(strings.NewReader(minimal+"\nservre:\n  listen: x\n"), noEnv())
	if err == nil || !strings.Contains(err.Error(), "servre") {
		t.Fatalf("want unknown-field error naming the key, got %v", err)
	}
}

func TestParseEmpty(t *testing.T) {
	_, err := Parse(strings.NewReader(""), noEnv())
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("want empty-file error, got %v", err)
	}
}

func TestSecretPrecedence(t *testing.T) {
	doc := minimal + "  url_file: /run/secrets/db\n"
	env := Env{
		LookupEnv: func(k string) (string, bool) {
			if k == EnvDatabaseURL {
				return "postgres://env@h/db", true
			}
			return "", false
		},
		ReadFile: func(string) ([]byte, error) { return []byte("postgres://file@h/db\n"), nil },
	}
	cfg, err := Parse(strings.NewReader(doc), env)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Database.URL != "postgres://env@h/db" || !strings.HasPrefix(cfg.Database.Source(), "env ") {
		t.Errorf("env should win: %q from %q", cfg.Database.URL, cfg.Database.Source())
	}

	env.LookupEnv = func(string) (string, bool) { return "", false }
	cfg, err = Parse(strings.NewReader(doc), env)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Database.URL != "postgres://file@h/db" || !strings.HasPrefix(cfg.Database.Source(), "file ") {
		t.Errorf("file should beat inline and be trimmed: %q from %q", cfg.Database.URL, cfg.Database.Source())
	}

	env.ReadFile = func(string) ([]byte, error) { return nil, errors.New("boom") }
	if _, err := Parse(strings.NewReader(doc), env); err == nil || !strings.Contains(err.Error(), "url_file") {
		t.Errorf("unreadable url_file must fail: %v", err)
	}
}

func TestValidateCollectsEverything(t *testing.T) {
	doc := `
version: 2
organization:
  name: "  "
server:
  listen: nope
  internal_listen: ""
  shutdown_timeout: 0s
database:
  url: mysql://x
  migrate: sometimes
  max_conns: 0
  connect_timeout: -1s
log:
  level: loud
  format: xml
claim:
  token_ttl: 0s
`
	_, err := Parse(strings.NewReader(doc), noEnv())
	if err == nil {
		t.Fatal("want error")
	}
	for _, want := range []string{
		"version:", "organization.name", "server.listen", "server.internal_listen", "server.shutdown_timeout",
		"database.url", "database.migrate", "database.max_conns", "database.connect_timeout",
		"log.level", "log.format", "claim.token_ttl",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q:\n%v", want, err)
		}
	}
}

func TestValidateMissingDatabase(t *testing.T) {
	_, err := Parse(strings.NewReader("version: 1\norganization:\n  name: x\n"), noEnv())
	if err == nil || !strings.Contains(err.Error(), EnvDatabaseURL) {
		t.Fatalf("want a hint naming %s, got %v", EnvDatabaseURL, err)
	}
}

func TestRedactedURL(t *testing.T) {
	d := Database{URL: "postgres://gravel:s3cret@db:5432/gravel?sslmode=disable"}
	got := d.RedactedURL()
	if strings.Contains(got, "s3cret") || !strings.Contains(got, "gravel:xxxxx@db:5432") {
		t.Errorf("redaction: %q", got)
	}
}

func TestSlogLevel(t *testing.T) {
	for _, l := range []string{"debug", "info", "warn", "warning", "error", "ERROR"} {
		if _, err := (Log{Level: l}).SlogLevel(); err != nil {
			t.Errorf("%s: %v", l, err)
		}
	}
	if _, err := (Log{Level: "loud"}).SlogLevel(); err == nil {
		t.Error("loud should fail")
	}
}
