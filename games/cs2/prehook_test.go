package cs2_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/gravel-project/gravel/games/cs2"
)

// preHook is the pre.sh the Game spec gives the hub to keep in the install.
func preHook(t *testing.T) string {
	t.Helper()
	var spec struct {
		CloudVM struct {
			Hooks []struct{ Path, Content string } `yaml:"hooks"`
		} `yaml:"cloudvm"`
	}
	if err := yaml.Unmarshal(cs2.GameSpec, &spec); err != nil {
		t.Fatal(err)
	}
	for _, h := range spec.CloudVM.Hooks {
		if h.Path == "pre.sh" {
			return h.Content
		}
	}
	t.Fatal("the Game spec has no pre.sh hook")
	return ""
}

// #170: the image's entry.sh sources pre.sh, then runs the server, which prints its command line
// with the login token, the RCON password and the join password on it. After pre.sh, none of the
// three values reaches the output, and everything else does, in order.
func TestPreHookMasksTheSecrets(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pre.sh"), []byte(preHook(t)), 0o600); err != nil {
		t.Fatal(err)
	}
	// What entry.sh does around the hook, with a fake server that prints as CS2 does and also
	// writes to stderr.
	entry := `set -u
echo "before the hook: steamcmd output"
source "$1/pre.sh"
server() {
  echo "Command line: ./cs2 -dedicated -port 27015 +map de_inferno +rcon_password R0nPass +sv_setsteamaccount 0123456789ABCDEF0123456789ABCDEF +sv_password joinpw +sv_lan 0"
  echo 'sv_password = "joinpw"'
  echo '"rcon_password" "R0nPass"'
  echo "Couldn't set sv_setsteamaccount: invalid" >&2
  echo "after"
}
server
`
	out, err := exec.CommandContext(t.Context(), "bash", "-c", entry, "entry", dir).CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	got := string(out)
	for _, secret := range []string{"R0nPass", "0123456789ABCDEF", "joinpw"} {
		if strings.Contains(got, secret) {
			t.Errorf("%q reached the output:\n%s", secret, got)
		}
	}
	for _, want := range []string{"before the hook: steamcmd output", "masked from here on", "+map de_inferno +rcon_password [redacted] +sv_setsteamaccount [redacted] +sv_password [redacted] +sv_lan 0",
		`sv_password = "[redacted]"`, `"rcon_password" "[redacted]"`, "Couldn't set sv_setsteamaccount: invalid", "after"} {
		if !strings.Contains(got, want) {
			t.Errorf("the output lacks %q:\n%s", want, got)
		}
	}
	if strings.Index(got, "Command line") > strings.Index(got, "after") {
		t.Errorf("out of order:\n%s", got)
	}
}
