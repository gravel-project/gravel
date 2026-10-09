package session

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gravel-project/gravel/internal/identity/identitytest"
	"github.com/gravel-project/gravel/internal/store"
)

var user = uuid.MustParse("00000000-0000-7000-8000-000000000001")

func newManager(secure bool) (*Manager, *identitytest.FakeStore, *time.Time) {
	now := time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)
	st := identitytest.NewFakeStore()
	m := New(st, 24*time.Hour, secure, slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.now = func() time.Time { return now }
	return m, st, &now
}

func cookieFrom(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == "__Host-gravel_session" {
			return c
		}
	}
	t.Fatalf("no session cookie in %v", rec.Header())
	return nil
}

func TestIssueLoadRevoke(t *testing.T) {
	m, st, now := newManager(true)
	ctx := context.Background()
	rec := httptest.NewRecorder()
	s, err := m.Issue(ctx, rec, user)
	if err != nil || s.UserID != user || s.CSRFToken == "" || !s.ExpiresAt.Equal(now.Add(24*time.Hour)) {
		t.Fatalf("issue: %v %+v", err, s)
	}
	c := cookieFrom(t, rec)
	if !c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Path != "/" || c.MaxAge != 24*60*60 || c.Value == "" {
		t.Errorf("cookie attributes: %+v", c)
	}
	if st.SessionCount() != 1 {
		t.Errorf("sessions stored: %d", st.SessionCount())
	}
	for _, stored := range st.Sessions {
		if string(stored.TokenHash) == c.Value || strings.Contains(string(stored.TokenHash), c.Value) {
			t.Error("the store must hold a hash, not the cookie value")
		}
	}

	r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
	r.AddCookie(c)
	got, err := m.Load(ctx, r)
	if err != nil || got.ID != s.ID {
		t.Fatalf("load: %v %+v", err, got)
	}
	if !got.LastSeenAt.Equal(*now) {
		t.Errorf("last seen: %v", got.LastSeenAt)
	}
	*now = now.Add(2 * time.Minute)
	if got, _ := m.Load(ctx, r); !got.LastSeenAt.Equal(now.Add(-2 * time.Minute)) {
		t.Errorf("a load within the touch interval must not write: %v", got.LastSeenAt)
	}
	*now = now.Add(4 * time.Minute)
	if got, _ := m.Load(ctx, r); !got.LastSeenAt.Equal(*now) {
		t.Errorf("a load past the touch interval moves last_seen_at: %v", got.LastSeenAt)
	}

	bad := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
	bad.AddCookie(&http.Cookie{Name: c.Name, Value: "forged"})
	if _, err := m.Load(ctx, bad); !errors.Is(err, ErrNoSession) {
		t.Errorf("forged cookie: %v", err)
	}
	if _, err := m.Load(ctx, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)); !errors.Is(err, ErrNoSession) {
		t.Errorf("no cookie: %v", err)
	}
	*now = now.Add(25 * time.Hour)
	if _, err := m.Load(ctx, r); !errors.Is(err, ErrNoSession) {
		t.Errorf("expired: %v", err)
	}
	*now = now.Add(-25 * time.Hour)

	rec = httptest.NewRecorder()
	if err := m.Revoke(ctx, rec, s); err != nil {
		t.Fatal(err)
	}
	if c := cookieFrom(t, rec); c.MaxAge >= 0 || c.Value != "" {
		t.Errorf("revoke must clear the cookie: %+v", c)
	}
	if _, err := m.Load(ctx, r); !errors.Is(err, ErrNoSession) {
		t.Errorf("after revoke: %v", err)
	}

	for range 3 {
		if _, err := m.Issue(ctx, httptest.NewRecorder(), user); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := m.RevokeAll(ctx, user); err != nil || n != 3 {
		t.Errorf("revoke all: %v %d", err, n)
	}
	if st.SessionCount() != 0 {
		t.Errorf("sessions left: %d", st.SessionCount())
	}
}

func TestInsecureCookieNames(t *testing.T) {
	m, _, _ := newManager(false)
	if m.CookieName(CookieSession) != "gravel_session" || m.CookieName(CookieAuth) != "gravel_auth" {
		t.Errorf("names: %s %s", m.CookieName(CookieSession), m.CookieName(CookieAuth))
	}
	if c := m.Cookie(CookieAuth, "v", 10*time.Minute); c.Secure || c.MaxAge != 600 || !c.HttpOnly {
		t.Errorf("cookie: %+v", c)
	}
	if c := m.Cookie(CookieFlash, "", 0); c.MaxAge != -1 {
		t.Errorf("deleting cookie: %+v", c)
	}
}

func TestMiddleware(t *testing.T) {
	m, st, now := newManager(true)
	ctx := context.Background()
	rec := httptest.NewRecorder()
	s, _ := m.Issue(ctx, rec, user)
	c := cookieFrom(t, rec)

	var seen *store.Session
	h := m.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, ok := FromContext(r.Context()); ok {
			seen = &got
		} else {
			seen = nil
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
	r.AddCookie(c)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusNoContent || seen == nil || seen.ID != s.ID {
		t.Errorf("with cookie: %d %+v", w.Code, seen)
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil))
	if w.Code != http.StatusNoContent || seen != nil || len(w.Result().Cookies()) != 0 {
		t.Errorf("anonymous: %d %+v %v", w.Code, seen, w.Result().Cookies())
	}

	*now = now.Add(48 * time.Hour)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusNoContent || seen != nil {
		t.Errorf("stale cookie: %d %+v", w.Code, seen)
	}
	if c := cookieFrom(t, w); c.MaxAge >= 0 {
		t.Errorf("a stale cookie is cleared: %+v", c)
	}
	*now = now.Add(-48 * time.Hour)

	st.ErrOn = "GetSessionByTokenHash"
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("store down with a cookie: %d", w.Code)
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/healthz", nil))
	if w.Code != http.StatusNoContent {
		t.Errorf("store down without a cookie is still served: %d", w.Code)
	}
}

func TestCheckCSRF(t *testing.T) {
	m, _, _ := newManager(true)
	s := store.Session{CSRFToken: "secret-token"}
	form := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/x", strings.NewReader("_csrf=secret-token&a=b"))
	form.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if !m.CheckCSRF(form, s) {
		t.Error("form field")
	}
	header := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/x", nil)
	header.Header.Set(CSRFHeader, "secret-token")
	if !m.CheckCSRF(header, s) {
		t.Error("header")
	}
	wrong := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/x", strings.NewReader("_csrf=nope"))
	wrong.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if m.CheckCSRF(wrong, s) {
		t.Error("wrong token")
	}
	if m.CheckCSRF(httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/x", nil), s) {
		t.Error("missing token")
	}
	if m.CheckCSRF(form, store.Session{CSRFToken: ""}) {
		t.Error("an empty session token never matches")
	}
}
