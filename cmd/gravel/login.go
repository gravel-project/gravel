package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"connectrpc.com/connect"
	"golang.org/x/term"

	"github.com/gravel-project/gravel/discord/hubclient"
	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
)

// sessionCookies are the names the hub gives its session cookie: with the __Host- prefix behind
// HTTPS, without it on a plain-HTTP development hub (internal/session).
var sessionCookies = []string{"__Host-gravel_session", "gravel_session"}

func loginCmd(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := newFlagSet("login", stderr)
	hf := addHubFlag(fs)
	local := fs.Bool("local", false, "log in with a local account's username and password (ADR-0026)")
	app := fs.Bool("app", false, "call the hub as a registered app: --client-id and --secret-file")
	username := fs.String("username", "", "local: the username (asked for when not given)")
	clientID := fs.String("client-id", "", "app: the app's client id (gravel-hub apps create printed it)")
	secretFile := fs.String("secret-file", "", "app: the file only you can read that holds the app's client secret")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *local == *app || fs.NArg() > 0 {
		fmt.Fprintln(stderr, "usage: gravel login --local [--username NAME] [--hub URL]\n       gravel login --app --client-id ID --secret-file FILE [--hub URL]")
		return exitUsage
	}
	fail := func(err error) int { fmt.Fprintln(stderr, "gravel login:", err); return exitFailed }
	path, err := credentialsPath()
	if err != nil {
		return fail(err)
	}
	creds, err := loadCredentials(path)
	if err != nil {
		return fail(err)
	}
	hub, err := hf.resolve(creds)
	if err != nil {
		return fail(err)
	}
	var cr credential
	if *local {
		cr, err = loginLocal(ctx, hub, *username, stdin, stdout, stderr)
	} else {
		cr, err = loginApp(ctx, hub, *clientID, *secretFile, stdout)
	}
	if err != nil {
		return fail(err)
	}
	creds.put(cr)
	if err := creds.save(path); err != nil {
		return fail(err)
	}
	return exitOK
}

// loginLocal posts the username and password to the hub's login form, as a browser does, and
// keeps the session cookie it answers with. The form is the one place the hub checks a password,
// with its lockout and its limit per address (ADR-0026), so the CLI drives it rather than a
// second way in.
func loginLocal(ctx context.Context, hub, username string, stdin io.Reader, stdout, stderr io.Writer) (credential, error) {
	cleartextWarning(stderr, hub)
	in := bufio.NewReader(stdin)
	if username == "" {
		fmt.Fprint(stderr, "Username: ")
		line, err := in.ReadString('\n')
		if err != nil && line == "" {
			return credential{}, errors.New("no username")
		}
		username = strings.TrimSpace(line)
	}
	password, err := readPassword(in, stdin, stderr)
	if err != nil {
		return credential{}, err
	}
	if username == "" || password == "" {
		return credential{}, errors.New("a username and a password are required")
	}
	cookie, err := postLocalLogin(ctx, hub, username, password)
	if err != nil {
		return credential{}, err
	}
	cr := credential{HubURL: hub, Kind: credentialSession, Cookie: cookie}
	c := &hubClient{hub: hub}
	if err := c.authenticate(cr); err != nil {
		return credential{}, err
	}
	me, err := c.identity().GetMe(ctx, connect.NewRequest(&hubv1.GetMeRequest{}))
	if err != nil {
		return credential{}, fmt.Errorf("the hub logged you in but GetMe failed: %s", message(err))
	}
	fmt.Fprintf(stdout, "Logged in to %s as %s.\n", hub, me.Msg.GetUser().GetDisplayName())
	return cr, nil
}

// readPassword asks for the password without echo on a terminal; otherwise it reads the next
// line of stdin, for a script that pipes it in.
func readPassword(in *bufio.Reader, stdin io.Reader, stderr io.Writer) (string, error) {
	if f, ok := stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) { //nolint:gosec // a file descriptor fits an int
		fmt.Fprint(stderr, "Password: ")
		b, err := term.ReadPassword(int(f.Fd())) //nolint:gosec // as above
		fmt.Fprintln(stderr)
		if err != nil {
			return "", fmt.Errorf("reading the password: %w", err)
		}
		return string(b), nil
	}
	line, err := in.ReadString('\n')
	if err != nil && line == "" {
		return "", errors.New("no password on stdin")
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// postLocalLogin answers the session cookie the hub set, as name=value.
func postLocalLogin(ctx context.Context, hub, username, password string) (string, error) {
	form := url.Values{"username": {username}, "password": {password}}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, hub+"/auth/local/login", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := plainHTTP().Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return "", errors.New("that username and password don't match an account that may log in now (or it is locked after many tries)")
	case resp.StatusCode == http.StatusTooManyRequests:
		return "", fmt.Errorf("too many tries from here; wait %s seconds and try again", resp.Header.Get("Retry-After"))
	case resp.StatusCode == http.StatusNotFound:
		return "", fmt.Errorf("%s has no username and password login", hub)
	case resp.StatusCode < 300 || resp.StatusCode >= 400:
		return "", fmt.Errorf("the hub answered the login with %s", resp.Status)
	}
	for _, c := range resp.Cookies() {
		if slices.Contains(sessionCookies, c.Name) && c.Value != "" && c.MaxAge >= 0 {
			return c.Name + "=" + c.Value, nil
		}
	}
	return "", fmt.Errorf("the hub answered the login with %s but set no session (is %s a gravel hub?)", resp.Status, hub)
}

// loginApp checks the app's client credentials by fetching a token, and keeps where the secret
// is, never the secret.
func loginApp(ctx context.Context, hub, clientID, secretFile string, stdout io.Writer) (credential, error) {
	if clientID == "" || secretFile == "" {
		return credential{}, errors.New("--app needs --client-id and --secret-file")
	}
	abs, err := filepath.Abs(secretFile)
	if err != nil {
		return credential{}, err
	}
	secret, err := readSecretFile(abs)
	if err != nil {
		return credential{}, fmt.Errorf("the app's secret: %w", err)
	}
	app, err := hubclient.New(hubclient.Config{URL: hub, ClientID: clientID, ClientSecret: secret, HTTPClient: plainHTTP()})
	if err != nil {
		return credential{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	if _, err := app.Token(ctx); err != nil {
		return credential{}, err
	}
	fmt.Fprintf(stdout, "Logged in to %s as the app %s.\n", hub, clientID)
	return credential{HubURL: hub, Kind: credentialApp, ClientID: clientID, SecretFile: abs}, nil
}

// logoutCmd ends the session on the hub and forgets the hub's credential on this machine.
func logoutCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("logout", stderr)
	hf := addHubFlag(fs)
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintln(stderr, "usage: gravel logout [--hub URL]")
		return exitUsage
	}
	fail := func(err error) int { fmt.Fprintln(stderr, "gravel logout:", err); return exitFailed }
	path, err := credentialsPath()
	if err != nil {
		return fail(err)
	}
	creds, err := loadCredentials(path)
	if err != nil {
		return fail(err)
	}
	hub, err := hf.resolve(creds)
	if err != nil {
		return fail(err)
	}
	cr, ok := creds.find(hub)
	if !ok {
		fmt.Fprintf(stdout, "Not logged in to %s.\n", hub)
		return exitOK
	}
	if cr.Kind == credentialSession {
		c := &hubClient{hub: hub}
		if err := c.authenticate(cr); err == nil {
			ctx, cancel := context.WithTimeout(ctx, requestTimeout)
			_, err = c.identity().Logout(ctx, connect.NewRequest(&hubv1.LogoutRequest{}))
			cancel()
			if err != nil && connect.CodeOf(err) != connect.CodeUnauthenticated {
				fmt.Fprintf(stderr, "warning: could not end the session on %s (%s); forgetting it here anyway\n", hub, message(err))
			}
		}
	}
	creds.remove(hub)
	if err := creds.save(path); err != nil {
		return fail(err)
	}
	fmt.Fprintf(stdout, "Logged out of %s.\n", hub)
	return exitOK
}

// whoamiCmd says who this machine is on the hub, and what they may do there.
func whoamiCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("whoami", stderr)
	hf := addHubFlag(fs)
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintln(stderr, "usage: gravel whoami [--hub URL]")
		return exitUsage
	}
	c, err := openHub(hf)
	if err != nil {
		fmt.Fprintln(stderr, "gravel whoami:", err)
		return exitFailed
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	if c.cred != nil && c.cred.Kind == credentialApp {
		// An app has no GetMe; listing the servers proves its token works.
		if _, err := c.servers().ListServers(ctx, connect.NewRequest(&hubv1.ListServersRequest{})); err != nil {
			return c.fail(stderr, "whoami", err)
		}
		tw := table(stdout)
		fmt.Fprintf(tw, "hub\t%s\n", c.hub)
		fmt.Fprintf(tw, "app\t%s\n", c.cred.ClientID)
		fmt.Fprintf(tw, "secret\t%s\n", c.cred.SecretFile)
		return flush(tw, stderr)
	}
	me, err := c.identity().GetMe(ctx, connect.NewRequest(&hubv1.GetMeRequest{}))
	if err != nil {
		return c.fail(stderr, "whoami", err)
	}
	u := me.Msg.GetUser()
	tw := table(stdout)
	fmt.Fprintf(tw, "hub\t%s\n", c.hub)
	fmt.Fprintf(tw, "member\t%s (%s)\n", u.GetDisplayName(), u.GetId())
	fmt.Fprintf(tw, "owner\t%s\n", yesNo(u.GetOwner()))
	fmt.Fprintf(tw, "roles\t%s\n", orNone(u.GetRoles()))
	var scoped []string
	for _, g := range u.GetGrants() {
		if g.GetGameId() != "" {
			scoped = append(scoped, g.GetRole()+" of "+g.GetGameId())
		}
	}
	if len(scoped) > 0 {
		fmt.Fprintf(tw, "game roles\t%s\n", strings.Join(scoped, ", "))
	}
	for _, id := range u.GetIdentities() {
		name := id.GetDisplayName()
		if name == "" {
			name = id.GetSubject()
		}
		fmt.Fprintf(tw, "identity\t%s %s\n", id.GetProvider(), name)
	}
	fmt.Fprintf(tw, "capabilities\t%s\n", orNone(u.GetCapabilities()))
	return flush(tw, stderr)
}

// claimCmd makes the logged-in member the hub's owner with the one-time token the hub printed at
// start (ADR-0002).
func claimCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	const usage = "usage: gravel claim [--hub URL] [--] TOKEN   (gravel hub status prints the token while the hub is unowned; -- before a token that starts with '-')"
	fs := newFlagSet("claim", stderr)
	hf := addHubFlag(fs)
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(stderr, usage) // a token starting with '-' lands here, read as a flag
		return exitUsage
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, usage)
		return exitUsage
	}
	c, err := openHub(hf)
	if err != nil {
		fmt.Fprintln(stderr, "gravel claim:", err)
		return exitFailed
	}
	if c.cred != nil && !c.session() {
		fmt.Fprintf(stderr, "gravel claim: a hub's owner is a member, not an app; gravel login --local --hub %s first\n", c.hub)
		return exitLoggedIn
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	res, err := c.organization().ClaimOwnership(ctx, connect.NewRequest(&hubv1.ClaimOwnershipRequest{Token: fs.Arg(0)}))
	switch connect.CodeOf(err) {
	case connect.CodeFailedPrecondition:
		fmt.Fprintf(stderr, "gravel claim: %s already has an owner\n", c.hub)
		return exitFailed
	case connect.CodePermissionDenied:
		fmt.Fprintln(stderr, "gravel claim: the hub refused the token: it is wrong, used, or expired (a restart mints a new one)")
		return exitFailed
	}
	if err != nil {
		return c.fail(stderr, "claim", err)
	}
	fmt.Fprintf(stdout, "You own %s (%s) now.\n", res.Msg.GetOrganization().GetName(), c.hub)
	return exitOK
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func orNone(s []string) string {
	if len(s) == 0 {
		return "none"
	}
	return strings.Join(s, ", ")
}
