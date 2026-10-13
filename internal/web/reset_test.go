package web_test

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gravel-project/gravel/internal/httpx"
	"github.com/gravel-project/gravel/internal/identity"
)

// A set-password link opens a page, sets the password once, ends the member's other sessions and
// logs this browser in; its token never reaches the access log or a referrer (gravel#187).
func TestPasswordLinkPage(t *testing.T) {
	r := newRigWith(t, testTheme(), true, withLocal(identity.LocalPolicy{}, nil))
	ctx := context.Background()
	_, link, err := r.ids.CreateLocal(ctx, "neckbeard", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	logs := &bytes.Buffer{}
	logged := httptest.NewServer(httpx.Logging(slog.New(slog.NewTextHandler(logs, nil)))(r.srv.Config.Handler))
	t.Cleanup(logged.Close)
	c := browser(t)

	resp, body := get(t, c, logged.URL+link.Path())
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "Choose your password") || !strings.Contains(body, "<strong>neckbeard</strong>") ||
		!strings.Contains(body, `action="`+link.Path()+`"`) || resp.Header.Get("Referrer-Policy") != "no-referrer" || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("page: %d %v %s", resp.StatusCode, resp.Header, body)
	}
	resp, body = post(t, c, logged.URL+link.Path(), url.Values{"password": {"neckbeard"}}, nil)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "at least 12 characters") {
		t.Errorf("weak: %d %s", resp.StatusCode, body)
	}

	// A browser already logged in as the member (they set a password on a phone, say) is logged out.
	other := browser(t)
	if resp, _ := post(t, other, r.srv.URL+"/auth/local/login", url.Values{"username": {"neckbeard"}, "password": {"anything at all here"}}, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("login before the link: %d", resp.StatusCode)
	}

	resp, _ = post(t, c, logged.URL+link.Path(), url.Values{"password": {"an old game and a few servers"}}, nil)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/account" {
		t.Fatalf("set: %d %v", resp.StatusCode, resp.Header)
	}
	if _, body = get(t, c, logged.URL+"/account"); !strings.Contains(body, "Your password is set") || !strings.Contains(body, "neckbeard") {
		t.Errorf("account: %s", body)
	}
	if resp, _ = get(t, c, logged.URL+link.Path()); resp.StatusCode != http.StatusNotFound {
		t.Errorf("used link: %d", resp.StatusCode)
	}
	if resp, _ = post(t, c, logged.URL+link.Path(), url.Values{"password": {"another long password"}}, nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("used link POST: %d", resp.StatusCode)
	}
	if resp, _ = get(t, c, logged.URL+"/auth/local/reset/nonsense"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown link: %d", resp.StatusCode)
	}
	if strings.Contains(logs.String(), link.Token) || !strings.Contains(logs.String(), "/auth/local/reset/<redacted>") {
		t.Errorf("the access log:\n%s", logs)
	}
	if resp, _ := post(t, browser(t), r.srv.URL+"/auth/local/login", url.Values{"username": {"neckbeard"}, "password": {"an old game and a few servers"}}, nil); resp.StatusCode != http.StatusSeeOther {
		t.Errorf("login with the new password: %d", resp.StatusCode)
	}
}

// A reset link ends every session the member had: one stolen with the old password goes too.
func TestPasswordResetEndsOtherSessions(t *testing.T) {
	r := newRigWith(t, testTheme(), true, withLocal(identity.LocalPolicy{}, nil))
	ctx := context.Background()
	_, set, err := r.ids.CreateLocal(ctx, "vps.dad", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.ids.UsePasswordLink(ctx, set.Token, "the first password!"); err != nil {
		t.Fatal(err)
	}
	stolen := browser(t)
	if resp, _ := post(t, stolen, r.srv.URL+"/auth/local/login", url.Values{"username": {"vps.dad"}, "password": {"the first password!"}}, nil); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login: %d", resp.StatusCode)
	}
	reset, err := r.ids.IssueReset(ctx, "vps.dad", nil)
	if err != nil {
		t.Fatal(err)
	}
	c := browser(t)
	if resp, _ := post(t, c, r.srv.URL+reset.Path(), url.Values{"password": {"the second password"}}, nil); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("reset: %d", resp.StatusCode)
	}
	if resp, _ := get(t, stolen, r.srv.URL+"/account"); resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
		t.Errorf("the old session survived: %d %v", resp.StatusCode, resp.Header)
	}
	if resp, _ := get(t, c, r.srv.URL+"/account"); resp.StatusCode != http.StatusOK {
		t.Errorf("the resetting browser is not logged in: %d", resp.StatusCode)
	}
}
