package apps_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gravel-project/gravel/internal/apps"
)

// A host's own scopes (host:…): gravel checks their shape, stores them on the app, carries them in
// tokens and introspection, and never lets a member grant one.
func TestHostScopes(t *testing.T) {
	for _, ok := range []string{"host:muster", "host:muster:write", "host:muster:sessions:write", "host:a.b-c_d:e:f:g", "host:" + strings.Repeat("m", apps.MaxHostScope-len("host:"))} {
		if !apps.IsHostScope(ok) {
			t.Errorf("%q should be a host scope", ok)
		}
	}
	for _, bad := range []string{"host:", "host", "hosts:muster", "host:Muster", "host:muster:", "host::write", "host:a:b:c:d:e", "host:mus ter", "host:-x", "host:" + strings.Repeat("m", apps.MaxHostScope), "muster:write", "identity:read"} {
		if apps.IsHostScope(bad) {
			t.Errorf("%q is not a host scope", bad)
		}
	}

	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	svc, _ := newService(t, &now)
	ctx := context.Background()
	c, err := svc.Create(ctx, "htg-bot", []string{"host:muster:write", apps.ScopeIdentityRead, "host:muster:read", "host:muster:write"}, "https://muster.example/callback")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"host:muster:read", "host:muster:write", apps.ScopeIdentityRead}; !slices.Equal(c.App.Scopes, want) {
		t.Errorf("stored scopes = %v, want %v (de-duplicated, sorted)", c.App.Scopes, want)
	}
	for _, bad := range [][]string{{"host:Muster"}, {"host:"}, {"muster:write"}, {"admin"}} {
		if _, err := svc.Create(ctx, "x", bad); !errors.Is(err, apps.ErrInvalidScope) {
			t.Errorf("Create(%v): %v, want ErrInvalidScope", bad, err)
		}
	}

	// Client credentials: a host scope is issued like any other the app holds, and only those.
	tok, err := svc.Issue(ctx, c.App.ClientID, c.Secret, []string{"host:muster:write"})
	if err != nil || !slices.Equal(tok.Scopes, []string{"host:muster:write"}) {
		t.Fatalf("Issue: %+v %v", tok, err)
	}
	if _, err := svc.Issue(ctx, c.App.ClientID, c.Secret, []string{"host:muster:admin"}); !errors.Is(err, apps.ErrInvalidScope) {
		t.Errorf("a host scope the app was not granted: %v", err)
	}
	a, err := svc.Authenticate(ctx, tok.Value)
	if err != nil || !a.Has("host:muster:write") || a.Has(apps.ScopeIdentityRead) {
		t.Errorf("Authenticate: %+v %v", a, err)
	}
	// Introspection carries it, for the host's service to check.
	in, err := svc.Introspect(ctx, tok.Value)
	if err != nil || !in.Active || in.Scope != "host:muster:write" || in.Sub != "" {
		t.Errorf("introspection: %+v %v", in, err)
	}

	// A member never grants one: the authorization endpoint refuses it.
	_, err = svc.CheckAuthorization(ctx, apps.AuthorizationRequest{ResponseType: "code", ClientID: c.App.ClientID, RedirectURI: "https://muster.example/callback",
		Scope: "host:muster:write", CodeChallenge: challenge(verifier), CodeChallengeMethod: "S256"}, [16]byte{1})
	var aerr *apps.AuthorizationError
	if !errors.As(err, &aerr) || aerr.Code != "invalid_scope" {
		t.Errorf("authorize with a host scope: %v", err)
	}
}
