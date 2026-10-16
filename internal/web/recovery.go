package web

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/gravel-project/gravel/internal/httpx"
	"github.com/gravel-project/gravel/internal/identity"
	"github.com/gravel-project/gravel/internal/ratelimit"
	"github.com/gravel-project/gravel/internal/session"
	"github.com/gravel-project/gravel/internal/web/templates"
)

// The recovery address and the mailed reset (gravel#234): "forgot your password" on the login page
// mails a password link to a proved recovery address; the account page sets the address and the
// link mailed to it proves it. Like the other /auth/ flows these call the identity service.

// forgotSent is the one answer to a request for a password link, whatever happened.
const forgotSent = "If that account has a confirmed recovery address, a link to choose a new password is on its way there. It works for an hour. No mail? Ask the hub's staff."

// verifyLogPath is what the access log says for a verification link, whose token proves an address
// for a day.
const verifyLogPath = "/auth/local/verify/<redacted>"

func (h *Handler) mailOff(w http.ResponseWriter, r *http.Request) bool {
	if h.ids.MailEnabled() {
		return false
	}
	h.fail(w, r, http.StatusNotFound, "No such page", "This hub mails no password links. Ask its staff for one.")
	return true
}

// forgotForm asks for the username whose password link to mail.
func (h *Handler) forgotForm(w http.ResponseWriter, r *http.Request) {
	if h.mailOff(w, r) {
		return
	}
	h.forgotPage(w, r, http.StatusOK, nil, templates.ForgotPage{})
}

// forgot mails a password link to the account's proved recovery address, if it has one, and says
// the same either way.
func (h *Handler) forgot(w http.ResponseWriter, r *http.Request) {
	if h.mailOff(w, r) {
		return
	}
	username := r.PostFormValue("username")
	if h.MailLimiter != nil {
		if ok, retry := h.MailLimiter.Allow(ratelimit.ClientIP(r)); !ok {
			w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
			h.forgotPage(w, r, http.StatusTooManyRequests, &templates.Flash{Kind: "err", Text: "Too many requests from here. Wait a while and try again."},
				templates.ForgotPage{Username: username})
			return
		}
	}
	if err := h.ids.RequestPasswordMail(r.Context(), username); err != nil {
		h.serverError(w, r, err)
		return
	}
	h.forgotPage(w, r, http.StatusOK, &templates.Flash{Kind: "ok", Text: forgotSent}, templates.ForgotPage{Sent: true})
}

func (h *Handler) forgotPage(w http.ResponseWriter, r *http.Request, status int, flash *templates.Flash, fp templates.ForgotPage) {
	p, b, err := h.page(r, "Forgot your password?")
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	if flash != nil {
		p.Flash = flash
	}
	fp.Page = p.Page
	h.render(w, r, status, b, templates.Forgot(fp), templates.ForgotContent(fp))
}

// verifyHeaders keep a verification link's token on its page and out of the access log.
func verifyHeaders(w http.ResponseWriter, r *http.Request) {
	httpx.SetLogPath(r.Context(), verifyLogPath)
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "no-store")
}

// verifyForm is the page a verification link opens: it spends nothing, so a mail scanner that
// follows the link proves nothing; the member presses Confirm.
func (h *Handler) verifyForm(w http.ResponseWriter, r *http.Request) {
	verifyHeaders(w, r)
	token := r.PathValue("token")
	v, err := h.ids.VerificationFor(r.Context(), token)
	if err != nil {
		h.verifyError(w, r, err)
		return
	}
	p, b, err := h.page(r, "Confirm your recovery address")
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	vp := templates.VerifyPage{Page: p.Page, Username: v.Username, Email: v.Email, Action: "/auth/local/verify/" + token}
	h.render(w, r, http.StatusOK, b, templates.Verify(vp), templates.VerifyContent(vp))
}

// verify spends the link: the address it was mailed to is proved.
func (h *Handler) verify(w http.ResponseWriter, r *http.Request) {
	verifyHeaders(w, r)
	v, err := h.ids.VerifyRecoveryEmail(r.Context(), r.PathValue("token"))
	if err != nil {
		h.verifyError(w, r, err)
		return
	}
	h.setFlash(w, "ok", "Your recovery address "+v.Email+" is confirmed. If you forget your password, ask for a link on the login page.")
	to := "/login"
	if _, ok := session.FromContext(r.Context()); ok {
		to = "/account"
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
}

func (h *Handler) verifyError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, identity.ErrLinkInvalid) {
		h.fail(w, r, http.StatusNotFound, "Link not valid", "This link is unknown, used or expired, or the address changed since. Set the address again on your account page for a new one.")
		return
	}
	h.serverError(w, r, err)
}

// setRecoveryEmail sets, changes or removes the recovery address of the member's local account,
// with their current password.
func (h *Handler) setRecoveryEmail(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(w, r) {
		return
	}
	_, b := withBrowser(r)
	if !h.localAllowed(w, r, func(f *templates.Flash) { h.accountDone(w, r, b, *f) }) {
		return
	}
	s, _ := session.FromContext(r.Context())
	email := r.PostFormValue("email")
	if r.PostFormValue("remove") != "" {
		email = ""
	}
	change, err := h.ids.SetRecoveryEmail(r.Context(), s.UserID, email, r.PostFormValue("password"))
	var flash templates.Flash
	switch {
	case err == nil:
		flash = templates.Flash{Kind: "ok", Text: recoveryDone(change, email)}
	case errors.Is(err, identity.ErrBadPassword), errors.Is(err, identity.ErrLocked):
		flash = templates.Flash{Kind: "err", Text: "That isn't your current password, or too many tries have locked the account for a while. Nothing changed."}
	case errors.Is(err, identity.ErrInvalidEmail):
		flash = templates.Flash{Kind: "err", Text: "That isn't one email address, like you@example.com. Nothing changed."}
	case errors.Is(err, identity.ErrMailLimited):
		flash = templates.Flash{Kind: "err", Text: "Too many links mailed for this account. Try again in an hour."}
	case errors.Is(err, identity.ErrMailDisabled), errors.Is(err, identity.ErrNoLocalLogin):
		flash = templates.Flash{Kind: "err", Text: "A recovery address needs a username and password on a hub that mails members."}
	default:
		h.serverError(w, r, err)
		return
	}
	h.accountDone(w, r, b, flash)
}

func recoveryDone(c identity.RecoveryChange, email string) string {
	switch c {
	case identity.RecoveryMailed:
		return "A link is on its way to " + email + ". Open it within a day to confirm the address; until then no password link goes there."
	case identity.RecoveryVerified:
		return email + " is already your confirmed recovery address."
	case identity.RecoveryRemoved:
		return "Your recovery address is removed. A forgotten password now needs the hub's staff."
	default:
		return "You have no recovery address."
	}
}
