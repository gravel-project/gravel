package vmdoc

import (
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata/")

// testKey is a well-formed OpenSSH public key line; it authorizes nothing anywhere.
const testKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGravelTestKeyGravelTestKeyGravelTestKey000 gravel-test"

func params() Params {
	return Params{
		Header:           "# A test host.",
		FirstBootComment: "# Test first boot.",
		Hostname:         "game-1",
		SSHKeys:          []string{testKey},
		DataDevice:       "/dev/disk/by-id/scsi-0DO_Volume_game-1-data",
		Files:            []File{{Path: "/usr/local/share/gravel/game.container", Mode: "0644", Content: "[Container]\nImage=example\n"}},
		FirstBootTail:    "as_user systemctl --user daemon-reload\n",
	}
}

func render(t *testing.T, p Params) string {
	t.Helper()
	doc, err := Render(p)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	return doc
}

func TestRenderGolden(t *testing.T) {
	doc := render(t, params())
	path := filepath.Join("testdata", "game.golden.yaml")
	if *update {
		if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if doc != string(want) {
		t.Errorf("the document differs from %s (go test -update to rewrite it after a deliberate change)", path)
	}
}

// doc is the part of a document the tests read back.
type doc struct {
	Users []struct {
		Name string `yaml:"name"`
		UID  int    `yaml:"uid"`
	} `yaml:"users"`
	Packages   []string `yaml:"packages"`
	WriteFiles []struct {
		Path    string `yaml:"path"`
		Content string `yaml:"content"`
	} `yaml:"write_files"`
}

func parse(t *testing.T, s string) doc {
	t.Helper()
	if !strings.HasPrefix(s, "#cloud-config\n") {
		t.Fatal("the document does not start with #cloud-config")
	}
	var d doc
	if err := yaml.Unmarshal([]byte(s), &d); err != nil {
		t.Fatalf("not YAML: %v", err)
	}
	return d
}

func (d doc) file(path string) (string, bool) {
	for _, f := range d.WriteFiles {
		if f.Path == path {
			return f.Content, true
		}
	}
	return "", false
}

func TestRenderStructure(t *testing.T) {
	d := parse(t, render(t, params()))
	if len(d.Users) != 2 || d.Users[0].Name != "admin" || d.Users[1].Name != DefaultUser || d.Users[1].UID != DefaultUID {
		t.Errorf("users %+v", d.Users)
	}
	if strings.Join(d.Packages, ",") != strings.Join(BasePackages, ",") {
		t.Errorf("packages %v, want the base set", d.Packages)
	}
	if c, ok := d.file("/usr/local/share/gravel/game.container"); !ok || !strings.Contains(c, "Image=example") {
		t.Error("the caller's file is missing")
	}
	script, ok := d.file(FirstBootPath)
	if !ok {
		t.Fatal("no first-boot script")
	}
	for _, want := range []string{"home_marker=$home/.gravel-home", "as_user() {", "as_user systemctl --user daemon-reload\n\necho \"first-boot: done\""} {
		if !strings.Contains(script, want) {
			t.Errorf("first-boot lacks %q", want)
		}
	}
	// It waits for the user's systemd instance to answer, not for its bus socket to exist: the
	// socket appears first, and a systemctl --user in that gap is refused (systemd 259, 2026-10-12).
	if strings.Contains(script, "-S /run/user/$uid/bus") || !strings.Contains(script, "as_user systemctl --user show-environment >/dev/null 2>&1 && break") {
		t.Error("first-boot waits for the bus socket instead of the user manager answering")
	}
	if strings.Index(script, "as_user() {") > strings.Index(script, "show-environment") {
		t.Error("the wait uses as_user before it is defined")
	}
	// No tail: the script ends right after the wait.
	p := params()
	p.FirstBootTail = ""
	script, _ = parse(t, render(t, p)).file(FirstBootPath)
	if !strings.Contains(script, "exit 1; }\n\necho \"first-boot: done\"") {
		t.Error("an empty tail left something between the wait and the end")
	}
}

func TestFirstBootScriptParses(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	shellcheck, _ := exec.LookPath("shellcheck")
	for name, tail := range map[string]string{"tail": "as_user true\n", "no tail": ""} {
		t.Run(name, func(t *testing.T) {
			p := params()
			p.FirstBootTail = tail
			script, _ := parse(t, render(t, p)).file(FirstBootPath)
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

func TestRenderValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		mod  func(*Params)
		want string
	}{
		{"no header", func(p *Params) { p.Header = "" }, "header"},
		{"header not a comment", func(p *Params) { p.Header = "# ok\nruncmd: [evil]" }, "comment"},
		{"first-boot comment not a comment", func(p *Params) { p.FirstBootComment = "reboot" }, "comment"},
		{"hostname upper case", func(p *Params) { p.Hostname = "Game-1" }, "hostname"},
		{"no ssh keys", func(p *Params) { p.SSHKeys = nil }, "SSH public key"},
		{"private key", func(p *Params) { p.SSHKeys = []string{"-----BEGIN OPENSSH PRIVATE KEY-----"} }, "not an OpenSSH public key"},
		{"device not under /dev", func(p *Params) { p.DataDevice = "/mnt/data" }, "data device"},
		{"package with a space", func(p *Params) { p.Packages = []string{"vim tmux"} }, "package"},
		{"file path relative", func(p *Params) { p.Files[0].Path = "etc/x" }, "file path"},
		{"file path with dots", func(p *Params) { p.Files[0].Path = "/usr/../etc/shadow" }, "file path"},
		{"file mode", func(p *Params) { p.Files[0].Mode = "644" }, "mode"},
		{"user root", func(p *Params) { p.User = "root" }, "runtime user"},
		{"uid 999", func(p *Params) { p.UID = 999 }, "uid"},
		{"subid start 99999", func(p *Params) { p.SubIDStart = 99999 }, "subordinate"},
		{"vars without the home marker", func(p *Params) { p.FirstBootVars = "x=1" }, "home_marker"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := params()
			tc.mod(&p)
			if _, err := Render(p); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err %v, want one mentioning %q", err, tc.want)
			}
		})
	}
}

func TestNormalizeDefaults(t *testing.T) {
	p := params()
	p.SSHKeys = []string{"  " + testKey + "  "}
	if err := p.Normalize(); err != nil {
		t.Fatal(err)
	}
	if p.User != DefaultUser || p.UID != DefaultUID || p.SubIDStart != DefaultSubIDStart || p.FirstBootVars != DefaultFirstBootVars {
		t.Errorf("defaults: %q %d %d %q", p.User, p.UID, p.SubIDStart, p.FirstBootVars)
	}
	if p.SSHKeys[0] != testKey {
		t.Errorf("key not trimmed: %q", p.SSHKeys[0])
	}
}

func TestValidPublicKey(t *testing.T) {
	if !ValidPublicKey(testKey) || ValidPublicKey("ssh-ed25519") || ValidPublicKey(testKey+"\nx") {
		t.Error("ValidPublicKey")
	}
}
