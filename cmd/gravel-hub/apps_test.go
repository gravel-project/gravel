package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/gravel-project/gravel/internal/store/storetest"
)

func TestAppsCreateListRevoke(t *testing.T) {
	url := storetest.DatabaseURL(t)
	// The binary's test database is shared; start from an empty schema and leave one behind.
	st := storetest.Open(t)
	storetest.Reset(t, st)
	t.Cleanup(func() { storetest.Reset(t, st) })
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "hub.yaml")
	if err := os.WriteFile(cfgPath, []byte("version: 1\norganization:\n  name: Test\ndatabase:\n  url: "+url+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	run := func(args ...string) (int, string, string) {
		var out, errOut bytes.Buffer
		code := run(ctx, args, &out, &errOut)
		return code, out.String(), errOut.String()
	}
	if code, _, errOut := run("migrate", "--config", cfgPath); code != 0 {
		t.Fatalf("migrate: %d %s", code, errOut)
	}
	ensureOrganization(t, ctx, cfgPath)

	if code, _, errOut := run("apps"); code != 2 || !strings.Contains(errOut, "usage") {
		t.Errorf("no subcommand: %d %q", code, errOut)
	}
	if code, out, _ := run("apps", "list", "--config", cfgPath); code != 0 || !strings.Contains(out, "no apps registered") {
		t.Errorf("empty list: %d %q", code, out)
	}
	if code, _, errOut := run("apps", "create", "--name", "bot", "--config", cfgPath); code != 2 || !strings.Contains(errOut, "usage") {
		t.Errorf("create without scopes: %d %q", code, errOut)
	}
	if code, _, errOut := run("apps", "create", "--name", "bot", "--scopes", "admin", "--config", cfgPath); code != 2 || !strings.Contains(errOut, "not a scope") {
		t.Errorf("create with a bad scope: %d %q", code, errOut)
	}
	code, out, errOut := run("apps", "create", "--name", "htg-bot", "--scopes", "identity:read", "--config", cfgPath)
	if code != 0 {
		t.Fatalf("create: %d %q %q", code, out, errOut)
	}
	clientID := regexp.MustCompile(`client_id: (gravel_[0-9a-f]{24})`).FindStringSubmatch(out)
	secret := regexp.MustCompile(`client_secret: ([A-Za-z0-9_-]{40,})`).FindStringSubmatch(out)
	if clientID == nil || secret == nil || !strings.Contains(out, "scopes: identity:read") || !strings.Contains(out, "shown once") {
		t.Fatalf("create output: %q", out)
	}
	code, out, _ = run("apps", "list", "--config", cfgPath)
	if code != 0 || !strings.Contains(out, clientID[1]) || !strings.Contains(out, "htg-bot") || !strings.Contains(out, "identity:read") || strings.Contains(out, secret[1]) {
		t.Errorf("list: %d %q", code, out)
	}
	if code, _, errOut := run("apps", "revoke", "--config", cfgPath); code != 2 || !strings.Contains(errOut, "usage") {
		t.Errorf("revoke without an id: %d %q", code, errOut)
	}
	if code, out, errOut := run("apps", "revoke", "--client-id", clientID[1], "--config", cfgPath); code != 0 || !strings.Contains(out, "app revoked") {
		t.Errorf("revoke: %d %q %q", code, out, errOut)
	}
	if code, _, errOut := run("apps", "revoke", "--client-id", clientID[1], "--config", cfgPath); code != 1 || !strings.Contains(errOut, "already revoked") {
		t.Errorf("revoke twice: %d %q", code, errOut)
	}
	code, out, _ = run("apps", "list", "--config", cfgPath)
	if code != 0 || strings.Contains(strings.Split(out, "\n")[1], "\t-") {
		t.Errorf("list after revoke shows a date: %d %q", code, out)
	}
}
