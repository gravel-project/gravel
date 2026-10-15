package deploy_test

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// composeFile is the part of a Compose file the tests read.
type composeFile struct {
	Services map[string]struct {
		Image    string   `yaml:"image"`
		Profiles []string `yaml:"profiles"`
		Secrets  []any    `yaml:"secrets"`
	} `yaml:"services"`
	Secrets map[string]struct {
		File string `yaml:"file"`
	} `yaml:"secrets"`
}

func readCompose(t *testing.T, path string) composeFile {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var c composeFile
	if err := yaml.Unmarshal(b, &c); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return c
}

var imageLine = regexp.MustCompile(`(?m)^Image=(\S+)$`)

// unitImage is the image a quadlet unit (or drop-in) pins.
func unitImage(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	m := imageLine.FindSubmatch(b)
	if m == nil {
		t.Fatalf("%s has no Image=", path)
	}
	return string(m[1])
}

// The Compose tier runs what the quadlets run (gravel#210): every image it shares with them is
// pinned by the same digest, so moving a pin in one place and not the other fails here.
func TestComposeMatchesTheQuadlets(t *testing.T) {
	c := readCompose(t, "compose/compose.yaml")
	for service, unit := range map[string]string{
		"hub":         "quadlet/gravel-hub.container",
		"bot":         "quadlet/gravel-bot.container",
		"cloudflared": "quadlet/gravel-cloudflared.container",
		"postgres":    "quadlet/backup/gravel-postgres.container.d/10-backup.conf", // WAL-G's image: archiving is a setting here
		"backup":      "quadlet/backup/gravel-backup.container",
		"prometheus":  "quadlet/observability/gravel-prometheus.container",
		"grafana":     "quadlet/observability/gravel-grafana.container",
	} {
		s, ok := c.Services[service]
		if !ok {
			t.Errorf("compose.yaml has no %s service", service)
			continue
		}
		if want := unitImage(t, unit); s.Image != want {
			t.Errorf("%s: compose pins %s, %s pins %s", service, s.Image, unit, want)
		}
	}
	// The hub reports the image it runs (/healthz): GRAVEL_IMAGE is the pin.
	b, _ := os.ReadFile("compose/compose.yaml")
	if want := "GRAVEL_IMAGE: " + c.Services["hub"].Image; !strings.Contains(string(b), want) {
		t.Errorf("compose.yaml's hub has no %q", want)
	}
}

// Every image is pinned by digest, from gravel's own namespace or Quay, never a floating tag or
// Docker Hub (#79); every secret a service uses is a file under local/secrets, which init.sh
// writes; the profiles are the ones the file and the README name.
func TestComposeIsPinnedAndComplete(t *testing.T) {
	c := readCompose(t, "compose/compose.yaml")
	var profiles []string
	for name, s := range c.Services {
		if !regexp.MustCompile(`^(ghcr\.io/gravel-project/|quay\.io/)[^@\s]+@sha256:[0-9a-f]{64}$`).MatchString(s.Image) {
			t.Errorf("%s: image %q is not pinned by digest from ghcr.io/gravel-project or quay.io", name, s.Image)
		}
		profiles = append(profiles, s.Profiles...)
		for _, sec := range s.Secrets {
			src, _ := sec.(string)
			if m, ok := sec.(map[string]any); ok {
				src, _ = m["source"].(string)
			}
			if _, ok := c.Secrets[src]; !ok {
				t.Errorf("%s uses secret %q, which the file doesn't define", name, src)
			}
		}
	}
	slices.Sort(profiles)
	if got := slices.Compact(profiles); !slices.Equal(got, []string{"backups", "bot", "https", "observability", "tunnel"}) {
		t.Errorf("profiles = %v", got)
	}
	script, err := os.ReadFile("compose/init.sh")
	if err != nil {
		t.Fatal(err)
	}
	for name, s := range c.Secrets {
		file := filepath.Base(s.File)
		if !strings.HasPrefix(s.File, "./local/secrets/") || !strings.Contains(string(script), file) {
			t.Errorf("secret %s is %q: not a file under local/secrets/ that init.sh writes", name, s.File)
		}
	}
	// Caddy's copy is in the mirror list, as every Docker Hub image is.
	mirror, _ := os.ReadFile("mirror/images.txt")
	if !strings.Contains(string(mirror), "ghcr.io/gravel-project/mirror/caddy:") {
		t.Error("deploy/mirror/images.txt doesn't copy caddy")
	}
}
