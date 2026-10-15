package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gravel-project/gravel/internal/org"
	"github.com/gravel-project/gravel/internal/store"
	"github.com/gravel-project/gravel/internal/store/storetest"
)

// A control plane that made a hub, or an operator who missed the log line, mints a claim token on
// the host (ADR-0036): on a database that was only migrated, it replaces any earlier token, it is
// recorded as an event without the token, and an owned hub refuses.
func TestClaimTokenOnTheHost(t *testing.T) {
	url := storetest.DatabaseURL(t)
	st := storetest.Open(t)
	storetest.Reset(t, st)
	t.Cleanup(func() { storetest.Reset(t, st) })
	cfg := filepath.Join(t.TempDir(), "hub.yaml")
	if err := os.WriteFile(cfg, []byte("version: 1\norganization:\n  name: Tenant 7\ndatabase:\n  url: "+url+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	run := func(args ...string) (int, string, string) {
		var out, errOut bytes.Buffer
		code := run(ctx, args, &out, &errOut)
		return code, out.String(), errOut.String()
	}
	if code, _, errOut := run("migrate", "--config", cfg); code != 0 {
		t.Fatalf("migrate: %d %s", code, errOut)
	}
	for _, args := range [][]string{{"claim-token", "extra"}, {"claim-token", "--ttl", "10s"}, {"claim-token", "--ttl", "8d"}, {"claim-token", "--ttl", "200h"}} {
		if code, _, _ := run(append(args, "--config", cfg)...); code != 2 {
			t.Errorf("%q: %d, want 2", args, code)
		}
	}

	// Never served: the organization is made with the configured name.
	code, first, errOut := run("claim-token", "--config", cfg)
	if code != 0 || len(strings.TrimSpace(first)) != 43 || strings.Count(first, "\n") != 1 || !strings.Contains(errOut, "works once") {
		t.Fatalf("first mint: %d %q %q", code, first, errOut)
	}
	code, second, _ := run("claim-token", "--ttl", "168h", "--config", cfg)
	if code != 0 || second == first {
		t.Fatalf("second mint: %d %q", code, second)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	orgSvc := org.New(st, 15*time.Minute, logger)
	o, err := orgSvc.Get(ctx)
	if err != nil || o.Name != "Tenant 7" || o.ClaimTokenExpiresAt == nil || o.ClaimTokenExpiresAt.Before(time.Now().Add(167*time.Hour)) {
		t.Fatalf("organization %+v %v", o, err)
	}
	u, err := st.RegisterUser(ctx, store.User{ID: uuid.New(), OrganizationID: o.ID, DisplayName: "grvl"},
		store.Identity{Provider: "local", Subject: "grvl", DisplayName: "grvl", LinkedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := orgSvc.Claim(ctx, strings.TrimSpace(first), store.Actor{UserID: &u.ID}); err == nil {
		t.Fatal("the replaced token claimed the hub")
	}
	if _, err := orgSvc.Claim(ctx, strings.TrimSpace(second), store.Actor{UserID: &u.ID}); err != nil {
		t.Fatalf("the minted token: %v", err)
	}

	evs, _, err := st.ListEvents(ctx, o.ID, store.EventQuery{Kinds: []string{"ownership"}, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, e := range evs {
		kinds = append(kinds, e.Kind)
		if strings.Contains(e.Summary, strings.TrimSpace(second)) || strings.Contains(e.Summary, strings.TrimSpace(first)) {
			t.Errorf("an event carries the token: %+v", e)
		}
		if e.Kind == "ownership.claim_token_minted" && e.Detail["via"] != store.ViaCLI {
			t.Errorf("minted by %v, want the host's command line", e.Detail["via"])
		}
	}
	if got, want := strings.Join(kinds, " "), "ownership.claimed ownership.claim_token_minted ownership.claim_token_minted"; got != want {
		t.Errorf("events %q, want %q", got, want)
	}

	if code, out, errOut := run("claim-token", "--config", cfg); code != 1 || out != "" || !strings.Contains(errOut, "is owned") {
		t.Errorf("owned: %d %q %q", code, out, errOut)
	}
}
