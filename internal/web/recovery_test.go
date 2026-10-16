package web_test

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gravel-project/gravel/internal/httpx"
	"github.com/gravel-project/gravel/internal/identity"
	"github.com/gravel-project/gravel/internal/ratelimit"
	"github.com/gravel-project/gravel/internal/web"
)

// fakeMailer keeps every message the hub mails a member.
type fakeMailer struct {
	mu   sync.Mutex
	sent []string // "to\nsubject\nbody"
}

func (m *fakeMailer) Mail(_ context.Context, to, subject, body string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, to+"\n"+subject+"\n"+body)
	return nil
}

func (m *fakeMailer) take() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := m.sent
	m.sent = nil
	return out
}

// withMail lets the rig's hub mail members (gravel#234); limiter bounds "forgot" per address.
func withMail(m *fakeMailer, limiter *ratelimit.Limiter) rigOption {
	return func(r *rig, ids *[]identity.Option, h *[]func(*web.Handler)) {
		*ids = append(*ids, identity.WithOrganizationName("HTG"), identity.WithMail(m, r.idSt, "https://hub.example.com"))
		*h = append(*h, func(w *web.Handler) { w.MailLimiter = limiter })
	}
}

var mailedPath = regexp.MustCompile(`https://hub\.example\.com(/auth/local/(?:verify|reset)/[A-Za-z0-9_-]+)`)

func pathIn(t *testing.T, mail, prefix string) string {
	t.Helper()
	m := mailedPath.FindStringSubmatch(mail)
	if m == nil || !strings.HasPrefix(m[1], prefix) {
		t.Fatalf("no %s link in:\n%s", prefix, mail)
	}
	return m[1]
}

// The whole mailed reset through the pages: the account page sets and proves a recovery address,
// "Forgot your password?" mails a link to it, and the link sets a new password.
func TestMailedResetPages(t *testing.T) {
	m := &fakeMailer{}
	r := newRigWith(t, testTheme(), true, withLocal(identity.LocalPolicy{}, nil), withMail(m, nil))
	ctx := context.Background()
	_, set, err := r.ids.CreateLocal(ctx, "neckbeard", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.ids.UsePasswordLink(ctx, set.Token, "an old game and a few servers"); err != nil {
		t.Fatal(err)
	}
	logs := &bytes.Buffer{}
	logged := httptest.NewServer(httpx.Logging(slog.New(slog.NewTextHandler(logs, nil)))(r.srv.Config.Handler))
	t.Cleanup(logged.Close)

	c := browser(t)
	if _, body := get(t, c, r.srv.URL+"/login"); !strings.Contains(body, `href="/auth/local/forgot"`) {
		t.Errorf("the login page offers no forgot link: %s", body)
	}
	if resp, _ := post(t, c, r.srv.URL+"/auth/local/login", url.Values{"username": {"neckbeard"}, "password": {"an old game and a few servers"}}, nil); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login: %d", resp.StatusCode)
	}
	_, body := get(t, c, r.srv.URL+"/account")
	if !strings.Contains(body, `action="/account/local/email"`) || !strings.Contains(body, "You have no recovery address.") {
		t.Fatalf("account: %s", body)
	}
	csrf := csrfOf(t, body)
	if resp, _ := post(t, c, r.srv.URL+"/account/local/email", url.Values{"email": {"neck@example.com"}, "password": {"an old game and a few servers"}}, nil); resp.StatusCode != http.StatusForbidden {
		t.Errorf("no csrf: %d", resp.StatusCode)
	}
	_, body = post(t, c, r.srv.URL+"/account/local/email", url.Values{"_csrf": {csrf}, "email": {"neck@example.com"}, "password": {"not my password"}}, hx)
	if !strings.Contains(body, "t your current password, or too many tries") || !strings.Contains(body, "You have no recovery address.") {
		t.Errorf("wrong password: %s", body)
	}
	_, body = post(t, c, r.srv.URL+"/account/local/email", url.Values{"_csrf": {csrf}, "email": {"neck@example.com"}, "password": {"an old game and a few servers"}}, hx)
	if !strings.Contains(body, "A link is on its way to neck@example.com") || !strings.Contains(body, "not confirmed yet") {
		t.Fatalf("set: %s", body)
	}
	r.ids.WaitMail()
	sent := m.take()
	if len(sent) != 1 || !strings.HasPrefix(sent[0], "neck@example.com\nHTG: confirm your recovery address\n") {
		t.Fatalf("verification mail: %q", sent)
	}
	verify := pathIn(t, sent[0], "/auth/local/verify/")

	// Opening the link proves nothing (a mail scanner follows links); Confirm does.
	resp, body := get(t, browser(t), logged.URL+verify)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "<strong>neck@example.com</strong>") || !strings.Contains(body, `action="`+verify+`"`) ||
		resp.Header.Get("Referrer-Policy") != "no-referrer" || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("verify page: %d %v %s", resp.StatusCode, resp.Header, body)
	}
	if _, body = get(t, c, r.srv.URL+"/account"); !strings.Contains(body, "not confirmed yet") {
		t.Errorf("opening the link proved the address: %s", body)
	}
	if resp, _ = post(t, c, logged.URL+verify, nil, nil); resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/account" {
		t.Fatalf("confirm: %d %v", resp.StatusCode, resp.Header)
	}
	if _, body = get(t, c, r.srv.URL+"/account"); !strings.Contains(body, "is confirmed") || !strings.Contains(body, ", confirmed "+time.Now().UTC().Format("2006-01-02")) {
		t.Errorf("confirmed: %s", body)
	}
	if resp, _ = post(t, c, logged.URL+verify, nil, nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("a used verification link: %d", resp.StatusCode)
	}

	// Forgotten: one answer for an account with a proved address and for nobody.
	if _, body = get(t, browser(t), r.srv.URL+"/auth/local/forgot"); !strings.Contains(body, `action="/auth/local/forgot"`) {
		t.Fatalf("forgot form: %s", body)
	}
	answers := map[string]string{}
	for _, name := range []string{"neckbeard", "nobody", "x"} {
		resp, body := post(t, browser(t), r.srv.URL+"/auth/local/forgot", url.Values{"username": {name}}, nil)
		if resp.StatusCode != http.StatusOK || !strings.Contains(body, "a link to choose a new password is on its way there") || strings.Contains(body, `action="/auth/local/forgot"`) {
			t.Fatalf("forgot %s: %d %s", name, resp.StatusCode, body)
		}
		answers[name] = body
	}
	if answers["neckbeard"] != answers["nobody"] || answers["nobody"] != answers["x"] {
		t.Error("the answers differ by username")
	}
	r.ids.WaitMail()
	sent = m.take()
	if len(sent) != 1 || !strings.HasPrefix(sent[0], "neck@example.com\nHTG: reset your password\n") {
		t.Fatalf("reset mail: %q", sent)
	}
	reset := pathIn(t, sent[0], "/auth/local/reset/")
	fresh := browser(t)
	if resp, _ := post(t, fresh, r.srv.URL+reset, url.Values{"password": {"a brand new sentence here"}}, nil); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("reset: %d", resp.StatusCode)
	}
	if resp, _ := get(t, c, r.srv.URL+"/account"); resp.StatusCode != http.StatusSeeOther {
		t.Errorf("the reset ends the other sessions: %d", resp.StatusCode)
	}
	if resp, body := get(t, fresh, r.srv.URL+reset); resp.StatusCode != http.StatusNotFound || !strings.Contains(body, "Forgot your password?") {
		t.Errorf("a used reset link points at the forgot page: %d %s", resp.StatusCode, body)
	}
	if strings.Contains(logs.String(), strings.TrimPrefix(verify, "/auth/local/verify/")) || !strings.Contains(logs.String(), "/auth/local/verify/<redacted>") {
		t.Errorf("the access log:\n%s", logs)
	}
}

// Removing the address takes the current password too.
func TestRemoveRecoveryAddressPage(t *testing.T) {
	m := &fakeMailer{}
	r := newRigWith(t, testTheme(), true, withLocal(identity.LocalPolicy{}, nil), withMail(m, nil))
	ctx := context.Background()
	_, set, err := r.ids.CreateLocal(ctx, "neckbeard", "neck@example.com", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.ids.UsePasswordLink(ctx, set.Token, "an old game and a few servers"); err != nil {
		t.Fatal(err)
	}
	c := browser(t)
	if resp, _ := post(t, c, r.srv.URL+"/auth/local/login", url.Values{"username": {"neckbeard"}, "password": {"an old game and a few servers"}}, nil); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login: %d", resp.StatusCode)
	}
	_, body := get(t, c, r.srv.URL+"/account")
	if !strings.Contains(body, "<strong>neck@example.com</strong>, not confirmed yet") || !strings.Contains(body, `name="remove"`) {
		t.Fatalf("an address staff typed shows unconfirmed: %s", body)
	}
	_, body = post(t, c, r.srv.URL+"/account/local/email", url.Values{"_csrf": {csrfOf(t, body)}, "email": {"neck@example.com"}, "remove": {"1"}, "password": {"an old game and a few servers"}}, hx)
	if !strings.Contains(body, "Your recovery address is removed.") || !strings.Contains(body, "You have no recovery address.") {
		t.Errorf("remove: %s", body)
	}
	r.ids.WaitMail()
	if sent := m.take(); len(sent) != 0 {
		t.Errorf("removing mails: %q", sent)
	}
}

// A hub that mails nobody offers none of it.
func TestMailOffPages(t *testing.T) {
	r := newRigWith(t, testTheme(), true, withLocal(identity.LocalPolicy{}, nil))
	ctx := context.Background()
	_, set, err := r.ids.CreateLocal(ctx, "neckbeard", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.ids.UsePasswordLink(ctx, set.Token, "an old game and a few servers"); err != nil {
		t.Fatal(err)
	}
	c := browser(t)
	if _, body := get(t, c, r.srv.URL+"/login"); strings.Contains(body, "/auth/local/forgot") {
		t.Errorf("forgot offered with no mail: %s", body)
	}
	for _, p := range []string{"/auth/local/forgot", "/auth/local/verify/x"} {
		if resp, _ := get(t, c, r.srv.URL+p); resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: %d", p, resp.StatusCode)
		}
	}
	if resp, _ := post(t, c, r.srv.URL+"/auth/local/forgot", url.Values{"username": {"neckbeard"}}, nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("forgot POST: %d", resp.StatusCode)
	}
	if resp, _ := post(t, c, r.srv.URL+"/auth/local/login", url.Values{"username": {"neckbeard"}, "password": {"an old game and a few servers"}}, nil); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login: %d", resp.StatusCode)
	}
	if _, body := get(t, c, r.srv.URL+"/account"); strings.Contains(body, "/account/local/email") {
		t.Errorf("recovery form with no mail: %s", body)
	}
}

// "Forgot" is limited per address, whatever the usernames.
func TestForgotRateLimit(t *testing.T) {
	m := &fakeMailer{}
	r := newRigWith(t, testTheme(), true, withLocal(identity.LocalPolicy{}, nil), withMail(m, ratelimit.NewEvery(2, time.Hour, 2)))
	c := browser(t)
	for i, name := range []string{"a1", "b22", "c333"} {
		resp, body := post(t, c, r.srv.URL+"/auth/local/forgot", url.Values{"username": {name}}, nil)
		want := http.StatusOK
		if i == 2 {
			want = http.StatusTooManyRequests
		}
		if resp.StatusCode != want {
			t.Errorf("request %d: %d %s", i, resp.StatusCode, body)
		}
		if i == 2 && (resp.Header.Get("Retry-After") == "" || !strings.Contains(body, "Too many requests from here")) {
			t.Errorf("limited: %v %s", resp.Header, body)
		}
	}
}
