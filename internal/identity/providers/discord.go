package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"unicode/utf8"

	"github.com/markbates/goth/providers/discord"
	"golang.org/x/oauth2"

	"github.com/gravel-project/gravel/discord/rolemeta"
	"github.com/gravel-project/gravel/internal/identity"
)

// Discord's OAuth2 endpoints. Goth knows them too, but keeps them private, and the token
// exchange is made with our own oauth2.Config (see the package comment).
const (
	discordAuthURL  = "https://discord.com/api/oauth2/authorize"
	discordTokenURL = "https://discord.com/api/oauth2/token"
	discordAPI      = "https://discord.com/api/v10"
)

// scopeRoleConnectionsWrite lets the hub write the member's Linked Roles metadata for the
// application. Goth has no constant for it.
const scopeRoleConnectionsWrite = "role_connections.write"

// Discord's limits on a role connection's platform fields.
const (
	maxPlatformName     = 50
	maxPlatformUsername = 100
)

// discordFlow is one OAuth2 flow: login asks for identify, publishing for identify and
// role_connections.write. Both use the application's one callback.
type discordFlow struct {
	goth  *discord.Provider // the authorization URL and the profile fetch
	oauth *oauth2.Config    // the token exchange
}

type discordProvider struct {
	clientID string
	login    discordFlow
	publish  discordFlow
	client   *http.Client
}

// Discord logs a member in with Discord OAuth2. Login asks for the "identify" scope only: the
// hub needs the user id, the name and the avatar, never the email or the guilds. It is also an
// identity.Publisher: the Linked Roles flow adds "role_connections.write" and writes the
// member's metadata for this application (the client id) with the token in hand. callbackURL is
// the hub's callback as registered with the Discord application. client may be nil, and then
// every call to Discord is bounded by a default timeout.
func Discord(clientID, clientSecret, callbackURL string, client *http.Client) identity.Publisher {
	client = httpClient(client)
	flow := func(scopes ...string) discordFlow {
		p := discord.New(clientID, clientSecret, callbackURL, scopes...)
		p.HTTPClient = client
		return discordFlow{goth: p, oauth: &oauth2.Config{
			ClientID:     clientID,
			ClientSecret: clientSecret,
			RedirectURL:  callbackURL,
			Endpoint:     oauth2.Endpoint{AuthURL: discordAuthURL, TokenURL: discordTokenURL},
			Scopes:       scopes,
		}}
	}
	return &discordProvider{
		clientID: clientID,
		login:    flow(discord.ScopeIdentify),
		publish:  flow(discord.ScopeIdentify, scopeRoleConnectionsWrite),
		client:   client,
	}
}

func (*discordProvider) Name() string { return "discord" }

func (*discordProvider) DisplayName() string { return "Discord" }

// Begin returns Discord's authorization URL with state as the OAuth2 state parameter. Goth adds
// prompt=none, so a member who already granted the hub access skips the consent screen; a new
// member still sees it.
func (d *discordProvider) Begin(_ context.Context, state string) (string, string, error) {
	return d.begin(d.login, state)
}

// BeginPublish is Begin with the scope that writes the member's Linked Roles metadata; a member
// sees the consent screen the first time they grant it.
func (d *discordProvider) BeginPublish(_ context.Context, state string) (string, string, error) {
	return d.begin(d.publish, state)
}

func (d *discordProvider) begin(f discordFlow, state string) (string, string, error) {
	sess, err := f.goth.BeginAuth(state)
	if err != nil {
		return "", "", fmt.Errorf("discord: begin: %w: %w", identity.ErrProviderFailed, err)
	}
	authURL, err := sess.GetAuthURL()
	if err != nil {
		return "", "", fmt.Errorf("discord: begin: %w: %w", identity.ErrProviderFailed, err)
	}
	return authURL, sess.Marshal(), nil
}

// Complete exchanges the callback's code for a token and reads the member's profile with it.
// The session from Begin holds only the authorization URL, which the exchange does not need, so
// it is accepted and ignored. The token is dropped.
func (d *discordProvider) Complete(ctx context.Context, _ string, params url.Values) (identity.Account, error) {
	acct, _, err := d.complete(ctx, d.login, params)
	return acct, err
}

// CompletePublish is Complete for the Linked Roles flow, and returns the function that writes
// the member's role connection with the access token it obtained. The token lives in that
// closure for the rest of the request and is never stored.
func (d *discordProvider) CompletePublish(ctx context.Context, _ string, params url.Values) (identity.Account, func(context.Context, identity.Profile) error, error) {
	acct, tok, err := d.complete(ctx, d.publish, params)
	if err != nil {
		return identity.Account{}, nil, err
	}
	return acct, func(ctx context.Context, p identity.Profile) error { return d.putRoleConnection(ctx, tok, p) }, nil
}

// putRoleConnection writes the member's role connection for this application: the platform
// (the organization), their name in the hub, and the metadata values (discord/rolemeta).
func (d *discordProvider) putRoleConnection(ctx context.Context, tok *oauth2.Token, p identity.Profile) error {
	body := map[string]any{"metadata": rolemeta.Values(p.Providers, 0)}
	if p.Organization != "" {
		body["platform_name"] = truncate(p.Organization, maxPlatformName)
	}
	if p.DisplayName != "" {
		body["platform_username"] = truncate(p.DisplayName, maxPlatformUsername)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("discord: role connection: %w", err)
	}
	endpoint := discordAPI + "/users/@me/applications/" + url.PathEscape(d.clientID) + "/role-connection"
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("discord: role connection: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	tok.SetAuthHeader(req)
	resp, err := d.client.Do(req)
	if err != nil {
		return fmt.Errorf("discord: role connection: %w: %w", identity.ErrProviderFailed, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		// Discord's error body is {"code": n, "message": "..."}; the code is what a log needs.
		var e struct {
			Code int `json:"code"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&e)
		return fmt.Errorf("discord: role connection: status %d, code %d: %w", resp.StatusCode, e.Code, identity.ErrProviderFailed)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	return nil
}

// truncate cuts s to at most n bytes on a rune boundary.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func (d *discordProvider) complete(ctx context.Context, f discordFlow, params url.Values) (identity.Account, *oauth2.Token, error) {
	if code := params.Get("error"); code != "" {
		if code == "access_denied" {
			return identity.Account{}, nil, fmt.Errorf("discord: %w", identity.ErrProviderDenied)
		}
		// error_description is free text; the code is what a log needs.
		return identity.Account{}, nil, fmt.Errorf("discord: callback error %q: %w", clip(code), identity.ErrProviderFailed)
	}
	code := params.Get("code")
	if code == "" {
		return identity.Account{}, nil, fmt.Errorf("discord: callback without a code: %w", identity.ErrProviderFailed)
	}
	// Our exchange, not Goth's: it honours the context and the client.
	tok, err := f.oauth.Exchange(context.WithValue(ctx, oauth2.HTTPClient, d.client), code)
	if err != nil {
		return identity.Account{}, nil, exchangeError(err)
	}
	if !tok.Valid() {
		return identity.Account{}, nil, fmt.Errorf("discord: token exchange: unusable token: %w", identity.ErrProviderFailed)
	}
	sess := &discord.Session{AccessToken: tok.AccessToken, RefreshToken: tok.RefreshToken, ExpiresAt: tok.Expiry}
	user, err := f.goth.FetchUser(sess)
	if err != nil {
		return identity.Account{}, nil, fmt.Errorf("discord: fetch user: %w: %w", identity.ErrProviderFailed, err)
	}
	if user.UserID == "" {
		return identity.Account{}, nil, fmt.Errorf("discord: fetch user: no id in the profile: %w", identity.ErrProviderFailed)
	}
	// The global name is what Discord shows; the username is the fallback for accounts that
	// never set one. Goth does not know global_name, so it is read from the raw profile.
	name := user.Name
	if global, ok := user.RawData["global_name"].(string); ok && global != "" {
		name = global
	}
	return identity.Account{
		Provider:    d.Name(),
		Subject:     user.UserID,
		DisplayName: name,
		AvatarURL:   user.AvatarURL,
		Method:      identity.MethodOAuth2,
	}, tok, nil
}

// exchangeError keeps what a log needs from a failed token exchange, the status and the RFC
// 6749 error code, and drops the response body that oauth2 quotes whole when it cannot parse it.
func exchangeError(err error) error {
	var re *oauth2.RetrieveError
	if !errors.As(err, &re) {
		return fmt.Errorf("discord: token exchange: %w: %w", identity.ErrProviderFailed, err)
	}
	status := 0
	if re.Response != nil {
		status = re.Response.StatusCode
	}
	return fmt.Errorf("discord: token exchange: status %d, error %q: %w", status, clip(re.ErrorCode), identity.ErrProviderFailed)
}
