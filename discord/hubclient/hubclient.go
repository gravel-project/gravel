// Package hubclient calls the gravel hub's API as a registered app (docs/hub.md, "Service
// credentials"): it trades the client credentials for a bearer token at /oauth/token, keeps it
// until shortly before it expires, and puts it on every request. A 401 refreshes the token once
// and retries, so a token revoked under the bot heals on the next call.
package hubclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gravel-project/gravel/gen/gravel/hub/v1/hubv1connect"
)

// Config is where the hub is and who the app is.
type Config struct {
	// URL is the hub's origin, http://gravel-hub:8080 on a stack's network.
	URL          string
	ClientID     string
	ClientSecret string
	// Scopes narrows the token to a subset of the app's scopes; empty asks for all of them.
	Scopes []string
	// HTTPClient carries the requests; nil means a default with a timeout.
	HTTPClient *http.Client
}

// Refresh this long before the token expires, so a request never carries one about to expire.
const refreshMargin = time.Minute

// Errors.
var (
	// ErrInvalidClient is the hub refusing the credentials: unknown, revoked, or a wrong secret.
	ErrInvalidClient = errors.New("hubclient: the hub refused the client credentials")
)

// Client is a hub client for one app.
type Client struct {
	cfg  Config
	base *http.Client
	http *http.Client // base, with the bearer transport
	now  func() time.Time

	mu      sync.Mutex
	token   string
	expires time.Time
}

// New builds a client; nothing is fetched until the first request.
func New(cfg Config) (*Client, error) {
	u, err := url.Parse(cfg.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("hubclient: url %q is not an http(s) origin", cfg.URL)
	}
	if cfg.ClientID == "" || cfg.ClientSecret == "" {
		return nil, errors.New("hubclient: a client id and secret are required")
	}
	cfg.URL = strings.TrimSuffix(cfg.URL, "/")
	base := cfg.HTTPClient
	if base == nil {
		base = &http.Client{Timeout: 30 * time.Second}
	}
	c := &Client{cfg: cfg, base: base, now: time.Now}
	authed := *base
	authed.Transport = &transport{c: c, next: base.Transport}
	c.http = &authed
	return c, nil
}

// HTTPClient is a client that authenticates every request as the app; the Connect clients
// below are built on it.
func (c *Client) HTTPClient() *http.Client { return c.http }

// URL is the hub's origin.
func (c *Client) URL() string { return c.cfg.URL }

// Identity is the IdentityService client, authenticated as the app.
func (c *Client) Identity() hubv1connect.IdentityServiceClient {
	return hubv1connect.NewIdentityServiceClient(c.http, c.cfg.URL)
}

// Organization is the OrganizationService client, authenticated as the app.
func (c *Client) Organization() hubv1connect.OrganizationServiceClient {
	return hubv1connect.NewOrganizationServiceClient(c.http, c.cfg.URL)
}

// Servers is the ServerService client, authenticated as the app: the games, the servers and
// their last observation (public), and who is on (the servers:read scope).
func (c *Client) Servers() hubv1connect.ServerServiceClient {
	return hubv1connect.NewServerServiceClient(c.http, c.cfg.URL)
}

// Moderation is the ModerationService client, authenticated as the app (the servers:moderate
// scope): kick, ban, unban, message, broadcast and move, naming the moderator the app acts for
// (on_behalf_of), the server's bans and the audit log.
func (c *Client) Moderation() hubv1connect.ModerationServiceClient {
	return hubv1connect.NewModerationServiceClient(c.http, c.cfg.URL)
}

// ServerConfig is the ServerConfigService client, authenticated as the app (the
// servers:configure scope): a server's configuration, planned and applied.
func (c *Client) ServerConfig() hubv1connect.ServerConfigServiceClient {
	return hubv1connect.NewServerConfigServiceClient(c.http, c.cfg.URL)
}

// Token returns a bearer token, fetching or refreshing one when needed.
func (c *Client) Token(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && c.now().Add(refreshMargin).Before(c.expires) {
		return c.token, nil
	}
	return c.fetchLocked(ctx)
}

// Invalidate forgets the token, so the next request fetches a fresh one.
func (c *Client) Invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.token, c.expires = "", time.Time{}
}

func (c *Client) fetchLocked(ctx context.Context) (string, error) {
	form := url.Values{"grant_type": {"client_credentials"}}
	if len(c.cfg.Scopes) > 0 {
		form.Set("scope", strings.Join(c.cfg.Scopes, " "))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.URL+"/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("hubclient: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(url.QueryEscape(c.cfg.ClientID), url.QueryEscape(c.cfg.ClientSecret))
	resp, err := c.base.Do(req)
	if err != nil {
		return "", fmt.Errorf("hubclient: token request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode == http.StatusUnauthorized {
		return "", ErrInvalidClient
	}
	if resp.StatusCode != http.StatusOK {
		var e struct{ Error, Description string }
		_ = json.Unmarshal(body, &e)
		return "", fmt.Errorf("hubclient: token request: %s: %s %s", resp.Status, e.Error, e.Description)
	}
	var tok struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tok); err != nil || tok.AccessToken == "" || !strings.EqualFold(tok.TokenType, "Bearer") {
		return "", fmt.Errorf("hubclient: token request: unexpected answer: %s", strings.TrimSpace(string(body)))
	}
	c.token = tok.AccessToken
	c.expires = c.now().Add(time.Duration(tok.ExpiresIn) * time.Second)
	return c.token, nil
}

// transport puts the bearer token on every request and, on a 401, refreshes it once and
// retries. A body is replayed only when the request can be rewound (GetBody), which Connect's
// unary requests allow.
type transport struct {
	c    *Client
	next http.RoundTripper
}

func (t *transport) RoundTrip(r *http.Request) (*http.Response, error) {
	next := t.next
	if next == nil {
		next = http.DefaultTransport
	}
	tok, err := t.c.Token(r.Context())
	if err != nil {
		return nil, err
	}
	first := r.Clone(r.Context())
	first.Header.Set("Authorization", "Bearer "+tok)
	resp, err := next.RoundTrip(first)
	if err != nil || resp.StatusCode != http.StatusUnauthorized || r.GetBody == nil && r.Body != nil && r.Body != http.NoBody {
		return resp, err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	t.c.Invalidate()
	tok, err = t.c.Token(r.Context())
	if err != nil {
		return nil, err
	}
	retry := r.Clone(r.Context())
	if r.GetBody != nil {
		body, err := r.GetBody()
		if err != nil {
			return nil, fmt.Errorf("hubclient: retry: %w", err)
		}
		retry.Body = body
	}
	retry.Header.Set("Authorization", "Bearer "+tok)
	return next.RoundTrip(retry)
}
