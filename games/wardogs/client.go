package wardogs

import (
	"bytes"
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
)

// Errors a call returns before or instead of the server's answer.
var (
	// ErrNotSupported is a capability the server does not advertise; no request was sent.
	ErrNotSupported = errors.New("wardogs: the server does not offer this")
	// ErrNoToken is a protected route on a client without a token; no request was sent, because
	// a request without one counts toward the server's three-strike throttle.
	ErrNoToken = errors.New("wardogs: this route needs a token and the client has none")
	// ErrTokenRefused is the server answering 401 to the client's token. The client sends nothing
	// protected again until SetToken gives it another: a second and third try would lock every
	// protected route for a while.
	ErrTokenRefused = errors.New("wardogs: the server refused the token; nothing protected is sent until a new one is set")
	// ErrBodyTooLarge is a request body over the server's limit; no request was sent.
	ErrBodyTooLarge = errors.New("wardogs: request body is over the server's limit")
)

// RateLimitedError is a 429: the server asked for nothing until RetryAfter has passed, and the
// client keeps to it (calls before Until fail with this error and send nothing).
type RateLimitedError struct {
	RetryAfter time.Duration
	Until      time.Time
}

func (e *RateLimitedError) Error() string {
	return fmt.Sprintf("wardogs: rate limited; retry after %s", e.RetryAfter)
}

// APIError is the server's error answer: the status and its {"error":{"code","message"}} body.
type APIError struct {
	StatusCode int
	Code       string
	Message    string
}

func (e *APIError) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("wardogs: %d %s", e.StatusCode, http.StatusText(e.StatusCode))
	}
	return fmt.Sprintf("wardogs: %d %s: %s", e.StatusCode, e.Code, e.Message)
}

// IsCode reports whether err is the server's error with this code ("player_not_found").
func IsCode(err error, code string) bool {
	var e *APIError
	return errors.As(err, &e) && e.Code == code
}

// Options configure a client.
type Options struct {
	// Token is the RCON password, sent as a bearer token on the protected routes. Empty means
	// only GET /v1/health and GET /v1/capabilities can be called.
	Token string
	// HTTPClient carries the requests; nil means one with a 15-second timeout.
	HTTPClient *http.Client
	// UserAgent names the caller in the server's logs.
	UserAgent string
	// Now is the clock (tests); nil means time.Now.
	Now func() time.Time
	// Strict makes an answer whose field has an unexpected JSON type an error. Without it the
	// client keeps every field that did decode, reports the mismatch to OnMismatch and goes on:
	// a type drift in a new build costs that field, not the call. The contract tests are strict.
	Strict bool
	// OnMismatch hears about each tolerated mismatch (route as the server writes it, the error).
	OnMismatch func(route string, err error)
}

const (
	// maxResponseBytes caps what is read of an answer; the whole catalog is about 13 KiB.
	maxResponseBytes = 1 << 20
	// defaultRetryAfter is the wait when a 429 names none; the server's own are 1–60 s.
	defaultRetryAfter = time.Minute
)

// Client talks to one War Dogs server. It is safe for concurrent use.
type Client struct {
	base       string
	http       *http.Client
	ua         string
	now        func() time.Time
	strict     bool
	onMismatch func(string, error)

	// probe serialises protected requests until the token has been accepted once, so concurrent
	// callers cannot spend more than one strike on a bad token.
	probe sync.Mutex

	mu        sync.Mutex
	token     string
	verified  bool // the token has been accepted
	refused   bool // the token was answered 401
	notBefore time.Time
	caps      *Capabilities
}

// New builds a client for a server's base URL ("http://203.0.113.10:7789"). Nothing is sent
// until the first call.
func New(baseURL string, opt Options) (*Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" {
		return nil, fmt.Errorf("wardogs: base URL %q is not an http(s) origin", baseURL)
	}
	hc := opt.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	now := opt.Now
	if now == nil {
		now = time.Now
	}
	return &Client{
		base:       strings.TrimSuffix(baseURL, "/"),
		http:       hc,
		ua:         opt.UserAgent,
		now:        now,
		strict:     opt.Strict,
		onMismatch: opt.OnMismatch,
		token:      strings.TrimSpace(opt.Token),
	}, nil
}

// BaseURL is the server's origin.
func (c *Client) BaseURL() string { return c.base }

// String never includes the token.
func (c *Client) String() string { return "wardogs.Client(" + c.base + ")" }

// SetToken replaces the token (after a rotation) and lifts a refusal.
func (c *Client) SetToken(token string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.token, c.verified, c.refused = strings.TrimSpace(token), false, false
}

// HasToken reports whether the client has a token it has not had refused.
func (c *Client) HasToken() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.token != "" && !c.refused
}

// Capabilities fetches GET /v1/capabilities (public) and keeps it: later calls check their route
// against it. Call it again on a build change.
func (c *Client) Capabilities(ctx context.Context) (Capabilities, error) {
	var caps Capabilities
	if err := c.getJSON(ctx, routeCapabilities, nil, &caps); err != nil {
		return Capabilities{}, err
	}
	c.mu.Lock()
	c.caps = &caps
	c.mu.Unlock()
	return caps, nil
}

// Cached is the capabilities the client last fetched; ok is false before the first fetch.
func (c *Client) Cached() (Capabilities, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.caps == nil {
		return Capabilities{}, false
	}
	return *c.caps, true
}

// Supports reports whether the server grants a capability, fetching the capabilities first if
// the client has none.
func (c *Client) Supports(ctx context.Context, cap Capability) (bool, error) {
	caps, err := c.ensureCaps(ctx)
	if err != nil {
		return false, err
	}
	return caps.Supports(cap), nil
}

func (c *Client) ensureCaps(ctx context.Context) (Capabilities, error) {
	if caps, ok := c.Cached(); ok {
		return caps, nil
	}
	return c.Capabilities(ctx)
}

// route is the server's route for a capability, or ErrNotSupported.
func (c *Client) route(ctx context.Context, cap Capability) (Route, error) {
	caps, err := c.ensureCaps(ctx)
	if err != nil {
		return Route{}, err
	}
	r, ok := caps.RouteOf(cap)
	if !ok {
		return Route{}, fmt.Errorf("%w: %s", ErrNotSupported, cap)
	}
	return r, nil
}

// Response is a raw answer.
type Response struct {
	StatusCode int
	Header     http.Header
	Body       []byte
}

// Get sends GET on an advertised route with its parameters filled in order and returns the body
// as the server sent it. A route the server did not advertise is ErrNotSupported. The fixtures
// recorder uses it; everything else uses the typed calls.
func (c *Client) Get(ctx context.Context, r Route, query url.Values, params ...string) ([]byte, error) {
	if r.Method != http.MethodGet {
		return nil, fmt.Errorf("wardogs: Get on %s", r)
	}
	if r.Shape() != routeCapabilities.Shape() {
		caps, err := c.ensureCaps(ctx)
		if err != nil {
			return nil, err
		}
		if !advertises(caps, r) {
			return nil, fmt.Errorf("%w: %s", ErrNotSupported, r)
		}
	}
	path, err := r.Path(params...)
	if err != nil {
		return nil, err
	}
	resp, err := c.send(ctx, request{route: r, path: path, query: query})
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

func advertises(caps Capabilities, r Route) bool {
	for _, s := range caps.Routes {
		if a, err := ParseRoute(s); err == nil && a.Shape() == r.Shape() {
			return true
		}
	}
	return false
}

// call sends a request on a capability's route and decodes a 2xx JSON answer into out (nil
// skips it). in is marshalled as JSON when not nil.
func (c *Client) call(ctx context.Context, cap Capability, in, out any, params ...string) error {
	r, err := c.route(ctx, cap)
	if err != nil {
		return err
	}
	path, err := r.Path(params...)
	if err != nil {
		return err
	}
	var body []byte
	ctype := ""
	if in != nil {
		if body, err = json.Marshal(in); err != nil {
			return fmt.Errorf("wardogs: %s: %w", cap, err)
		}
		ctype = "application/json"
	}
	resp, err := c.send(ctx, request{route: r, path: path, body: body, ctype: ctype})
	if err != nil {
		return err
	}
	return c.decode(resp.Body, out, r)
}

func (c *Client) getJSON(ctx context.Context, r Route, query url.Values, out any, params ...string) error {
	path, err := r.Path(params...)
	if err != nil {
		return err
	}
	resp, err := c.send(ctx, request{route: r, path: path, query: query})
	if err != nil {
		return err
	}
	return c.decode(resp.Body, out, r)
}

// decode reads a JSON answer into out, tolerating a field of an unexpected type unless the
// client is strict (Options.Strict).
func (c *Client) decode(body []byte, out any, r Route) error {
	if out == nil || len(bytes.TrimSpace(body)) == 0 {
		return nil
	}
	err := json.Unmarshal(body, out)
	var typeErr *json.UnmarshalTypeError
	if err != nil && errors.As(err, &typeErr) && !c.strict {
		if c.onMismatch != nil {
			c.onMismatch(r.String(), err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("wardogs: %s: decoding the answer: %w", r, err)
	}
	return nil
}

// request is one request on a route.
type request struct {
	route  Route
	path   string // the route with its parameters filled
	query  url.Values
	body   []byte // nil sends none
	ctype  string
	header http.Header
	// pass are the non-2xx statuses that come back as a Response instead of an error (the
	// config routes read their 412 and 422 bodies).
	pass map[int]bool
}

// send does one request and maps the answer: a 2xx or a status in pass comes back as is;
// anything else is an error.
func (c *Client) send(ctx context.Context, q request) (*Response, error) {
	r, body := q.route, q.body
	public := publicShapes[r.Shape()]

	c.mu.Lock()
	if wait := c.notBefore.Sub(c.now()); wait > 0 {
		c.mu.Unlock()
		return nil, &RateLimitedError{RetryAfter: wait, Until: c.notBefore}
	}
	if body != nil && c.caps != nil && c.caps.Limits.MaxBodyBytes > 0 && len(body) > c.caps.Limits.MaxBodyBytes {
		c.mu.Unlock()
		return nil, fmt.Errorf("%w: %d bytes, limit %d", ErrBodyTooLarge, len(body), c.caps.Limits.MaxBodyBytes)
	}
	verified := c.verified
	c.mu.Unlock()

	if !public && !verified {
		c.probe.Lock()
		defer c.probe.Unlock()
	}

	c.mu.Lock()
	token, refused := c.token, c.refused
	c.mu.Unlock()
	if !public {
		switch {
		case refused:
			return nil, ErrTokenRefused
		case token == "":
			return nil, ErrNoToken
		}
	}

	u := c.base + q.path
	if len(q.query) > 0 {
		u += "?" + q.query.Encode()
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, r.Method, u, rd)
	if err != nil {
		return nil, fmt.Errorf("wardogs: %s: %w", r, err)
	}
	for k, vs := range q.header {
		req.Header[k] = vs
	}
	req.Header.Set("Accept", "application/json")
	if q.ctype != "" {
		req.Header.Set("Content-Type", q.ctype)
	}
	if c.ua != "" {
		req.Header.Set("User-Agent", c.ua)
	}
	if !public {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("wardogs: %s: %w", r, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("wardogs: %s: reading the answer: %w", r, err)
	}
	if len(data) > maxResponseBytes {
		return nil, fmt.Errorf("wardogs: %s: answer is over %d bytes", r, maxResponseBytes)
	}

	switch {
	case resp.StatusCode == http.StatusUnauthorized && !public:
		c.mu.Lock()
		if c.token == token {
			c.refused, c.verified = true, false
		}
		c.mu.Unlock()
		return nil, ErrTokenRefused
	case resp.StatusCode == http.StatusTooManyRequests:
		wait := retryAfter(resp.Header.Get("Retry-After"), c.now())
		c.mu.Lock()
		until := c.now().Add(wait)
		if until.After(c.notBefore) {
			c.notBefore = until
		}
		c.mu.Unlock()
		return nil, &RateLimitedError{RetryAfter: wait, Until: until}
	}
	// Any other answer below 500 got past the token check; a 5xx proves nothing.
	if !public && resp.StatusCode < 500 {
		c.mu.Lock()
		if c.token == token {
			c.verified = true
		}
		c.mu.Unlock()
	}
	out := &Response{StatusCode: resp.StatusCode, Header: resp.Header, Body: data}
	if resp.StatusCode/100 == 2 || q.pass[resp.StatusCode] {
		return out, nil
	}
	return nil, apiError(resp.StatusCode, data)
}

func apiError(status int, body []byte) *APIError {
	var e struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &e)
	return &APIError{StatusCode: status, Code: e.Error.Code, Message: e.Error.Message}
}

// retryAfter reads a Retry-After header: seconds or an HTTP date; absent or unreadable means
// the default.
func retryAfter(h string, now time.Time) time.Duration {
	h = strings.TrimSpace(h)
	if s, err := strconv.Atoi(h); err == nil && s >= 0 {
		return time.Duration(s) * time.Second
	}
	if t, err := http.ParseTime(h); err == nil {
		if d := t.Sub(now); d > 0 {
			return d
		}
		return 0
	}
	return defaultRetryAfter
}
