package streams

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"
)

// Stream is one live stream, as a platform reports it: metadata only.
type Stream struct {
	Platform, Login string
	// ID tells one stream from the next: Twitch's stream id; for Kick, which has none, the
	// stream's start as a Unix time.
	ID          string
	Title, Game string
	StartedAt   time.Time
}

// UnknownID is a Kick stream's id when Kick gives no start time: one stream while it stays live,
// never the same stream as an earlier one.
const UnknownID = "0"

// Platform is a streaming service the module asks who is live.
type Platform interface {
	// Name is the settings' platform: "twitch" or "kick".
	Name() string
	// Live returns the streams live now among logins (at most 50). A login that isn't live, or
	// isn't a channel, is simply absent. Errors are ErrUnauthorized, a *ThrottledError or any
	// other failure; on an error nothing is known about anyone.
	Live(ctx context.Context, logins []string) ([]Stream, error)
}

// ErrUnauthorized is a platform refusing the application's credentials or token.
var ErrUnauthorized = errors.New("the platform refused the application's credentials")

// ThrottledError is a platform's rate limit: ask again after RetryAfter.
type ThrottledError struct{ RetryAfter time.Duration }

func (e *ThrottledError) Error() string {
	return fmt.Sprintf("rate limited, retry after %s", e.RetryAfter)
}

// App is a platform's developer application and where its API is.
type App struct {
	ClientID, ClientSecret string
	// TokenURL and APIURL default to the platform's own; tests point them elsewhere.
	TokenURL, APIURL string
}

// Endpoints of the platforms.
const (
	TwitchTokenURL = "https://id.twitch.tv/oauth2/token"
	TwitchAPIURL   = "https://api.twitch.tv"
	KickTokenURL   = "https://id.kick.com/oauth/token"
	KickAPIURL     = "https://api.kick.com"
)

const (
	// maxBody bounds what is read of a platform's answer.
	maxBody = 1 << 20
	// defaultRetry is the back-off for a 429 that names no time.
	defaultRetry = time.Minute
	// maxRetry caps a 429's back-off.
	maxRetry = 15 * time.Minute
)

// appClient holds an app access token from the client credentials, fetched when needed and
// fetched anew after the platform refuses it.
type appClient struct {
	cfg  clientcredentials.Config
	base *http.Client

	mu  sync.Mutex
	src oauth2.TokenSource
}

func newAppClient(app App, tokenURL string, base *http.Client) *appClient {
	return &appClient{base: base, cfg: clientcredentials.Config{ClientID: app.ClientID, ClientSecret: app.ClientSecret, TokenURL: tokenURL,
		AuthStyle: oauth2.AuthStyleInParams}}
}

// token is the app token; the token source outlives a pass, so it fetches with no deadline of
// the pass's (the base client's timeout bounds it).
func (c *appClient) token() (string, error) {
	c.mu.Lock()
	if c.src == nil {
		c.src = c.cfg.TokenSource(context.WithValue(context.Background(), oauth2.HTTPClient, c.base))
	}
	src := c.src
	c.mu.Unlock()
	tok, err := src.Token()
	if err != nil {
		var re *oauth2.RetrieveError
		if errors.As(err, &re) && re.Response != nil && (re.Response.StatusCode == http.StatusUnauthorized || re.Response.StatusCode == http.StatusBadRequest || re.Response.StatusCode == http.StatusForbidden) {
			return "", fmt.Errorf("%w: the token endpoint answered %d", ErrUnauthorized, re.Response.StatusCode)
		}
		return "", fmt.Errorf("app token: %w", err)
	}
	return tok.AccessToken, nil
}

// forget drops the token, so the next call fetches a new one.
func (c *appClient) forget() {
	c.mu.Lock()
	c.src = nil
	c.mu.Unlock()
}

// get calls the API with the app token, once more with a fresh token when the first is refused,
// and decodes a 200's JSON into out.
func (c *appClient) get(ctx context.Context, u string, header http.Header, out any) error {
	for attempt := 0; ; attempt++ {
		tok, err := c.token()
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return err
		}
		for k, v := range header {
			req.Header[k] = v
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Accept", "application/json")
		resp, err := c.base.Do(req)
		if err != nil {
			return err
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
		_ = resp.Body.Close()
		if err != nil {
			return err
		}
		switch resp.StatusCode {
		case http.StatusOK:
			if err := json.Unmarshal(body, out); err != nil {
				return fmt.Errorf("decode the answer: %w", err)
			}
			return nil
		case http.StatusUnauthorized:
			c.forget()
			if attempt == 0 {
				continue
			}
			return ErrUnauthorized
		case http.StatusTooManyRequests:
			return &ThrottledError{RetryAfter: retryAfter(resp.Header, time.Now())}
		default:
			return fmt.Errorf("the API answered %d", resp.StatusCode)
		}
	}
}

// retryAfter reads a 429's wait: Retry-After in seconds, or Twitch's Ratelimit-Reset as a Unix
// time; neither is a minute. It is never more than maxRetry.
func retryAfter(h http.Header, now time.Time) time.Duration {
	d := defaultRetry
	if s, err := strconv.Atoi(strings.TrimSpace(h.Get("Retry-After"))); err == nil && s > 0 {
		d = time.Duration(s) * time.Second
	} else if at, err := strconv.ParseInt(strings.TrimSpace(h.Get("Ratelimit-Reset")), 10, 64); err == nil && at > now.Unix() {
		d = time.Unix(at, 0).Sub(now)
	}
	return min(d, maxRetry)
}

// Twitch asks Twitch's Helix API (GET /helix/streams) with an app access token.
type Twitch struct {
	app    App
	client *appClient
}

// NewTwitch returns the Twitch platform for the application.
func NewTwitch(app App, hc *http.Client) *Twitch {
	if app.TokenURL == "" {
		app.TokenURL = TwitchTokenURL
	}
	if app.APIURL == "" {
		app.APIURL = TwitchAPIURL
	}
	return &Twitch{app: app, client: newAppClient(app, app.TokenURL, hc)}
}

// Name is "twitch".
func (t *Twitch) Name() string { return "twitch" }

// Live asks which of logins are live.
func (t *Twitch) Live(ctx context.Context, logins []string) ([]Stream, error) {
	q := url.Values{"type": {"live"}, "first": {"100"}}
	for _, l := range logins {
		q.Add("user_login", l)
	}
	var out struct {
		Data []struct {
			ID        string    `json:"id"`
			UserLogin string    `json:"user_login"`
			GameName  string    `json:"game_name"`
			Type      string    `json:"type"`
			Title     string    `json:"title"`
			StartedAt time.Time `json:"started_at"`
		} `json:"data"`
	}
	if err := t.client.get(ctx, strings.TrimSuffix(t.app.APIURL, "/")+"/helix/streams?"+q.Encode(), http.Header{"Client-Id": {t.app.ClientID}}, &out); err != nil {
		return nil, err
	}
	var live []Stream
	for _, s := range out.Data {
		if s.Type != "live" || s.ID == "" {
			continue
		}
		live = append(live, Stream{Platform: "twitch", Login: strings.ToLower(s.UserLogin), ID: s.ID, Title: s.Title, Game: s.GameName, StartedAt: s.StartedAt})
	}
	return live, nil
}

// Kick asks Kick's public API (GET /public/v1/channels) with an app access token.
type Kick struct {
	app    App
	client *appClient
}

// NewKick returns the Kick platform for the application.
func NewKick(app App, hc *http.Client) *Kick {
	if app.TokenURL == "" {
		app.TokenURL = KickTokenURL
	}
	if app.APIURL == "" {
		app.APIURL = KickAPIURL
	}
	return &Kick{app: app, client: newAppClient(app, app.TokenURL, hc)}
}

// Name is "kick".
func (k *Kick) Name() string { return "kick" }

// Live asks which of logins (Kick's slugs) are live.
func (k *Kick) Live(ctx context.Context, logins []string) ([]Stream, error) {
	q := url.Values{}
	for _, l := range logins {
		q.Add("slug", l)
	}
	var out struct {
		Data []struct {
			Slug        string `json:"slug"`
			StreamTitle string `json:"stream_title"`
			Category    struct {
				Name string `json:"name"`
			} `json:"category"`
			Stream struct {
				IsLive    bool   `json:"is_live"`
				StartTime string `json:"start_time"`
			} `json:"stream"`
		} `json:"data"`
	}
	if err := k.client.get(ctx, strings.TrimSuffix(k.app.APIURL, "/")+"/public/v1/channels?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	var live []Stream
	for _, c := range out.Data {
		if !c.Stream.IsLive {
			continue
		}
		started := kickTime(c.Stream.StartTime)
		id := UnknownID
		if !started.IsZero() {
			id = strconv.FormatInt(started.Unix(), 10)
		}
		live = append(live, Stream{Platform: "kick", Login: strings.ToLower(c.Slug), ID: id, Title: c.StreamTitle, Game: c.Category.Name, StartedAt: started})
	}
	return live, nil
}

// kickTime reads Kick's start_time, RFC 3339 or "2006-01-02 15:04:05" in UTC; zero if neither.
func kickTime(s string) time.Time {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, strings.TrimSpace(s)); err == nil && t.Year() > 2000 {
			return t.UTC()
		}
	}
	return time.Time{}
}
