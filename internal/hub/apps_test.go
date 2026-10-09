package hub_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
	"github.com/gravel-project/gravel/gen/gravel/hub/v1/hubv1connect"
	"github.com/gravel-project/gravel/internal/apps"
	"github.com/gravel-project/gravel/internal/hub"
	"github.com/gravel-project/gravel/internal/store"
)

type bearerTransport struct {
	next  http.RoundTripper
	token string
}

func (bt bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+bt.token)
	return bt.next.RoundTrip(r)
}

// TestServiceCredentials registers an app the way the operator does, trades its credentials at
// the hub's token endpoint and calls the API with the token, through the real middleware chain.
func TestServiceCredentials(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(t)
	// The binary's test database is shared and the end-to-end test wants it unmigrated.
	t.Cleanup(func() {
		st, err := store.Open(context.Background(), cfg.Database.URL, 1, 10*time.Second, quiet())
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		if err := st.MigrateDownTo(context.Background(), 0); err != nil {
			t.Fatal(err)
		}
	})
	h, err := hub.New(ctx, hub.Options{Config: cfg, Logger: quiet(), Version: "test", Providers: fakeProviders()})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	public := newH2CServer(h.Handler())
	defer public.Close()

	st, err := store.Open(ctx, cfg.Database.URL, 2, 10*time.Second, quiet())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	o, err := st.GetBuiltinOrganization(ctx)
	if err != nil {
		t.Fatal(err)
	}
	created, err := apps.New(st, o.ID, time.Hour, quiet()).Create(ctx, "bot", []string{apps.ScopeIdentityRead})
	if err != nil {
		t.Fatal(err)
	}

	form := url.Values{"grant_type": {"client_credentials"}}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, public.URL+"/oauth/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(created.App.ClientID, created.Secret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	var tok struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int    `json:"expires_in"`
		Scope       string `json:"scope"`
	}
	if err := json.Unmarshal(body, &tok); err != nil || resp.StatusCode != 200 || tok.TokenType != "Bearer" || tok.AccessToken == "" || tok.ExpiresIn != 3600 || tok.Scope != "identity:read" {
		t.Fatalf("token endpoint: %d %s (%v)", resp.StatusCode, body, err)
	}

	bot := hubv1connect.NewIdentityServiceClient(&http.Client{Transport: bearerTransport{next: h2cClient().Transport, token: tok.AccessToken}}, public.URL)
	if _, err := bot.LookupUser(ctx, connect.NewRequest(&hubv1.LookupUserRequest{Provider: "discord", Subject: "nobody"})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("LookupUser with the token reaches the procedure: %v", err)
	}
	if users, err := bot.ListUsers(ctx, connect.NewRequest(&hubv1.ListUsersRequest{})); err != nil || len(users.Msg.GetUsers()) != 0 {
		t.Errorf("ListUsers on an empty hub: %v %v", err, users)
	}
	bad := hubv1connect.NewIdentityServiceClient(&http.Client{Transport: bearerTransport{next: h2cClient().Transport, token: "nope"}}, public.URL)
	if _, err := bad.LookupUser(ctx, connect.NewRequest(&hubv1.LookupUserRequest{Provider: "discord", Subject: "nobody"})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("a bad token: %v", err)
	}
	// Prune leaves a live token alone.
	h.Prune(ctx)
	if _, err := bot.ListUsers(ctx, connect.NewRequest(&hubv1.ListUsersRequest{})); err != nil {
		t.Errorf("after Prune: %v", err)
	}
}
