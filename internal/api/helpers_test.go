package api_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gravel-project/gravel/gen/gravel/hub/v1/hubv1connect"
	"github.com/gravel-project/gravel/internal/api"
	"github.com/gravel-project/gravel/internal/identity"
	"github.com/gravel-project/gravel/internal/identity/identitytest"
	"github.com/gravel-project/gravel/internal/org"
	"github.com/gravel-project/gravel/internal/org/orgtest"
	"github.com/gravel-project/gravel/internal/session"
	"github.com/gravel-project/gravel/internal/store"
)

// rig is an API server over in-memory stores: the organization fake and the identity fake,
// with the session middleware in front, as the hub wires it.
type rig struct {
	srv   *httptest.Server
	orgSt *orgtest.FakeStore
	idSt  *identitytest.FakeStore
	org   *org.Service
	ids   *identity.Service
	sess  *session.Manager
	token string
}

func newRig(t *testing.T) *rig {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := &rig{orgSt: &orgtest.FakeStore{}, idSt: identitytest.NewFakeStore()}
	r.org = org.New(r.orgSt, 15*time.Minute, logger)
	o, token, err := r.org.EnsureBuiltin(context.Background(), "Test")
	if err != nil {
		t.Fatal(err)
	}
	r.token = token
	r.ids = identity.New(r.idSt, o.ID, nil, 10*time.Minute, logger)
	r.sess = session.New(r.idSt, time.Hour, false, logger)
	mux := http.NewServeMux()
	mux.Handle(hubv1connect.NewOrganizationServiceHandler(api.NewOrganizationServer(r.org, logger)))
	mux.Handle(hubv1connect.NewIdentityServiceHandler(api.NewIdentityServer(r.ids, r.sess, r.org, logger)))
	r.srv = newH2CServer(r.sess.Middleware(mux))
	t.Cleanup(r.srv.Close)
	return r
}

// user registers a user with one Discord identity and returns them.
func (r *rig) user(t *testing.T, name, discordID string) store.User {
	t.Helper()
	now := time.Now().UTC()
	u, err := r.idSt.RegisterUser(context.Background(), store.User{ID: uuid.New(), OrganizationID: r.orgSt.Org.ID, DisplayName: name},
		store.Identity{Provider: "discord", Subject: discordID, DisplayName: name, VerificationMethod: identity.MethodOAuth2, VerifiedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// login issues a session for a user and returns its Cookie header value.
func (r *rig) login(t *testing.T, userID uuid.UUID) string {
	t.Helper()
	rec := httptest.NewRecorder()
	if _, err := r.sess.Issue(context.Background(), rec, userID); err != nil {
		t.Fatal(err)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == r.sess.CookieName(session.CookieSession) {
			return c.Name + "=" + c.Value
		}
	}
	t.Fatal("no session cookie issued")
	return ""
}

// cookieClient is an HTTP client that sends one Cookie header on every request.
func cookieClient(base *http.Client, cookie string) *http.Client {
	c := *base
	c.Transport = cookieTransport{next: base.Transport, cookie: cookie}
	return &c
}

type cookieTransport struct {
	next   http.RoundTripper
	cookie string
}

func (t cookieTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if t.cookie != "" {
		r = r.Clone(r.Context())
		r.Header.Set("Cookie", t.cookie)
	}
	next := t.next
	if next == nil {
		next = http.DefaultTransport
	}
	return next.RoundTrip(r)
}
