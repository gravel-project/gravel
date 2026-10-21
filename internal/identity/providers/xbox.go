package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/markbates/goth/providers/discord"
	"golang.org/x/oauth2"

	"github.com/gravel-project/gravel/internal/identity"
)

// maxConnections bounds the connections response: a member has a few dozen at most.
const maxConnections = 256 << 10

type xboxProvider struct {
	discord *discordProvider
	flow    discordFlow
}

// Xbox links a member's Xbox account through Discord's connections (gravel#217, Hidden Token
// Gaming's ADR-0004): Microsoft's own Xbox sign-in is for publishers only, and linking Xbox in
// Discord requires signing in with Microsoft, so an Xbox connection on the member's Discord
// account proves they control it. The flow is Discord OAuth2 on the hub's Discord application with
// the scopes identify and connections, its own callback (callbackURL, /auth/xbox/callback), and
// one read of the member's connections; the token is dropped.
//
// The identity's subject is the connection's id exactly as Discord gives it: Discord withholds the
// XUID under its agreement with Xbox and gives an id of its own (a base64 SHA-256 of the XUID in
// 2021, 16 digits in 2026), stable across an unlink and re-link. It is never decoded or compared
// with anything but itself. The display name is the gamertag. A revoked connection is none, and
// an unlinked one is simply gone from the list. It is link-only: nobody signs in with Xbox.
func Xbox(clientID, clientSecret, callbackURL string, client *http.Client) identity.Provider {
	client = httpClient(client)
	scopes := []string{discord.ScopeIdentify, discord.ScopeConnections}
	p := discord.New(clientID, clientSecret, callbackURL, scopes...)
	p.HTTPClient = client
	return &xboxProvider{
		discord: &discordProvider{clientID: clientID, client: client},
		flow: discordFlow{goth: p, oauth: &oauth2.Config{
			ClientID:     clientID,
			ClientSecret: clientSecret,
			RedirectURL:  callbackURL,
			Endpoint:     oauth2.Endpoint{AuthURL: discordAuthURL, TokenURL: discordTokenURL},
			Scopes:       scopes,
		}},
	}
}

func (*xboxProvider) Name() string { return "xbox" }

func (*xboxProvider) DisplayName() string { return "Xbox" }

// Begin returns Discord's authorization URL asking for identify and connections; a member sees
// Discord's consent screen the first time.
func (x *xboxProvider) Begin(_ context.Context, state string) (string, string, error) {
	return x.discord.begin(x.flow, state)
}

// Complete exchanges the code, reads who signed in and their connections, and returns their Xbox
// account, proven through that Discord account (Account.Via). No Xbox connection is
// identity.ErrNoAccount.
func (x *xboxProvider) Complete(ctx context.Context, _ string, params url.Values) (identity.Account, error) {
	who, tok, err := x.discord.complete(ctx, x.flow, params)
	if err != nil {
		return identity.Account{}, err
	}
	conns, err := x.connections(ctx, tok)
	if err != nil {
		return identity.Account{}, err
	}
	for _, c := range conns {
		if c.Type != "xbox" || c.Revoked || c.ID == "" {
			continue
		}
		return identity.Account{
			Provider:    x.Name(),
			Subject:     c.ID,
			DisplayName: c.Name,
			Method:      identity.MethodOAuth2,
			Via:         &identity.Via{Provider: who.Provider, Subject: who.Subject},
		}, nil
	}
	return identity.Account{}, fmt.Errorf("xbox: no Xbox connection on the Discord account: %w", identity.ErrNoAccount)
}

// connection is one of a Discord user's connections, as much of it as the link reads.
type connection struct {
	Type    string `json:"type"`
	ID      string `json:"id"`
	Name    string `json:"name"`
	Revoked bool   `json:"revoked"`
}

// connections reads the member's connections with the token in hand.
func (x *xboxProvider) connections(ctx context.Context, tok *oauth2.Token) ([]connection, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, discordAPI+"/users/@me/connections", nil)
	if err != nil {
		return nil, fmt.Errorf("xbox: connections: %w", err)
	}
	tok.SetAuthHeader(req)
	resp, err := x.discord.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("xbox: connections: %w: %w", identity.ErrProviderFailed, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		// Discord's error body is {"code": n, "message": "..."}; the code is what a log needs.
		var e struct {
			Code int `json:"code"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&e)
		return nil, fmt.Errorf("xbox: connections: status %d, code %d: %w", resp.StatusCode, e.Code, identity.ErrProviderFailed)
	}
	var out []connection
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxConnections)).Decode(&out); err != nil {
		return nil, fmt.Errorf("xbox: connections: %w: %w", identity.ErrProviderFailed, err)
	}
	return out, nil
}
