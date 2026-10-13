package web

import (
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"connectrpc.com/connect"

	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
	"github.com/gravel-project/gravel/internal/session"
	"github.com/gravel-project/gravel/internal/web/templates"
)

// The authorization endpoint (ADR-0015 §3) is a page of the hub: GET /oauth/authorize checks the
// app's request through AppService, sends a visitor without a session through the login and back,
// asks the member for consent unless a remembered grant covers the request, and redirects to the
// app with a code. The consent form posts back to /oauth/authorize with the session's CSRF token.
// A bad client or redirect URI shows an error page and never redirects; any other refusal goes
// back to the app with an error and its state (RFC 6749 §4.1.2.1).

// returnTTL bounds how long a login may take to come back to the page that sent the member.
const returnTTL = 15 * time.Minute

// maxReturnPath bounds a return path; an authorization request fits easily.
const maxReturnPath = 4096

// SafeReturnPath reports whether p is a path on this origin that a login may send the member
// back to: it starts with one "/", names no scheme, host or user, and carries no backslash or
// control character (which browsers may read as "//"). Anything else is ignored, so the login
// can never redirect off-site.
func SafeReturnPath(p string) bool {
	if p == "" || len(p) > maxReturnPath || p[0] != '/' || strings.HasPrefix(p, "//") {
		return false
	}
	if strings.ContainsFunc(p, func(r rune) bool { return r < 0x20 || r == 0x7f || r == '\\' }) {
		return false
	}
	u, err := url.Parse(p)
	return err == nil && u.Scheme == "" && u.Host == "" && u.User == nil && u.Opaque == "" && strings.HasPrefix(u.Path, "/")
}

// The parameters of an authorization request (RFC 6749 §4.1.1, RFC 7636 §4.3). Each may be sent
// once (RFC 6749 §3.1).
var authorizationParams = []string{"response_type", "client_id", "redirect_uri", "scope", "state", "code_challenge", "code_challenge_method"}

func readAuthorizationParams(v url.Values) (templates.AuthorizationParams, string) {
	repeated := ""
	for _, k := range authorizationParams {
		if len(v[k]) > 1 && repeated == "" {
			repeated = k
		}
	}
	return templates.AuthorizationParams{
		ResponseType: v.Get("response_type"), ClientID: v.Get("client_id"), RedirectURI: v.Get("redirect_uri"), Scope: v.Get("scope"),
		State: v.Get("state"), CodeChallenge: v.Get("code_challenge"), CodeChallengeMethod: v.Get("code_challenge_method"),
	}, repeated
}

func protoParams(p templates.AuthorizationParams) *hubv1.AuthorizationParams {
	return &hubv1.AuthorizationParams{ResponseType: p.ResponseType, ClientId: p.ClientID, RedirectUri: p.RedirectURI, Scope: p.Scope,
		CodeChallenge: p.CodeChallenge, CodeChallengeMethod: p.CodeChallengeMethod}
}

func (h *Handler) observeAuthorization(result string) {
	if h.ObserveAuthorization != nil {
		h.ObserveAuthorization(result)
	}
}

// unknownApp is the page for a request that names no app this hub knows, or a redirect URI the
// app did not register: nothing goes back to anyone.
func (h *Handler) unknownApp(w http.ResponseWriter, r *http.Request) {
	h.observeAuthorization("rejected")
	h.fail(w, r, http.StatusBadRequest, "Unknown app", "This sign-in link names an app, or a return address, that this hub does not know. Nothing was sent to the app.")
}

// prepare checks the request through the API. It answers the browser itself and returns false
// when the request goes no further: an unknown app (an error page) or a refusal the app hears
// about (a redirect with the error and the state).
func (h *Handler) prepare(w http.ResponseWriter, r *http.Request, b *browser, p templates.AuthorizationParams, repeated string) (*hubv1.PrepareAuthorizationResponse, bool) {
	if repeated == "client_id" || repeated == "redirect_uri" {
		h.unknownApp(w, r)
		return nil, false
	}
	resp, err := h.appAPI.PrepareAuthorization(withBrowserContext(r, b), connect.NewRequest(&hubv1.PrepareAuthorizationRequest{Params: protoParams(p)}))
	switch {
	case connect.CodeOf(err) == connect.CodeNotFound:
		h.unknownApp(w, r)
		return nil, false
	case err != nil:
		h.observeAuthorization("error")
		h.serverError(w, r, err)
		return nil, false
	}
	switch {
	case repeated != "":
		h.backToApp(w, r, "rejected", p, url.Values{"error": {"invalid_request"}, "error_description": {repeated + " was sent more than once"}})
		return nil, false
	case resp.Msg.GetError() != "":
		h.backToApp(w, r, "rejected", p, url.Values{"error": {resp.Msg.GetError()}, "error_description": {resp.Msg.GetErrorDescription()}})
		return nil, false
	}
	return resp.Msg, true
}

// authorize is GET /oauth/authorize.
func (h *Handler) authorize(w http.ResponseWriter, r *http.Request) {
	p, repeated := readAuthorizationParams(r.URL.Query())
	_, b := withBrowser(r)
	prep, ok := h.prepare(w, r, b, p, repeated)
	if !ok {
		return
	}
	if _, loggedIn := session.FromContext(r.Context()); !loggedIn {
		// The login comes back here, to the same request (SafeReturnPath).
		http.Redirect(w, r, "/login?return_to="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
		return
	}
	if !prep.GetConsentRequired() {
		h.issue(w, r, b, p)
		return
	}
	h.consent(w, r, p, prep)
}

// consent renders the consent page. Its Content-Security-Policy lets the form's answer redirect
// to the app's origin, which form-action 'self' alone would block in browsers that apply it to
// redirects.
func (h *Handler) consent(w http.ResponseWriter, r *http.Request, p templates.AuthorizationParams, prep *hubv1.PrepareAuthorizationResponse) {
	pd, b, err := h.page(r, "Sign in to "+prep.GetAppName())
	if err != nil {
		h.observeAuthorization("error")
		h.serverError(w, r, err)
		return
	}
	if pd.User == nil { // the session's user is gone
		h.sess.Clear(w)
		http.Redirect(w, r, "/login?return_to="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
		return
	}
	cp := templates.ConsentPage{Page: pd.Page, AppName: prep.GetAppName(), Params: p}
	for _, s := range prep.GetScopes() {
		cp.Scopes = append(cp.Scopes, templates.Scope{Name: s.GetName(), Description: s.GetDescription()})
	}
	ctx := withBrowserContext(r, b)
	if grants, err := h.appAPI.ListAppGrants(ctx, connect.NewRequest(&hubv1.ListAppGrantsRequest{})); err == nil {
		for _, g := range grants.Msg.GetGrants() {
			if g.GetClientId() == prep.GetClientId() {
				cp.Wider = true
			}
		}
	}
	policy := CSP
	if u, err := url.Parse(p.RedirectURI); err == nil {
		cp.ReturnHost = u.Host
		if origin := u.Scheme + "://" + u.Host; plainOrigin.MatchString(origin) {
			policy = strings.Replace(CSP, "form-action 'self'", "form-action 'self' "+origin, 1)
		}
	}
	h.renderWithPolicy(w, r, http.StatusOK, b, templates.Consent(cp), templates.ConsentContent(cp), policy)
}

// plainOrigin is an origin safe to put in a header: a scheme and a host with an optional port.
var plainOrigin = regexp.MustCompile(`^https?://[A-Za-z0-9.\-]+(:[0-9]+)?$|^https?://\[[0-9A-Fa-f:.]+\](:[0-9]+)?$`)

// authorizeDecision is the consent form's POST: Allow issues a code, anything else goes back to
// the app with access_denied.
func (h *Handler) authorizeDecision(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(w, r) {
		return
	}
	p, repeated := readAuthorizationParams(r.PostForm)
	_, b := withBrowser(r)
	if _, ok := h.prepare(w, r, b, p, repeated); !ok {
		return
	}
	if r.PostFormValue("decision") != "allow" {
		h.backToApp(w, r, "denied", p, url.Values{"error": {"access_denied"}, "error_description": {"the member did not allow the app"}})
		return
	}
	h.issue(w, r, b, p)
}

// issue records the consent, gets a code and sends the member back to the app with it.
func (h *Handler) issue(w http.ResponseWriter, r *http.Request, b *browser, p templates.AuthorizationParams) {
	resp, err := h.appAPI.Authorize(withBrowserContext(r, b), connect.NewRequest(&hubv1.AuthorizeRequest{Params: protoParams(p)}))
	switch connect.CodeOf(err) {
	case connect.CodeUnauthenticated:
		h.sess.Clear(w)
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	case connect.CodeNotFound:
		h.unknownApp(w, r)
		return
	}
	if err != nil {
		h.observeAuthorization("error")
		h.serverError(w, r, err)
		return
	}
	h.backToApp(w, r, "issued", p, url.Values{"code": {resp.Msg.GetCode()}})
}

// backToApp redirects to the app's redirect URI (one it registered: the API checked it) with the
// answer and the app's state added to its query (RFC 6749 §4.1.2), keeping any query it had.
func (h *Handler) backToApp(w http.ResponseWriter, r *http.Request, result string, p templates.AuthorizationParams, answer url.Values) {
	u, err := url.Parse(p.RedirectURI)
	if err != nil {
		h.observeAuthorization("error")
		h.serverError(w, r, errors.New("a registered redirect URI does not parse"))
		return
	}
	q := u.Query()
	for k, vs := range answer {
		q[k] = vs
	}
	if p.State != "" {
		q.Set("state", p.State)
	}
	u.RawQuery = q.Encode()
	h.observeAuthorization(result)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.Redirect(w, r, u.String(), http.StatusSeeOther)
}

// revokeApp is the account page's Revoke: the grant and the member's tokens for the app go.
func (h *Handler) revokeApp(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(w, r) {
		return
	}
	ctx, b := withBrowser(r)
	_, err := h.appAPI.RevokeAppGrant(ctx, connect.NewRequest(&hubv1.RevokeAppGrantRequest{ClientId: r.PostFormValue("client_id")}))
	var flash templates.Flash
	switch connect.CodeOf(err) {
	case connect.CodeNotFound, connect.CodeInvalidArgument:
		flash = templates.Flash{Kind: "err", Text: "You have not allowed that app."}
	default:
		if err != nil {
			h.apiError(w, r, b, err)
			return
		}
		flash = templates.Flash{Kind: "ok", Text: "Revoked. The app can no longer act as you, and must ask again."}
	}
	h.accountDone(w, r, b, flash)
}

// rememberReturn keeps a safe return path for the login's callback.
func (h *Handler) rememberReturn(w http.ResponseWriter, path string) {
	http.SetCookie(w, h.sess.Cookie(session.CookieReturn, url.QueryEscape(path), returnTTL))
}

// takeReturn reads and clears the return path; "" when there is none or it is not safe.
func (h *Handler) takeReturn(w http.ResponseWriter, r *http.Request) string {
	c, err := r.Cookie(h.sess.CookieName(session.CookieReturn))
	if err != nil || c.Value == "" {
		return ""
	}
	http.SetCookie(w, h.sess.Cookie(session.CookieReturn, "", 0))
	p, err := url.QueryUnescape(c.Value)
	if err != nil || !SafeReturnPath(p) {
		return ""
	}
	return p
}
