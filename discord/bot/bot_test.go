package bot_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/gateway"
	"github.com/disgoorg/disgo/handler"

	"github.com/gravel-project/gravel/discord/bot"
	"github.com/gravel-project/gravel/discord/hubclient"
	"github.com/gravel-project/gravel/discord/modules/core"
	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
	"github.com/gravel-project/gravel/gen/gravel/hub/v1/hubv1connect"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fakeDiscord answers the REST calls the runtime makes at start: command registration.
type fakeDiscord struct {
	srv  *httptest.Server
	mu   sync.Mutex
	puts []string
	last []map[string]any
}

func newFakeDiscord(t *testing.T) *fakeDiscord {
	t.Helper()
	f := &fakeDiscord{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/commands") {
			var cmds []map[string]any
			_ = json.NewDecoder(r.Body).Decode(&cmds)
			f.mu.Lock()
			f.puts = append(f.puts, r.URL.Path)
			f.last = cmds
			f.mu.Unlock()
			for i := range cmds {
				cmds[i]["id"] = strconv.Itoa(100 + i)
				cmds[i]["application_id"] = "1558123590786748516"
				cmds[i]["version"] = "1"
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(cmds)
			return
		}
		// With the gateway on, building the client asks for the gateway URL; nothing dials it.
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/gateway") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"url":"wss://gateway.invalid"}`)
			return
		}
		http.Error(w, "unexpected: "+r.Method+" "+r.URL.Path, http.StatusNotFound)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// fakeHub answers /oauth/token and LookupUser as the hub would.
type fakeHub struct {
	hubv1connect.UnimplementedIdentityServiceHandler
	srv    *httptest.Server
	tokens int
}

func newFakeHub(t *testing.T) *fakeHub {
	t.Helper()
	f := &fakeHub{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /oauth/token", func(w http.ResponseWriter, r *http.Request) {
		id, secret, ok := r.BasicAuth()
		if !ok || id != "gravel_bot" || secret != "s3cret" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"invalid_client"}`))
			return
		}
		f.tokens++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"tok` + strconv.Itoa(f.tokens) + `","token_type":"Bearer","expires_in":3600,"scope":"identity:read"}`))
	})
	path, h := hubv1connect.NewIdentityServiceHandler(f)
	mux.Handle(path, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer tok") {
			http.Error(w, "no bearer", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	}))
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeHub) LookupUser(_ context.Context, req *connect.Request[hubv1.LookupUserRequest]) (*connect.Response[hubv1.LookupUserResponse], error) {
	if req.Msg.GetProvider() == "discord" && req.Msg.GetSubject() == "42" {
		return connect.NewResponse(&hubv1.LookupUserResponse{User: &hubv1.User{Id: "u1", DisplayName: "Jo", Identities: []*hubv1.Identity{
			{Provider: "discord", Subject: "42", DisplayName: "jo"}, {Provider: "steam", Subject: "7656", DisplayName: "JoSteam"},
		}}}), nil
	}
	return nil, connect.NewError(connect.CodeNotFound, errors.New("not found"))
}

type signer struct {
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
}

func newSigner(t *testing.T) signer {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return signer{pub: pub, priv: priv}
}

// post sends an interaction to the runtime's handler, signed like Discord signs it.
func (s signer) post(t *testing.T, h http.Handler, body string, sign bool) (int, map[string]any) {
	t.Helper()
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/interactions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Signature-Timestamp", ts)
	sig := ed25519.Sign(s.priv, []byte(ts+body))
	if !sign {
		sig[0] ^= 0xff
	}
	req.Header.Set("X-Signature-Ed25519", hex.EncodeToString(sig))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func testConfig(s signer, hub *fakeHub) bot.Config {
	cfg := bot.Default()
	cfg.Discord.ApplicationID = "1558123590786748516"
	cfg.Discord.PublicKey = hex.EncodeToString(s.pub)
	cfg.Discord.Token = fakeToken("1558123590786748516")
	cfg.Discord.Gateway = false
	cfg.Discord.GuildIDs = []string{"1558121195340042350"}
	cfg.Hub.URL = hub.srv.URL
	cfg.Hub.PublicURL = "https://app.example.com"
	cfg.Hub.ClientID = "gravel_bot"
	cfg.Hub.ClientSecret = "s3cret"
	cfg.Server.Listen = "127.0.0.1:0"
	cfg.Server.InternalListen = "127.0.0.1:0"
	return cfg
}

// fakeToken has a bot token's shape: the application id in base64, a dot, then the rest.
func fakeToken(appID string) string {
	return base64.RawStdEncoding.EncodeToString([]byte(appID)) + ".Xa1b2c.not-a-real-token-signature"
}

func slash(name, userID string) string {
	return `{"type":2,"id":"900","application_id":"1558123590786748516","token":"itoken","version":1,"guild_id":"1558121195340042350","channel_id":"7",` +
		`"data":{"id":"100","name":"` + name + `","type":1},"member":{"user":{"id":"` + userID + `","username":"jo","discriminator":"0","avatar":null},"roles":[],"joined_at":"2026-10-09T12:00:00Z","permissions":"0"}}`
}

func TestRuntime(t *testing.T) {
	s := newSigner(t)
	discordAPI := newFakeDiscord(t)
	hubSrv := newFakeHub(t)
	cfg := testConfig(s, hubSrv)
	hub, err := hubclient.New(hubclient.Config{URL: cfg.Hub.URL, ClientID: cfg.Hub.ClientID, ClientSecret: cfg.Hub.ClientSecret})
	if err != nil {
		t.Fatal(err)
	}
	rt, err := bot.New(bot.Options{Config: cfg, Logger: quiet(), Version: "test", RESTURL: discordAPI.srv.URL + "/api/"}, core.New(hub, cfg.Hub.PublicURL))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rt.Close(context.Background()) })
	if names := commandNames(rt.Commands()); strings.Join(names, ",") != "whoami,link" {
		t.Errorf("commands: %v", names)
	}

	// Before Start: alive, not ready.
	rec := httptest.NewRecorder()
	rt.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("readyz before start: %d", rec.Code)
	}
	if err := rt.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(discordAPI.puts) != 1 || !strings.Contains(discordAPI.puts[0], "/guilds/1558121195340042350/commands") || len(discordAPI.last) != 2 {
		t.Errorf("registration: %v %v", discordAPI.puts, discordAPI.last)
	}
	rec = httptest.NewRecorder()
	rt.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("readyz after start (gateway off): %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	rt.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"version":"test"`) {
		t.Errorf("healthz: %d %s", rec.Code, rec.Body.String())
	}

	// Discord's PING, signed, is answered PONG; a bad signature is refused.
	if code, out := s.post(t, rt.Handler(), `{"type":1}`, true); code != 200 || out["type"] != float64(1) {
		t.Errorf("ping: %d %v", code, out)
	}
	if code, _ := s.post(t, rt.Handler(), `{"type":1}`, false); code != http.StatusUnauthorized {
		t.Errorf("bad signature: %d", code)
	}

	// /link needs no hub; /whoami resolves the member through the hub with a bearer token.
	code, out := s.post(t, rt.Handler(), slash("link", "42"), true)
	if code != 200 || out["type"] != float64(4) {
		t.Fatalf("/link: %d %v", code, out)
	}
	data := out["data"].(map[string]any)
	if !strings.Contains(data["content"].(string), "https://app.example.com/account") || data["flags"] != float64(64) {
		t.Errorf("/link reply: %v", data)
	}
	code, out = s.post(t, rt.Handler(), slash("whoami", "42"), true)
	if code != 200 || out["type"] != float64(4) {
		t.Fatalf("/whoami: %d %v", code, out)
	}
	content := out["data"].(map[string]any)["content"].(string)
	if !strings.Contains(content, "**Jo**") || !strings.Contains(content, "Steam: JoSteam") || !strings.Contains(content, "Discord: jo") {
		t.Errorf("/whoami reply: %q", content)
	}
	code, out = s.post(t, rt.Handler(), slash("whoami", "99"), true)
	if code != 200 || !strings.Contains(out["data"].(map[string]any)["content"].(string), "doesn't know this Discord account") {
		t.Errorf("/whoami unknown: %d %v", code, out)
	}
	if hubSrv.tokens != 1 {
		t.Errorf("one token fetch serves both calls: %d", hubSrv.tokens)
	}
	code, out = s.post(t, rt.Handler(), slash("nope", "42"), true)
	if code != 200 || !strings.Contains(out["data"].(map[string]any)["content"].(string), "not available") {
		t.Errorf("unknown command: %d %v", code, out)
	}

	// Metrics count the routes.
	rec = httptest.NewRecorder()
	rt.InternalHandler().ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/metrics", nil))
	body := rec.Body.String()
	for _, want := range []string{`gravel_bot_interactions_total{result="ok",route="/whoami"} 2`, `gravel_bot_interactions_total{result="ok",route="/link"} 1`, `gravel_bot_build_info{`,
		`gravel_bot_interaction_duration_seconds_count{route="/whoami"} 2`, `gravel_bot_interaction_duration_seconds_bucket{route="/link",le="3"} 1`,
		"gravel_bot_gateway_connected 0"} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics lack %q", want)
		}
	}
}

func commandNames(cmds []discord.ApplicationCommandCreate) []string {
	var out []string
	for _, c := range cmds {
		out = append(out, c.CommandName())
	}
	return out
}

type dupModule struct{}

func (dupModule) Name() string { return "dup" }
func (dupModule) Register(r *bot.Registry) error {
	return r.SlashCommand(discord.SlashCommandCreate{Name: "link", Description: "again"}, func(e *handler.CommandEvent) error { return nil })
}

type jobModule struct{ ran chan struct{} }

func (jobModule) Name() string { return "job" }
func (m jobModule) Register(r *bot.Registry) error {
	r.Job("tick", func(ctx context.Context) error {
		close(m.ran)
		<-ctx.Done()
		return ctx.Err()
	})
	return nil
}

func TestRegistryAndRun(t *testing.T) {
	s := newSigner(t)
	discordAPI := newFakeDiscord(t)
	hubSrv := newFakeHub(t)
	cfg := testConfig(s, hubSrv)
	hub, _ := hubclient.New(hubclient.Config{URL: cfg.Hub.URL, ClientID: "gravel_bot", ClientSecret: "s3cret"})
	if _, err := bot.New(bot.Options{Config: cfg, Logger: quiet()}, core.New(hub, cfg.Hub.PublicURL), dupModule{}); err == nil || !strings.Contains(err.Error(), "registered by both core and dup") {
		t.Errorf("a duplicate command name: %v", err)
	}
	bad := cfg
	bad.Discord.PublicKey = "zz"
	if _, err := bot.New(bot.Options{Config: bad, Logger: quiet()}); err == nil {
		t.Error("a bad public key is refused")
	}
	bad = cfg
	bad.Discord.Token = "not-a-token"
	if _, err := bot.New(bot.Options{Config: bad, Logger: quiet()}); err == nil || !strings.Contains(err.Error(), "not a bot token") {
		t.Errorf("a token without the application id: %v", err)
	}
	bad = cfg
	bad.Discord.Token = fakeToken("1111111111111111111")
	if _, err := bot.New(bot.Options{Config: bad, Logger: quiet()}); err == nil || !strings.Contains(err.Error(), "belongs to application 1111111111111111111") {
		t.Errorf("a token of another application: %v", err)
	}

	jm := jobModule{ran: make(chan struct{})}
	rt, err := bot.New(bot.Options{Config: cfg, Logger: quiet(), Version: "test", RESTURL: discordAPI.srv.URL + "/api/"}, jm)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- rt.Run(ctx) }()
	public, internal, err := rt.Addrs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-jm.ran:
	case <-time.After(5 * time.Second):
		t.Fatal("the job did not start")
	}
	resp, err := http.Get("http://" + public.String() + "/readyz") //nolint:noctx // a test probe
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("readyz over the listener: %d", resp.StatusCode)
	}
	resp, err = http.Get("http://" + internal.String() + "/metrics") //nolint:noctx // a test probe
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !bytes.Contains(b, []byte("gravel_bot_build_info")) {
		t.Error("metrics over the internal listener")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run after cancel: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not stop")
	}
}

type failModule struct{}

func (failModule) Name() string { return "fail" }
func (failModule) Register(r *bot.Registry) error {
	return r.SlashCommand(discord.SlashCommandCreate{Name: "boom", Description: "fails"}, func(e *handler.CommandEvent) error {
		return errors.New("boom")
	})
}

// The gateway's live status drives readiness and both gateway metrics: a dropped session reads
// as not ready and disconnected at once, and a resumed one as ready again.
func TestGatewayStateDrivesReadinessAndMetrics(t *testing.T) {
	s := newSigner(t)
	discordAPI := newFakeDiscord(t)
	hubSrv := newFakeHub(t)
	cfg := testConfig(s, hubSrv)
	cfg.Discord.Gateway = true
	rt, err := bot.New(bot.Options{Config: cfg, Logger: quiet(), Version: "test", RESTURL: discordAPI.srv.URL + "/api/"}, failModule{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rt.Close(context.Background()) })
	bot.MarkReady(rt)

	var (
		mu      sync.Mutex
		status  = gateway.StatusReady
		latency = 42 * time.Millisecond
	)
	bot.SetGatewayState(rt, func() (gateway.Status, time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		return status, latency
	})
	set := func(st gateway.Status, l time.Duration) {
		mu.Lock()
		status, latency = st, l
		mu.Unlock()
	}
	readyz := func() int {
		rec := httptest.NewRecorder()
		rt.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/readyz", nil))
		return rec.Code
	}
	metrics := func() string {
		rec := httptest.NewRecorder()
		rt.InternalHandler().ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/metrics", nil))
		return rec.Body.String()
	}

	if code, m := readyz(), metrics(); code != http.StatusOK || !strings.Contains(m, "gravel_bot_gateway_connected 1") || !strings.Contains(m, "gravel_bot_gateway_latency_seconds 0.042") {
		t.Errorf("ready session: readyz %d\n%s", code, grepLines(m, "gravel_bot_gateway"))
	}
	for _, st := range []gateway.Status{gateway.StatusDisconnected, gateway.StatusResuming, gateway.StatusUnconnected, gateway.StatusWaitingForReady} {
		set(st, 42*time.Millisecond)
		if code, m := readyz(), metrics(); code != http.StatusServiceUnavailable || !strings.Contains(m, "gravel_bot_gateway_connected 0") || strings.Contains(m, "gravel_bot_gateway_latency_seconds") {
			t.Errorf("%s: readyz %d\n%s", st, code, grepLines(m, "gravel_bot_gateway"))
		}
	}
	// A heartbeat in flight: ready, but disgo's latency is negative, so no latency sample.
	set(gateway.StatusReady, -time.Second)
	if code, m := readyz(), metrics(); code != http.StatusOK || !strings.Contains(m, "gravel_bot_gateway_connected 1") || strings.Contains(m, "gravel_bot_gateway_latency_seconds") {
		t.Errorf("heartbeat in flight: readyz %d\n%s", code, grepLines(m, "gravel_bot_gateway"))
	}

	// A failing handler is timed and counted as an error.
	if code, _ := s.post(t, rt.Handler(), slash("boom", "42"), true); code == 0 {
		t.Fatal("no answer")
	}
	m := metrics()
	for _, want := range []string{`gravel_bot_interactions_total{result="error",route="/boom"} 1`, `gravel_bot_interaction_duration_seconds_count{route="/boom"} 1`} {
		if !strings.Contains(m, want) {
			t.Errorf("metrics lack %q", want)
		}
	}
}

func grepLines(s, sub string) string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.Contains(l, sub) {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}
