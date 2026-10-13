package identity_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gravel-project/gravel/internal/identity"
	"github.com/gravel-project/gravel/internal/store"
)

// linkFixture is localFixture with password links, a log the test reads, and the links' results.
func linkFixture(t *testing.T) (*fixture, *bytes.Buffer, *[]string) {
	t.Helper()
	f := newFixture(t)
	logs := &bytes.Buffer{}
	var results []string
	policy := identity.LocalPolicy{}
	f.svc = identity.New(f.st, orgID, []identity.Registration{{Provider: f.discord, Login: true}}, 10*time.Minute,
		slog.New(slog.NewTextHandler(logs, nil)), identity.WithClock(func() time.Time { return *f.now }),
		identity.WithLocalAccounts(f.st, func(context.Context) (identity.LocalPolicy, error) { return policy, nil }),
		identity.WithPasswordLinks(f.st, func(r string) { results = append(results, r) }),
		identity.WithPasswordParams(cheap))
	return f, logs, &results
}

// Staff make an account; its link sets the password once, and the account then logs in.
func TestCreateLocalAndSetPassword(t *testing.T) {
	f, logs, results := linkFixture(t)
	ctx := context.Background()
	staff := uuid.New()
	if _, _, err := f.svc.CreateLocal(ctx, "x", "", &staff); !errors.Is(err, identity.ErrInvalidUsername) {
		t.Errorf("bad username: %v", err)
	}
	if _, _, err := f.svc.CreateLocal(ctx, "vps.dad", "Dad <dad@example.com>", &staff); !errors.Is(err, identity.ErrInvalidEmail) {
		t.Errorf("bad email: %v", err)
	}
	u, link, err := f.svc.CreateLocal(ctx, "VPS.Dad", "dad@example.com", &staff)
	if err != nil || u.DisplayName != "vps.dad" || link.Username != "vps.dad" || link.Purpose != store.PasswordLinkSet || len(link.Token) < 40 ||
		!link.ExpiresAt.Equal(f.now.Add(time.Hour)) || link.Path() != "/auth/local/reset/"+link.Token {
		t.Fatalf("create: %v %+v %+v", err, u, link)
	}
	if c := f.st.Locals["vps.dad"]; c.Email == nil || *c.Email != "dad@example.com" {
		t.Errorf("email: %+v", c)
	}
	if strings.Contains(logs.String(), link.Token) {
		t.Fatalf("the token was logged:\n%s", logs)
	}
	if _, _, err := f.svc.CreateLocal(ctx, "vps.dad", "", nil); !errors.Is(err, identity.ErrIdentityTaken) {
		t.Errorf("taken: %v", err)
	}
	// Nobody knows the password until the link is used.
	if _, err := f.svc.LocalLogin(ctx, "vps.dad", ""); !errors.Is(err, identity.ErrBadCredentials) {
		t.Errorf("login before the link: %v", err)
	}
	if got, err := f.svc.PasswordLinkFor(ctx, link.Token); err != nil || got.Username != "vps.dad" || got.Token != "" {
		t.Fatalf("read: %v %+v", err, got)
	}
	if _, _, err := f.svc.UsePasswordLink(ctx, link.Token, "vps.dad"); !errors.Is(err, identity.ErrWeakPassword) {
		t.Errorf("username as password: %v", err)
	}
	set, _, err := f.svc.UsePasswordLink(ctx, link.Token, "a dashboard and no cluster")
	if err != nil || set.ID != u.ID {
		t.Fatalf("use: %v %+v", err, set)
	}
	if _, _, err := f.svc.UsePasswordLink(ctx, link.Token, "a dashboard and no cluster"); !errors.Is(err, identity.ErrLinkInvalid) {
		t.Errorf("second use: %v", err)
	}
	if in, err := f.svc.LocalLogin(ctx, "vps.dad", "a dashboard and no cluster"); err != nil || in.User.ID != u.ID {
		t.Errorf("login: %v", err)
	}
	var events []string
	for _, e := range f.st.Events {
		events = append(events, e.Event)
	}
	if strings.Join(events, ",") != "registered,password_reset_issued,password_set,login" {
		t.Errorf("events: %v", events)
	}
	if strings.Join(*results, ",") != "issued,refused,used,invalid" {
		t.Errorf("results: %v", *results)
	}
}

// A reset link expires in an hour, a newer one ends it, and using one clears a lockout.
func TestPasswordResetLinks(t *testing.T) {
	f, _, _ := linkFixture(t)
	ctx := context.Background()
	if _, err := f.svc.IssueReset(ctx, "nobody", nil); !errors.Is(err, identity.ErrNoLocal) {
		t.Errorf("unknown: %v", err)
	}
	if _, err := f.svc.IssueReset(ctx, "x", nil); !errors.Is(err, identity.ErrNoLocal) {
		t.Errorf("malformed: %v", err)
	}
	_, first, err := f.svc.CreateLocal(ctx, "neckbeard", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.svc.UsePasswordLink(ctx, first.Token, "the first password"); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		_, _ = f.svc.LocalLogin(ctx, "neckbeard", "wrong")
	}
	if c := f.st.Locals["neckbeard"]; c.LockedUntil == nil {
		t.Fatal("not locked")
	}
	old, err := f.svc.IssueReset(ctx, "neckbeard", nil)
	if err != nil || old.Purpose != store.PasswordLinkReset {
		t.Fatal(err)
	}
	fresh, err := f.svc.IssueReset(ctx, "NECKBEARD", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.PasswordLinkFor(ctx, old.Token); !errors.Is(err, identity.ErrLinkInvalid) {
		t.Errorf("a newer link left the old one working: %v", err)
	}
	*f.now = f.now.Add(61 * time.Minute)
	if _, _, err := f.svc.UsePasswordLink(ctx, fresh.Token, "the second password"); !errors.Is(err, identity.ErrLinkInvalid) {
		t.Errorf("expired: %v", err)
	}
	again, err := f.svc.IssueReset(ctx, "neckbeard", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.svc.UsePasswordLink(ctx, again.Token, "the second password"); err != nil {
		t.Fatal(err)
	}
	if c := f.st.Locals["neckbeard"]; c.LockedUntil != nil || c.Lockouts != 0 {
		t.Errorf("the reset left the lockout: %+v", c)
	}
	if _, err := f.svc.LocalLogin(ctx, "neckbeard", "the second password"); err != nil {
		t.Errorf("login with the new password: %v", err)
	}
	if _, err := f.svc.PasswordLinkFor(ctx, ""); !errors.Is(err, identity.ErrLinkInvalid) {
		t.Errorf("empty token: %v", err)
	}
	*f.now = f.now.Add(31 * 24 * time.Hour)
	if _, err := f.svc.Prune(ctx); err != nil || len(f.st.Links) != 0 {
		t.Errorf("prune: %v %d left", err, len(f.st.Links))
	}
}

// Without local accounts there are no links.
func TestPasswordLinksNeedLocalAccounts(t *testing.T) {
	f := newFixture(t)
	if _, _, err := f.svc.CreateLocal(context.Background(), "neckbeard", "", nil); !errors.Is(err, identity.ErrLocalDisabled) {
		t.Errorf("create: %v", err)
	}
	if _, err := f.svc.IssueReset(context.Background(), "neckbeard", nil); !errors.Is(err, identity.ErrLocalDisabled) {
		t.Errorf("reset: %v", err)
	}
}
