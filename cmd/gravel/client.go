package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"time"

	"connectrpc.com/connect"

	"github.com/gravel-project/gravel/discord/hubclient"
	"github.com/gravel-project/gravel/gen/gravel/hub/v1/hubv1connect"
)

// The commands that act on a running hub call its API like the dashboard does (ADR-0031 §5):
// Connect's HTTP-JSON protocol through connect-go with WithProtoJSON, never the gRPC stack
// (principle 4; TestNoGRPC holds the line).

// Exit codes of the commands that call a hub.
const (
	exitOK          = 0
	exitFailed      = 1 // the call failed, or (fleet) something is down
	exitUsage       = 2
	exitWouldChange = 3 // a --dry-run found something to change, as gravel-hub and gravel hub say it
	exitLoggedIn    = 4 // no credential for the hub, or the hub no longer takes it: log in again
)

// requestTimeout bounds one call to the hub.
const requestTimeout = 30 * time.Second

// hubFlag is --hub, shared by every command that calls a hub.
type hubFlag struct{ hub *string }

func addHubFlag(fs *flag.FlagSet) hubFlag {
	return hubFlag{hub: fs.String("hub", "", "the hub's address, https://…; default GRAVEL_HUB, else the hub last logged in to")}
}

// resolve picks the hub: --hub, else GRAVEL_HUB, else the current hub of the credentials file.
func (f hubFlag) resolve(creds credentials) (string, error) {
	h := *f.hub
	if h == "" {
		h = os.Getenv("GRAVEL_HUB")
	}
	if h == "" {
		h = creds.Current
	}
	if h == "" {
		return "", errors.New("no hub: pass --hub https://… or set GRAVEL_HUB (gravel login remembers the last one)")
	}
	return normalizeHub(h)
}

// hubClient calls one hub with this machine's credential for it, or as nobody without one.
type hubClient struct {
	hub  string
	cred *credential // nil: anonymous
	http *http.Client
}

// openHub loads the credentials file and returns a client for the hub the flags name. Without a
// credential for it the client is anonymous: the public procedures answer, the rest say log in.
func openHub(f hubFlag) (*hubClient, error) {
	path, err := credentialsPath()
	if err != nil {
		return nil, err
	}
	creds, err := loadCredentials(path)
	if err != nil {
		return nil, err
	}
	hub, err := f.resolve(creds)
	if err != nil {
		return nil, err
	}
	c := &hubClient{hub: hub, http: plainHTTP()}
	if cr, ok := creds.find(hub); ok {
		if err := c.authenticate(cr); err != nil {
			return nil, err
		}
	}
	return c, nil
}

// authenticate makes every request carry the credential: the session cookie, or the app's
// bearer token, fetched with its client credentials and refreshed by hubclient.
func (c *hubClient) authenticate(cr credential) error {
	switch cr.Kind {
	case credentialSession:
		if cr.Cookie == "" {
			return fmt.Errorf("the credential for %s has no session cookie; log in again", cr.HubURL)
		}
		base := plainHTTP()
		base.Transport = cookieTransport{cookie: cr.Cookie, next: base.Transport}
		c.http = base
	case credentialApp:
		secret, err := readSecretFile(cr.SecretFile)
		if err != nil {
			return fmt.Errorf("the app's secret: %w", err)
		}
		app, err := hubclient.New(hubclient.Config{URL: cr.HubURL, ClientID: cr.ClientID, ClientSecret: secret, HTTPClient: plainHTTP()})
		if err != nil {
			return err
		}
		c.http = app.HTTPClient()
	default:
		return fmt.Errorf("the credential for %s is of an unknown kind %q; log in again", cr.HubURL, cr.Kind)
	}
	c.cred = &cr
	return nil
}

// plainHTTP is a client with gravel's user agent and a timeout; it follows no redirect, so a
// misconfigured address fails instead of carrying the credential somewhere else.
func plainHTTP() *http.Client {
	return &http.Client{
		Timeout:       requestTimeout,
		Transport:     userAgent{next: http.DefaultTransport},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

type userAgent struct{ next http.RoundTripper }

func (t userAgent) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("User-Agent", "gravel/"+buildVersion())
	return t.next.RoundTrip(r)
}

// cookieTransport puts the session cookie on every request.
type cookieTransport struct {
	cookie string
	next   http.RoundTripper
}

func (t cookieTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Cookie", t.cookie)
	return t.next.RoundTrip(r)
}

func (c *hubClient) identity() hubv1connect.IdentityServiceClient {
	return hubv1connect.NewIdentityServiceClient(c.http, c.hub, connect.WithProtoJSON())
}

func (c *hubClient) organization() hubv1connect.OrganizationServiceClient {
	return hubv1connect.NewOrganizationServiceClient(c.http, c.hub, connect.WithProtoJSON())
}

func (c *hubClient) servers() hubv1connect.ServerServiceClient {
	return hubv1connect.NewServerServiceClient(c.http, c.hub, connect.WithProtoJSON())
}

// session reports whether the client calls as a member through a session.
func (c *hubClient) session() bool { return c.cred != nil && c.cred.Kind == credentialSession }

// fail prints why a call failed and returns the exit code: exitLoggedIn when the hub wants a
// login (none, an ended session, an app's credentials refused), else exitFailed.
func (c *hubClient) fail(stderr io.Writer, cmd string, err error) int {
	login := "gravel login --local --hub " + c.hub
	switch {
	case errors.Is(err, hubclient.ErrInvalidClient):
		fmt.Fprintf(stderr, "gravel %s: %s refused the app's client credentials (revoked, or a wrong secret); %s\n", cmd, c.hub, "gravel login --app again with a new secret")
		return exitLoggedIn
	case connect.CodeOf(err) == connect.CodeUnauthenticated && c.cred == nil:
		fmt.Fprintf(stderr, "gravel %s: not logged in to %s; %s\n", cmd, c.hub, login)
		return exitLoggedIn
	case connect.CodeOf(err) == connect.CodeUnauthenticated && c.session():
		fmt.Fprintf(stderr, "gravel %s: your session on %s has ended; %s\n", cmd, c.hub, login)
		return exitLoggedIn
	case connect.CodeOf(err) == connect.CodeUnauthenticated:
		fmt.Fprintf(stderr, "gravel %s: %s does not accept the app here: %s\n", cmd, c.hub, message(err))
		return exitFailed
	case connect.CodeOf(err) == connect.CodePermissionDenied:
		fmt.Fprintf(stderr, "gravel %s: refused: %s\n", cmd, message(err))
		return exitFailed
	}
	fmt.Fprintf(stderr, "gravel %s: %s\n", cmd, message(err))
	return exitFailed
}

// message is an error as a person reads it: a Connect error's message and code, without the
// wrapping.
func message(err error) string {
	var ce *connect.Error
	if errors.As(err, &ce) {
		if ce.Message() == "" {
			return ce.Code().String()
		}
		return ce.Message() + " (" + ce.Code().String() + ")"
	}
	return err.Error()
}

// cleartextWarning says when a login would send a password or a cookie over plain HTTP to
// another machine.
func cleartextWarning(stderr io.Writer, hub string) {
	u, err := url.Parse(hub)
	if err != nil || u.Scheme != "http" {
		return
	}
	host := u.Hostname()
	if ip := net.ParseIP(host); host == "localhost" || (ip != nil && ip.IsLoopback()) {
		return
	}
	fmt.Fprintf(stderr, "warning: %s is plain HTTP: the password and the session cross the network in the clear; use https\n", hub)
}
