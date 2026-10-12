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

	"github.com/gravel-project/gravel/cloud"
	"github.com/gravel-project/gravel/cloud/fake"
	"github.com/gravel-project/gravel/drivers/cloudvm"
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

// cloudSpecs are the shipped specs: Counter-Strike 2 is run by the cloud-VM driver.
func cloudSpecs() map[string]Spec { return Specs() }

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
		{"small volume", func(m *Manifest) { m.Servers[0].VM.VolumeGiB = 50 }, "the game's install needs 100"},
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
		{"scaling beside max_players", func(m *Manifest) { m.Servers[0].VM.Scaling = &ScalingSpec{} }, "settings.max_players is the ladder's"},
		{"lease: a bad pool", func(m *Manifest) { m.Servers[0].VM.Lease = &LeaseSpec{Pool: "PUG pool"} }, "vm.lease.pool"},
		{"lease: too short a release", func(m *Manifest) { m.Servers[0].VM.Lease = &LeaseSpec{ReleaseAfter: Duration(time.Second)} }, "vm.lease.release_after"},
		{"lease: too long", func(m *Manifest) { m.Servers[0].VM.Lease = &LeaseSpec{Max: Duration(24 * time.Hour)} }, "vm.lease.max"},
		{"lease: an unknown setting", func(m *Manifest) { m.Servers[0].VM.Lease = &LeaseSpec{Settings: []string{"rcon"}} }, "vm.lease.settings: rcon is not one"},
		{"lease: the players setting", func(m *Manifest) { m.Servers[0].VM.Lease = &LeaseSpec{Settings: []string{"max_players"}} }, "the server's to set"},
		{"lease: a setting twice", func(m *Manifest) { m.Servers[0].VM.Lease = &LeaseSpec{Settings: []string{"tv", "tv"}} }, "tv is listed twice"},
		{"scaling: size off the ladder", func(m *Manifest) {
			delete(m.Servers[0].Settings, "max_players")
			m.Servers[0].VM.Size = "c-8"
			m.Servers[0].VM.Scaling = &ScalingSpec{}
		}, "vm.size c-8 is not on the ladder"},
		{"scaling: one rung", func(m *Manifest) {
			delete(m.Servers[0].Settings, "max_players")
			m.Servers[0].VM.Scaling = &ScalingSpec{Ladder: []cloudvm.Tier{{Size: "c-2", Players: 10}}}
		}, "at least two rungs"},
		{"scaling: players not growing", func(m *Manifest) {
			delete(m.Servers[0].Settings, "max_players")
			m.Servers[0].VM.Scaling = &ScalingSpec{Ladder: []cloudvm.Tier{{Size: "c-2", Players: 10}, {Size: "c-4", Players: 10}}}
		}, "is not more than the rung below's"},
		{"scaling: players the game refuses", func(m *Manifest) {
			delete(m.Servers[0].Settings, "max_players")
			m.Servers[0].VM.Scaling = &ScalingSpec{Ladder: []cloudvm.Tier{{Size: "c-2", Players: 10}, {Size: "c-4", Players: 99}}}
		}, "99 players: max_players"},
		{"scaling: thresholds crossed", func(m *Manifest) {
			delete(m.Servers[0].Settings, "max_players")
			m.Servers[0].VM.Scaling = &ScalingSpec{UpAt: 0.3, DownAt: 0.5}
		}, "0 < down_at < up_at"},
		{"scaling: up_at over 1", func(m *Manifest) {
			delete(m.Servers[0].Settings, "max_players")
			m.Servers[0].VM.Scaling = &ScalingSpec{UpAt: 1.5}
		}, "up_at"},
		{"scaling: hold too short", func(m *Manifest) {
			delete(m.Servers[0].Settings, "max_players")
			m.Servers[0].VM.Scaling = &ScalingSpec{UpFor: Duration(10 * time.Second)}
		}, "up_for: 10s is outside"},
		{"scaling: cooldown too short", func(m *Manifest) {
			delete(m.Servers[0].Settings, "max_players")
			m.Servers[0].VM.Scaling = &ScalingSpec{Cooldown: Duration(time.Minute)}
		}, "cooldown: 1m0s is outside"},
		{"seed: both id and name", func(m *Manifest) { m.Servers[0].VM.SeedSnapshot = &SeedSnapshot{ID: "1", Name: "x"} }, "an id or a name, one of them"},
		{"seed: neither", func(m *Manifest) { m.Servers[0].VM.SeedSnapshot = &SeedSnapshot{} }, "an id or a name, one of them"},
		{"seed: bad name", func(m *Manifest) { m.Servers[0].VM.SeedSnapshot = &SeedSnapshot{Name: "my install!"} }, "is not a snapshot name"},
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
	// A hub without the cloud: the location's provider is refused.
	m := parseCloud(t)
	if err := m.Validate(Specs(), cloudDrivers, nil); err == nil || !strings.Contains(err.Error(), "is not a cloud this hub has") {
		t.Errorf("no providers: %v", err)
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

// vm.scaling: {} turns the policy on with the game's ladder and the defaults; it round-trips
// through the JSON column, and the hub's spec carries it.
func TestCloudManifestScaling(t *testing.T) {
	m := parseCloud(t)
	delete(m.Servers[0].Settings, "max_players")
	m.Servers[0].VM.Scaling = &ScalingSpec{}
	if err := m.Validate(cloudSpecs(), cloudDrivers, []string{"digitalocean"}); err != nil {
		t.Fatal(err)
	}
	p := m.Servers[0].VM.Scaling.Policy(Specs()["cs2"].CloudVM.Ladder)
	if len(p.Ladder) != 2 || p.Ladder[1].Size != "c-4" || p.UpAt != cloudvm.DefaultUpAt || p.DownFor != cloudvm.DefaultDownFor || p.Cooldown != cloudvm.DefaultCooldown {
		t.Errorf("policy %+v: the game's ladder and the defaults", p)
	}
	parsed, err := ParseManifest([]byte(strings.Replace(strings.Replace(cloudManifest, "      max_players: 10\n", "", 1),
		"      park_after: 30m\n", "      park_after: 30m\n      scaling:\n        up_for: 2m\n        cooldown: 45m\n", 1)))
	if err != nil {
		t.Fatal(err)
	}
	sc := parsed.Servers[0].VM.Scaling
	if sc == nil || time.Duration(sc.UpFor) != 2*time.Minute || time.Duration(sc.Cooldown) != 45*time.Minute {
		t.Errorf("parsed scaling %+v", sc)
	}
}

// Apply walks the ladder against the cloud (#37): a host made at c-2 can walk c-2 and c-4 both
// ways; one made at c-4 has c-4's disk and can never come back down, so it is refused when saved.
func TestCloudManifestApplyWalksTheLadder(t *testing.T) {
	ctx := context.Background()
	svc := New(serverstest.NewFakeStore(), uuid.New(), cloudDrivers, slog.New(slog.NewTextHandler(io.Discard, nil))).WithProviders("digitalocean")
	m := parseCloud(t)
	delete(m.Servers[0].Settings, "max_players")
	m.Servers[0].VM.Scaling = &ScalingSpec{}
	dir := t.TempDir()
	write := func(name, v string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(v), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	m.Locations[0].TokenFile, m.Locations[0].SSHKeyFile = write("token", "do-token\n"), write("key", "k\n")
	m.Servers[0].CredentialFile, m.Servers[0].Feed.TokenFile = write("rcon", "r\n"), write("feed", "f\n")
	m.Servers[0].VM.PasswordFile = write("pw", "pw\n")
	// The fake cloud with DigitalOcean's two rungs in sfo3: c-2 (25 GB disk) and c-4 (50 GB).
	p := fake.New(fake.WithRegions(cloud.Region{Slug: "sfo3", Available: true}), fake.WithSizes(
		cloud.Size{Slug: "c-2", VCPUs: 2, MemoryMiB: 4096, DiskGiB: 25, PriceHourly: 0.0625, Family: "cpu", DedicatedCPU: true, Regions: []string{"sfo3"}},
		cloud.Size{Slug: "c-4", VCPUs: 4, MemoryMiB: 8192, DiskGiB: 50, PriceHourly: 0.125, Family: "cpu", DedicatedCPU: true, Regions: []string{"sfo3"}}))
	var gotToken string
	clouds := func(_ context.Context, l Location, token string) (cloud.Provider, error) {
		gotToken = token
		return p, nil
	}
	if _, err := svc.Apply(ctx, m, ApplyOptions{DryRun: true, CheckCredentials: true, Clouds: clouds}); err != nil {
		t.Fatalf("a walkable ladder: %v", err)
	}
	if gotToken != "do-token" {
		t.Errorf("the provider got token %q, want the location's file's first line", gotToken)
	}
	m.Servers[0].VM.Size = "c-4"
	_, err := svc.Apply(ctx, m, ApplyOptions{DryRun: true, CheckCredentials: true, Clouds: clouds})
	if err == nil || !errors.Is(err, ErrInvalidManifest) || !strings.Contains(err.Error(), "c-4 back to c-2 with the 50 GiB root disk") {
		t.Errorf("made at the top rung: %v", err)
	}
	// Without Clouds (servers check, in CI) nothing asks the cloud.
	if _, err := svc.Apply(ctx, m, ApplyOptions{DryRun: true, CheckCredentials: true}); err != nil {
		t.Errorf("no clouds: %v", err)
	}
}

// A seed snapshot is looked up at apply (#166): one in the location's region no bigger than the
// volume is taken; a missing one, one in another region and one too big for the volume are not.
func TestCloudManifestApplyChecksTheSeed(t *testing.T) {
	ctx := context.Background()
	svc := New(serverstest.NewFakeStore(), uuid.New(), cloudDrivers, slog.New(slog.NewTextHandler(io.Discard, nil))).WithProviders("digitalocean")
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
	m.Servers[0].VM.PasswordFile = write("pw", "pw\n")
	p := fake.New(fake.WithRegions(cloud.Region{Slug: "sfo3", Available: true}, cloud.Region{Slug: "nyc3", Available: true}))
	mk := func(region, name string, gib int) string {
		v, err := p.CreateVolume(ctx, cloud.VolumeSpec{Name: name + "-src", Region: region, SizeGiB: gib})
		if err != nil {
			t.Fatal(err)
		}
		snap, err := p.CreateVolumeSnapshot(ctx, v.ID, name, nil)
		if err != nil {
			t.Fatal(err)
		}
		return snap.ID
	}
	mk("sfo3", "install", 100)
	elsewhere := mk("nyc3", "elsewhere", 100)
	mk("sfo3", "big", 200)
	clouds := func(context.Context, Location, string) (cloud.Provider, error) { return p, nil }
	for _, c := range []struct {
		seed SeedSnapshot
		want string
	}{
		{SeedSnapshot{Name: "install"}, ""},
		{SeedSnapshot{Name: "missing"}, "not found"},
		{SeedSnapshot{Name: "elsewhere"}, "not found"}, // by name, in the location's region only
		{SeedSnapshot{ID: elsewhere}, "in its own region"},
		{SeedSnapshot{Name: "big"}, "needs at least 200"},
	} {
		m.Servers[0].VM.SeedSnapshot = &c.seed
		_, err := svc.Apply(ctx, m, ApplyOptions{DryRun: true, CheckCredentials: true, Clouds: clouds})
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%+v: %v", c.seed, err)
		case c.want != "" && (err == nil || !errors.Is(err, ErrInvalidManifest) || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%+v: %v, want it to say %q", c.seed, err, c.want)
		}
	}
}

// A lease section (#20): parsed, its defaults filled, valid with the settings a lease may set.
func TestCloudManifestLease(t *testing.T) {
	parsed, err := ParseManifest([]byte(strings.Replace(cloudManifest, "      park_after: 30m\n",
		"      park_after: 30m\n      lease:\n        pool: pug\n        idle_after: 20m\n        settings: [start_map, tv]\n", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if err := parsed.Validate(cloudSpecs(), cloudDrivers, []string{"digitalocean"}); err != nil {
		t.Fatal(err)
	}
	l := parsed.Servers[0].VM.Lease.WithDefaults()
	if l.Pool != "pug" || time.Duration(l.IdleAfter) != 20*time.Minute || time.Duration(l.ReleaseAfter) != DefaultLeaseReleaseAfter ||
		time.Duration(l.Max) != DefaultLeaseMax || time.Duration(l.ReadyWithin) != DefaultLeaseReadyWithin || len(l.Settings) != 2 {
		t.Errorf("lease %+v", l)
	}
}
