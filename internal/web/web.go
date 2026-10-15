// Package web is the hub's browser surface: the pages (templ components over the hub's own
// Connect HTTP-JSON API, called in process) and the provider redirect flows (start, callback,
// logout, unlink, claim). htmx is served from the binary and swaps a page's main content on
// its forms; every page also works without it. A strict Content-Security-Policy allows no
// inline script or style, so a host's theme is a stylesheet route (/theme.css) built from
// tokens (gravel#7 stores them; the defaults apply until then). ADR-0005 has the reasoning.
//
// Every state-changing route is a POST that carries the session's CSRF token; the hub also runs
// net/http's cross-origin protection in front of everything. The start routes are GETs:
// starting an attempt changes nothing a member can see, and the callback binds to the browser
// that started it through the auth cookie, so a forced start is harmless.
package web

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/a-h/templ"
	"github.com/google/uuid"

	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
	"github.com/gravel-project/gravel/gen/gravel/hub/v1/hubv1connect"
	"github.com/gravel-project/gravel/internal/httpx"
	"github.com/gravel-project/gravel/internal/identity"
	"github.com/gravel-project/gravel/internal/ratelimit"
	"github.com/gravel-project/gravel/internal/session"
	"github.com/gravel-project/gravel/internal/web/templates"
)

//go:embed static
var files embed.FS

// flashTTL is how long a one-shot message waits for the next page.
const flashTTL = time.Minute

// CSP is the pages' Content-Security-Policy: scripts and styles from this origin only (htmx
// and the stylesheets), images over https, requests from htmx to this origin only. The admin
// dashboard (internal/web/admin, ADR-0029) is served under the same one.
const CSP = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' https: data:; connect-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'"

// Handler serves the pages and the flows.
type Handler struct {
	ids       *identity.Service
	sess      *session.Manager
	theme     ThemeSource
	logger    *slog.Logger
	static    http.Handler
	orgAPI    hubv1connect.OrganizationServiceClient
	idAPI     hubv1connect.IdentityServiceClient
	serverAPI hubv1connect.ServerServiceClient
	statsAPI  hubv1connect.StatsServiceClient
	modAPI    hubv1connect.ModerationServiceClient
	hostAPI   hubv1connect.ServerHostServiceClient
	leaseAPI  hubv1connect.ServerLeaseServiceClient
	appAPI    hubv1connect.AppServiceClient

	// Observe, when set, is told how every callback ended: result is "ok", "denied", "failed",
	// "invalid", "mismatch", "taken" or "error"; provider is a configured provider or "other";
	// intent is "unknown" on a failure, because the attempt that knew it is consumed by then. The
	// hub counts them.
	Observe func(provider string, intent identity.Intent, result string)
	// ObserveLocal, when set, is told how every local login and registration ended (ADR-0026):
	// "ok", "refused", "locked", "invalid", "rate_limited" or "error". The hub counts them.
	ObserveLocal func(result string)
	// LocalLimiter, when set, bounds local logins and registrations per client address: each
	// hashes a password.
	LocalLimiter *ratelimit.Limiter
	// ObserveAuthorization, when set, is told how every request at the authorization endpoint
	// ended: "issued" (a code went back to the app), "denied" (the member pressed Cancel),
	// "rejected" (an unknown app or redirect URI, or a request the app got an error for) or
	// "error". The hub counts them.
	ObserveAuthorization func(result string)
}

// New wires the pages. api is the hub's Connect API with the session middleware in front; the
// pages call it in process as the HTTP-JSON client every other client is. theme says how the
// pages look; nil reads the Organization settings through that same API.
func New(ids *identity.Service, sess *session.Manager, api http.Handler, theme ThemeSource, logger *slog.Logger) (*Handler, error) {
	static, err := fs.Sub(files, "static")
	if err != nil {
		return nil, fmt.Errorf("web: static: %w", err)
	}
	client := &http.Client{Transport: inProcess{api: api}}
	orgAPI := hubv1connect.NewOrganizationServiceClient(client, "http://hub", connect.WithProtoJSON())
	if theme == nil {
		theme = apiTheme{client: orgAPI}
	}
	return &Handler{
		ids: ids, sess: sess, theme: theme, logger: logger,
		static:    http.StripPrefix("/static/", http.FileServerFS(static)),
		orgAPI:    orgAPI,
		idAPI:     hubv1connect.NewIdentityServiceClient(client, "http://hub", connect.WithProtoJSON()),
		serverAPI: hubv1connect.NewServerServiceClient(client, "http://hub", connect.WithProtoJSON()),
		statsAPI:  hubv1connect.NewStatsServiceClient(client, "http://hub", connect.WithProtoJSON()),
		modAPI:    hubv1connect.NewModerationServiceClient(client, "http://hub", connect.WithProtoJSON()),
		hostAPI:   hubv1connect.NewServerHostServiceClient(client, "http://hub", connect.WithProtoJSON()),
		leaseAPI:  hubv1connect.NewServerLeaseServiceClient(client, "http://hub", connect.WithProtoJSON()),
		appAPI:    hubv1connect.NewAppServiceClient(client, "http://hub", connect.WithProtoJSON()),
	}, nil
}

// Mux is where Register adds the routes: an *http.ServeMux, or a test's recorder listing them.
type Mux interface {
	HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request))
}

// Register adds the routes to mux. None is under /admin, the admin dashboard's (ADR-0029).
func (h *Handler) Register(mux Mux) {
	mux.HandleFunc("GET /{$}", h.home)
	mux.HandleFunc("GET /login", h.login)
	mux.HandleFunc("GET /account", h.account)
	mux.HandleFunc("GET /servers", h.servers)
	mux.HandleFunc("GET /servers/{id}", h.server)
	mux.HandleFunc("GET /servers/{id}/recap", h.recap)
	mux.HandleFunc("GET /results/{id}", h.result)
	mux.HandleFunc("GET /results/{id}/demo/{map}", h.resultDemo)
	mux.HandleFunc("GET /boards", h.boards)
	mux.HandleFunc("GET /participation", h.participation)
	mux.HandleFunc("GET /profile", h.profile)
	mux.HandleFunc("GET /members/{id}", h.member)
	mux.HandleFunc("POST /profile/name", h.setName)
	mux.HandleFunc("POST /servers/{id}/moderate", h.moderate)
	mux.HandleFunc("POST /servers/{id}/host", h.host)
	mux.HandleFunc("GET /servers/{id}/log", h.auditLog)
	// People moved to the admin dashboard (gravel#203); an old link lands there.
	mux.HandleFunc("GET /members", http.RedirectHandler("/admin/people", http.StatusMovedPermanently).ServeHTTP)
	mux.HandleFunc("POST /auth/local/login", h.localLogin)
	mux.HandleFunc("GET /auth/local/register", h.registerForm)
	mux.HandleFunc("POST /auth/local/register", h.register)
	mux.HandleFunc("POST /account/local", h.setLocal)
	mux.HandleFunc("GET /auth/local/reset/{token}", h.resetForm)
	mux.HandleFunc("POST /auth/local/reset/{token}", h.reset)
	mux.HandleFunc("GET /auth/{provider}/start", h.startLogin)
	mux.HandleFunc("GET /auth/{provider}/link", h.startLink)
	mux.HandleFunc("GET /auth/{provider}/roles", h.startRoles)
	mux.HandleFunc("GET /auth/{provider}/callback", h.callback)
	mux.HandleFunc("POST /auth/logout", h.logout)
	mux.HandleFunc("POST /account/unlink", h.unlink)
	mux.HandleFunc("POST /account/claim", h.claim)
	mux.HandleFunc("POST /account/ownership/accept", h.acceptOwnership)
	mux.HandleFunc("POST /account/ownership/decline", h.endNomination)
	mux.HandleFunc("POST /account/ownership/cancel", h.endNomination)
	mux.HandleFunc("POST /account/apps/revoke", h.revokeApp)
	mux.HandleFunc("POST /account/prove", h.startProof)
	mux.HandleFunc("GET /account/prove/{id}", h.proofStatus)
	mux.HandleFunc("GET /oauth/authorize", h.authorize)
	mux.HandleFunc("POST /oauth/authorize", h.authorizeDecision)
	mux.HandleFunc("GET /theme.css", h.themeStylesheet)
	mux.HandleFunc("GET "+MapPlaceholderPath, mapPlaceholderImage)
	mux.HandleFunc("GET /static/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		h.static.ServeHTTP(w, r)
	})
}

// isHX reports whether htmx made the request; it then wants the page's content, not the page.
func isHX(r *http.Request) bool { return r.Header.Get("HX-Request") == "true" }

// home is the servers: what a visitor comes for, logged in or not.
func (h *Handler) home(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/servers", http.StatusSeeOther)
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	// return_to is where the login comes back to (the authorization endpoint, ADR-0015): only a
	// path on this origin, never another site.
	returnTo := r.URL.Query().Get("return_to")
	if !SafeReturnPath(returnTo) {
		returnTo = ""
	}
	if _, ok := session.FromContext(r.Context()); ok {
		if returnTo == "" {
			returnTo = "/account"
		}
		http.Redirect(w, r, returnTo, http.StatusSeeOther)
		return
	}
	if returnTo != "" {
		h.rememberReturn(w, returnTo)
	}
	h.loginPage(w, r, http.StatusOK, nil, "")
}

// loginPage renders the login page: the providers that sign members in, the provider this browser
// last used first, and the username and password form when the hub has local accounts. flash and
// username are a refused local login's.
func (h *Handler) loginPage(w http.ResponseWriter, r *http.Request, status int, flash *templates.Flash, username string) {
	p, b, err := h.page(r, "Log in")
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	if flash != nil {
		p.Flash = flash
	}
	last := ""
	if c, err := r.Cookie(h.sess.CookieName(session.CookieLastProvider)); err == nil {
		last = c.Value
	}
	lp := templates.LoginPage{Page: p.Page, Local: h.ids.LocalEnabled(), Username: username}
	for _, reg := range h.ids.Providers() {
		if reg.Login {
			pr := templates.Provider{Name: reg.Provider.Name(), DisplayName: reg.Provider.DisplayName(), LastUsed: reg.Provider.Name() == last}
			if pr.LastUsed {
				lp.Providers = append([]templates.Provider{pr}, lp.Providers...)
			} else {
				lp.Providers = append(lp.Providers, pr)
			}
		}
	}
	if lp.Local {
		lp.LocalFirst = last == identity.ProviderLocal || len(lp.Providers) == 0
		if policy, err := h.ids.LocalPolicy(r.Context()); err == nil {
			lp.Register = policy.RegistrationOpen
		} else {
			h.logger.WarnContext(r.Context(), "local account policy unreadable", "error", err.Error())
		}
	}
	h.render(w, r, status, b, templates.Login(lp), templates.LoginContent(lp))
}

// canLogIn reports whether anyone can sign in: a provider that logs in (not only links), or
// local accounts.
func (h *Handler) canLogIn() bool {
	if h.ids.LocalEnabled() {
		return true
	}
	for _, reg := range h.ids.Providers() {
		if reg.Login {
			return true
		}
	}
	return false
}

func (h *Handler) account(w http.ResponseWriter, r *http.Request) {
	if _, ok := session.FromContext(r.Context()); !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	ap, b, err := h.accountPage(r, nil)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	if ap.User == nil { // the session's user is gone
		h.sess.Clear(w)
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	h.render(w, r, http.StatusOK, b, templates.Account(ap), templates.AccountContent(ap))
}

// accountPage builds the account page from the API; flash, when set, replaces the cookie's.
func (h *Handler) accountPage(r *http.Request, flash *templates.Flash) (templates.AccountPage, *browser, error) {
	p, b, err := h.page(r, "Account")
	if err != nil {
		return templates.AccountPage{}, b, err
	}
	if flash != nil {
		p.Flash = flash
	}
	ap := templates.AccountPage{Page: p.Page}
	if p.User == nil {
		return ap, b, nil
	}
	grants, err := h.appAPI.ListAppGrants(withBrowserContext(r, b), connect.NewRequest(&hubv1.ListAppGrantsRequest{}))
	if err != nil {
		return ap, b, fmt.Errorf("ListAppGrants: %w", err)
	}
	for _, g := range grants.Msg.GetGrants() {
		a := templates.AppGrant{ClientID: g.GetClientId(), Name: g.GetAppName(), GrantedAt: g.GetGrantedAt().AsTime()}
		for _, s := range g.GetScopes() {
			a.Scopes = append(a.Scopes, templates.Scope{Name: s.GetName(), Description: s.GetDescription()})
		}
		ap.Apps = append(ap.Apps, a)
	}
	linked := map[string]bool{}
	for _, i := range p.identities {
		linked[i.Provider] = true
		i.ProviderDisplay = h.providerDisplay(i.Provider)
		i.CanUnlink = len(p.identities) > 1
		ap.Identities = append(ap.Identities, i)
	}
	for _, reg := range h.ids.Providers() {
		pr := templates.Provider{Name: reg.Provider.Name(), DisplayName: reg.Provider.DisplayName()}
		if !linked[pr.Name] {
			ap.LinkProviders = append(ap.LinkProviders, pr)
		}
		if _, ok := reg.Provider.(identity.Publisher); ok && reg.Login && linked[pr.Name] {
			ap.RoleProviders = append(ap.RoleProviders, pr)
		}
	}
	if h.ids.LocalEnabled() && !linked[identity.ProviderLocal] {
		policy, err := h.ids.LocalPolicy(r.Context())
		if err != nil {
			return ap, b, fmt.Errorf("local account policy: %w", err)
		}
		ap.Local = &templates.LocalForm{MinPasswordLength: policy.MinPasswordLength}
	}
	if err := h.proofOffer(r, b, &ap); err != nil {
		return ap, b, fmt.Errorf("proof servers: %w", err)
	}
	return ap, b, nil
}

func (h *Handler) startLogin(w http.ResponseWriter, r *http.Request) {
	if _, ok := session.FromContext(r.Context()); ok {
		http.Redirect(w, r, "/account", http.StatusSeeOther)
		return
	}
	h.start(w, r, identity.IntentLogin, nil)
}

func (h *Handler) startLink(w http.ResponseWriter, r *http.Request) {
	s, ok := session.FromContext(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	h.start(w, r, identity.IntentLink, &s.UserID)
}

// startRoles is the verification URL a Discord application points Linked Roles at: the member
// arrives from Discord, logged in to the hub or not, and the flow logs them in and publishes
// their linked accounts (ADR-0008 §4). It always runs, so it also refreshes what Discord knows.
func (h *Handler) startRoles(w http.ResponseWriter, r *http.Request) {
	h.start(w, r, identity.IntentRoles, nil)
}

func (h *Handler) start(w http.ResponseWriter, r *http.Request, intent identity.Intent, userID *uuid.UUID) {
	provider := r.PathValue("provider")
	b, err := h.ids.Begin(r.Context(), provider, intent, userID)
	switch {
	case errors.Is(err, identity.ErrUnknownProvider), errors.Is(err, identity.ErrLoginNotAllowed):
		h.fail(w, r, http.StatusNotFound, "No such login", "This hub has no such login provider.")
		return
	case errors.Is(err, identity.ErrNotPublisher):
		h.fail(w, r, http.StatusNotFound, "No linked roles", "This hub publishes no linked roles to that provider.")
		return
	case errors.Is(err, identity.ErrProviderFailed):
		h.logger.WarnContext(r.Context(), "provider could not start", "provider", provider, "error", err.Error(), "request_id", httpx.RequestIDFromContext(r.Context()))
		h.fail(w, r, http.StatusBadGateway, "Provider unavailable", "The provider could not start the login. Try again in a moment.")
		return
	case err != nil:
		h.serverError(w, r, err)
		return
	}
	http.SetCookie(w, h.sess.Cookie(session.CookieAuth, b.AttemptToken, time.Until(b.ExpiresAt)))
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, b.AuthURL, http.StatusSeeOther)
}

func (h *Handler) callback(w http.ResponseWriter, r *http.Request) {
	provider := r.PathValue("provider")
	token := ""
	if c, err := r.Cookie(h.sess.CookieName(session.CookieAuth)); err == nil {
		token = c.Value
	}
	http.SetCookie(w, h.sess.Cookie(session.CookieAuth, "", 0)) // one callback per attempt, whatever happens
	returnTo := h.takeReturn(w, r)                              // a login started from the authorization endpoint goes back to it
	current, loggedIn := session.FromContext(r.Context())
	var sessionUser *uuid.UUID
	if loggedIn {
		sessionUser = &current.UserID
	}
	done, err := h.ids.Complete(r.Context(), token, provider, r.URL.Query(), sessionUser)
	if err != nil {
		h.observe(provider, "unknown", authResult(err))
		h.authError(w, r, provider, err)
		return
	}
	h.observe(provider, done.Intent, "ok")
	display := done.Identity.DisplayName
	if display == "" {
		display = done.Identity.Subject
	}
	switch done.Intent {
	case identity.IntentLink:
		h.setFlash(w, "ok", h.providerDisplay(provider)+" account "+display+" linked.")
	case identity.IntentRoles:
		// A login, except that the member's own session stays when they were already in it.
		if !loggedIn || current.UserID != done.User.ID {
			if loggedIn {
				if err := h.sess.Revoke(r.Context(), w, current); err != nil {
					h.serverError(w, r, err)
					return
				}
			}
			if _, err := h.sess.Issue(r.Context(), w, done.User.ID); err != nil {
				h.serverError(w, r, err)
				return
			}
		}
		h.rolesFlash(w, provider, done)
	default:
		if loggedIn { // a fresh login replaces the session it was started from
			if err := h.sess.Revoke(r.Context(), w, current); err != nil {
				h.serverError(w, r, err)
				return
			}
		}
		if _, err := h.sess.Issue(r.Context(), w, done.User.ID); err != nil {
			h.serverError(w, r, err)
			return
		}
		h.rememberProvider(w, provider)
		if returnTo != "" {
			http.Redirect(w, r, returnTo, http.StatusSeeOther)
			return
		}
		if done.Registered {
			h.setFlash(w, "ok", "Welcome, "+done.User.DisplayName+". Your account is ready.")
		} else {
			h.setFlash(w, "ok", "Logged in as "+done.User.DisplayName+".")
		}
	}
	http.Redirect(w, r, "/account", http.StatusSeeOther)
}

// rolesFlash says what the provider now shows, or that it would not take it.
func (h *Handler) rolesFlash(w http.ResponseWriter, provider string, done identity.Completed) {
	name := h.providerDisplay(provider)
	if done.PublishErr != nil || done.Published == nil {
		h.setFlash(w, "err", "You are logged in, but "+name+" did not take the update to your linked roles. Try again in a moment.")
		return
	}
	var others []string
	for _, p := range done.Published.Providers {
		if p != provider {
			others = append(others, h.providerDisplay(p))
		}
	}
	if len(others) == 0 {
		h.setFlash(w, "ok", name+" now knows you have no other accounts linked. Link one here, then update your linked roles again.")
		return
	}
	h.setFlash(w, "ok", name+" now knows your linked accounts: "+strings.Join(others, ", ")+". Roles that need them update in "+name+".")
}

// observe reports a callback's end. The provider comes from the request path, so a name the
// hub doesn't have is reported as "other": anonymous requests can't mint new metric series.
func (h *Handler) observe(provider string, intent identity.Intent, result string) {
	if h.Observe == nil {
		return
	}
	if _, ok := h.ids.Provider(provider); !ok {
		provider = "other"
	}
	h.Observe(provider, intent, result)
}

// authResult names a callback failure for the metrics.
func authResult(err error) string {
	switch {
	case errors.Is(err, identity.ErrProviderDenied):
		return "denied"
	case errors.Is(err, identity.ErrProviderFailed):
		return "failed"
	case errors.Is(err, identity.ErrAttemptInvalid), errors.Is(err, identity.ErrWrongUser), errors.Is(err, identity.ErrUnknownProvider), errors.Is(err, identity.ErrLoginNotAllowed), errors.Is(err, identity.ErrNotPublisher):
		return "invalid"
	case errors.Is(err, identity.ErrStateMismatch):
		return "mismatch"
	case errors.Is(err, identity.ErrIdentityTaken):
		return "taken"
	}
	return "error"
}

func (h *Handler) authError(w http.ResponseWriter, r *http.Request, provider string, err error) {
	name := h.providerDisplay(provider)
	switch {
	case errors.Is(err, identity.ErrAttemptInvalid):
		h.fail(w, r, http.StatusBadRequest, "Login attempt expired", "This login attempt expired or was already used. Start again.")
	case errors.Is(err, identity.ErrStateMismatch):
		h.fail(w, r, http.StatusBadRequest, "Login attempt mismatch", "The response from "+name+" does not match the login this browser started. Start again.")
	case errors.Is(err, identity.ErrWrongUser):
		h.fail(w, r, http.StatusForbidden, "Different login", "This link was started from a different login. Log in again and retry.")
	case errors.Is(err, identity.ErrProviderDenied):
		h.fail(w, r, http.StatusBadRequest, "Cancelled", "You cancelled at "+name+".")
	case errors.Is(err, identity.ErrIdentityTaken):
		h.fail(w, r, http.StatusConflict, "Already linked", "That "+name+" account is already linked to another member.")
	case errors.Is(err, identity.ErrUnknownProvider), errors.Is(err, identity.ErrLoginNotAllowed), errors.Is(err, identity.ErrNotPublisher):
		h.fail(w, r, http.StatusNotFound, "No such login", "This hub has no such login provider.")
	case errors.Is(err, identity.ErrProviderFailed):
		h.logger.WarnContext(r.Context(), "provider failed", "provider", provider, "error", err.Error(), "request_id", httpx.RequestIDFromContext(r.Context()))
		h.fail(w, r, http.StatusBadGateway, "Provider failed", name+" could not complete the login. Try again in a moment.")
	default:
		h.serverError(w, r, err)
	}
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(w, r) {
		return
	}
	ctx, b := withBrowser(r)
	var flash string
	if r.PostFormValue("everywhere") == "1" {
		resp, err := h.idAPI.RevokeSessions(ctx, connect.NewRequest(&hubv1.RevokeSessionsRequest{}))
		if err != nil {
			h.apiError(w, r, b, err)
			return
		}
		flash = fmt.Sprintf("Logged out everywhere (%d sessions).", resp.Msg.GetRevoked())
	} else {
		if _, err := h.idAPI.Logout(ctx, connect.NewRequest(&hubv1.LogoutRequest{})); err != nil {
			h.apiError(w, r, b, err)
			return
		}
		flash = "Logged out."
	}
	b.relay(w) // the API cleared the session cookie
	h.setFlash(w, "ok", flash)
	h.redirect(w, r, "/login")
}

func (h *Handler) unlink(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(w, r) {
		return
	}
	ctx, b := withBrowser(r)
	provider := r.PostFormValue("provider")
	_, err := h.idAPI.UnlinkIdentity(ctx, connect.NewRequest(&hubv1.UnlinkIdentityRequest{Provider: provider, Subject: r.PostFormValue("subject")}))
	var flash templates.Flash
	switch connect.CodeOf(err) {
	case connect.CodeFailedPrecondition:
		flash = templates.Flash{Kind: "err", Text: "You cannot unlink your only account."}
	case connect.CodeNotFound:
		flash = templates.Flash{Kind: "err", Text: "That account is not linked to you."}
	case connect.CodeInvalidArgument:
		flash = templates.Flash{Kind: "err", Text: "Choose an account to unlink."}
	default:
		if err != nil {
			h.apiError(w, r, b, err)
			return
		}
		flash = templates.Flash{Kind: "ok", Text: h.providerDisplay(provider) + " account unlinked."}
	}
	h.accountDone(w, r, b, flash)
}

func (h *Handler) claim(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(w, r) {
		return
	}
	ctx, b := withBrowser(r)
	_, err := h.orgAPI.ClaimOwnership(ctx, connect.NewRequest(&hubv1.ClaimOwnershipRequest{Token: r.PostFormValue("token")}))
	var flash templates.Flash
	switch connect.CodeOf(err) {
	case connect.CodeFailedPrecondition:
		flash = templates.Flash{Kind: "err", Text: "This hub already has an owner."}
	case connect.CodePermissionDenied:
		flash = templates.Flash{Kind: "err", Text: "That token is not valid or has expired. The hub logs a fresh one at every start while it is unowned."}
	case connect.CodeInvalidArgument:
		flash = templates.Flash{Kind: "err", Text: "Paste the owner-claim token first."}
	default:
		if err != nil {
			h.apiError(w, r, b, err)
			return
		}
		flash = templates.Flash{Kind: "ok", Text: "You now own this hub."}
	}
	h.accountDone(w, r, b, flash)
}

// acceptOwnership takes the hub over, for the member the owner nominated (#197).
func (h *Handler) acceptOwnership(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(w, r) {
		return
	}
	ctx, b := withBrowser(r)
	_, err := h.orgAPI.AcceptOwnership(ctx, connect.NewRequest(&hubv1.AcceptOwnershipRequest{}))
	var flash templates.Flash
	switch connect.CodeOf(err) {
	case connect.CodeFailedPrecondition:
		flash = templates.Flash{Kind: "err", Text: "You can't accept: " + connectMessage(err) + "."}
	default:
		if err != nil {
			h.apiError(w, r, b, err)
			return
		}
		flash = templates.Flash{Kind: "ok", Text: "You own this hub now. The old owner stays on as an admin."}
	}
	h.accountDone(w, r, b, flash)
}

// endNomination ends a nomination to take the hub over: the nominee declines it, the owner
// cancels it (#197). The API tells the two apart.
func (h *Handler) endNomination(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(w, r) {
		return
	}
	ctx, b := withBrowser(r)
	_, err := h.orgAPI.CancelOwnershipTransfer(ctx, connect.NewRequest(&hubv1.CancelOwnershipTransferRequest{}))
	var flash templates.Flash
	switch connect.CodeOf(err) {
	case connect.CodeFailedPrecondition, connect.CodePermissionDenied:
		flash = templates.Flash{Kind: "err", Text: "There is no nomination of yours to end."}
	default:
		if err != nil {
			h.apiError(w, r, b, err)
			return
		}
		flash = templates.Flash{Kind: "ok", Text: "Nomination cancelled: nothing changed."}
		if strings.HasSuffix(r.URL.Path, "/decline") {
			flash.Text = "You declined: nothing changed."
		}
	}
	h.accountDone(w, r, b, flash)
}

// accountDone ends a form on the account page: htmx gets the fresh content with the flash
// inline, a plain browser gets the flash as a cookie and a redirect.
func (h *Handler) accountDone(w http.ResponseWriter, r *http.Request, b *browser, flash templates.Flash) {
	if !isHX(r) {
		b.relay(w)
		h.setFlash(w, flash.Kind, flash.Text)
		http.Redirect(w, r, "/account", http.StatusSeeOther)
		return
	}
	ap, b2, err := h.accountPage(r, &flash)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	b.relay(w)
	h.render(w, r, http.StatusOK, b2, templates.Account(ap), templates.AccountContent(ap))
}

// redirect sends a plain browser on, and tells htmx to navigate.
func (h *Handler) redirect(w http.ResponseWriter, r *http.Request, to string) {
	if isHX(r) {
		w.Header().Set("HX-Redirect", to)
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
}

// apiError answers an API failure a form did not expect: a lost session goes back to login,
// anything else is a server error (the API already logged it with the request id).
func (h *Handler) apiError(w http.ResponseWriter, r *http.Request, b *browser, err error) {
	b.relay(w)
	if connect.CodeOf(err) == connect.CodeUnauthenticated {
		h.sess.Clear(w)
		h.redirect(w, r, "/login")
		return
	}
	h.serverError(w, r, err)
}

// authorized requires a session and the CSRF token on a state-changing form.
func (h *Handler) authorized(w http.ResponseWriter, r *http.Request) bool {
	s, ok := session.FromContext(r.Context())
	if !ok {
		h.redirect(w, r, "/login")
		return false
	}
	if !h.sess.CheckCSRF(r, s) {
		h.fail(w, r, http.StatusForbidden, "Form expired", "This form did not carry a valid token. Go back and try again.")
		return false
	}
	return true
}

func (h *Handler) providerDisplay(name string) string {
	if name == identity.ProviderLocal {
		return "Username and password"
	}
	if reg, ok := h.ids.Provider(name); ok {
		return reg.Provider.DisplayName()
	}
	return name
}

// pageData is a Page plus the identities the API returned, before the account page shapes them.
type pageData struct {
	templates.Page
	identities []templates.Identity
}

// page builds what every page starts from, through the API: the organization, the session's
// user (nil when anonymous or gone) with their identities, the theme and its links, and the
// pending flash.
func (h *Handler) page(r *http.Request, title string) (pageData, *browser, error) {
	ctx, b := withBrowser(r)
	p := pageData{Page: templates.Page{Title: title, CanLogIn: h.canLogIn()}}
	org, err := h.orgAPI.GetOrganization(ctx, connect.NewRequest(&hubv1.GetOrganizationRequest{}))
	if err != nil {
		return p, b, fmt.Errorf("GetOrganization: %w", err)
	}
	o := org.Msg.GetOrganization()
	p.Org = templates.Org{Name: o.GetName(), Owned: o.GetOwned(), Demo: o.GetDemo()}
	if id := o.GetPendingOwnerUserId(); id != "" {
		exp := o.GetPendingOwnerExpiresAt().AsTime()
		p.Org.Pending = &templates.PendingOwner{UserID: id, Name: o.GetPendingOwnerName(), ExpiresAt: exp, Expired: !exp.After(time.Now())}
	}
	if s, ok := session.FromContext(r.Context()); ok {
		me, err := h.idAPI.GetMe(ctx, connect.NewRequest(&hubv1.GetMeRequest{}))
		switch {
		case err == nil:
			u := me.Msg.GetUser()
			p.User = &templates.User{ID: u.GetId(), DisplayName: u.GetDisplayName(), Owner: u.GetOwner(),
				Moderator: holdsSomewhere(u, "servers:moderate", "moderator", "admin"), CanGrant: holdsSomewhere(u, "roles:grant", "admin")}
			p.CSRF = s.CSRFToken
			for _, i := range u.GetIdentities() {
				v := templates.Identity{Provider: i.GetProvider(), Subject: i.GetSubject(), DisplayName: i.GetDisplayName(), AvatarURL: i.GetAvatarUrl(), Method: i.GetVerificationMethod(), VerifiedAt: i.GetVerifiedAt().AsTime()}
				if i.GetLastLoginAt() != nil {
					t := i.GetLastLoginAt().AsTime()
					v.LastLoginAt = &t
				}
				p.identities = append(p.identities, v)
			}
		case connect.CodeOf(err) == connect.CodeUnauthenticated, connect.CodeOf(err) == connect.CodeNotFound:
			// The session's user is gone; the page stays anonymous and the handler redirects.
		default:
			return p, b, fmt.Errorf("GetMe: %w", err)
		}
	}
	t, err := h.theme.Theme(ctx)
	if err != nil {
		h.logger.WarnContext(ctx, "theme unavailable, using the defaults", "error", err.Error())
		t = DefaultTheme()
	}
	p.Theme = normalizeTheme(t)
	for _, l := range p.Theme.Nav {
		if l.Role == "owner" && (p.User == nil || !p.User.Owner) {
			continue
		}
		if l.Placement == "footer" {
			p.FooterNav = append(p.FooterNav, l)
		} else {
			p.HeaderNav = append(p.HeaderNav, l)
		}
	}
	if c, err := r.Cookie(h.sess.CookieName(session.CookieFlash)); err == nil && c.Value != "" {
		if kind, text, ok := strings.Cut(c.Value, ":"); ok {
			if text, err := url.QueryUnescape(text); err == nil && (kind == "ok" || kind == "err") {
				p.Flash = &templates.Flash{Kind: kind, Text: text}
			}
		}
	}
	return p, b, nil
}

// withGame makes p one of a game's pages: its content renders inside the game's overlay (its
// accent, banner and links, gravel#66), or plainly when the settings give the game none. name is
// the game's display name, the label of its links' navigation.
func withGame(p *templates.Page, id, name string) {
	g := &templates.Game{ID: id, Name: name}
	if o, ok := p.Theme.Games[id]; ok {
		g.BannerURL, g.BannerAlt = o.BannerURL, o.BannerAlt
		for _, l := range o.Nav {
			if l.Role == "owner" && (p.User == nil || !p.User.Owner) {
				continue
			}
			g.Nav = append(g.Nav, l)
		}
	}
	p.Game = g
}

func (h *Handler) setFlash(w http.ResponseWriter, kind, text string) {
	http.SetCookie(w, h.sess.Cookie(session.CookieFlash, kind+":"+url.QueryEscape(text), flashTTL))
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, status int, title, message string) {
	p, b, err := h.page(r, title)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	ep := templates.ErrorPage{Page: p.Page, Message: message}
	h.render(w, r, status, b, templates.Error(ep), templates.ErrorContent(ep))
}

func (h *Handler) serverError(w http.ResponseWriter, r *http.Request, err error) {
	h.logger.ErrorContext(r.Context(), "page failed", "error", err.Error(), "path", r.URL.Path, "request_id", httpx.RequestIDFromContext(r.Context()))
	http.Error(w, "Something went wrong on our side. Try again in a moment.", http.StatusInternalServerError)
}

// render writes a page, or only its content for htmx, after relaying what the API set.
func (h *Handler) render(w http.ResponseWriter, r *http.Request, status int, b *browser, full, content templ.Component) {
	h.renderWithPolicy(w, r, status, b, full, content, CSP)
}

// renderWithPolicy is render with a page's own Content-Security-Policy.
func (h *Handler) renderWithPolicy(w http.ResponseWriter, r *http.Request, status int, b *browser, full, content templ.Component, policy string) {
	c := full
	if isHX(r) {
		c = content
	}
	var buf bytes.Buffer
	if err := c.Render(context.WithoutCancel(r.Context()), &buf); err != nil {
		h.serverError(w, r, err)
		return
	}
	if b != nil {
		b.relay(w)
	}
	if c, err := r.Cookie(h.sess.CookieName(session.CookieFlash)); err == nil && c.Value != "" {
		http.SetCookie(w, h.sess.Cookie(session.CookieFlash, "", 0))
	}
	hdr := w.Header()
	hdr.Set("Content-Type", "text/html; charset=utf-8")
	hdr.Set("Content-Security-Policy", policy)
	if hdr.Get("Referrer-Policy") == "" { // a page with a secret in its URL sets no-referrer first
		hdr.Set("Referrer-Policy", "same-origin")
	}
	hdr.Set("X-Content-Type-Options", "nosniff")
	hdr.Set("Cache-Control", "no-store")
	hdr.Set("Vary", "HX-Request, Cookie")
	w.WriteHeader(status)
	_, _ = buf.WriteTo(w)
}

// holdsSomewhere reports whether a member holds a capability instance-wide (capabilities) or for
// some game (a game-scoped grant of one of roles, whose bundle has it; ADR-0027 §4).
func holdsSomewhere(u *hubv1.User, capability string, roles ...string) bool {
	if u.GetOwner() || slices.Contains(u.GetCapabilities(), capability) {
		return true
	}
	return slices.ContainsFunc(u.GetGrants(), func(g *hubv1.RoleGrant) bool { return g.GetGameId() != "" && slices.Contains(roles, g.GetRole()) })
}
