package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gravel-project/gravel/internal/identity"
	"github.com/gravel-project/gravel/internal/org"
	"github.com/gravel-project/gravel/internal/store"
	"github.com/gravel-project/gravel/internal/store/storetest"
)

// A hub with no provider gets its first member from the host: create-local prints a link, the
// link sets the password, the member logs in and claims the hub (gravel#187, ADR-0025's unplug
// test's first step).
func TestUsersBootstrapAnUnownedHub(t *testing.T) {
	url := storetest.DatabaseURL(t)
	st := storetest.Open(t)
	storetest.Reset(t, st)
	t.Cleanup(func() { storetest.Reset(t, st) })
	dir := t.TempDir()
	write := func(local bool) string {
		p := filepath.Join(dir, map[bool]string{true: "on.yaml", false: "off.yaml"}[local])
		body := "version: 1\norganization:\n  name: Test\ndatabase:\n  url: " + url + "\n"
		if local {
			body += "auth:\n  base_url: https://hub.example.com\n  local:\n    enabled: true\n"
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	off, on := write(false), write(true)
	ctx := context.Background()
	run := func(args ...string) (int, string, string) {
		var out, errOut bytes.Buffer
		code := run(ctx, args, &out, &errOut)
		return code, out.String(), errOut.String()
	}
	if code, _, errOut := run("migrate", "--config", on); code != 0 {
		t.Fatalf("migrate: %d %s", code, errOut)
	}
	claimToken := ensureOrganization(t, ctx, on)
	if claimToken == "" {
		t.Fatal("the hub is owned already")
	}

	if code, _, errOut := run("users"); code != 2 || !strings.Contains(errOut, "usage") {
		t.Errorf("no subcommand: %d %q", code, errOut)
	}
	if code, _, errOut := run("users", "create-local", "--username", "neckbeard", "--config", off); code != 1 || !strings.Contains(errOut, "auth.local.enabled") {
		t.Errorf("local accounts off: %d %q", code, errOut)
	}
	if code, _, errOut := run("users", "create-local", "--username", "admin", "--config", on); code != 2 || !strings.Contains(errOut, "reserved") {
		t.Errorf("reserved: %d %q", code, errOut)
	}
	code, out, errOut := run("users", "create-local", "--username", "Neckbeard", "--config", on)
	m := regexp.MustCompile(`https://hub\.example\.com/auth/local/reset/([A-Za-z0-9_-]{40,})`).FindStringSubmatch(out)
	if code != 0 || m == nil || !strings.Contains(out, "local account created: neckbeard") || !strings.Contains(out, "shown once") {
		t.Fatalf("create-local: %d %q %q", code, out, errOut)
	}
	if code, _, errOut := run("users", "create-local", "--username", "neckbeard", "--config", on); code != 1 || !strings.Contains(errOut, "already linked") {
		t.Errorf("taken: %d %q", code, errOut)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	orgSvc := org.New(st, 15*time.Minute, logger)
	o, err := orgSvc.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ids := identity.New(st, o.ID, nil, time.Minute, logger,
		identity.WithLocalAccounts(st, func(context.Context) (identity.LocalPolicy, error) { return identity.LocalPolicy{}, nil }),
		identity.WithPasswordLinks(st, nil))
	u, _, err := ids.UsePasswordLink(ctx, m[1], "an old game and a few servers")
	if err != nil {
		t.Fatal(err)
	}
	in, err := ids.LocalLogin(ctx, "neckbeard", "an old game and a few servers")
	if err != nil || in.User.ID != u.ID {
		t.Fatalf("login: %v", err)
	}
	if got, err := orgSvc.Claim(ctx, claimToken, store.Actor{UserID: &in.User.ID}); err != nil || got.OwnerUserID == nil || *got.OwnerUserID != u.ID {
		t.Fatalf("claim: %v %+v", err, got)
	}

	code, out, _ = run("users", "reset-password", "--username", "NECKBEARD", "--config", on)
	reset := regexp.MustCompile(`/auth/local/reset/([A-Za-z0-9_-]{40,})`).FindStringSubmatch(out)
	if code != 0 || reset == nil || reset[1] == m[1] || !strings.Contains(out, "password reset for: neckbeard") {
		t.Fatalf("reset-password: %d %q", code, out)
	}
	if code, _, errOut := run("users", "reset-password", "--username", "nobody", "--config", on); code != 1 || !strings.Contains(errOut, "no member has that username") {
		t.Errorf("unknown: %d %q", code, errOut)
	}
	var events []string
	evs, err := st.ListIdentityEventsAfter(ctx, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range evs {
		events = append(events, e.Event)
	}
	if strings.Join(events, ",") != "registered,password_reset_issued,password_set,login,password_reset_issued" {
		t.Errorf("events: %v", events)
	}
}
