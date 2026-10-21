package providers_test

import (
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"testing"

	"github.com/gravel-project/gravel/internal/identity"
	"github.com/gravel-project/gravel/internal/identity/providers"
)

const connectionsPath = "/api/v10/users/@me/connections"

// xboxFake is Discord with the member's connections: body, or status with no body.
func xboxFake(t *testing.T, status int, body string) *fake {
	t.Helper()
	f := discordFake(t, discordOpts{userJSON: `{"id":"123","username":"jo","global_name":"Jo"}`})
	f.mux.HandleFunc(connectionsPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("connections: %s, Authorization %q", r.Method, r.Header.Get("Authorization"))
		}
		if status != 0 {
			w.WriteHeader(status)
			fmt.Fprint(w, `{"code":50001,"message":"Missing Access"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, body)
	})
	return f
}

// The Xbox link asks Discord for identify and connections, on its own callback.
func TestXboxBegin(t *testing.T) {
	p := providers.Xbox(clientID, clientSecret, callbackURL, newFake(t).client())
	if p.Name() != "xbox" || p.DisplayName() != "Xbox" {
		t.Errorf("names = %q, %q", p.Name(), p.DisplayName())
	}
	authURL, _, err := p.Begin(t.Context(), "st4te")
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if u.Host != "discord.com" || q.Get("scope") != "identify connections" || q.Get("redirect_uri") != callbackURL || q.Get("state") != "st4te" || q.Get("client_id") != clientID {
		t.Errorf("auth URL = %s", authURL)
	}
}

// The account is the Xbox connection's id as Discord gives it and the gamertag, proven through the
// Discord account that signed in; a hidden connection still counts (htg ADR-0004's live check).
func TestXboxComplete(t *testing.T) {
	const conns = `[{"type":"steam","id":"76561190000000001","name":"jo","verified":true,"visibility":1},
		{"type":"xbox","id":"3062899446890994","name":"Hidden Token","verified":true,"visibility":0,"revoked":false,"friend_sync":false}]`
	f := xboxFake(t, 0, conns)
	p := providers.Xbox(clientID, clientSecret, callbackURL, f.client())
	acct, err := p.Complete(t.Context(), "", url.Values{"code": {"good"}, "state": {"s"}})
	if err != nil {
		t.Fatal(err)
	}
	want := identity.Account{Provider: "xbox", Subject: "3062899446890994", DisplayName: "Hidden Token", Method: identity.MethodOAuth2,
		Via: &identity.Via{Provider: "discord", Subject: "123"}}
	if acct.Provider != want.Provider || acct.Subject != want.Subject || acct.DisplayName != want.DisplayName || acct.Method != want.Method ||
		acct.AvatarURL != "" || acct.Via == nil || *acct.Via != *want.Via {
		t.Errorf("account = %+v, via %+v", acct, acct.Via)
	}
	if served := f.served(); !slices.Equal(served, []string{"/api/oauth2/token", "/api/users/@me", connectionsPath}) {
		t.Errorf("served %v", served)
	}
}

// No Xbox connection, or only a revoked one, is no account; Discord refusing the read, or an
// unreadable answer, is a provider failure; a member who cancels at Discord is never read.
func TestXboxCompleteFailures(t *testing.T) {
	for name, c := range map[string]struct {
		status int
		body   string
		want   error
	}{
		"no xbox":      {0, `[{"type":"steam","id":"1","name":"jo"}]`, identity.ErrNoAccount},
		"none at all":  {0, `[]`, identity.ErrNoAccount},
		"revoked":      {0, `[{"type":"xbox","id":"3062899446890994","name":"Hidden Token","revoked":true}]`, identity.ErrNoAccount},
		"no id":        {0, `[{"type":"xbox","id":"","name":"Hidden Token"}]`, identity.ErrNoAccount},
		"refused":      {http.StatusForbidden, "", identity.ErrProviderFailed},
		"not json":     {0, `<html>`, identity.ErrProviderFailed},
		"not an array": {0, `{"type":"xbox"}`, identity.ErrProviderFailed},
	} {
		t.Run(name, func(t *testing.T) {
			p := providers.Xbox(clientID, clientSecret, callbackURL, xboxFake(t, c.status, c.body).client())
			_, err := p.Complete(t.Context(), "", url.Values{"code": {"good"}})
			assertFailed(t, err, c.want)
		})
	}
	f := xboxFake(t, 0, `[]`)
	_, err := providers.Xbox(clientID, clientSecret, callbackURL, f.client()).Complete(t.Context(), "", url.Values{"error": {"access_denied"}})
	assertFailed(t, err, identity.ErrProviderDenied)
	if served := f.served(); len(served) != 0 {
		t.Errorf("a cancelled link reached Discord: %v", served)
	}
}
