package servers

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gravel-project/gravel/internal/servers/serverstest"
)

const cloudManifest = `version: 1
games:
  - id: cs2
locations:
  - id: do-sfo3
    provider: digitalocean
    region: sfo3
    project: 7fd3841c-8bd5-49b1-bf2b-8956a8e252e0
    token_file: /run/secrets/do-token
    ssh_key_file: /run/secrets/cloudvm-key
    image: fedora-44-x64
    extra_ssh_keys: ["ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGravelTestKeyGravelTestKeyGravelTestKey000 john"]
servers:
  - id: htg-cs2-1
    name: "HTG CS2 | NA WEST | 10-MAN #1"
    game: cs2
    driver: cloudvm
    location: do-sfo3
    credential_file: /run/secrets/cs2-rcon
    trust: official
    feed:
      url: https://ingest.example.com
      token_file: /run/secrets/cs2-feed
    vm:
      size: c-2
      volume_gib: 100
      park_after: 30m
      password_file: /run/secrets/cs2-password
    settings:
      game_alias: competitive
      map_group: mg_active
      start_map: de_mirage
      max_players: 10
      tv: false
`

// cloudSpecs are the shipped specs with Counter-Strike 2 run by the cloud-VM driver (the spec
// gains the driver with it; until then a manifest naming it is refused as a catalog entry).
func cloudSpecs() map[string]Spec {
	specs := Specs()
	cs2 := specs["cs2"]
	cs2.Drivers = []string{"cloudvm"}
	specs["cs2"] = cs2
	return specs
}

var cloudDrivers = []string{"cloudvm", "wardogs"}

func parseCloud(t *testing.T) Manifest {
	t.Helper()
	m, err := ParseManifest([]byte(cloudManifest))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestCloudManifestParses(t *testing.T) {
	m := parseCloud(t)
	if err := m.Validate(cloudSpecs(), cloudDrivers, []string{"digitalocean"}); err != nil {
		t.Fatal(err)
	}
	s := m.Servers[0]
	if s.VM == nil || s.VM.Size != "c-2" || s.VM.VolumeGiB != 100 || s.VM.Address != AddressReserved || time.Duration(s.VM.ParkAfter) != 30*time.Minute {
		t.Errorf("vm %+v (address defaults to reserved)", s.VM)
	}
	if s.Settings["max_players"] != "10" || s.Settings["tv"] != "false" {
		t.Errorf("settings %v: numbers and bools read as their text", s.Settings)
	}
	out, err := m.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	again, err := ParseManifest(out)
	if err != nil {
		t.Fatalf("export does not parse: %v\n%s", err, out)
	}
	if d := diff(m, again); len(d) != 0 {
		t.Errorf("export and parse changed it: %v", d)
	}
}

func TestCloudManifestValidation(t *testing.T) {
	for _, c := range []struct {
		name string
		edit func(*Manifest)
		want string
	}{
		{"unknown provider", func(m *Manifest) { m.Locations[0].Provider = "aws" }, "locations[0].provider"},
		{"region", func(m *Manifest) { m.Locations[0].Region = "SFO 3" }, "locations[0].region"},
		{"token relative", func(m *Manifest) { m.Locations[0].TokenFile = "token" }, "locations[0].token_file"},
		{"token is the key", func(m *Manifest) { m.Locations[0].SSHKeyFile = m.Locations[0].TokenFile }, "different files"},
		{"bad extra key", func(m *Manifest) { m.Locations[0].ExtraSSHKeys = []string{"-----BEGIN OPENSSH PRIVATE KEY-----"} }, "extra_ssh_keys[0]"},
		{"ssh from everywhere", func(m *Manifest) { m.Locations[0].SSHFrom = []string{"0.0.0.0/0"} }, "ssh_from"},
		{"location twice", func(m *Manifest) { m.Locations = append(m.Locations, m.Locations[0]) }, "listed twice"},
		{"undefined location", func(m *Manifest) { m.Servers[0].Location = "elsewhere" }, "is not in locations"},
		{"an endpoint", func(m *Manifest) { m.Servers[0].Endpoint = "http://203.0.113.1:27015" }, "has none"},
		{"no feed", func(m *Manifest) { m.Servers[0].Feed = nil }, "feed: required"},
		{"no vm", func(m *Manifest) { m.Servers[0].VM = nil }, "vm: required"},
		{"digit-first id", func(m *Manifest) { m.Servers[0].ID = "1cs2" }, "a letter first"},
		{"long id", func(m *Manifest) { m.Servers[0].ID = "a" + strings.Repeat("b", 40) }, "at most 40"},
		{"size", func(m *Manifest) { m.Servers[0].VM.Size = "C 2" }, "vm.size"},
		{"small volume", func(m *Manifest) { m.Servers[0].VM.VolumeGiB = 50 }, "the game's install needs 80"},
		{"address kind", func(m *Manifest) { m.Servers[0].VM.Address = "floating" }, "vm.address"},
		{"park too soon", func(m *Manifest) { m.Servers[0].VM.ParkAfter = Duration(time.Minute) }, "vm.park_after"},
		{"password is the credential", func(m *Manifest) { m.Servers[0].VM.PasswordFile = m.Servers[0].CredentialFile }, "the same file"},
		{"password relative", func(m *Manifest) { m.Servers[0].VM.PasswordFile = "pw" }, "vm.password_file"},
		{"unknown setting", func(m *Manifest) { m.Servers[0].Settings["rcon_password"] = "x" }, `"rcon_password" is not a setting`},
		{"setting out of range", func(m *Manifest) { m.Servers[0].Settings["max_players"] = "65" }, "max_players"},
		{"setting not a number", func(m *Manifest) { m.Servers[0].Settings["max_players"] = "ten" }, "not a whole number"},
		{"enum", func(m *Manifest) { m.Servers[0].Settings["game_alias"] = "arms_race" }, "game_alias"},
		{"conflict", func(m *Manifest) { m.Servers[0].Settings["game_type"] = "0" }, "may not both be set"},
		{"requires", func(m *Manifest) {
			delete(m.Servers[0].Settings, "game_alias")
			m.Servers[0].Settings["game_mode"] = "1"
		}, "game_mode needs game_type"},
		{"map with a command", func(m *Manifest) { m.Servers[0].Settings["start_map"] = "de_mirage; quit" }, "start_map"},
		{"name the image would mangle", func(m *Manifest) { m.Servers[0].Name = "HTG / CS2" }, "from the server's name"},
		{"vm on a wardogs server", func(m *Manifest) {
			m.Games = append(m.Games, GameRef{ID: "wardogs"})
			m.Servers = append(m.Servers, Server{ID: "wd-1", Name: "W", Game: "wardogs", Driver: "wardogs", Location: "x",
				Endpoint: "http://203.0.113.1:7789", CredentialFile: "/run/secrets/wd", PollInterval: Duration(DefaultPollInterval),
				Trust: TrustOfficial, VM: &VMSpec{Size: "c-2", VolumeGiB: 100}})
		}, "vm and settings are a cloud-VM server's"},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := parseCloud(t)
			c.edit(&m)
			err := m.Validate(cloudSpecs(), cloudDrivers, []string{"digitalocean"})
			if err == nil || !errors.Is(err, ErrInvalidManifest) || !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want %q", err, c.want)
			}
		})
	}
	// Today's shipped CS2 spec names no driver: the cloud-VM driver arrives with it.
	m := parseCloud(t)
	if err := m.Validate(Specs(), cloudDrivers, []string{"digitalocean"}); err != nil && !strings.Contains(err.Error(), "catalog entry") && !strings.Contains(err.Error(), "does not control") {
		t.Errorf("the shipped spec: %v", err)
	}
}

func TestCloudManifestApply(t *testing.T) {
	ctx := context.Background()
	st := serverstest.NewFakeStore()
	svc := New(st, uuid.New(), cloudDrivers, slog.New(slog.NewTextHandler(io.Discard, nil))).WithProviders("digitalocean")
	svc.specs = cloudSpecs()
	m := parseCloud(t)

	dir := t.TempDir()
	write := func(name, v string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(v), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	m.Locations[0].TokenFile, m.Locations[0].SSHKeyFile = write("token", "t\n"), write("key", "k\n")
	m.Servers[0].CredentialFile, m.Servers[0].Feed.TokenFile = write("rcon", "r\n"), write("feed", "f\n")
	m.Servers[0].VM.PasswordFile = write("pw", "")

	if _, err := svc.Apply(ctx, m, ApplyOptions{CheckCredentials: true}); err == nil || !errors.Is(err, ErrCredentialFile) || !strings.Contains(err.Error(), "is empty") {
		t.Fatalf("an empty password file: %v", err)
	}
	m.Servers[0].VM.PasswordFile = write("pw", "hunter2\n")
	changes, err := svc.Apply(ctx, m, ApplyOptions{CheckCredentials: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 3 {
		t.Errorf("changes %v, want the game, the location and the server", changes)
	}
	// Stored and read back, the same manifest: a second apply changes nothing (settings keep
	// their text through the JSON column).
	if changes, err := svc.Apply(ctx, m, ApplyOptions{}); err != nil || len(changes) != 0 {
		t.Errorf("a second apply: %v %v", changes, err)
	}
	got, err := svc.Server(ctx, "htg-cs2-1")
	if err != nil || got.VM == nil || got.VM.PasswordFile != m.Servers[0].VM.PasswordFile || got.Settings["start_map"] != "de_mirage" {
		t.Errorf("read back %+v %v", got, err)
	}
	if l, err := svc.Location(ctx, "do-sfo3"); err != nil || l.Region != "sfo3" || len(l.ExtraSSHKeys) != 1 {
		t.Errorf("location %+v %v", l, err)
	}
	// Removing the location's server and the location: both removed.
	m.Servers, m.Locations = nil, nil
	changes, err = svc.Apply(ctx, m, ApplyOptions{})
	if err != nil || len(changes) != 2 {
		t.Errorf("removal: %v %v", changes, err)
	}
	if _, err := svc.Location(ctx, "do-sfo3"); err == nil {
		t.Error("a removed location is still read")
	}
}
