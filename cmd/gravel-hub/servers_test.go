package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func serversManifest(credFile, name string) string {
	return `version: 1
games:
  - id: wardogs
servers:
  - id: wd-1
    name: "` + name + `"
    game: wardogs
    driver: wardogs
    location: slc
    endpoint: http://203.0.113.10:7789
    credential_file: ` + credFile + `
    trust: official
`
}

func TestServersCheck(t *testing.T) {
	// No configuration, no database and no secret: the credential file does not exist.
	t.Setenv("GRAVEL_CONFIG", filepath.Join(t.TempDir(), "absent.yaml"))
	dir := t.TempDir()
	run := func(args ...string) (int, string, string) {
		var out, errOut bytes.Buffer
		code := run(context.Background(), args, &out, &errOut)
		return code, out.String(), errOut.String()
	}
	m := filepath.Join(dir, "servers.yaml")
	writeFile(t, m, serversManifest("/run/secrets/absent", "War Dogs #1"))
	if code, out, errOut := run("servers", "check", m); code != 0 || out != "servers valid (1 games [wardogs], 1 servers [wd-1])\n" || errOut != "" {
		t.Errorf("valid: %d %q %q", code, out, errOut)
	}
	writeFile(t, m, strings.Replace(serversManifest("rcon.txt", "War Dogs #1"), "trust: official", "trust: maybe", 1))
	if code, out, errOut := run("servers", "check", m); code != 1 || out != "" || !strings.Contains(errOut, "servers[0].credential_file") || !strings.Contains(errOut, "servers[0].trust") {
		t.Errorf("every problem named: %d %q %q", code, out, errOut)
	}
	writeFile(t, m, serversManifest("/run/secrets/x", "x")+"    extra: 1\n")
	if code, _, errOut := run("servers", "check", m); code != 1 || !strings.Contains(errOut, "field extra not found") {
		t.Errorf("an unknown key: %d %q", code, errOut)
	}
	for _, args := range [][]string{{"servers"}, {"servers", "check"}, {"servers", "check", m, m}, {"servers", "nope"}, {"servers", "apply"}} {
		if code, _, _ := run(args...); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
}

// apply, export and the dry run against a real database (the integration job).
func TestServersApplyAndExport(t *testing.T) {
	_, cfgPath, dir, run := freshHub(t)
	cred := filepath.Join(dir, "rcon")
	m := filepath.Join(dir, "servers.yaml")
	writeFile(t, m, serversManifest(cred, "War Dogs #1"))

	// Export before anything was served: an empty manifest.
	if code, out, errOut := run("servers", "export", "--config", cfgPath); code != 0 || out != "version: 1\ngames: []\nservers: []\n" {
		t.Errorf("export of a never-served hub: %d %q %q", code, out, errOut)
	}
	// The dry run of a never-served hub changes nothing and says so.
	if code, out, _ := run("servers", "apply", m, "--dry-run", "--config", cfgPath); code != 3 || !strings.Contains(out, "servers would change") {
		t.Errorf("dry run, never served: %d %q", code, out)
	}
	// The credential file must be there where apply runs.
	if code, _, errOut := run("servers", "apply", m, "--config", cfgPath); code != 1 || !strings.Contains(errOut, "credential file: server wd-1") {
		t.Errorf("missing credential: %d %q", code, errOut)
	}
	writeFile(t, cred, "secret\n")
	if code, out, errOut := run("servers", "apply", "--config", cfgPath, m); code != 0 || !strings.Contains(out, "servers applied") || !strings.Contains(out, "server wd-1 added") {
		t.Errorf("apply: %d %q %q", code, out, errOut)
	}
	if code, out, _ := run("servers", "apply", m, "--config", cfgPath); code != 0 || !strings.HasPrefix(out, "servers unchanged") {
		t.Errorf("re-apply: %d %q", code, out)
	}
	code, out, _ := run("servers", "export", "--config", cfgPath)
	if code != 0 || !strings.Contains(out, "id: wd-1") || !strings.Contains(out, "poll_interval: 15s") || !strings.Contains(out, cred) {
		t.Errorf("export: %d %q", code, out)
	}
	if strings.Contains(out, "secret") {
		t.Error("export printed the credential")
	}

	writeFile(t, m, serversManifest(cred, "Renamed"))
	if code, out, _ := run("servers", "apply", m, "--dry-run", "--config", cfgPath); code != 3 || !strings.Contains(out, "server wd-1 changed") {
		t.Errorf("dry run with a change: %d %q", code, out)
	}
	if code, out, _ := run("servers", "export", "--config", cfgPath); code != 0 || strings.Contains(out, "Renamed") {
		t.Errorf("the dry run wrote: %q", out)
	}
	// Exported, then applied: unchanged (export is a manifest apply accepts as is).
	exported := filepath.Join(dir, "exported.yaml")
	_, out, _ = run("servers", "export", "--config", cfgPath)
	writeFile(t, exported, out)
	if code, out, errOut := run("servers", "apply", exported, "--config", cfgPath); code != 0 || !strings.HasPrefix(out, "servers unchanged") {
		t.Errorf("export round trip: %d %q %q", code, out, errOut)
	}
}
