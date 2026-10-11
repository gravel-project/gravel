package hubvm

import (
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata/")

// testKey is a well-formed OpenSSH public key line; it authorizes nothing anywhere.
const testKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGravelTestKeyGravelTestKeyGravelTestKey000 gravel-test"

const (
	testDevice = "/dev/disk/by-id/scsi-0DO_Volume_hub-1-data"
	testImage  = "ghcr.io/gravel-project/gravel-hub@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

func hostParams() Params {
	return Params{
		Profile:       ProfileHost,
		Hostname:      "hub-1",
		SSHKeys:       []string{testKey},
		DataDevice:    testDevice,
		ExtraPackages: []string{"golang", "gh"},
	}
}

func stockParams() Params {
	p := hostParams()
	p.Profile = ProfileStock
	p.Stock = &StockParams{
		PublicURL:    "https://hub.example.com",
		OrgName:      "Example",
		HubImage:     testImage,
		BehindTunnel: true,
	}
	return p
}

func mustRender(t *testing.T, p Params) string {
	t.Helper()
	doc, err := Render(p)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	return doc
}

func TestRenderGolden(t *testing.T) {
	for _, tc := range []struct {
		name   string
		params Params
	}{
		{"host", hostParams()},
		{"stock", stockParams()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := mustRender(t, tc.params)
			golden := filepath.Join("testdata", tc.name+".golden.yaml")
			if *update {
				if err := os.MkdirAll("testdata", 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("%v (run go test ./internal/hubvm -update to write it)", err)
			}
			if got != string(want) {
				t.Errorf("%s differs from the rendered document (a deploy/quadlet or template change? rerun with -update and review the diff)\n%s", golden, firstDiff(string(want), got))
			}
		})
	}
}

// firstDiff shows the first line where two documents part.
func firstDiff(want, got string) string {
	wl, gl := strings.Split(want, "\n"), strings.Split(got, "\n")
	for i := range max(len(wl), len(gl)) {
		var w, g string
		if i < len(wl) {
			w = wl[i]
		}
		if i < len(gl) {
			g = gl[i]
		}
		if w != g {
			return "line " + strconv.Itoa(i+1) + ":\n  want: " + w + "\n  got:  " + g
		}
	}
	return "(same lines)"
}

// cloudConfig is the part of a cloud-init document the tests look at.
type cloudConfig struct {
	PreserveHostname bool             `yaml:"preserve_hostname"`
	Users            []map[string]any `yaml:"users"`
	FSSetup          []map[string]any `yaml:"fs_setup"`
	Mounts           [][]string       `yaml:"mounts"`
	Packages         []string         `yaml:"packages"`
	RunCmd           [][]string       `yaml:"runcmd"`
	WriteFiles       []struct {
		Path        string `yaml:"path"`
		Permissions string `yaml:"permissions"`
		Content     string `yaml:"content"`
	} `yaml:"write_files"`
}

func parseDoc(t *testing.T, doc string) (cloudConfig, map[string]any) {
	t.Helper()
	var c cloudConfig
	if err := yaml.Unmarshal([]byte(doc), &c); err != nil {
		t.Fatalf("the document is not YAML: %v\n%s", err, doc)
	}
	var raw map[string]any
	if err := yaml.Unmarshal([]byte(doc), &raw); err != nil {
		t.Fatal(err)
	}
	return c, raw
}

func (c cloudConfig) file(path string) (string, string, bool) {
	for _, f := range c.WriteFiles {
		if f.Path == path {
			return f.Content, f.Permissions, true
		}
	}
	return "", "", false
}

func TestRenderStructure(t *testing.T) {
	for _, tc := range []struct {
		name   string
		params Params
	}{
		{"host", hostParams()},
		{"stock", stockParams()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := tc.params
			p.ExtraPackages = append(p.ExtraPackages, "podman") // a duplicate of a base package
			doc := mustRender(t, p)
			if first, _, _ := strings.Cut(doc, "\n"); first != "#cloud-config" {
				t.Errorf("first line %q, want #cloud-config", first)
			}
			c, raw := parseDoc(t, doc)

			if !c.PreserveHostname {
				t.Error("preserve_hostname is not true")
			}
			if _, ok := raw["hostname"]; ok {
				t.Error("the document has a top-level hostname key; runcmd sets the hostname")
			}

			if len(c.Users) != 2 {
				t.Fatalf("users: %v", c.Users)
			}
			admin, user := c.Users[0], c.Users[1]
			if admin["name"] != "admin" || admin["sudo"] != "ALL=(ALL) NOPASSWD:ALL" {
				t.Errorf("admin user: %v", admin)
			}
			if g, _ := admin["groups"].([]any); !slices.Contains(g, any("wheel")) {
				t.Errorf("admin groups: %v", admin["groups"])
			}
			if _, ok := user["sudo"]; ok || user["name"] != DefaultUser {
				t.Errorf("runtime user: %v", user)
			}
			if uid, ok := user["uid"].(int); !ok || uid != DefaultUID {
				t.Errorf("runtime uid %#v (%T), want the integer %d", user["uid"], user["uid"], DefaultUID)
			}
			if user["homedir"] != "/home/gravel" {
				t.Errorf("runtime homedir: %v", user["homedir"])
			}
			for _, u := range c.Users {
				keys, _ := u["ssh_authorized_keys"].([]any)
				if len(keys) != 1 || keys[0] != testKey {
					t.Errorf("user %v keys: %v", u["name"], keys)
				}
			}

			if len(c.FSSetup) != 1 || c.FSSetup[0]["overwrite"] != false || c.FSSetup[0]["device"] != testDevice || c.FSSetup[0]["label"] != "gravel-data" {
				t.Errorf("fs_setup: %v", c.FSSetup)
			}
			if len(c.Mounts) != 1 || c.Mounts[0][0] != "LABEL=gravel-data" || c.Mounts[0][1] != "/home/gravel" {
				t.Errorf("mounts: %v", c.Mounts)
			}

			wantPkgs := append(slices.Clone(BasePackages), "golang", "gh")
			if !slices.Equal(c.Packages, wantPkgs) {
				t.Errorf("packages %v, want %v", c.Packages, wantPkgs)
			}

			for _, want := range [][]string{
				{"hostnamectl", "set-hostname", "hub-1"},
				{"systemctl", "enable", "--now", "dnf5-automatic.timer"},
				{"/usr/local/libexec/gravel/first-boot"},
			} {
				if !slices.ContainsFunc(c.RunCmd, func(cmd []string) bool { return slices.Equal(cmd, want) }) {
					t.Errorf("runcmd %v lacks %v", c.RunCmd, want)
				}
			}

			firstBoot, mode, ok := c.file("/usr/local/libexec/gravel/first-boot")
			if !ok || mode != "0755" || !strings.HasPrefix(firstBoot, "#!/bin/bash\n") {
				t.Errorf("first-boot: present %v, mode %q, starts %q", ok, mode, firstBoot[:min(len(firstBoot), 20)])
			}

			stockPaths := []string{
				"/usr/local/share/gravel/quadlet/gravel-hub.container",
				"/usr/local/share/gravel/quadlet/gravel-postgres.container",
				"/usr/local/share/gravel/quadlet/gravel-postgres.volume",
				"/usr/local/share/gravel/quadlet/gravel.network",
				"/usr/local/share/gravel/quadlet/gravel-cloudflared.container",
				"/usr/local/share/gravel/hub.yaml",
				"/usr/local/share/gravel/gravel-hub-vm.conf",
			}
			if tc.name == "host" {
				for _, f := range c.WriteFiles {
					if strings.HasPrefix(f.Path, "/usr/local/share/gravel/") {
						t.Errorf("the host profile writes %s", f.Path)
					}
				}
				if strings.Contains(firstBoot, "podman secret") {
					t.Error("the host profile's first-boot makes podman secrets")
				}
				return
			}
			for _, path := range stockPaths {
				if _, mode, ok := c.file(path); !ok || mode != "0644" {
					t.Errorf("stock file %s: present %v, mode %q", path, ok, mode)
				}
			}
			n := 0
			for _, f := range c.WriteFiles {
				if strings.HasPrefix(f.Path, "/usr/local/share/gravel/quadlet/") {
					n++
				}
			}
			if n != 5 {
				t.Errorf("%d quadlet files, want 5", n)
			}
			dropIn, _, _ := c.file("/usr/local/share/gravel/gravel-hub-vm.conf")
			if !strings.Contains(dropIn, "Secret=gravel-stats-pseudonym-key\n") || !strings.Contains(dropIn, "Image="+testImage+"\n") {
				t.Errorf("drop-in:\n%s", dropIn)
			}
			if !strings.Contains(firstBoot, "podman secret create gravel-db-password -") {
				t.Error("first-boot does not make the database password on the host")
			}
		})
	}
}

// stockHubYAML renders the stock profile and returns its hub.yaml, parsed.
func stockHubYAML(t *testing.T, s StockParams) (map[string]any, string) {
	t.Helper()
	p := stockParams()
	p.Stock = &s
	c, _ := parseDoc(t, mustRender(t, p))
	content, _, ok := c.file("/usr/local/share/gravel/hub.yaml")
	if !ok {
		t.Fatal("no hub.yaml")
	}
	var m map[string]any
	if err := yaml.Unmarshal([]byte(content), &m); err != nil {
		t.Fatalf("hub.yaml is not YAML: %v\n%s", err, content)
	}
	dropIn, _, _ := c.file("/usr/local/share/gravel/gravel-hub-vm.conf")
	return m, dropIn
}

func TestStockHubYAML(t *testing.T) {
	m, dropIn := stockHubYAML(t, StockParams{PublicURL: "https://hub.example.com", OrgName: `Ex "ample": #1`, BehindTunnel: true})
	sub := func(key string) map[string]any { v, _ := m[key].(map[string]any); return v }
	if got := sub("auth")["base_url"]; got != "https://hub.example.com" {
		t.Errorf("auth.base_url %v", got)
	}
	if got := sub("organization")["name"]; got != `Ex "ample": #1` {
		t.Errorf("organization.name %v", got)
	}
	if got := sub("server")["client_ip_header"]; got != "CF-Connecting-IP" {
		t.Errorf("server.client_ip_header %v behind a tunnel", got)
	}
	stats := sub("stats")
	if stats["pseudonym_key_file"] != "/run/secrets/gravel-stats-pseudonym-key" {
		t.Errorf("stats.pseudonym_key_file %v", stats["pseudonym_key_file"])
	}
	if _, ok := stats["pseudonym_key"]; ok {
		t.Error("stats.pseudonym_key is inline")
	}
	if strings.Contains(dropIn, "Image=") {
		t.Errorf("drop-in sets an image without --hub-image:\n%s", dropIn)
	}
	if !strings.Contains(dropIn, "Secret=gravel-stats-pseudonym-key") {
		t.Errorf("drop-in:\n%s", dropIn)
	}

	m, _ = stockHubYAML(t, StockParams{PublicURL: "http://localhost:8080"})
	if _, ok := sub("server")["client_ip_header"]; ok {
		t.Error("server.client_ip_header set without a tunnel")
	}
	if got := sub("organization")["name"]; got != "gravel" {
		t.Errorf("default organization.name %v", got)
	}
}

func TestRenderHoldsNoSecret(t *testing.T) {
	doc := mustRender(t, stockParams())
	if regexp.MustCompile(`pseudonym_key:\s*\S`).MatchString(doc) {
		t.Error("an inline pseudonym_key")
	}
	if m := regexp.MustCompile(`postgres://gravel:([^%]|%[^s])`).FindString(doc); m != "" {
		t.Errorf("a database URL with a password: %q", m)
	}
	if strings.Contains(doc, "TUNNEL_TOKEN") {
		t.Error("the document mentions TUNNEL_TOKEN")
	}
	if !strings.Contains(doc, "podman secret create gravel-db-password -") {
		t.Error("the database password is not generated on the host")
	}
}

func TestRenderValidation(t *testing.T) {
	long := strings.Repeat("a", 64)
	for _, tc := range []struct {
		name string
		mod  func(*Params)
		want string
	}{
		{"unknown profile", func(p *Params) { p.Profile = "full" }, "profile"},
		{"stock without stock params", func(p *Params) { p.Profile = ProfileStock }, "public URL"},
		{"public URL with a path", func(p *Params) { *p = stockParams(); p.Stock.PublicURL = "https://hub.example.com/x" }, "origin"},
		{"public URL not http", func(p *Params) { *p = stockParams(); p.Stock.PublicURL = "ftp://hub.example.com" }, "origin"},
		{"public URL empty", func(p *Params) { *p = stockParams(); p.Stock.PublicURL = "" }, "origin"},
		{"bad hub image", func(p *Params) { *p = stockParams(); p.Stock.HubImage = "Not An Image" }, "image"},
		{"hub image with newline", func(p *Params) { *p = stockParams(); p.Stock.HubImage = "ghcr.io/x\nExec=evil" }, "image"},
		{"org name newline", func(p *Params) { *p = stockParams(); p.Stock.OrgName = "a\nb" }, "one line"},
		{"hostname upper case", func(p *Params) { p.Hostname = "Hub-1" }, "hostname"},
		{"hostname leading hyphen", func(p *Params) { p.Hostname = "-hub" }, "hostname"},
		{"hostname 64 chars", func(p *Params) { p.Hostname = long }, "hostname"},
		{"no ssh keys", func(p *Params) { p.SSHKeys = nil }, "SSH public key"},
		{"private key", func(p *Params) { p.SSHKeys = []string{"-----BEGIN OPENSSH PRIVATE KEY-----"} }, "not an OpenSSH public key"},
		{"key with a newline", func(p *Params) { p.SSHKeys = []string{testKey + "\nruncmd: [evil]"} }, "not an OpenSSH public key"},
		{"device not under /dev", func(p *Params) { p.DataDevice = "/mnt/data" }, "data device"},
		{"device with a space", func(p *Params) { p.DataDevice = "/dev/disk/by-id/a b" }, "data device"},
		{"device with a quote", func(p *Params) { p.DataDevice = `/dev/sd"a` }, "data device"},
		{"package with a space", func(p *Params) { p.ExtraPackages = []string{"vim tmux"} }, "package"},
		{"package with a semicolon", func(p *Params) { p.ExtraPackages = []string{"vim;reboot"} }, "package"},
		{"user root", func(p *Params) { p.User = "root" }, "runtime user"},
		{"user admin", func(p *Params) { p.User = "admin" }, "runtime user"},
		{"user invalid", func(p *Params) { p.User = "Gravel User" }, "runtime user"},
		{"uid 999", func(p *Params) { p.UID = 999 }, "uid"},
		{"uid 60001", func(p *Params) { p.UID = 60001 }, "uid"},
		{"subid start 99999", func(p *Params) { p.SubIDStart = 99999 }, "subordinate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := hostParams()
			tc.mod(&p)
			_, err := Render(p)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err %v, want one mentioning %q", err, tc.want)
			}
		})
	}
}

func TestNormalizeDefaults(t *testing.T) {
	p := hostParams()
	p.SSHKeys = []string{"  " + testKey + "  "}
	if err := p.normalize(); err != nil {
		t.Fatal(err)
	}
	if p.User != DefaultUser || p.UID != DefaultUID || p.SubIDStart != DefaultSubIDStart {
		t.Errorf("defaults: %q %d %d", p.User, p.UID, p.SubIDStart)
	}
	if p.SSHKeys[0] != testKey {
		t.Errorf("key not trimmed: %q", p.SSHKeys[0])
	}

	p = hostParams()
	p.User, p.UID, p.SubIDStart = "hub", 2000, 200000
	doc := mustRender(t, p)
	if !strings.Contains(doc, "uid: 2000\n") || !strings.Contains(doc, "hub:200000:65536") || !strings.Contains(doc, "/home/hub") {
		t.Error("custom user, uid and subordinate ids not rendered")
	}

	// The host profile ignores stock parameters, even invalid ones.
	p = hostParams()
	p.Stock = &StockParams{PublicURL: "not a url"}
	doc = mustRender(t, p)
	if strings.Contains(doc, "/usr/local/share/gravel/") {
		t.Error("the host profile rendered stock files")
	}
}

func TestFirstBootScriptParses(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	shellcheck, _ := exec.LookPath("shellcheck")
	for _, tc := range []struct {
		name   string
		params Params
	}{
		{"host", hostParams()},
		{"stock", stockParams()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := parseDoc(t, mustRender(t, tc.params))
			script, _, ok := c.file("/usr/local/libexec/gravel/first-boot")
			if !ok {
				t.Fatal("no first-boot")
			}
			path := filepath.Join(t.TempDir(), "first-boot")
			if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
				t.Fatal(err)
			}
			if out, err := exec.CommandContext(t.Context(), bash, "-n", path).CombinedOutput(); err != nil {
				t.Errorf("bash -n: %v\n%s", err, out)
			}
			if shellcheck == "" {
				return
			}
			if out, err := exec.CommandContext(t.Context(), shellcheck, "--severity=error", path).CombinedOutput(); err != nil {
				t.Errorf("shellcheck: %v\n%s", err, out)
			}
		})
	}
}

func TestIndent(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"a\nb\n", "  a\n  b"},
		{"a\n\nb", "  a\n\n  b"},
		{"a\n\n\n", "  a"},
		{"", ""},
	} {
		if got := indent(2, tc.in); got != tc.want {
			t.Errorf("indent(2, %q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A key's comment is free text: a ": " or " #" in it must not turn the key into a mapping or cut
// it short, or the host is made with no key it can be reached by.
func TestRenderKeyComments(t *testing.T) {
	for _, comment := range []string{"john: laptop", "work #2", `quote " and \ backslash`, "ünïcode"} {
		key := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGravelTestKey " + comment
		p := hostParams()
		p.SSHKeys = []string{key}
		c, _ := parseDoc(t, mustRender(t, p))
		for _, u := range c.Users {
			keys, _ := u["ssh_authorized_keys"].([]any)
			if len(keys) != 1 || keys[0] != key {
				t.Errorf("comment %q: user %v keys %#v", comment, u["name"], keys)
			}
		}
	}
}
