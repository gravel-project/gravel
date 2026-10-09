package bot

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

const goodKey = "f61a8fd49fee0b5bf031dad66740c7225d06a4c4b60b412b875f2482a91ca808"

func env(vars map[string]string, files map[string]string) Env {
	return Env{
		LookupEnv: func(k string) (string, bool) { v, ok := vars[k]; return v, ok },
		ReadFile: func(p string) ([]byte, error) {
			if v, ok := files[p]; ok {
				return []byte(v), nil
			}
			return nil, errors.New("no such file")
		},
	}
}

const minimal = `
version: 1
hub:
  url: http://hub:8080
  client_id: gravel_abc
  client_secret: sec
discord:
  application_id: "1558123590786748516"
  public_key: "` + goodKey + `"
  token: tok
`

func TestParseDefaults(t *testing.T) {
	c, err := Parse(strings.NewReader(minimal), env(nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	if !c.Discord.Gateway || c.Server.Listen != "127.0.0.1:8081" || c.Server.InternalListen != "127.0.0.1:9091" || c.Server.ShutdownTimeout != 15*time.Second || c.Log.Level != "info" || c.Log.Format != "json" {
		t.Errorf("defaults: %+v", c)
	}
	if c.Hub.PublicURL != "http://hub:8080" {
		t.Errorf("public_url defaults to url: %q", c.Hub.PublicURL)
	}
	if !c.RoleSync.Enabled || c.RoleSync.Interval != 10*time.Minute || c.RoleSync.PollInterval != 30*time.Second || c.RoleSync.DryRun || !c.LinkedRoles.Enabled {
		t.Errorf("role sync and linked roles defaults: %+v %+v", c.RoleSync, c.LinkedRoles)
	}
	tuned, err := Parse(strings.NewReader(minimal+"role_sync:\n  interval: 5m\n  poll_interval: 1m\n  dry_run: true\nlinked_roles:\n  enabled: false\n"), env(nil, nil))
	if err != nil || !tuned.RoleSync.Enabled || tuned.RoleSync.Interval != 5*time.Minute || tuned.RoleSync.PollInterval != time.Minute || !tuned.RoleSync.DryRun || tuned.LinkedRoles.Enabled {
		t.Errorf("tuned: %v %+v %+v", err, tuned.RoleSync, tuned.LinkedRoles)
	}
	if c.Discord.TokenSource() != "discord.token" || c.Hub.SecretSource() != "hub.client_secret" {
		t.Errorf("sources: %q %q", c.Discord.TokenSource(), c.Hub.SecretSource())
	}
	if c.Discord.AppID().String() != "1558123590786748516" || len(c.Discord.Guilds()) != 0 {
		t.Errorf("application id: %v", c.Discord.ApplicationID)
	}
	withGuilds, err := Parse(strings.NewReader(minimal+"  guild_ids: [\"1558121195340042350\"]\n"), env(nil, nil))
	if err != nil || len(withGuilds.Discord.Guilds()) != 1 || withGuilds.Discord.Guilds()[0].String() != "1558121195340042350" {
		t.Errorf("guild ids: %v %v", err, withGuilds.Discord.GuildIDs)
	}
}

func TestSecretPrecedence(t *testing.T) {
	withFiles := strings.Replace(minimal, "  token: tok\n", "  token: inline\n  token_file: /run/secrets/t\n", 1)
	withFiles = strings.Replace(withFiles, "  client_secret: sec\n", "  client_secret: inline\n  client_secret_file: /run/secrets/s\n", 1)
	files := map[string]string{"/run/secrets/t": "from-file\n", "/run/secrets/s": " file-secret "}
	c, err := Parse(strings.NewReader(withFiles), env(nil, files))
	if err != nil {
		t.Fatal(err)
	}
	if c.Discord.Token != "from-file" || c.Hub.ClientSecret != "file-secret" || c.Discord.TokenSource() != "file /run/secrets/t" {
		t.Errorf("file beats inline, trimmed: %q %q %q", c.Discord.Token, c.Hub.ClientSecret, c.Discord.TokenSource())
	}
	c, err = Parse(strings.NewReader(withFiles), env(map[string]string{EnvDiscordToken: "from-env", EnvHubClientSecret: "env-secret"}, files))
	if err != nil {
		t.Fatal(err)
	}
	if c.Discord.Token != "from-env" || c.Hub.ClientSecret != "env-secret" || c.Hub.SecretSource() != "env "+EnvHubClientSecret {
		t.Errorf("env beats file: %q %q %q", c.Discord.Token, c.Hub.ClientSecret, c.Hub.SecretSource())
	}
	if _, err := Parse(strings.NewReader(withFiles), env(nil, nil)); err == nil || !strings.Contains(err.Error(), "discord.token_file") {
		t.Errorf("an unreadable file is an error: %v", err)
	}
}

func TestParseRejectsUnknownKeys(t *testing.T) {
	if _, err := Parse(strings.NewReader(minimal+"extra: 1\n"), env(nil, nil)); err == nil || !strings.Contains(err.Error(), "extra") {
		t.Errorf("unknown key: %v", err)
	}
}

func TestValidateCollectsEverything(t *testing.T) {
	bad := `
version: 2
discord:
  application_id: "12"
  public_key: "zz"
  guild_ids: ["abc"]
hub:
  url: "hub:8080/path"
  public_url: "ftp://x"
role_sync:
  interval: 30s
  poll_interval: 1s
server:
  listen: ""
  shutdown_timeout: 0s
log:
  level: loud
  format: xml
`
	_, err := Parse(strings.NewReader(bad), env(nil, nil))
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{"version:", "discord.application_id", "discord.guild_ids[0]", "discord.public_key", "discord: no token", "hub.url", "hub.public_url", "hub.client_id", "hub: no client secret", "role_sync.interval", "role_sync.poll_interval", "server.listen", "shutdown_timeout", "log.level", "log.format"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in %v", want, err)
		}
	}
}

func TestLogger(t *testing.T) {
	if l := (Log{Level: "debug", Format: "text"}).NewLogger(&strings.Builder{}); l == nil {
		t.Fatal("no logger")
	}
	if _, err := (Log{Level: "loud"}).SlogLevel(); err == nil {
		t.Error("a bad level is an error")
	}
}

func TestRoleSyncPollWithinInterval(t *testing.T) {
	_, err := Parse(strings.NewReader(minimal+"role_sync:\n  interval: 2m\n  poll_interval: 3m\n"), env(nil, nil))
	if err == nil || !strings.Contains(err.Error(), "role_sync.poll_interval") || strings.Contains(err.Error(), "role_sync.interval:") {
		t.Errorf("a poll longer than the pass: %v", err)
	}
}

// The compose stack's example carries placeholders that fail validation by design; every key in
// it must still be one this build reads.
func TestDeployExampleDecodes(t *testing.T) {
	f, err := os.Open("../../deploy/bot.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	c := Default()
	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		t.Fatalf("deploy/bot.yaml: %v", err)
	}
	if !c.RoleSync.Enabled || !c.RoleSync.DryRun || !c.LinkedRoles.Enabled {
		t.Errorf("the example runs role sync dry and registers the schema: %+v %+v", c.RoleSync, c.LinkedRoles)
	}
}
