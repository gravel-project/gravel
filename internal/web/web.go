// Package web is the hub's browser surface: the login and account pages and the provider
// redirect flows (start, callback, logout, unlink, claim). The pages are deliberately plain:
// html/template, one stylesheet, no script, a strict Content-Security-Policy. gravel#14 restyles
// them with templ and htmx and keeps these routes; the flows under /auth/ are the login the
// issue describes and are not a page concern.
//
// Every state-changing route is a POST that carries the session's CSRF token; the hub also runs
// net/http's cross-origin protection in front of everything. The start routes are GETs: starting
// an attempt changes nothing a member can see, and the callback binds to the browser that
// started it through the auth cookie, so a forced start is harmless.
package web

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/gravel-project/gravel/internal/httpx"
	"github.com/gravel-project/gravel/internal/identity"
	"github.com/gravel-project/gravel/internal/org"
	"github.com/gravel-project/gravel/internal/session"
	"github.com/gravel-project/gravel/internal/store"
)

//go:embed templates/*.html static/*
var files embed.FS

// flashTTL is how long a one-shot message waits for the next page.
const flashTTL = time.Minute

// Handler serves the pages and the flows.
type Handler struct {
	ids    *identity.Service
	sess   *session.Manager
	org    *org.Service
	logger *slog.Logger
	pages  map[string]*template.Template
	static http.Handler

	// Observe, when set, is told how every callback ended: result is "ok", "denied", "failed",
	// "invalid", "mismatch", "taken" or "error"; intent is "unknown" on a failure, because the
	// attempt that knew it is consumed by then. The hub counts them.
	Observe func(provider string, intent identity.Intent, result string)
}

// New parses the templates and wires the services.
func New(ids *identity.Service, sess *session.Manager, orgSvc *org.Service, logger *slog.Logger) (*Handler, error) {
	h := &Handler{ids: ids, sess: sess, org: orgSvc, logger: logger, pages: map[string]*template.Template{}}
	for _, name := range []string{"login", "account", "error"} {
		t, err := template.New("layout.html").ParseFS(files, "templates/layout.html", "templates/"+name+".html")
		if err != nil {
			return nil, fmt.Errorf("web: template %s: %w", name, err)
		}
		h.pages[name] = t
	}
	static, err := fs.Sub(files, "static")
	if err != nil {
		return nil, fmt.Errorf("web: static: %w", err)
	}
	h.static = http.StripPrefix("/static/", http.FileServerFS(static))
	return h, nil
}

// Register adds the routes to mux.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /{$}", h.home)
	mux.HandleFunc("GET /login", h.login)
	mux.HandleFunc("GET /account", h.account)
	mux.HandleFunc("GET /auth/{provider}/start", h.startLogin)
	mux.HandleFunc("GET /auth/{provider}/link", h.startLink)
	mux.HandleFunc("GET /auth/{provider}/callback", h.callback)
	mux.HandleFunc("POST /auth/logout", h.logout)
	mux.HandleFunc("POST /account/unlink", h.unlink)
	mux.HandleFunc("POST /account/claim", h.claim)
	mux.HandleFunc("GET /static/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		h.static.ServeHTTP(w, r)
	})
}

type flash struct {
	Kind string // "ok" or "err"
	Text string
}

type providerView struct {
	Name        string
	DisplayName string
}

type identityView struct {
	store.Identity
	ProviderDisplay string
	CanUnlink       bool
}

type page struct {
	Title          string
	Org            store.Organization
	User           *identity.User
	Owner          bool
	CSRF           string
	Flash          *flash
	LoginProviders []providerView
	LinkProviders  []providerView
	Identities     []identityView
	Message        string
}

func (h *Handler) home(w http.ResponseWriter, r *http.Request) {
	if _, ok := session.FromContext(r.Context()); ok {
		http.Redirect(w, r, "/account", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	if _, ok := session.FromContext(r.Context()); ok {
		http.Redirect(w, r, "/account", http.StatusSeeOther)
		return
	}
	p, err := h.page(r, "Log in")
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	for _, reg := range h.ids.Providers() {
		if reg.Login {
			p.LoginProviders = append(p.LoginProviders, providerView{Name: reg.Provider.Name(), DisplayName: reg.Provider.DisplayName()})
		}
	}
	h.render(w, r, http.StatusOK, "login", p)
}

func (h *Handler) account(w http.ResponseWriter, r *http.Request) {
	s, ok := session.FromContext(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	p, err := h.page(r, "Account")
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	if p.User == nil { // the session's user is gone; the cookie is already cleared
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	p.CSRF = s.CSRFToken
	linked := map[string]bool{}
	for _, i := range p.User.Identities {
		linked[i.Provider] = true
		display := i.Provider
		if reg, ok := h.ids.Provider(i.Provider); ok {
			display = reg.Provider.DisplayName()
		}
		p.Identities = append(p.Identities, identityView{Identity: i, ProviderDisplay: display, CanUnlink: len(p.User.Identities) > 1})
	}
	for _, reg := range h.ids.Providers() {
		if !linked[reg.Provider.Name()] {
			p.LinkProviders = append(p.LinkProviders, providerView{Name: reg.Provider.Name(), DisplayName: reg.Provider.DisplayName()})
		}
	}
	h.render(w, r, http.StatusOK, "account", p)
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

func (h *Handler) start(w http.ResponseWriter, r *http.Request, intent identity.Intent, userID *uuid.UUID) {
	provider := r.PathValue("provider")
	b, err := h.ids.Begin(r.Context(), provider, intent, userID)
	switch {
	case errors.Is(err, identity.ErrUnknownProvider), errors.Is(err, identity.ErrLoginNotAllowed):
		h.fail(w, r, http.StatusNotFound, "No such login", "This hub has no such login provider.")
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
		if done.Registered {
			h.setFlash(w, "ok", "Welcome, "+done.User.DisplayName+". Your account is ready.")
		} else {
			h.setFlash(w, "ok", "Logged in as "+done.User.DisplayName+".")
		}
	}
	http.Redirect(w, r, "/account", http.StatusSeeOther)
}

func (h *Handler) observe(provider string, intent identity.Intent, result string) {
	if h.Observe != nil {
		h.Observe(provider, intent, result)
	}
}

// authResult names a callback failure for the metrics.
func authResult(err error) string {
	switch {
	case errors.Is(err, identity.ErrProviderDenied):
		return "denied"
	case errors.Is(err, identity.ErrProviderFailed):
		return "failed"
	case errors.Is(err, identity.ErrAttemptInvalid), errors.Is(err, identity.ErrWrongUser), errors.Is(err, identity.ErrUnknownProvider), errors.Is(err, identity.ErrLoginNotAllowed):
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
	case errors.Is(err, identity.ErrUnknownProvider), errors.Is(err, identity.ErrLoginNotAllowed):
		h.fail(w, r, http.StatusNotFound, "No such login", "This hub has no such login provider.")
	case errors.Is(err, identity.ErrProviderFailed):
		h.logger.WarnContext(r.Context(), "provider failed", "provider", provider, "error", err.Error(), "request_id", httpx.RequestIDFromContext(r.Context()))
		h.fail(w, r, http.StatusBadGateway, "Provider failed", name+" could not complete the login. Try again in a moment.")
	default:
		h.serverError(w, r, err)
	}
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	s, ok := h.authorized(w, r)
	if !ok {
		return
	}
	if r.PostFormValue("everywhere") == "1" {
		n, err := h.sess.RevokeAll(r.Context(), s.UserID)
		if err != nil {
			h.serverError(w, r, err)
			return
		}
		h.sess.Clear(w)
		h.setFlash(w, "ok", fmt.Sprintf("Logged out everywhere (%d sessions).", n))
	} else {
		if err := h.sess.Revoke(r.Context(), w, s); err != nil {
			h.serverError(w, r, err)
			return
		}
		h.setFlash(w, "ok", "Logged out.")
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (h *Handler) unlink(w http.ResponseWriter, r *http.Request) {
	s, ok := h.authorized(w, r)
	if !ok {
		return
	}
	provider, subject := r.PostFormValue("provider"), r.PostFormValue("subject")
	err := h.ids.Unlink(r.Context(), s.UserID, provider, subject)
	switch {
	case errors.Is(err, identity.ErrLastIdentity):
		h.setFlash(w, "err", "You cannot unlink your only account.")
	case errors.Is(err, identity.ErrNotLinked):
		h.setFlash(w, "err", "That account is not linked to you.")
	case err != nil:
		h.serverError(w, r, err)
		return
	default:
		h.setFlash(w, "ok", h.providerDisplay(provider)+" account unlinked.")
	}
	http.Redirect(w, r, "/account", http.StatusSeeOther)
}

func (h *Handler) claim(w http.ResponseWriter, r *http.Request) {
	s, ok := h.authorized(w, r)
	if !ok {
		return
	}
	_, err := h.org.Claim(r.Context(), r.PostFormValue("token"), s.UserID)
	switch {
	case errors.Is(err, org.ErrAlreadyOwned):
		h.setFlash(w, "err", "This hub already has an owner.")
	case errors.Is(err, org.ErrInvalidToken):
		h.setFlash(w, "err", "That token is not valid or has expired. The hub logs a fresh one at every start while it is unowned.")
	case err != nil:
		h.serverError(w, r, err)
		return
	default:
		h.setFlash(w, "ok", "You now own this hub.")
	}
	http.Redirect(w, r, "/account", http.StatusSeeOther)
}

// authorized requires a session and the CSRF token on a state-changing form.
func (h *Handler) authorized(w http.ResponseWriter, r *http.Request) (store.Session, bool) {
	s, ok := session.FromContext(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return store.Session{}, false
	}
	if !h.sess.CheckCSRF(r, s) {
		h.fail(w, r, http.StatusForbidden, "Form expired", "This form did not carry a valid token. Go back and try again.")
		return store.Session{}, false
	}
	return s, true
}

func (h *Handler) providerDisplay(name string) string {
	if reg, ok := h.ids.Provider(name); ok {
		return reg.Provider.DisplayName()
	}
	return name
}

// page builds the data every page starts from: the organization, the session's user (nil when
// anonymous or gone) and the pending flash.
func (h *Handler) page(r *http.Request, title string) (*page, error) {
	o, err := h.org.Get(r.Context())
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	p := &page{Title: title, Org: o}
	if s, ok := session.FromContext(r.Context()); ok {
		u, err := h.ids.Me(r.Context(), s.UserID)
		switch {
		case errors.Is(err, store.ErrNotFound):
			// Leave User nil; the caller redirects. The middleware's cookie is cleared by the handler.
		case err != nil:
			return nil, err
		default:
			p.User = &u
			p.CSRF = s.CSRFToken
			p.Owner = o.OwnerUserID != nil && *o.OwnerUserID == u.ID
		}
	}
	if c, err := r.Cookie(h.sess.CookieName(session.CookieFlash)); err == nil && c.Value != "" {
		if kind, text, ok := strings.Cut(c.Value, ":"); ok {
			if text, err := url.QueryUnescape(text); err == nil && (kind == "ok" || kind == "err") {
				p.Flash = &flash{Kind: kind, Text: text}
			}
		}
	}
	return p, nil
}

func (h *Handler) setFlash(w http.ResponseWriter, kind, text string) {
	http.SetCookie(w, h.sess.Cookie(session.CookieFlash, kind+":"+url.QueryEscape(text), flashTTL))
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, status int, title, message string) {
	p, err := h.page(r, title)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	p.Message = message
	h.render(w, r, status, "error", p)
}

func (h *Handler) serverError(w http.ResponseWriter, r *http.Request, err error) {
	h.logger.ErrorContext(r.Context(), "page failed", "error", err.Error(), "path", r.URL.Path, "request_id", httpx.RequestIDFromContext(r.Context()))
	http.Error(w, "Something went wrong on our side. Try again in a moment.", http.StatusInternalServerError)
}

func (h *Handler) render(w http.ResponseWriter, r *http.Request, status int, name string, p *page) {
	if p.Flash != nil {
		http.SetCookie(w, h.sess.Cookie(session.CookieFlash, "", 0))
	}
	var buf bytes.Buffer
	if err := h.pages[name].ExecuteTemplate(&buf, "layout.html", p); err != nil {
		h.serverError(w, r, err)
		return
	}
	hdr := w.Header()
	hdr.Set("Content-Type", "text/html; charset=utf-8")
	hdr.Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; img-src https:; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
	hdr.Set("Referrer-Policy", "same-origin")
	hdr.Set("X-Content-Type-Options", "nosniff")
	hdr.Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = buf.WriteTo(w)
}
