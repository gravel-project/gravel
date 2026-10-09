package providers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"github.com/markbates/goth/providers/discord"
	"golang.org/x/oauth2"

	"github.com/gravel-project/gravel/internal/identity"
)

// Discord's OAuth2 endpoints. Goth knows them too, but keeps them private, and the token
// exchange is made with our own oauth2.Config (see the package comment).
const (
	discordAuthURL  = "https://discord.com/api/oauth2/authorize"
	discordTokenURL = "https://discord.com/api/oauth2/token"
)

type discordProvider struct {
	goth   *discord.Provider // the authorization URL and the profile fetch
	oauth  *oauth2.Config    // the token exchange
	client *http.Client
}

// Discord logs a member in with Discord OAuth2. It asks for the "identify" scope only: the hub
// needs the user id, the name and the avatar, never the email or the guilds. callbackURL is the
// hub's callback as registered with the Discord application. client may be nil, and then every
// call to Discord is bounded by a default timeout.
func Discord(clientID, clientSecret, callbackURL string, client *http.Client) identity.Provider {
	client = httpClient(client)
	p := discord.New(clientID, clientSecret, callbackURL, discord.ScopeIdentify)
	p.HTTPClient = client
	return &discordProvider{
		goth: p,
		oauth: &oauth2.Config{
			ClientID:     clientID,
			ClientSecret: clientSecret,
			RedirectURL:  callbackURL,
			Endpoint:     oauth2.Endpoint{AuthURL: discordAuthURL, TokenURL: discordTokenURL},
			Scopes:       []string{discord.ScopeIdentify},
		},
		client: client,
	}
}

func (*discordProvider) Name() string { return "discord" }

func (*discordProvider) DisplayName() string { return "Discord" }

// Begin returns Discord's authorization URL with state as the OAuth2 state parameter. Goth adds
// prompt=none, so a member who already granted the hub access skips the consent screen; a new
// member still sees it.
func (d *discordProvider) Begin(_ context.Context, state string) (string, string, error) {
	sess, err := d.goth.BeginAuth(state)
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
// it is accepted and ignored.
func (d *discordProvider) Complete(ctx context.Context, _ string, params url.Values) (identity.Account, error) {
	if code := params.Get("error"); code != "" {
		if code == "access_denied" {
			return identity.Account{}, fmt.Errorf("discord: %w", identity.ErrProviderDenied)
		}
		// error_description is free text; the code is what a log needs.
		return identity.Account{}, fmt.Errorf("discord: callback error %q: %w", clip(code), identity.ErrProviderFailed)
	}
	code := params.Get("code")
	if code == "" {
		return identity.Account{}, fmt.Errorf("discord: callback without a code: %w", identity.ErrProviderFailed)
	}
	// Our exchange, not Goth's: it honours the context and the client.
	tok, err := d.oauth.Exchange(context.WithValue(ctx, oauth2.HTTPClient, d.client), code)
	if err != nil {
		return identity.Account{}, exchangeError(err)
	}
	if !tok.Valid() {
		return identity.Account{}, fmt.Errorf("discord: token exchange: unusable token: %w", identity.ErrProviderFailed)
	}
	sess := &discord.Session{AccessToken: tok.AccessToken, RefreshToken: tok.RefreshToken, ExpiresAt: tok.Expiry}
	user, err := d.goth.FetchUser(sess)
	if err != nil {
		return identity.Account{}, fmt.Errorf("discord: fetch user: %w: %w", identity.ErrProviderFailed, err)
	}
	if user.UserID == "" {
		return identity.Account{}, fmt.Errorf("discord: fetch user: no id in the profile: %w", identity.ErrProviderFailed)
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
	}, nil
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
