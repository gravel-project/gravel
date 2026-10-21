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

// The lease key (#20) reads like every secret, and a short one is refused.
func TestLeaseKey(t *testing.T) {
	key := strings.Repeat("k", MinLeaseKey)
	env := Env{
		LookupEnv: func(k string) (string, bool) { return "", false },
		ReadFile:  func(p string) ([]byte, error) { return []byte(key + "\n"), nil },
	}
	cfg, err := Parse(strings.NewReader(minimal+"leases:\n  key_file: /run/secrets/lease\n"), env)
	if err != nil || cfg.Leases.Key != key || cfg.Leases.KeySource() != "file /run/secrets/lease" {
		t.Fatalf("from a file: %q %q %v", cfg.Leases.Key, cfg.Leases.KeySource(), err)
	}
	env.LookupEnv = func(k string) (string, bool) { return key + "e", k == EnvLeaseKey }
	if cfg, err := Parse(strings.NewReader(minimal), env); err != nil || cfg.Leases.Key != key+"e" || cfg.Leases.KeySource() != "env "+EnvLeaseKey {
		t.Errorf("from the environment: %q %v", cfg.Leases.KeySource(), err)
	}
	if cfg, err := Parse(strings.NewReader(minimal), noEnv()); err != nil || cfg.Leases.Key != "" || cfg.Leases.KeySource() != "" {
		t.Errorf("none: %v", err)
	}
	if _, err := Parse(strings.NewReader(minimal+"leases:\n  key: short\n"), noEnv()); err == nil || !strings.Contains(err.Error(), "leases.key: 5 characters") {
		t.Errorf("a short key: %v", err)
	}
}

// The demo bucket (ADR-0024): its keys read like every secret, and a bucket needs the rest.
func TestDemos(t *testing.T) {
	env := Env{
		LookupEnv: func(k string) (string, bool) { return "env-secret", k == EnvDemosSecretKey },
		ReadFile:  func(p string) ([]byte, error) { return []byte("file-access\n"), nil },
	}
	doc := minimal + "demos:\n  endpoint: acct.r2.cloudflarestorage.com\n  region: auto\n  bucket: demos\n  access_key_file: /run/secrets/a\n"
	cfg, err := Parse(strings.NewReader(doc), env)
	if err != nil || !cfg.Demos.Enabled() || cfg.Demos.AccessKey != "file-access" || cfg.Demos.SecretKey != "env-secret" || cfg.Demos.MaxMiB != 512 {
		t.Fatalf("demos %+v %v", cfg.Demos, err)
	}
	if cfg, err := Parse(strings.NewReader(minimal), noEnv()); err != nil || cfg.Demos.Enabled() {
		t.Errorf("none: %v", err)
	}
	for name, d := range map[string]string{
		"no endpoint": "demos:\n  region: auto\n  bucket: demos\n  access_key: a\n  secret_key: s\n",
		"no keys":     "demos:\n  endpoint: e\n  region: auto\n  bucket: demos\n",
		"too big":     "demos:\n  endpoint: e\n  region: auto\n  bucket: demos\n  access_key: a\n  secret_key: s\n  max_mib: 9000\n",
	} {
		if _, err := Parse(strings.NewReader(minimal+d), noEnv()); err == nil || !strings.Contains(err.Error(), "demos") {
			t.Errorf("%s: %v", name, err)
		}
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

func TestAuthDefaultsAndOverrides(t *testing.T) {
	cfg, err := Parse(strings.NewReader(minimal), noEnv())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Auth.SessionTTL != 30*24*time.Hour || cfg.Auth.AttemptTTL != 10*time.Minute || cfg.Auth.BaseURL != "" {
		t.Errorf("auth defaults: %+v", cfg.Auth)
	}
	if cfg.Auth.Discord.Enabled || !cfg.Auth.Discord.Login || cfg.Auth.Steam.Enabled || cfg.Auth.Steam.Login {
		t.Errorf("provider defaults: %+v %+v", cfg.Auth.Discord, cfg.Auth.Steam)
	}
	if cfg.RateLimit.PerIP != (Limit{RequestsPerMinute: 120, Burst: 40}) || cfg.RateLimit.PerUser != (Limit{RequestsPerMinute: 600, Burst: 100}) {
		t.Errorf("rate limit defaults: %+v", cfg.RateLimit)
	}
	if cfg.Auth.Secure() || cfg.Server.ClientIPHeader != "" {
		t.Errorf("insecure by default: %+v", cfg.Auth)
	}

	doc := minimal + `
server:
  client_ip_header: CF-Connecting-IP
auth:
  base_url: https://app.example.com
  session_ttl: 1h
  attempt_ttl: 2m
  discord:
    enabled: true
    client_id: "123"
    client_secret: dev-secret
  steam:
    enabled: true
    api_key: dev-key
    login: true
rate_limit:
  per_ip: {requests_per_minute: 10, burst: 5}
  per_user: {requests_per_minute: 20, burst: 6}
`
	cfg, err = Parse(strings.NewReader(doc), noEnv())
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Auth.Secure() || cfg.Auth.CallbackURL("discord") != "https://app.example.com/auth/discord/callback" {
		t.Errorf("secure/callback: %v %q", cfg.Auth.Secure(), cfg.Auth.CallbackURL("discord"))
	}
	if got := cfg.Auth.RolesURL("discord"); got != "https://app.example.com/auth/discord/roles" {
		t.Errorf("roles url: %q", got)
	}
	if cfg.Auth.SessionTTL != time.Hour || cfg.Auth.AttemptTTL != 2*time.Minute || cfg.Server.ClientIPHeader != "CF-Connecting-IP" {
		t.Errorf("overrides: %+v %+v", cfg.Auth, cfg.Server)
	}
	if !cfg.Auth.Discord.Enabled || cfg.Auth.Discord.ClientSecret != "dev-secret" || cfg.Auth.Discord.SecretSource() != "auth.discord.client_secret" {
		t.Errorf("discord: %+v (%s)", cfg.Auth.Discord, cfg.Auth.Discord.SecretSource())
	}
	if !cfg.Auth.Steam.Enabled || !cfg.Auth.Steam.Login || cfg.Auth.Steam.APIKey != "dev-key" || cfg.Auth.Steam.KeySource() != "auth.steam.api_key" {
		t.Errorf("steam: %+v (%s)", cfg.Auth.Steam, cfg.Auth.Steam.KeySource())
	}
	if cfg.RateLimit.PerIP != (Limit{10, 5}) || cfg.RateLimit.PerUser != (Limit{20, 6}) {
		t.Errorf("rate limits: %+v", cfg.RateLimit)
	}
}

func TestAuthSecretPrecedence(t *testing.T) {
	doc := minimal + `
auth:
  base_url: http://127.0.0.1:8080
  discord:
    enabled: true
    client_id: "1"
    client_secret: inline
    client_secret_file: /run/secrets/discord
  steam:
    enabled: true
    api_key_file: /run/secrets/steam
`
	env := Env{
		LookupEnv: func(k string) (string, bool) {
			if k == EnvDiscordClientSecret {
				return "from-env", true
			}
			return "", false
		},
		ReadFile: func(p string) ([]byte, error) { return []byte("from-file:" + p + "\n"), nil },
	}
	cfg, err := Parse(strings.NewReader(doc), env)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Auth.Discord.ClientSecret != "from-env" || !strings.HasPrefix(cfg.Auth.Discord.SecretSource(), "env ") {
		t.Errorf("env wins: %q %q", cfg.Auth.Discord.ClientSecret, cfg.Auth.Discord.SecretSource())
	}
	if cfg.Auth.Steam.APIKey != "from-file:/run/secrets/steam" || !strings.HasPrefix(cfg.Auth.Steam.KeySource(), "file ") {
		t.Errorf("file beats inline and is trimmed: %q %q", cfg.Auth.Steam.APIKey, cfg.Auth.Steam.KeySource())
	}
	env.ReadFile = func(string) ([]byte, error) { return nil, errors.New("boom") }
	env.LookupEnv = func(string) (string, bool) { return "", false }
	if _, err := Parse(strings.NewReader(doc), env); err == nil || !strings.Contains(err.Error(), "client_secret_file") {
		t.Errorf("unreadable secret file must fail naming the key: %v", err)
	}
}

func TestAuthValidation(t *testing.T) {
	cases := map[string]string{
		"discord without id":      "auth:\n  base_url: https://a.example\n  discord:\n    enabled: true\n    client_secret: x\n",
		"discord without secret":  "auth:\n  base_url: https://a.example\n  discord:\n    enabled: true\n    client_id: '1'\n",
		"provider without base":   "auth:\n  steam:\n    enabled: true\n",
		"base with a path":        "auth:\n  base_url: https://a.example/app\n",
		"base without scheme":     "auth:\n  base_url: a.example\n",
		"everything link-only":    "auth:\n  base_url: https://a.example\n  discord:\n    enabled: true\n    client_id: '1'\n    client_secret: x\n    login: false\n",
		"zero rate":               "rate_limit:\n  per_ip: {requests_per_minute: 0, burst: 1}\n",
		"zero burst":              "rate_limit:\n  per_user: {requests_per_minute: 1, burst: 0}\n",
		"session ttl":             "auth:\n  session_ttl: 0s\n",
		"attempt ttl":             "auth:\n  attempt_ttl: -1s\n",
		"client ip header spaced": "server:\n  client_ip_header: 'X Forwarded'\n",
		"xbox without discord":    "auth:\n  base_url: https://a.example\n  steam:\n    enabled: true\n    login: true\n  xbox:\n    enabled: true\n",
		"xbox login":              "auth:\n  xbox:\n    enabled: true\n    login: true\n",
	}
	for name, extra := range cases {
		if _, err := Parse(strings.NewReader(minimal+extra), noEnv()); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	// Steam alone with login is a valid hub; so is Steam link-only beside Discord.
	ok := minimal + "auth:\n  base_url: https://a.example\n  steam:\n    enabled: true\n    login: true\n"
	if _, err := Parse(strings.NewReader(ok), noEnv()); err != nil {
		t.Errorf("steam-only login: %v", err)
	}
	// Xbox beside Discord, on its application, with its own callback (gravel#217).
	xbox := minimal + "auth:\n  base_url: https://a.example\n  discord:\n    enabled: true\n    client_id: '1'\n    client_secret: x\n  xbox:\n    enabled: true\n"
	cfg, err := Parse(strings.NewReader(xbox), noEnv())
	if err != nil || !cfg.Auth.Xbox.Enabled || cfg.Auth.CallbackURL("xbox") != "https://a.example/auth/xbox/callback" {
		t.Errorf("xbox: %v", err)
	}
	if _, err := Parse(strings.NewReader(minimal+"auth:\n  base_url: https://a.example\n  steam:\n    enabled: true\n    login: true\n  xbox:\n    enabled: true\n"), noEnv()); err == nil ||
		!strings.Contains(err.Error(), "auth.xbox: needs auth.discord enabled") {
		t.Errorf("xbox without discord: %v", err)
	}
}

func TestDisabledProviderSecretFilesAreNotRead(t *testing.T) {
	doc := minimal + `
auth:
  base_url: https://a.example
  discord:
    enabled: false
    client_secret_file: /run/secrets/missing
  steam:
    enabled: false
    api_key_file: /run/secrets/missing
`
	env := noEnv() // every ReadFile fails
	cfg, err := Parse(strings.NewReader(doc), env)
	if err != nil {
		t.Fatalf("a disabled provider's file must not be read: %v", err)
	}
	if cfg.Auth.Discord.ClientSecret != "" || cfg.Auth.Steam.APIKey != "" {
		t.Errorf("no secret resolved for disabled providers: %+v %+v", cfg.Auth.Discord, cfg.Auth.Steam)
	}
}

func TestAppsValidation(t *testing.T) {
	cfg := Default()
	if cfg.Apps.TokenTTL != time.Hour || cfg.RateLimit.PerApp.RequestsPerMinute != 600 {
		t.Errorf("defaults: %+v %+v", cfg.Apps, cfg.RateLimit.PerApp)
	}
	cfg.Organization.Name = "x"
	cfg.Database.URL = "postgres://u:p@h/db"
	cfg.Apps.TokenTTL = 0
	cfg.RateLimit.PerApp.RequestsPerMinute = 0
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "apps.token_ttl") || !strings.Contains(err.Error(), "rate_limit.per_app") {
		t.Errorf("validation: %v", err)
	}
}

func TestDemo(t *testing.T) {
	cfg, err := Parse(strings.NewReader(minimal), noEnv())
	if err != nil || cfg.Demo {
		t.Fatalf("a hub is not a demo by default: %v %v", cfg.Demo, err)
	}
	if cfg, err = Parse(strings.NewReader(minimal+"demo: true\n"), noEnv()); err != nil || !cfg.Demo {
		t.Errorf("demo: true: %v %v", cfg.Demo, err)
	}
	if _, err := Parse(strings.NewReader(minimal+"demo: yes please\n"), noEnv()); err == nil {
		t.Error("demo takes a boolean")
	}
}

func TestCloudEgress(t *testing.T) {
	cfg, err := Parse(strings.NewReader(minimal+"\ncloud:\n  egress_cidrs: [\"144.126.220.232\"]\n"), noEnv())
	if err != nil || len(cfg.Cloud.EgressCIDRs) != 1 {
		t.Fatalf("%+v %v", cfg.Cloud, err)
	}
	for _, bad := range []string{`["0.0.0.0/0"]`, `["not-an-address"]`} {
		if _, err := Parse(strings.NewReader(minimal+"\ncloud:\n  egress_cidrs: "+bad+"\n"), noEnv()); err == nil || !strings.Contains(err.Error(), "cloud.egress_cidrs") {
			t.Errorf("%s: %v", bad, err)
		}
	}
}

// The metrics store /admin/metrics reads (gravel#204): optional; an address, a token from a file,
// the scrape jobs' names and a bounded timeout.
func TestMetrics(t *testing.T) {
	cfg, err := Parse(strings.NewReader(minimal), noEnv())
	if err != nil || cfg.Metrics.Configured() || cfg.Metrics.Job("hub") != "gravel-hub" || cfg.Metrics.Job("bot") != "gravel-bot" || cfg.Metrics.QueryTimeout() != 10*time.Second {
		t.Fatalf("none: %+v %v", cfg.Metrics, err)
	}
	env := Env{
		LookupEnv: func(string) (string, bool) { return "", false },
		ReadFile:  func(p string) ([]byte, error) { return []byte("tok\n"), nil },
	}
	cfg, err = Parse(strings.NewReader(minimal+"metrics:\n  query_url: https://vm.example.org/select/0/prometheus\n  bearer_token_file: /run/secrets/vm\n  hub_job: htg-hub\n  timeout: 5s\n"), env)
	if err != nil || !cfg.Metrics.Configured() || cfg.Metrics.BearerToken != "tok" || cfg.Metrics.Job("hub") != "htg-hub" || cfg.Metrics.Job("bot") != "gravel-bot" || cfg.Metrics.QueryTimeout() != 5*time.Second {
		t.Errorf("configured: %+v %v", cfg.Metrics, err)
	}
	for name, doc := range map[string]string{
		"a user in the address": "metrics:\n  query_url: https://me:pw@vm.example.org\n",
		"a query":               "metrics:\n  query_url: http://vm:8428?x=1\n",
		"not http":              "metrics:\n  query_url: vm:8428\n",
		"a long timeout":        "metrics:\n  query_url: http://vm:8428\n  timeout: 2m\n",
	} {
		if _, err := Parse(strings.NewReader(minimal+doc), noEnv()); err == nil || !strings.Contains(err.Error(), "metrics.") {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := Parse(strings.NewReader(minimal+"metrics:\n  bearer_token_file: /run/secrets/vm\n"), noEnv()); err != nil {
		t.Errorf("a token file without a store is not read: %v", err)
	}
}
