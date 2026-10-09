package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/markbates/goth"
	"github.com/markbates/goth/providers/steam"

	"github.com/gravel-project/gravel/internal/identity"
)

// steamSummariesURL turns a SteamID64 into a persona name and an avatar. HTTPS, unlike Goth's,
// because the key rides in the query.
const steamSummariesURL = "https://api.steampowered.com/ISteamUser/GetPlayerSummaries/v0002/"

// maxSummaryBody bounds what is read of a player-summary response; a real one is a few hundred
// bytes.
const maxSummaryBody = 1 << 20

type steamProvider struct {
	apiKey      string
	callbackURL string
	client      *http.Client
}

// Steam proves a SteamID64 over Steam's OpenID 2.0. apiKey is a Steam Web API key, used only to
// read the persona name and the avatar once the id is proven; it may be empty, and then the
// account carries neither. callbackURL is the hub's callback without a query. client may be
// nil, and then every call to Steam is bounded by a default timeout.
func Steam(apiKey, callbackURL string, client *http.Client) identity.Provider {
	return &steamProvider{apiKey: apiKey, callbackURL: callbackURL, client: httpClient(client)}
}

func (*steamProvider) Name() string { return "steam" }

func (*steamProvider) DisplayName() string { return "Steam" }

// Begin returns Steam's OpenID login URL. OpenID 2.0 has no state parameter, so the state rides
// in the return_to URL: Steam signs it and echoes it to the callback, and Goth's verification
// requires the echo to equal what Begin sent, so a callback whose state was swapped fails before
// the caller even compares it.
func (s *steamProvider) Begin(_ context.Context, state string) (string, string, error) {
	p := s.goth(s.callbackURL + "?state=" + url.QueryEscape(state))
	sess, err := p.BeginAuth("")
	if err != nil {
		return "", "", fmt.Errorf("steam: begin: %w: %w", identity.ErrProviderFailed, err)
	}
	authURL, err := sess.GetAuthURL()
	if err != nil {
		return "", "", fmt.Errorf("steam: begin: %w: %w", identity.ErrProviderFailed, err)
	}
	return authURL, sess.Marshal(), nil
}

// goth builds a Goth Steam provider for one call. One per call because Goth keys the return_to
// check on the provider's callback URL, which carries the state and so differs per attempt.
func (s *steamProvider) goth(callbackURL string) *steam.Provider {
	p := steam.New(s.apiKey, callbackURL)
	p.HTTPClient = s.client
	return p
}

// Complete verifies the OpenID assertion with Steam and, when the provider has an API key, reads
// the profile. The assertion proves the SteamID; failing the login when the profile fetch fails
// is deliberate, because the configuration asked for the profile and a silent blank would hide a
// bad key.
func (s *steamProvider) Complete(ctx context.Context, session string, params url.Values) (identity.Account, error) {
	// The callback URL to compare against comes from the session, not from the provider.
	p := s.goth("")
	sess, err := p.UnmarshalSession(session)
	if err != nil {
		return identity.Account{}, fmt.Errorf("steam: session: %w: %w", identity.ErrProviderFailed, err)
	}
	// Goth checks the mode, the echoed return_to, the signature (check_authentication at Steam,
	// through the client) and the claimed_id pattern.
	if err := authorize(sess, p, params); err != nil {
		return identity.Account{}, fmt.Errorf("steam: verify: %w: %w", identity.ErrProviderFailed, err)
	}
	ss, ok := sess.(*steam.Session)
	if !ok || ss.SteamID == "" {
		return identity.Account{}, fmt.Errorf("steam: verify: no SteamID in the assertion: %w", identity.ErrProviderFailed)
	}
	acct := identity.Account{Provider: s.Name(), Subject: ss.SteamID, Method: identity.MethodOpenID}
	if s.apiKey == "" {
		return acct, nil
	}
	acct.DisplayName, acct.AvatarURL, err = s.summary(ctx, ss.SteamID)
	if err != nil {
		return identity.Account{}, err
	}
	return acct, nil
}

// summary reads the persona name and the avatar for id from the Web API. The key is in the URL,
// so no error from here may carry the URL: the transport's errors do, and are unwrapped first.
func (s *steamProvider) summary(ctx context.Context, id string) (name, avatar string, err error) {
	q := url.Values{"key": {s.apiKey}, "steamids": {id}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, steamSummariesURL+"?"+q.Encode(), nil)
	if err != nil {
		return "", "", fmt.Errorf("steam: player summary: %w", identity.ErrProviderFailed)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return "", "", fmt.Errorf("steam: player summary: %w: %w", identity.ErrProviderFailed, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("steam: player summary: status %d: %w", resp.StatusCode, identity.ErrProviderFailed)
	}
	var body struct {
		Response struct {
			Players []struct {
				SteamID     string `json:"steamid"`
				PersonaName string `json:"personaname"`
				AvatarFull  string `json:"avatarfull"`
			} `json:"players"`
		} `json:"response"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxSummaryBody)).Decode(&body); err != nil {
		return "", "", fmt.Errorf("steam: player summary: decode: %w: %w", identity.ErrProviderFailed, err)
	}
	players := body.Response.Players
	if len(players) != 1 {
		return "", "", fmt.Errorf("steam: player summary: %d players for one id: %w", len(players), identity.ErrProviderFailed)
	}
	if players[0].SteamID != id {
		return "", "", fmt.Errorf("steam: player summary: answered for another id: %w", identity.ErrProviderFailed)
	}
	return players[0].PersonaName, players[0].AvatarFull, nil
}

// authorize runs Goth's OpenID verification. Goth indexes the second line of Steam's
// check_authentication reply without looking, so a one-line reply panics inside it; that is a
// failed verification, not a crashed request.
func authorize(sess goth.Session, p goth.Provider, params url.Values) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("malformed check_authentication reply: %v", r)
		}
	}()
	_, err = sess.Authorize(p, params)
	return err
}
