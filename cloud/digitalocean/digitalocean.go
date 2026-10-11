package digitalocean

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/digitalocean/godo"

	"github.com/gravel-project/gravel/cloud"
)

// ErrUnauthorized is the API refusing the token (401) or a token without the scope a call needs
// (403). It wraps no cloud error: no retry or wait fixes it.
var ErrUnauthorized = errors.New("digitalocean: the API refused the token")

// Defaults for Config's zero values.
const (
	// DefaultPollInterval is Wait's first pause between polls; it grows to five times this.
	// Two seconds is DigitalOcean's own suggestion for watching an action, and the cap keeps a
	// ten-minute resize inside a few hundred requests of the hourly budget.
	DefaultPollInterval = 2 * time.Second
	// DefaultRetries is how many times a request the API answered 429 (or, for a request that is
	// safe to repeat, 5xx) is sent again before the error reaches the caller.
	DefaultRetries = 3
	// DefaultRetryWait is the first pause before a retry; it doubles each time.
	DefaultRetryWait = time.Second
	defaultUserAgent = "gravel"
	// perPage is the API's largest page, so a list costs as few requests as it can.
	perPage = 200
)

// Config configures a Provider.
type Config struct {
	// Token is the API token: a hub secret, read from a file by the caller.
	Token string
	// Project is the project every created Droplet, volume and reserved IP is put in, by id
	// (a UUID) or by name. Empty leaves them in the team's default project.
	Project string
	// BaseURL is the API's root; empty is DigitalOcean's. Tests point it at dotest.
	BaseURL string
	// HTTPClient sends the requests; nil is a client with a one-minute timeout. Its Transport is
	// wrapped with the token and the retries.
	HTTPClient *http.Client
	// PollInterval is Wait's first pause between polls (DefaultPollInterval when zero).
	PollInterval time.Duration
	// UserAgent prefixes godo's own ("gravel-hub/0.8.0"); empty is "gravel".
	UserAgent string
	// Retries is how many times a rate-limited request is retried (DefaultRetries when zero,
	// none when negative). RetryWait is the first pause (DefaultRetryWait when zero).
	Retries   int
	RetryWait time.Duration
}

// String prints the configuration without the token, so a Config in a log line is safe.
func (c Config) String() string {
	tok := "unset"
	if c.Token != "" {
		tok = "set"
	}
	return fmt.Sprintf("digitalocean.Config{Token: %s, Project: %q, BaseURL: %q}", tok, c.Project, c.BaseURL)
}

// Provider is cloud.Provider for DigitalOcean.
type Provider struct {
	c       *godo.Client
	project string
	poll    time.Duration

	mu       sync.Mutex
	resolved string // the project id, resolved from project once
}

var _ cloud.Provider = (*Provider)(nil)

// New returns a provider. It sends nothing: the token and the project are checked by the first
// call that needs them.
func New(cfg Config) (*Provider, error) {
	if cfg.Token == "" {
		return nil, errors.New("digitalocean: no API token")
	}
	hc := http.Client{Timeout: time.Minute}
	if cfg.HTTPClient != nil {
		hc = *cfg.HTTPClient
	}
	base := hc.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	t := &transport{base: base, token: cfg.Token, retries: cfg.Retries, wait: cfg.RetryWait}
	if t.retries == 0 {
		t.retries = DefaultRetries
	}
	if t.wait <= 0 {
		t.wait = DefaultRetryWait
	}
	hc.Transport = t
	ua := cfg.UserAgent
	if ua == "" {
		ua = defaultUserAgent
	}
	opts := []godo.ClientOpt{godo.SetUserAgent(ua)}
	if cfg.BaseURL != "" {
		opts = append(opts, godo.SetBaseURL(cfg.BaseURL))
	}
	c, err := godo.New(&hc, opts...)
	if err != nil {
		return nil, fmt.Errorf("digitalocean: %w", err)
	}
	p := &Provider{c: c, project: cfg.Project, poll: cfg.PollInterval}
	if p.poll <= 0 {
		p.poll = DefaultPollInterval
	}
	return p, nil
}

// Name is "digitalocean".
func (p *Provider) Name() string { return "digitalocean" }

// transport adds the token and retries what the API asked to have retried. godo's own
// WithRetryAndBackoffs replaces the caller's HTTP client with go-retryablehttp's and needs an
// oauth2 transport, and it retries a POST that failed with a 5xx: a Droplet create the API
// carried out before failing would then be made twice. This one retries a 429 always (the API
// did nothing) and a 5xx only for a method that is safe to repeat.
type transport struct {
	base    http.RoundTripper
	token   string
	retries int
	wait    time.Duration
}

// maxRetryWait caps one pause; a Retry-After longer than it is the hourly budget running out,
// which a retry does not help, so the 429 goes to the caller.
const maxRetryWait = 30 * time.Second

func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	r.Header.Set("Authorization", "Bearer "+t.token)
	for attempt := 0; ; attempt++ {
		if attempt > 0 && req.Body != nil {
			if req.GetBody == nil {
				return nil, errors.New("digitalocean: cannot resend a request body")
			}
			body, err := req.GetBody()
			if err != nil {
				return nil, err
			}
			r.Body = body
		}
		resp, err := t.base.RoundTrip(r)
		if err != nil || attempt >= t.retries || !retryable(r.Method, resp.StatusCode) {
			return resp, err
		}
		wait := t.wait << attempt
		if s := resp.Header.Get("Retry-After"); s != "" {
			if n, err := strconv.Atoi(s); err == nil {
				wait = time.Duration(n) * time.Second
			}
		}
		if wait > maxRetryWait {
			return resp, nil
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		_ = resp.Body.Close()
		timer := time.NewTimer(wait)
		select {
		case <-req.Context().Done():
			timer.Stop()
			return nil, req.Context().Err()
		case <-timer.C:
		}
	}
}

func retryable(method string, status int) bool {
	if status == http.StatusTooManyRequests {
		return true
	}
	if status < 500 {
		return false
	}
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPut, http.MethodDelete:
		return true
	}
	return false
}

// apiErr turns godo's error into the cloud's, naming what was being done. It wraps godo's error
// too, whose text is the method, the URL and DigitalOcean's message, never the token.
func apiErr(what string, err error) error {
	if err == nil {
		return nil
	}
	var er *godo.ErrorResponse
	if !errors.As(err, &er) || er.Response == nil {
		return fmt.Errorf("digitalocean: %s: %w", what, err)
	}
	switch er.Response.StatusCode {
	case http.StatusNotFound:
		return fmt.Errorf("digitalocean: %s: %w (%w)", what, cloud.ErrNotFound, err)
	case http.StatusUnprocessableEntity, http.StatusConflict, http.StatusBadRequest:
		return fmt.Errorf("digitalocean: %s: %w: %s (%w)", what, cloud.ErrInvalid, er.Message, err)
	case http.StatusTooManyRequests:
		return fmt.Errorf("digitalocean: %s: %w (%w)", what, cloud.ErrRateLimited, err)
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("digitalocean: %s: %w (%w)", what, ErrUnauthorized, err)
	default:
		return fmt.Errorf("digitalocean: %s: %w", what, err)
	}
}

// listAll pages through a list until its last page.
func listAll[T any](ctx context.Context, what string, list func(context.Context, *godo.ListOptions) ([]T, *godo.Response, error)) ([]T, error) {
	var all []T
	opt := &godo.ListOptions{Page: 1, PerPage: perPage}
	for {
		items, resp, err := list(ctx, opt)
		if err != nil {
			return nil, apiErr(what, err)
		}
		all = append(all, items...)
		if resp == nil || resp.Links == nil || resp.Links.IsLastPage() {
			return all, nil
		}
		page, err := resp.Links.CurrentPage()
		if err != nil {
			return nil, fmt.Errorf("digitalocean: %s: reading the page links: %w", what, err)
		}
		if page < opt.Page { // a server that does not say where it is: never loop forever
			return nil, fmt.Errorf("digitalocean: %s: page links went backwards", what)
		}
		opt = &godo.ListOptions{Page: page + 1, PerPage: perPage}
	}
}

var uuidRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// projectID resolves Config.Project to an id once; "" is the default project. A failed lookup
// is not cached, so a transient error does not stick.
func (p *Provider) projectID(ctx context.Context) (string, error) {
	if p.project == "" {
		return "", nil
	}
	if uuidRE.MatchString(p.project) {
		return p.project, nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.resolved != "" {
		return p.resolved, nil
	}
	projects, err := listAll(ctx, "list projects", p.c.Projects.List)
	if err != nil {
		return "", err
	}
	for _, pr := range projects {
		if pr.Name == p.project {
			p.resolved = pr.ID
			return pr.ID, nil
		}
	}
	return "", fmt.Errorf("digitalocean: project %q: %w", p.project, cloud.ErrNotFound)
}

// assign puts created resources in a project resolved with projectID before they were made, so
// an unknown project fails before anything exists; "" is the default project and sends nothing.
func (p *Provider) assign(ctx context.Context, projectID string, urns ...string) error {
	if projectID == "" {
		return nil
	}
	res := make([]any, len(urns))
	for i, u := range urns {
		res[i] = u
	}
	_, _, err := p.c.Projects.AssignResources(ctx, projectID, res...)
	return apiErr("assign to project "+p.project, err)
}

// ensureTags creates tags that do not exist yet. Droplets and volumes create their tags
// themselves; a firewall's targets and a tag added to an existing Droplet need them first.
// DigitalOcean's answer to creating a tag that exists is not documented as success, so a
// refusal is checked by reading the tag back.
func (p *Provider) ensureTags(ctx context.Context, tags []string) error {
	for _, tag := range tags {
		_, _, err := p.c.Tags.Create(ctx, &godo.TagCreateRequest{Name: tag})
		if err == nil {
			continue
		}
		if _, _, gerr := p.c.Tags.Get(ctx, tag); gerr == nil {
			continue
		}
		return apiErr("create tag "+tag, err)
	}
	return nil
}

// dropletID parses a cloud id into DigitalOcean's integer Droplet id. Anything else names no
// Droplet, so it is not found rather than invalid: a caller holding an id from another provider
// or a deleted record gets the answer it would get for a Droplet that is gone.
func dropletID(id string) (int, error) {
	n, err := strconv.Atoi(id)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("digitalocean: %q is not a Droplet id: %w", id, cloud.ErrNotFound)
	}
	return n, nil
}

func hasTag(tags []string, tag string) bool {
	for _, t := range tags {
		if t == tag {
			return true
		}
	}
	return false
}
