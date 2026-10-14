package servers

import (
	"strings"
	"testing"
)

func TestHostOnly(t *testing.T) {
	base := func() Manifest {
		return Manifest{
			Games:     []GameRef{{ID: "wardogs"}, {ID: "cs2"}},
			Locations: []Location{{ID: "do-sfo3", Provider: "digitalocean", Region: "sfo3", TokenFile: "/run/secrets/do", SSHKeyFile: "/run/secrets/ssh"}},
			Servers: []Server{
				{ID: "wd-1", Name: "War Dogs #1", Game: "wardogs", Driver: "wardogs", Location: "slc", Endpoint: "http://10.0.0.5:8080", CredentialFile: "/run/secrets/wd-1", Trust: TrustOfficial},
				{ID: "cs2-1", Name: "CS2 #1", Game: "cs2", Driver: "cloudvm", Location: "do-sfo3", CredentialFile: "/run/secrets/cs2-rcon", Trust: TrustOfficial,
					VM: &VMSpec{Size: "c-2", VolumeGiB: 100, PasswordFile: "/run/secrets/cs2-pw", PublisherTokenFile: "/run/secrets/gslt"}},
			},
		}
	}
	cases := []struct {
		name string
		edit func(m *Manifest)
		want []string
	}{
		{"nothing", func(*Manifest) {}, nil},
		{"what the API may change", func(m *Manifest) {
			m.Servers[0].Name, m.Servers[0].Trust, m.Servers[0].PollInterval = "War Dogs One", TrustCommunity, Duration(30e9)
			m.Servers[1].VM.Size, m.Servers[1].Settings = "c-4", map[string]string{"start_map": "de_nuke"}
		}, nil},
		{"removing a server", func(m *Manifest) { m.Servers = m.Servers[:1] }, nil},
		{"the endpoint (where the credential goes)", func(m *Manifest) { m.Servers[0].Endpoint = "https://attacker.example" }, []string{"server wd-1: its endpoint"}},
		{"the credential file", func(m *Manifest) { m.Servers[0].CredentialFile = "/run/secrets/gravel-db-url" }, []string{"server wd-1: its credential_file"}},
		{"the driver and location", func(m *Manifest) { m.Servers[0].Driver, m.Servers[0].Location = "cloudvm", "do-sfo3" }, []string{"server wd-1: its driver", "server wd-1: its location"}},
		{"a feed", func(m *Manifest) {
			m.Servers[0].Feed = &Feed{URL: "https://attacker.example", TokenFile: "/run/secrets/x"}
		}, []string{"server wd-1: its feed"}},
		{"the VM's files and password sharing", func(m *Manifest) {
			m.Servers[1].VM.PasswordFile, m.Servers[1].VM.PublisherTokenFile, m.Servers[1].VM.SharePassword = "/etc/shadow", "", true
		}, []string{"server cs2-1: its vm.password_file", "server cs2-1: its vm.publisher_token_file", "server cs2-1: its vm.share_password"}},
		{"a new server", func(m *Manifest) {
			m.Servers = append(m.Servers, Server{ID: "wd-2", Name: "War Dogs #2", Game: "wardogs", Driver: "wardogs", Location: "slc", Endpoint: "http://x", CredentialFile: "/run/secrets/wd-1", Trust: TrustOfficial})
		}, []string{"server wd-2: adding a server"}},
		{"a location's extra key", func(m *Manifest) { m.Locations[0].ExtraSSHKeys = []string{"ssh-ed25519 AAAA attacker"} }, []string{"location do-sfo3: changing a location"}},
		{"a new location", func(m *Manifest) {
			m.Locations = append(m.Locations, Location{ID: "do-nyc3", Provider: "digitalocean", Region: "nyc3", TokenFile: "/a", SSHKeyFile: "/b"})
		}, []string{"location do-nyc3: adding a location"}},
		{"removing a location", func(m *Manifest) { m.Locations = nil }, []string{"location do-sfo3: removing a location"}},
	}
	for _, c := range cases {
		want := base()
		c.edit(&want)
		got := HostOnly(base(), want)
		if len(got) != len(c.want) {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
			continue
		}
		for i := range got {
			if !strings.HasPrefix(got[i], c.want[i]) {
				t.Errorf("%s: [%d] %q, want %q…", c.name, i, got[i], c.want[i])
			}
		}
	}
}
