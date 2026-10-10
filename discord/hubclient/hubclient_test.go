package hubclient_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/gravel-project/gravel/discord/hubclient"
	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
	"github.com/gravel-project/gravel/gen/gravel/hub/v1/hubv1connect"
)

type hub struct {
	hubv1connect.UnimplementedIdentityServiceHandler
	issued  atomic.Int32
	valid   atomic.Int32 // tokens below this number are refused (revoked)
	expires string
}

func (h *hub) LookupUser(_ context.Context, req *connect.Request[hubv1.LookupUserRequest]) (*connect.Response[hubv1.LookupUserResponse], error) {
	return connect.NewResponse(&hubv1.LookupUserResponse{User: &hubv1.User{Id: "u", DisplayName: req.Msg.GetSubject()}}), nil
}

func newHub(t *testing.T) (*hub, *httptest.Server) {
	t.Helper()
	h := &hub{expires: "3600"}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /oauth/token", func(w http.ResponseWriter, r *http.Request) {
		id, secret, ok := r.BasicAuth()
		if r.FormValue("grant_type") != "client_credentials" || !ok || id != "gravel_x" || secret != "s" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"invalid_client"}`))
			return
		}
		n := h.issued.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"tok` + strconv.Itoa(int(n)) + `","token_type":"Bearer","expires_in":` + h.expires + `,"scope":"` + r.FormValue("scope") + `"}`))
	})
	path, handler := hubv1connect.NewIdentityServiceHandler(h)
	mux.Handle(path, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		n, _ := strconv.Atoi(strings.TrimPrefix(auth, "Bearer tok"))
		if !strings.HasPrefix(auth, "Bearer tok") || int32(n) <= h.valid.Load() {
			w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
			http.Error(w, `{"error":"invalid_token"}`, http.StatusUnauthorized)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return h, srv
}

func TestTokenAndCalls(t *testing.T) {
	h, srv := newHub(t)
	ctx := context.Background()
	c, err := hubclient.New(hubclient.Config{URL: srv.URL + "/", ClientID: "gravel_x", ClientSecret: "s", Scopes: []string{"identity:read"}})
	if err != nil {
		t.Fatal(err)
	}
	tok, err := c.Token(ctx)
	if err != nil || tok != "tok1" {
		t.Fatalf("first token: %q %v", tok, err)
	}
	if tok, _ := c.Token(ctx); tok != "tok1" || h.issued.Load() != 1 {
		t.Errorf("the token is cached: %q (%d fetches)", tok, h.issued.Load())
	}
	resp, err := c.Identity().LookupUser(ctx, connect.NewRequest(&hubv1.LookupUserRequest{Provider: "discord", Subject: "42"}))
	if err != nil || resp.Msg.GetUser().GetDisplayName() != "42" {
		t.Fatalf("LookupUser with the token: %v %v", err, resp)
	}
	// The hub revokes tok1: the next call gets a 401, refreshes once and succeeds with tok2.
	h.valid.Store(1)
	resp, err = c.Identity().LookupUser(ctx, connect.NewRequest(&hubv1.LookupUserRequest{Provider: "discord", Subject: "43"}))
	if err != nil || resp.Msg.GetUser().GetDisplayName() != "43" || h.issued.Load() != 2 {
		t.Fatalf("retry after a 401: %v %v (%d fetches)", err, resp, h.issued.Load())
	}
	// Everything revoked: the retry's 401 surfaces as unauthenticated, with exactly one more fetch.
	h.valid.Store(100)
	_, err = c.Identity().LookupUser(ctx, connect.NewRequest(&hubv1.LookupUserRequest{Provider: "discord", Subject: "44"}))
	if connect.CodeOf(err) != connect.CodeUnauthenticated || h.issued.Load() != 3 {
		t.Errorf("a second 401 is not retried: %v (%d fetches)", err, h.issued.Load())
	}
	if c.URL() != srv.URL {
		t.Errorf("URL trimmed: %q", c.URL())
	}
}

func TestTokenExpiry(t *testing.T) {
	h, srv := newHub(t)
	h.expires = "90" // refreshed a minute before expiry: after 31 s
	ctx := context.Background()
	c, err := hubclient.New(hubclient.Config{URL: srv.URL, ClientID: "gravel_x", ClientSecret: "s"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Token(ctx); err != nil {
		t.Fatal(err)
	}
	hubclient.SetClock(c, func() time.Time { return time.Now().Add(31 * time.Second) })
	if tok, err := c.Token(ctx); err != nil || tok != "tok2" {
		t.Errorf("refreshed near expiry: %q %v", tok, err)
	}
	c.Invalidate()
	if tok, err := c.Token(ctx); err != nil || tok != "tok3" {
		t.Errorf("after Invalidate: %q %v", tok, err)
	}
}

func TestErrors(t *testing.T) {
	_, srv := newHub(t)
	if _, err := hubclient.New(hubclient.Config{URL: "hub:8080", ClientID: "a", ClientSecret: "b"}); err == nil {
		t.Error("a URL without a scheme is refused")
	}
	if _, err := hubclient.New(hubclient.Config{URL: srv.URL, ClientID: "", ClientSecret: "b"}); err == nil {
		t.Error("credentials are required")
	}
	c, err := hubclient.New(hubclient.Config{URL: srv.URL, ClientID: "gravel_x", ClientSecret: "wrong"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Token(context.Background()); !errors.Is(err, hubclient.ErrInvalidClient) {
		t.Errorf("wrong secret: %v", err)
	}
	if _, err := c.Identity().LookupUser(context.Background(), connect.NewRequest(&hubv1.LookupUserRequest{})); !errors.Is(err, hubclient.ErrInvalidClient) {
		t.Errorf("a call without a token fails with the token error: %v", err)
	}
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "boom", 500) }))
	t.Cleanup(down.Close)
	c, _ = hubclient.New(hubclient.Config{URL: down.URL, ClientID: "gravel_x", ClientSecret: "s"})
	if _, err := c.Token(context.Background()); err == nil || !strings.Contains(err.Error(), "500") {
		t.Errorf("a failing hub: %v", err)
	}
}

type serversHub struct {
	hubv1connect.UnimplementedServerServiceHandler
	auth atomic.Value
}

func (h *serversHub) ListServerPlayers(_ context.Context, req *connect.Request[hubv1.ListServerPlayersRequest]) (*connect.Response[hubv1.ListServerPlayersResponse], error) {
	h.auth.Store(req.Header().Get("Authorization"))
	return connect.NewResponse(&hubv1.ListServerPlayersResponse{Players: []*hubv1.ServerPlayer{{Name: req.Msg.GetServerId()}}}), nil
}

// Servers calls ServerService as the app, with its token.
func TestServersCallsAsTheApp(t *testing.T) {
	sh := &serversHub{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /oauth/token", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"tok1","token_type":"Bearer","expires_in":3600}`))
	})
	mux.Handle(hubv1connect.NewServerServiceHandler(sh))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c, err := hubclient.New(hubclient.Config{URL: srv.URL, ClientID: "gravel_x", ClientSecret: "s"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.Servers().ListServerPlayers(context.Background(), connect.NewRequest(&hubv1.ListServerPlayersRequest{ServerId: "wd-1"}))
	if err != nil {
		t.Fatal(err)
	}
	if got.Msg.GetPlayers()[0].GetName() != "wd-1" || sh.auth.Load() != "Bearer tok1" {
		t.Errorf("players %v, auth %v", got.Msg.GetPlayers(), sh.auth.Load())
	}
}

type moderationHub struct {
	hubv1connect.UnimplementedModerationServiceHandler
	auth atomic.Value
}

func (h *moderationHub) KickPlayer(_ context.Context, req *connect.Request[hubv1.KickPlayerRequest]) (*connect.Response[hubv1.KickPlayerResponse], error) {
	h.auth.Store(req.Header().Get("Authorization") + " " + req.Msg.GetOnBehalfOf().GetSubject())
	return connect.NewResponse(&hubv1.KickPlayerResponse{AuditId: 7}), nil
}

// Moderation calls ModerationService as the app, with its token.
func TestModerationCallsAsTheApp(t *testing.T) {
	mh := &moderationHub{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /oauth/token", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"tok1","token_type":"Bearer","expires_in":3600}`))
	})
	mux.Handle(hubv1connect.NewModerationServiceHandler(mh))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c, err := hubclient.New(hubclient.Config{URL: srv.URL, ClientID: "gravel_x", ClientSecret: "s"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.Moderation().KickPlayer(context.Background(), connect.NewRequest(&hubv1.KickPlayerRequest{
		ServerId: "wd-1", Subject: "76561190000000001", Reason: "r", OnBehalfOf: &hubv1.Actor{Provider: "discord", Subject: "42"}}))
	if err != nil {
		t.Fatal(err)
	}
	if got.Msg.GetAuditId() != 7 || mh.auth.Load() != "Bearer tok1 42" {
		t.Errorf("audit id %d, auth %v", got.Msg.GetAuditId(), mh.auth.Load())
	}
}

type configHub struct {
	hubv1connect.UnimplementedServerConfigServiceHandler
	auth atomic.Value
}

func (h *configHub) GetServerConfig(_ context.Context, req *connect.Request[hubv1.GetServerConfigRequest]) (*connect.Response[hubv1.GetServerConfigResponse], error) {
	h.auth.Store(req.Header().Get("Authorization"))
	return connect.NewResponse(&hubv1.GetServerConfigResponse{Revision: req.Msg.GetServerId()}), nil
}

// ServerConfig calls ServerConfigService as the app, with its token.
func TestServerConfigCallsAsTheApp(t *testing.T) {
	ch := &configHub{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /oauth/token", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"tok1","token_type":"Bearer","expires_in":3600}`))
	})
	mux.Handle(hubv1connect.NewServerConfigServiceHandler(ch))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c, err := hubclient.New(hubclient.Config{URL: srv.URL, ClientID: "gravel_x", ClientSecret: "s"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.ServerConfig().GetServerConfig(context.Background(), connect.NewRequest(&hubv1.GetServerConfigRequest{ServerId: "wd-1"}))
	if err != nil || got.Msg.GetRevision() != "wd-1" || ch.auth.Load() != "Bearer tok1" {
		t.Errorf("config %v, %v, auth %v", got, err, ch.auth.Load())
	}
}
