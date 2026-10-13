package web

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/gravel-project/gravel/internal/httpx"
	"github.com/gravel-project/gravel/internal/identity"
	"github.com/gravel-project/gravel/internal/ratelimit"
	"github.com/gravel-project/gravel/internal/session"
	"github.com/gravel-project/gravel/internal/web/templates"
)

// Local accounts (ADR-0026): the username and password form posts here, with no provider and no
// redirect. Like the other /auth/ flows these call the identity service, not the API.

// lastProviderTTL is how long the login page remembers the provider a browser last used.
const lastProviderTTL = 365 * 24 * time.Hour

// refusedLogin is the one answer to an unknown username, a wrong password and a locked account.
const refusedLogin = "That username and password don't match an account that may log in now. Check them, or wait a while if you tried many times."

func (h *Handler) rememberProvider(w http.ResponseWriter, provider string) {
	http.SetCookie(w, h.sess.Cookie(session.CookieLastProvider, provider, lastProviderTTL))
}

func (h *Handler) observeLocal(result string) {
	if h.ObserveLocal != nil {
		h.ObserveLocal(result)
	}
}

// localAllowed applies the per-address limit on password hashing; false has answered 429.
func (h *Handler) localAllowed(w http.ResponseWriter, r *http.Request, again func(*templates.Flash)) bool {
	if h.LocalLimiter == nil {
		return true
	}
	ok, retry := h.LocalLimiter.Allow(ratelimit.ClientIP(r))
	if ok {
		return true
	}
	h.observeLocal("rate_limited")
	w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
	again(&templates.Flash{Kind: "err", Text: "Too many tries from here. Wait a minute and try again."})
	return false
}

// localLogin checks a username and password and starts a session, replacing any the browser had.
func (h *Handler) localLogin(w http.ResponseWriter, r *http.Request) {
	if !h.ids.LocalEnabled() {
		h.fail(w, r, http.StatusNotFound, "No such login", "This hub has no username and password login.")
		return
	}
	username := r.PostFormValue("username")
	status := http.StatusTooManyRequests
	if !h.localAllowed(w, r, func(f *templates.Flash) { h.loginPage(w, r, status, f, username) }) {
		return
	}
	done, err := h.ids.LocalLogin(r.Context(), username, r.PostFormValue("password"))
	switch {
	case errors.Is(err, identity.ErrLocked):
		h.observeLocal("locked")
	case errors.Is(err, identity.ErrBadCredentials):
		h.observeLocal("refused")
	case err != nil:
		h.observeLocal("error")
		h.serverError(w, r, err)
		return
	}
	if err != nil {
		h.loginPage(w, r, http.StatusUnauthorized, &templates.Flash{Kind: "err", Text: refusedLogin}, username)
		return
	}
	h.observeLocal("ok")
	if !h.startSession(w, r, done) {
		return
	}
	h.setFlash(w, "ok", "Logged in as "+done.User.DisplayName+".")
	h.afterLogin(w, r)
}

// registerForm shows registration when the host opened it; otherwise there is no such page.
func (h *Handler) registerForm(w http.ResponseWriter, r *http.Request) {
	if _, ok := session.FromContext(r.Context()); ok {
		http.Redirect(w, r, "/account", http.StatusSeeOther) // a member adds a password on the account page
		return
	}
	minLen, ok := h.registrationOpen(w, r)
	if !ok {
		return
	}
	h.registerPage(w, r, http.StatusOK, nil, "", minLen)
}

// register makes a member with a local account and logs them in.
func (h *Handler) register(w http.ResponseWriter, r *http.Request) {
	minLen, ok := h.registrationOpen(w, r)
	if !ok {
		return
	}
	username := r.PostFormValue("username")
	status := http.StatusTooManyRequests
	if !h.localAllowed(w, r, func(f *templates.Flash) { h.registerPage(w, r, status, f, username, minLen) }) {
		return
	}
	done, err := h.ids.LocalRegister(r.Context(), username, r.PostFormValue("password"))
	if err != nil {
		if problem, status, known := localProblem(err); known {
			h.observeLocal("invalid")
			if errors.Is(err, identity.ErrRegistrationClosed) {
				h.fail(w, r, http.StatusNotFound, "No registration", "This hub does not take registrations. Ask its staff for an account.")
				return
			}
			h.registerPage(w, r, status, &templates.Flash{Kind: "err", Text: problem}, username, minLen)
			return
		}
		h.observeLocal("error")
		h.serverError(w, r, err)
		return
	}
	h.observeLocal("ok")
	if !h.startSession(w, r, done) {
		return
	}
	h.setFlash(w, "ok", "Welcome, "+done.User.DisplayName+". Your account is ready.")
	h.afterLogin(w, r)
}

// setLocal gives the logged-in member a username and password beside their other accounts.
func (h *Handler) setLocal(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(w, r) {
		return
	}
	if !h.ids.LocalEnabled() {
		h.fail(w, r, http.StatusNotFound, "No such login", "This hub has no username and password login.")
		return
	}
	_, b := withBrowser(r)
	s, _ := session.FromContext(r.Context())
	done, err := h.ids.LocalLink(r.Context(), s.UserID, r.PostFormValue("username"), r.PostFormValue("password"))
	var flash templates.Flash
	switch problem, _, known := localProblem(err); {
	case err == nil:
		flash = templates.Flash{Kind: "ok", Text: "You can log in as " + done.Identity.Subject + " with your password too."}
	case known:
		flash = templates.Flash{Kind: "err", Text: problem}
	default:
		h.serverError(w, r, err)
		return
	}
	h.accountDone(w, r, b, flash)
}

// startSession replaces the browser's session with the member's and remembers the login.
func (h *Handler) startSession(w http.ResponseWriter, r *http.Request, done identity.Completed) bool {
	if current, ok := session.FromContext(r.Context()); ok {
		if err := h.sess.Revoke(r.Context(), w, current); err != nil {
			h.serverError(w, r, err)
			return false
		}
	}
	if _, err := h.sess.Issue(r.Context(), w, done.User.ID); err != nil {
		h.serverError(w, r, err)
		return false
	}
	h.rememberProvider(w, identity.ProviderLocal)
	return true
}

// afterLogin goes where the login came from (the authorization endpoint) or to the account.
func (h *Handler) afterLogin(w http.ResponseWriter, r *http.Request) {
	if to := h.takeReturn(w, r); to != "" {
		http.Redirect(w, r, to, http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/account", http.StatusSeeOther)
}

// registrationOpen is the minimum password length when local accounts are on and the host opened
// registration; otherwise it answers 404 and false.
func (h *Handler) registrationOpen(w http.ResponseWriter, r *http.Request) (int, bool) {
	if !h.ids.LocalEnabled() {
		h.fail(w, r, http.StatusNotFound, "No registration", "This hub has no username and password accounts.")
		return 0, false
	}
	policy, err := h.ids.LocalPolicy(r.Context())
	if err != nil {
		h.serverError(w, r, err)
		return 0, false
	}
	if !policy.RegistrationOpen {
		h.fail(w, r, http.StatusNotFound, "No registration", "This hub does not take registrations. Ask its staff for an account.")
		return 0, false
	}
	return policy.MinPasswordLength, true
}

func (h *Handler) registerPage(w http.ResponseWriter, r *http.Request, status int, flash *templates.Flash, username string, minLen int) {
	p, b, err := h.page(r, "Make an account")
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	if flash != nil {
		p.Flash = flash
	}
	rp := templates.RegisterPage{Page: p.Page, Username: username, MinPasswordLength: minLen}
	h.render(w, r, status, b, templates.Register(rp), templates.RegisterContent(rp))
}

// localProblem says what a member can fix in a refused username or password, with the status a
// page answers; known is false for a failure that is the hub's.
func localProblem(err error) (problem string, status int, known bool) {
	reason := func(sentinel error) string {
		r := strings.TrimPrefix(err.Error(), sentinel.Error()+": ")
		if r == "" {
			return r
		}
		return string(unicode.ToUpper(rune(r[0]))) + r[1:] + "."
	}
	switch {
	case errors.Is(err, identity.ErrInvalidUsername):
		return reason(identity.ErrInvalidUsername), http.StatusBadRequest, true
	case errors.Is(err, identity.ErrWeakPassword):
		return reason(identity.ErrWeakPassword), http.StatusBadRequest, true
	case errors.Is(err, identity.ErrIdentityTaken):
		return "That username is taken. Choose another.", http.StatusConflict, true
	case errors.Is(err, identity.ErrAlreadyLocal):
		return "You already have a username and password.", http.StatusConflict, true
	case errors.Is(err, identity.ErrRegistrationClosed):
		return identity.ErrRegistrationClosed.Error(), http.StatusNotFound, true
	}
	return "", 0, false
}

// resetLogPath is what the access log says for a password link: the token is a password for an
// hour and never reaches a log.
const resetLogPath = "/auth/local/reset/<redacted>"

// resetHeaders keep the link's token on this page: no referrer to anything it loads or links to.
func resetHeaders(w http.ResponseWriter, r *http.Request) {
	httpx.SetLogPath(r.Context(), resetLogPath)
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "no-store")
}

// resetForm is the page a password link opens (gravel#187); it spends nothing.
func (h *Handler) resetForm(w http.ResponseWriter, r *http.Request) {
	resetHeaders(w, r)
	token := r.PathValue("token")
	link, err := h.ids.PasswordLinkFor(r.Context(), token)
	if err != nil {
		h.linkError(w, r, err)
		return
	}
	h.resetPage(w, r, http.StatusOK, nil, link, token)
}

// reset sets the password from the link, once, ends every session of the member and logs this
// browser in.
func (h *Handler) reset(w http.ResponseWriter, r *http.Request) {
	resetHeaders(w, r)
	token := r.PathValue("token")
	link, err := h.ids.PasswordLinkFor(r.Context(), token)
	if err != nil {
		h.linkError(w, r, err)
		return
	}
	status := http.StatusTooManyRequests
	if !h.localAllowed(w, r, func(f *templates.Flash) { h.resetPage(w, r, status, f, link, token) }) {
		return
	}
	u, _, err := h.ids.UsePasswordLink(r.Context(), token, r.PostFormValue("password"))
	if err != nil {
		if problem, status, known := localProblem(err); known {
			h.resetPage(w, r, status, &templates.Flash{Kind: "err", Text: problem}, link, token)
			return
		}
		h.linkError(w, r, err)
		return
	}
	if _, err := h.sess.RevokeAll(r.Context(), u.ID); err != nil {
		h.serverError(w, r, err)
		return
	}
	if !h.startSession(w, r, identity.Completed{User: u}) {
		return
	}
	h.setFlash(w, "ok", "Your password is set, and you are logged in as "+link.Username+".")
	http.Redirect(w, r, "/account", http.StatusSeeOther)
}

func (h *Handler) linkError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, identity.ErrLinkInvalid) {
		h.fail(w, r, http.StatusNotFound, "Link not valid", "This link is unknown, used or expired. Ask the hub's staff for a new one.")
		return
	}
	h.serverError(w, r, err)
}

func (h *Handler) resetPage(w http.ResponseWriter, r *http.Request, status int, flash *templates.Flash, link identity.PasswordLink, token string) {
	p, b, err := h.page(r, "Choose a password")
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	if flash != nil {
		p.Flash = flash
	}
	minLen := identity.DefaultMinPassword
	if policy, err := h.ids.LocalPolicy(r.Context()); err == nil {
		minLen = policy.MinPasswordLength
	}
	rp := templates.ResetPage{Page: p.Page, Username: link.Username, Set: link.Purpose == "set", MinPasswordLength: minLen, Action: "/auth/local/reset/" + token}
	h.render(w, r, status, b, templates.Reset(rp), templates.ResetContent(rp))
}
