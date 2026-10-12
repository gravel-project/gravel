package cs2_test

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
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
	cmd := exec.CommandContext(t.Context(), "bash", "-c", entry, "entry", dir)
	cmd.Env = hookEnv(nil)
	out, err := cmd.CombinedOutput()
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

// hookEnv is the test's environment without anything of the host's that the hook reads or that
// would send the fetches somewhere else, plus extra.
func hookEnv(extra []string) []string {
	var env []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if name == "STEAMAPPDIR" || strings.HasPrefix(name, "GRAVEL_") || strings.HasSuffix(strings.ToLower(name), "_proxy") {
			continue
		}
		env = append(env, kv)
	}
	return append(env, extra...)
}

// gameinfo is CS2's game/csgo/gameinfo.gi in the shape SteamCMD leaves it at every start, cut to
// the parts around its search paths.
var gameinfo = strings.Join([]string{
	`"GameInfo"`,
	`{`,
	"\tgame\t\t\"Counter-Strike 2\"",
	"\ttitle\t\t\"Counter-Strike 2\"",
	"\ttype\t\tmultiplayer_only",
	"\tFileSystem",
	"\t{",
	"\t\tSearchPaths",
	"\t\t{",
	"\t\t\tGame_LowViolence\tcsgo_lv // Perfect World content override",
	"\t\t\tGame\tcsgo",
	"\t\t\tGame\tcsgo_imported",
	"\t\t\tGame\tcsgo_core",
	"\t\t\tGame\tcore",
	"",
	"\t\t\tMod\tcsgo",
	"\t\t\tMod\tcsgo_imported",
	"\t\t\tMod\tcsgo_core",
	"",
	"\t\t\tAddonRoot_LowViolence\tcsgo_addons_lv",
	"\t\t\tAddonRoot\tcsgo_addons",
	"",
	"\t\t\tWrite\tcsgo",
	"\t\t\tWrite\tcsgo_core",
	"\t\t}",
	"\t\t\"UserSettingsPathID\"\t\"USRLOCAL\"",
	"\t}",
	"}",
	"",
}, "\n")

// metamodLine is the search path Metamod:Source's documentation adds, indented as the line it
// follows.
const metamodLine = "\t\t\tGame\tcsgo/addons/metamod"

// shippedConfig is the part of the cfg/MatchZy/config.cfg MatchZy ships that the test looks at:
// keys the hook replaces and one it must keep.
const shippedConfig = `// MatchZy's own configuration.
matchzy_chat_prefix "[{Green}MatchZy{Default}]"
matchzy_remote_log_url ""
matchzy_remote_log_header_key ""
matchzy_remote_log_header_value ""
matchzy_kick_when_no_match_loaded true
`

// The secrets the fake server prints and the environment carries; none may reach the output.
const (
	testToken  = "tok-5ecret-matchzy"
	testRCON   = "R0nPass"
	testGSLT   = "0123456789ABCDEF0123456789ABCDEF"
	testJoinPW = "joinpw"
)

// pluginHost is a CS2 install after SteamCMD's update, the hook in it, and a web server holding a
// pinned Metamod and MatchZy that counts each fetch.
type pluginHost struct {
	dir   string
	files map[string][]byte
	url   string

	mu   sync.Mutex
	hits map[string]int
}

func newPluginHost(t *testing.T) *pluginHost {
	t.Helper()
	for _, tool := range []string{"bash", "tar", "unzip", "sha256sum", "sed", "grep"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip("no " + tool)
		}
	}
	_, wget := exec.LookPath("wget")
	_, curl := exec.LookPath("curl")
	if wget != nil && curl != nil {
		t.Skip("neither wget nor curl")
	}
	h := &pluginHost{dir: t.TempDir(), hits: map[string]int{}, files: map[string][]byte{
		"/mmsource-2.0.0-git1476-linux.tar.gz": tarGz(t, map[string]string{
			"addons/metamod.vdf":                                 "\"Plugin\"\n{\n\t\"file\"\t\"addons/metamod/bin/linuxsteamrt64/metamod\"\n}\n",
			"addons/metamod/bin/linuxsteamrt64/libserver.so":     "metamod",
			"addons/metamod/bin/linuxsteamrt64/metamod.2.cs2.so": "metamod",
		}),
		"/MatchZy-0.9.1-with-cssharp-linux.zip": zipped(t, map[string]string{
			"addons/counterstrikesharp/plugins/MatchZy/MatchZy.dll": "matchzy",
			"addons/metamod/counterstrikesharp.vdf":                 "\"Metamod Plugin\"\n{\n}\n",
			"cfg/MatchZy/config.cfg":                                shippedConfig,
		}),
	}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.hits[r.URL.Path]++
		h.mu.Unlock()
		b, ok := h.files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(b)
	}))
	t.Cleanup(srv.Close)
	h.url = srv.URL
	h.write(t, "game/csgo/gameinfo.gi", gameinfo)
	h.write(t, "pre.sh", preHook(t))
	return h
}

func (h *pluginHost) write(t *testing.T, rel, content string) {
	t.Helper()
	p := filepath.Join(h.dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (h *pluginHost) read(t *testing.T, rel string) string {
	t.Helper()
	return read(t, filepath.Join(h.dir, rel))
}

func (h *pluginHost) fetches(path string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.hits[path]
}

// pin is one GRAVEL_PLUGIN_FILES entry for a file the server holds.
func (h *pluginHost) pin(path, kind string) string {
	sum := sha256.Sum256(h.files[path])
	return h.url + path + "|" + hex.EncodeToString(sum[:]) + "|" + kind + "|."
}

// matchzyEnv is what the hub writes for a server with the matchzy plugin set.
func (h *pluginHost) matchzyEnv() []string {
	return []string{
		"GRAVEL_PLUGINS=matchzy",
		"GRAVEL_PLUGIN_FILES=" + h.pin("/mmsource-2.0.0-git1476-linux.tar.gz", "tar.gz") + " " + h.pin("/MatchZy-0.9.1-with-cssharp-linux.zip", "zip"),
		"GRAVEL_MATCHZY_EVENTS_URL=https://hub.example/ingest/matchzy/srv-1",
		"GRAVEL_MATCHZY_DEMO_URL=https://hub.example/demos/srv-1",
		"GRAVEL_MATCHZY_TOKEN=" + testToken,
	}
}

// start does what entry.sh does around the hook: it sources pre.sh in game/ with the environment
// file's variables, then runs a fake server that prints its command line and the MatchZy header as
// CS2 would. It returns the output, after checking the server started and printed no secret.
func (h *pluginHost) start(t *testing.T, env []string) string {
	t.Helper()
	entry := `set -u
cd "$STEAMAPPDIR/game"
source "$STEAMAPPDIR/pre.sh"
echo "Command line: ./cs2 -dedicated +map de_inferno +rcon_password ` + testRCON + ` +sv_setsteamaccount ` + testGSLT + ` +sv_password ` + testJoinPW + `"
echo 'matchzy_remote_log_header_value "Bearer ` + testToken + `"'
echo "the server starts"
`
	cmd := exec.CommandContext(t.Context(), "bash", "-c", entry)
	cmd.Env = hookEnv(append([]string{"STEAMAPPDIR=" + h.dir}, env...))
	out, err := cmd.CombinedOutput()
	got := string(out)
	if err != nil {
		t.Fatalf("%v: %s", err, got)
	}
	if !strings.Contains(got, "the server starts") {
		t.Fatalf("the server did not start:\n%s", got)
	}
	for _, secret := range []string{testToken, testRCON, testGSLT, testJoinPW} {
		if strings.Contains(got, secret) {
			t.Errorf("%q reached the output:\n%s", secret, got)
		}
	}
	return got
}

// metamodLines counts Metamod's search path in gameinfo.gi.
func (h *pluginHost) metamodLines(t *testing.T) int {
	t.Helper()
	return strings.Count(h.read(t, "game/csgo/gameinfo.gi"), "csgo/addons/metamod")
}

func (h *pluginHost) status(t *testing.T) string {
	t.Helper()
	return h.read(t, ".gravel/plugins.status")
}

// #176: with the matchzy plugin set the hook fetches and checks the pinned files, unpacks them,
// writes MatchZy's config from the environment, and puts Metamod's search path right after
// Game_LowViolence. A second start changes nothing and fetches nothing.
func TestPreHookInstallsThePinnedPlugins(t *testing.T) {
	h := newPluginHost(t)
	out := h.start(t, h.matchzyEnv())

	if got := h.status(t); got != "ok matchzy\n" {
		t.Fatalf("status %q:\n%s", got, out)
	}
	want := strings.Replace(gameinfo, "csgo_lv // Perfect World content override\n", "csgo_lv // Perfect World content override\n"+metamodLine+"\n", 1)
	if got := h.read(t, "game/csgo/gameinfo.gi"); got != want {
		t.Errorf("gameinfo.gi:\n%s\nwant:\n%s", got, want)
	}
	for _, f := range []string{"addons/metamod.vdf", "addons/metamod/bin/linuxsteamrt64/metamod.2.cs2.so",
		"addons/counterstrikesharp/plugins/MatchZy/MatchZy.dll", "addons/metamod/counterstrikesharp.vdf"} {
		if _, err := os.Stat(filepath.Join(h.dir, "game/csgo", f)); err != nil {
			t.Errorf("%s was not unpacked: %v", f, err)
		}
	}
	cfg := h.read(t, "game/csgo/cfg/MatchZy/config.cfg")
	for _, line := range []string{
		`matchzy_chat_prefix "[{Green}MatchZy{Default}]"`,
		`matchzy_remote_log_url "https://hub.example/ingest/matchzy/srv-1"`,
		`matchzy_remote_log_header_key "Authorization"`,
		`matchzy_remote_log_header_value "Bearer ` + testToken + `"`,
		`matchzy_demo_upload_url "https://hub.example/demos/srv-1"`,
		`matchzy_demo_upload_header_key "Authorization"`,
		`matchzy_demo_upload_header_value "Bearer ` + testToken + `"`,
		`matchzy_autostart_mode 1`,
		`matchzy_kick_when_no_match_loaded false`,
	} {
		if n := strings.Count(cfg, line+"\n"); n != 1 {
			t.Errorf("config.cfg holds %q %d times:\n%s", line, n, cfg)
		}
	}
	for _, gone := range []string{`matchzy_remote_log_url ""`, "matchzy_kick_when_no_match_loaded true"} {
		if strings.Contains(cfg, gone) {
			t.Errorf("config.cfg still holds %q:\n%s", gone, cfg)
		}
	}
	for _, want := range []string{"plugin set matchzy asked for", "matches its pin", "unpacked MatchZy-0.9.1-with-cssharp-linux.zip",
		"wrote MatchZy's config: match events on, demo upload on, with a token", "added Metamod's search path", "status: ok matchzy",
		"+rcon_password [redacted] +sv_setsteamaccount [redacted] +sv_password [redacted]", `"Bearer [redacted]"`} {
		if !strings.Contains(out, want) {
			t.Errorf("the output lacks %q:\n%s", want, out)
		}
	}

	// SteamCMD leaves gameinfo.gi as it was when nothing changed upstream; either way the second
	// start ends where the first did.
	out = h.start(t, h.matchzyEnv())
	if got := h.read(t, "game/csgo/gameinfo.gi"); got != want {
		t.Errorf("gameinfo.gi after a second start:\n%s", got)
	}
	if got := h.read(t, "game/csgo/cfg/MatchZy/config.cfg"); got != cfg {
		t.Errorf("config.cfg after a second start:\n%s\nwant:\n%s", got, cfg)
	}
	for _, p := range []string{"/mmsource-2.0.0-git1476-linux.tar.gz", "/MatchZy-0.9.1-with-cssharp-linux.zip"} {
		if n := h.fetches(p); n != 1 {
			t.Errorf("%s fetched %d times", p, n)
		}
	}
	for _, want := range []string{"MatchZy-0.9.1-with-cssharp-linux.zip is cached", "already unpacked", "status: ok matchzy"} {
		if !strings.Contains(out, want) {
			t.Errorf("the second start's output lacks %q:\n%s", want, out)
		}
	}

	// SteamCMD's update rewrites gameinfo.gi; the next start puts the line back.
	h.write(t, "game/csgo/gameinfo.gi", gameinfo)
	h.start(t, h.matchzyEnv())
	if got := h.read(t, "game/csgo/gameinfo.gi"); got != want {
		t.Errorf("gameinfo.gi after an update:\n%s", got)
	}

	// An edit on the volume to a line MatchZy ships survives a restart; the hook's keys do not.
	h.write(t, "game/csgo/cfg/MatchZy/config.cfg", strings.Replace(cfg, "[{Green}MatchZy{Default}]", "[HTG]", 1)+"matchzy_autostart_mode 0\n")
	h.start(t, h.matchzyEnv())
	cfg = h.read(t, "game/csgo/cfg/MatchZy/config.cfg")
	if !strings.Contains(cfg, `matchzy_chat_prefix "[HTG]"`) || strings.Contains(cfg, "matchzy_autostart_mode 0") || strings.Count(cfg, "matchzy_autostart_mode 1\n") != 1 {
		t.Errorf("config.cfg after an edit:\n%s", cfg)
	}
}

// #176: without a demo URL or a token, their keys are left out, and an earlier start's are gone.
func TestPreHookConfigLeavesOutUnsetKeys(t *testing.T) {
	h := newPluginHost(t)
	h.start(t, h.matchzyEnv())
	env := h.matchzyEnv()[:3]
	out := h.start(t, env)
	cfg := h.read(t, "game/csgo/cfg/MatchZy/config.cfg")
	if strings.Contains(cfg, "matchzy_demo_upload") || strings.Contains(cfg, "header") || !strings.Contains(cfg, `matchzy_remote_log_url "https://hub.example/ingest/matchzy/srv-1"`) {
		t.Errorf("config.cfg:\n%s", cfg)
	}
	if !strings.Contains(out, "match events on, demo upload off, without a token") {
		t.Errorf("output:\n%s", out)
	}
}

// #176: with no plugins asked for, the server runs vanilla: Metamod's search path is removed and
// the plugins stay on disk, inert.
func TestPreHookRunsVanilla(t *testing.T) {
	h := newPluginHost(t)
	out := h.start(t, nil)
	if got := h.status(t); got != "vanilla off\n" || h.metamodLines(t) != 0 {
		t.Fatalf("status %q:\n%s", got, out)
	}
	if got := h.read(t, "game/csgo/gameinfo.gi"); got != gameinfo {
		t.Errorf("a vanilla start changed gameinfo.gi:\n%s", got)
	}

	h.start(t, h.matchzyEnv())
	if h.metamodLines(t) != 1 {
		t.Fatal("matchzy did not install")
	}
	out = h.start(t, []string{"GRAVEL_PLUGINS="})
	if got := h.status(t); got != "vanilla off\n" {
		t.Errorf("status %q:\n%s", got, out)
	}
	if got := h.read(t, "game/csgo/gameinfo.gi"); got != gameinfo {
		t.Errorf("gameinfo.gi after plugins went off:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(h.dir, "game/csgo/addons/metamod.vdf")); err != nil {
		t.Errorf("the plugins were removed from disk: %v", err)
	}
	if !strings.Contains(out, "removed Metamod's search path") {
		t.Errorf("output:\n%s", out)
	}
}

// #176: any step failing prints why, takes Metamod's search path out, records the reason and
// still starts the server.
func TestPreHookFallsBackToVanilla(t *testing.T) {
	sumOf := func(s string) string { b := sha256.Sum256([]byte(s)); return hex.EncodeToString(b[:]) }
	for _, tc := range []struct {
		name   string
		env    func(t *testing.T, h *pluginHost) []string
		reason string
	}{
		{"checksum mismatch", func(t *testing.T, h *pluginHost) []string {
			env := h.matchzyEnv()
			env[1] = "GRAVEL_PLUGIN_FILES=" + h.pin("/mmsource-2.0.0-git1476-linux.tar.gz", "tar.gz") + " " + h.url + "/MatchZy-0.9.1-with-cssharp-linux.zip|" + sumOf("other") + "|zip|."
			return env
		}, "MatchZy-0.9.1-with-cssharp-linux.zip has sha256 "},
		{"missing file", func(t *testing.T, h *pluginHost) []string {
			env := h.matchzyEnv()
			env[1] = "GRAVEL_PLUGIN_FILES=" + h.url + "/gone.zip|" + sumOf("gone") + "|zip|."
			return env
		}, "fetching gone.zip failed"},
		{"no pins", func(t *testing.T, h *pluginHost) []string { return append(h.matchzyEnv(), "GRAVEL_PLUGIN_FILES=") }, "GRAVEL_PLUGIN_FILES pins no files"},
		{"bad entry", func(t *testing.T, h *pluginHost) []string {
			return append(h.matchzyEnv(), "GRAVEL_PLUGIN_FILES="+h.url+"/x.zip|"+sumOf("x")+"|rar|.")
		}, "holds an entry that is not"},
		{"escaping dest", func(t *testing.T, h *pluginHost) []string {
			return append(h.matchzyEnv(), "GRAVEL_PLUGIN_FILES="+h.url+"/x.zip|"+sumOf("x")+"|zip|../..")
		}, "holds an entry that is not"},
		{"unknown set", func(t *testing.T, h *pluginHost) []string { return append(h.matchzyEnv(), "GRAVEL_PLUGINS=sourcemod") }, "GRAVEL_PLUGINS names a plugin set"},
		{"quote in the token", func(t *testing.T, h *pluginHost) []string {
			return append(h.matchzyEnv(), `GRAVEL_MATCHZY_TOKEN=`+testToken+`" ; quit`)
		}, "GRAVEL_MATCHZY_TOKEN holds a quote, semicolon or line break"},
		{"semicolon in a url", func(t *testing.T, h *pluginHost) []string {
			return append(h.matchzyEnv(), "GRAVEL_MATCHZY_EVENTS_URL=https://hub.example/x;quit")
		}, "GRAVEL_MATCHZY_EVENTS_URL holds a quote"},
		{"line break in a url", func(t *testing.T, h *pluginHost) []string {
			return append(h.matchzyEnv(), "GRAVEL_MATCHZY_DEMO_URL=https://hub.example/x\nquit")
		}, "GRAVEL_MATCHZY_DEMO_URL holds a quote"},
		{"no search paths", func(t *testing.T, h *pluginHost) []string {
			h.write(t, "game/csgo/gameinfo.gi", strings.Replace(gameinfo, "Game_LowViolence", "Game_Other", 1))
			return h.matchzyEnv()
		}, "no single Game_LowViolence line"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newPluginHost(t)
			h.start(t, h.matchzyEnv())
			if h.metamodLines(t) != 1 {
				t.Fatal("matchzy did not install")
			}
			cfg := h.read(t, "game/csgo/cfg/MatchZy/config.cfg")
			env := tc.env(t, h)
			out := h.start(t, env)
			got := h.status(t)
			if !strings.HasPrefix(got, "vanilla ") || !strings.Contains(got, tc.reason) || strings.Count(got, "\n") != 1 {
				t.Errorf("status %q, want vanilla ...%s...:\n%s", got, tc.reason, out)
			}
			if h.metamodLines(t) != 0 {
				t.Errorf("Metamod's search path is still in gameinfo.gi:\n%s", h.read(t, "game/csgo/gameinfo.gi"))
			}
			if !strings.Contains(out, tc.reason) || !strings.Contains(out, "so the server starts vanilla") {
				t.Errorf("the output does not say why:\n%s", out)
			}
			if strings.Contains(tc.name, "token") || strings.Contains(tc.name, "url") {
				if now := h.read(t, "game/csgo/cfg/MatchZy/config.cfg"); now != cfg {
					t.Errorf("a refused value reached config.cfg:\n%s", now)
				}
			}
			if strings.HasPrefix(tc.name, "checksum") {
				if left, _ := filepath.Glob(filepath.Join(h.dir, ".gravel/plugins/*.part")); len(left) != 0 {
					t.Errorf("a refused download was kept: %v", left)
				}
			}
		})
	}
}

// A cached file that no longer matches its pin is fetched again.
func TestPreHookRefetchesACorruptCache(t *testing.T) {
	h := newPluginHost(t)
	h.start(t, h.matchzyEnv())
	pin := h.pin("/MatchZy-0.9.1-with-cssharp-linux.zip", "zip")
	sum := strings.Split(pin, "|")[1]
	h.write(t, ".gravel/plugins/"+sum, "corrupt")
	out := h.start(t, h.matchzyEnv())
	if got := h.status(t); got != "ok matchzy\n" {
		t.Fatalf("status %q:\n%s", got, out)
	}
	if n := h.fetches("/MatchZy-0.9.1-with-cssharp-linux.zip"); n != 2 || !strings.Contains(out, "does not match its pin, so it is fetched again") {
		t.Errorf("fetched %d times:\n%s", n, out)
	}
}

// The hook fetches with curl on a host without wget, and starts vanilla, saying so, with neither.
func TestPreHookFetchTools(t *testing.T) {
	h := newPluginHost(t)
	tools := []string{"bash", "sed", "grep", "mkdir", "mv", "rm", "sha256sum", "tar", "gzip", "unzip"}
	bin := func(extra ...string) string {
		dir := t.TempDir()
		for _, tool := range append(tools, extra...) {
			p, err := exec.LookPath(tool)
			if err != nil {
				t.Skip("no " + tool)
			}
			if err := os.Symlink(p, filepath.Join(dir, tool)); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}

	out := h.start(t, append(h.matchzyEnv(), "PATH="+bin()))
	if got := h.status(t); got != "vanilla fetching mmsource-2.0.0-git1476-linux.tar.gz failed\n" || !strings.Contains(out, "neither wget nor curl is here") {
		t.Errorf("status %q:\n%s", got, out)
	}

	out = h.start(t, append(h.matchzyEnv(), "PATH="+bin("curl")))
	if got := h.status(t); got != "ok matchzy\n" {
		t.Errorf("status %q:\n%s", got, out)
	}
}

func tarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func zipped(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
