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
	"github.com/prometheus/client_golang/prometheus"

	"github.com/gravel-project/gravel/gen/gravel/hub/v1/hubv1connect"
	"github.com/gravel-project/gravel/internal/api"
	"github.com/gravel-project/gravel/internal/apps"
	"github.com/gravel-project/gravel/internal/apps/appstest"
	"github.com/gravel-project/gravel/internal/identity"
	"github.com/gravel-project/gravel/internal/identity/identitytest"
	"github.com/gravel-project/gravel/internal/org"
	"github.com/gravel-project/gravel/internal/org/orgtest"
	"github.com/gravel-project/gravel/internal/servers"
	"github.com/gravel-project/gravel/internal/servers/serverstest"
	"github.com/gravel-project/gravel/internal/session"
	"github.com/gravel-project/gravel/internal/store"
)

// rig is an API server over in-memory stores: the organization fake and the identity fake,
// with the session middleware in front, as the hub wires it.
type rig struct {
	srv   *httptest.Server
	orgSt *orgtest.FakeStore
	idSt  *identitytest.FakeStore
	appSt *appstest.FakeStore
	org   *org.Service
	ids   *identity.Service
	sess  *session.Manager
	apps  *apps.Service
	srvs  *servers.Service
	obs   *fakeObserver
	drv   *fakeDriver
	audit *serverstest.AuditStore
	token string
}

// fakeObserver is the monitor's observations, set by the test.
type fakeObserver struct {
	obs map[string]servers.Observation
}

func (f *fakeObserver) Observation(id string) (servers.Observation, bool) {
	o, ok := f.obs[id]
	return o, ok
}

func newRig(t *testing.T) *rig {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := &rig{orgSt: &orgtest.FakeStore{}, idSt: identitytest.NewFakeStore(), appSt: appstest.NewFakeStore()}
	r.org = org.New(r.orgSt, 15*time.Minute, logger)
	o, token, err := r.org.EnsureBuiltin(context.Background(), "Test")
	if err != nil {
		t.Fatal(err)
	}
	r.token = token
	r.ids = identity.New(r.idSt, o.ID, nil, 10*time.Minute, logger)
	r.sess = session.New(r.idSt, time.Hour, false, logger)
	r.apps = apps.New(r.appSt, o.ID, time.Hour, logger)
	r.srvs = servers.New(serverstest.NewFakeStore(), o.ID, []string{"wardogs"}, logger)
	r.obs = &fakeObserver{obs: map[string]servers.Observation{}}
	r.drv = &fakeDriver{errs: map[string]error{}}
	r.audit = serverstest.NewAuditStore()
	mod := servers.NewModeration(r.srvs, r.drv, r.audit, o.ID, prometheus.NewRegistry(), logger)
	mux := http.NewServeMux()
	mux.Handle(hubv1connect.NewOrganizationServiceHandler(api.NewOrganizationServer(r.org, logger)))
	mux.Handle(hubv1connect.NewIdentityServiceHandler(api.NewIdentityServer(r.ids, r.sess, r.org, logger)))
	mux.Handle(hubv1connect.NewServerServiceHandler(api.NewServerServer(r.srvs, r.obs, r.ids, logger)))
	mux.Handle(hubv1connect.NewModerationServiceHandler(api.NewModerationServer(mod, r.org, r.ids, logger)))
	r.srv = newH2CServer(r.sess.Middleware(r.apps.Middleware(mux)))
	t.Cleanup(r.srv.Close)
	return r
}

// bearer registers an app with the scopes and returns a token's Authorization header value.
func (r *rig) bearer(t *testing.T, name string, scopes ...string) string {
	t.Helper()
	c, err := r.apps.Create(context.Background(), name, scopes)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := r.apps.Issue(context.Background(), c.App.ClientID, c.Secret, nil)
	if err != nil {
		t.Fatal(err)
	}
	return "Bearer " + tok.Value
}

// bearerClient is an HTTP client that sends one Authorization header on every request.
func bearerClient(base *http.Client, auth string) *http.Client {
	c := *base
	c.Transport = headerTransport{next: base.Transport, name: "Authorization", value: auth}
	return &c
}

type headerTransport struct {
	next        http.RoundTripper
	name, value string
}

func (t headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set(t.name, t.value)
	next := t.next
	if next == nil {
		next = http.DefaultTransport
	}
	return next.RoundTrip(r)
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
