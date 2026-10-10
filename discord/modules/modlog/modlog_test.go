package modlog_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
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
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/gravel-project/gravel/discord/bot"
	"github.com/gravel-project/gravel/discord/hubclient"
	"github.com/gravel-project/gravel/discord/modules/modlog"
	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
	"github.com/gravel-project/gravel/gen/gravel/hub/v1/hubv1connect"
)

const (
	appID   = "1558123590786748516"
	channel = "1558200000000000001"
)

// fakeHub serves the settings, the server list and the audit log (newest first, paged by id).
type fakeHub struct {
	hubv1connect.UnimplementedOrganizationServiceHandler
	hubv1connect.UnimplementedServerServiceHandler
	hubv1connect.UnimplementedModerationServiceHandler
	srv *httptest.Server

	mu      sync.Mutex
	modLog  string
	entries []*hubv1.AuditEntry // ascending by id
	reads   int
}

func newFakeHub(t *testing.T) *fakeHub {
	t.Helper()
	f := &fakeHub{modLog: channel}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /oauth/token", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"tok","token_type":"Bearer","expires_in":3600,"scope":"servers:moderate"}`)
	})
	for _, h := range []func() (string, http.Handler){
		func() (string, http.Handler) { return hubv1connect.NewOrganizationServiceHandler(f) },
		func() (string, http.Handler) { return hubv1connect.NewServerServiceHandler(f) },
		func() (string, http.Handler) { return hubv1connect.NewModerationServiceHandler(f) },
	} {
		path, handler := h()
		mux.Handle(path, handler)
	}
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeHub) GetOrganizationSettings(context.Context, *connect.Request[hubv1.GetOrganizationSettingsRequest]) (*connect.Response[hubv1.GetOrganizationSettingsResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return connect.NewResponse(&hubv1.GetOrganizationSettingsResponse{Settings: &hubv1.OrganizationSettings{Discord: &hubv1.DiscordSettings{ModLog: f.modLog}}}), nil
}

func (f *fakeHub) ListServers(context.Context, *connect.Request[hubv1.ListServersRequest]) (*connect.Response[hubv1.ListServersResponse], error) {
	return connect.NewResponse(&hubv1.ListServersResponse{Servers: []*hubv1.Server{{Id: "wd-1", Name: "War Dogs #1"}}}), nil
}

func (f *fakeHub) ListAuditLog(_ context.Context, req *connect.Request[hubv1.ListAuditLogRequest]) (*connect.Response[hubv1.ListAuditLogResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	before := int64(1 << 62)
	if t := req.Msg.GetPageToken(); t != "" {
		before, _ = strconv.ParseInt(t, 10, 64)
	}
	size := int(req.Msg.GetPageSize())
	out := &hubv1.ListAuditLogResponse{}
	for i := len(f.entries) - 1; i >= 0 && len(out.Entries) < size; i-- {
		if f.entries[i].GetId() < before {
			out.Entries = append(out.Entries, f.entries[i])
		}
	}
	if n := len(out.Entries); n == size && out.Entries[n-1].GetId() > 1 {
		out.NextPageToken = strconv.FormatInt(out.Entries[n-1].GetId(), 10)
	}
	return connect.NewResponse(out), nil
}

func (f *fakeHub) add(e *hubv1.AuditEntry) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e.Id = int64(len(f.entries) + 1)
	if e.At == nil {
		e.At = timestamppb.New(time.Now().Add(-time.Hour))
	}
	if e.ServerId == "" {
		e.ServerId = "wd-1"
	}
	f.entries = append(f.entries, e)
}

// fakeDiscord is one channel's messages; forbid refuses every call with 403.
type fakeDiscord struct {
	srv    *httptest.Server
	mu     sync.Mutex
	posts  []string // contents, oldest first
	forbid bool
}

func newFakeDiscord(t *testing.T) *fakeDiscord {
	t.Helper()
	f := &fakeDiscord{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if f.forbid {
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"code":50001,"message":"Missing Access"}`)
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/channels/"+channel+"/messages") {
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
			return
		}
		switch r.Method {
		case http.MethodGet:
			var msgs []map[string]any // newest first, as Discord answers
			for i := len(f.posts) - 1; i >= 0; i-- {
				msgs = append(msgs, map[string]any{"id": strconv.Itoa(1000 + i), "channel_id": channel, "content": f.posts[i],
					"author": map[string]any{"id": appID, "username": "bot", "discriminator": "0"}, "timestamp": "2026-10-10T20:00:00Z"})
			}
			msgs = append(msgs, map[string]any{"id": "999", "channel_id": channel, "content": "-# audit 999999 (someone else's)",
				"author": map[string]any{"id": "42", "username": "jo", "discriminator": "0"}, "timestamp": "2026-10-10T20:00:00Z"})
			_ = json.NewEncoder(w).Encode(msgs)
		case http.MethodPost:
			var m struct {
				Content         string         `json:"content"`
				AllowedMentions map[string]any `json:"allowed_mentions"`
			}
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &m)
			if m.AllowedMentions == nil {
				http.Error(w, "a post without allowed_mentions", http.StatusBadRequest)
				return
			}
			f.posts = append(f.posts, m.Content)
			_, _ = io.WriteString(w, `{"id":"`+strconv.Itoa(1000+len(f.posts)-1)+`","channel_id":"`+channel+`","content":"","author":{"id":"`+appID+`","username":"bot","discriminator":"0"},"timestamp":"2026-10-10T20:00:00Z"}`)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeDiscord) all() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.posts...)
}

type rig struct {
	hub     *fakeHub
	discord *fakeDiscord
	mod     *modlog.Module
	rt      *bot.Runtime
}

func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{hub: newFakeHub(t), discord: newFakeDiscord(t)}
	r.restart(t)
	return r
}

func (r *rig) restart(t *testing.T) {
	t.Helper()
	hc, err := hubclient.New(hubclient.Config{URL: r.hub.srv.URL, ClientID: "gravel_bot", ClientSecret: "s"})
	if err != nil {
		t.Fatal(err)
	}
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	bc := bot.Default()
	bc.Discord.ApplicationID, bc.Discord.PublicKey, bc.Discord.Gateway = appID, hex.EncodeToString(pub), false
	bc.Discord.Token = base64.RawStdEncoding.EncodeToString([]byte(appID)) + ".Xa1b2c.not-a-real-token-signature"
	r.mod = modlog.New(hc, modlog.Config{Interval: time.Hour})
	r.rt, err = bot.New(bot.Options{Config: bc, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), RESTURL: r.discord.srv.URL + "/api"}, r.mod)
	if err != nil {
		t.Fatal(err)
	}
	rt := r.rt
	t.Cleanup(func() { rt.Close(context.Background()) })
}

func (r *rig) pass(t *testing.T) modlog.Result {
	t.Helper()
	res, err := r.mod.Pass(context.Background())
	if err != nil {
		t.Fatalf("pass: %v", err)
	}
	return res
}

func (r *rig) metrics(t *testing.T) string {
	t.Helper()
	rec := httptest.NewRecorder()
	r.rt.InternalHandler().ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/metrics", nil))
	return rec.Body.String()
}

func kick(subject, reason, outcome string) *hubv1.AuditEntry {
	return &hubv1.AuditEntry{Action: "kick", TargetSubject: subject, Reason: reason, Outcome: outcome, UserId: "u1", UserName: "Jo"}
}

func TestPostsWhatHappensAfterItStarts(t *testing.T) {
	r := newRig(t)
	r.hub.add(kick("76561190000000001", "old news", "ok"))

	// Turned on: history stays out of the channel.
	if res := r.pass(t); res.Posted != 0 || len(r.discord.all()) != 0 {
		t.Fatalf("first pass posted %d: %v", res.Posted, r.discord.all())
	}

	// New entries post in order; one still running waits, and so does everything after it.
	r.hub.add(kick("76561190000000002", "afk", "ok"))
	r.hub.add(&hubv1.AuditEntry{Action: "ban", TargetSubject: "76561190000000003", Reason: "cheating", AppName: "htg-bot",
		OnBehalfOf: &hubv1.Actor{Provider: "discord", Subject: "421234567890123456"}, At: timestamppb.Now()}) // no outcome yet
	r.hub.add(&hubv1.AuditEntry{Action: "broadcast", Message: "restart in 5", Outcome: "ok", UserName: "Jo"})
	res := r.pass(t)
	if res.Posted != 1 || res.Waiting != 2 {
		t.Errorf("pass = %+v", res)
	}
	posts := r.discord.all()
	if len(posts) != 1 || !strings.HasPrefix(posts[0], "**Kick** · War Dogs #1 · `76561190000000002` · by **Jo** · afk · ok\n-# audit 2 · ") {
		t.Errorf("posts = %q", posts)
	}

	// The ban finishes: it and the broadcast post, in order.
	r.hub.mu.Lock()
	r.hub.entries[2].Outcome = "ok"
	r.hub.mu.Unlock()
	if res := r.pass(t); res.Posted != 2 || res.Waiting != 0 {
		t.Errorf("pass = %+v", res)
	}
	posts = r.discord.all()
	if len(posts) != 3 || !strings.Contains(posts[1], "by <@421234567890123456> (via htg-bot)") || !strings.Contains(posts[2], "**Broadcast** · War Dogs #1 · by **Jo** · “restart in 5” · ok") {
		t.Errorf("posts = %q", posts)
	}
	if res := r.pass(t); res.Posted != 0 {
		t.Errorf("a quiet pass posted %d", res.Posted)
	}
	if m := r.metrics(t); !strings.Contains(m, `gravel_bot_modlog_posts_total{result="posted"} 3`) || !strings.Contains(m, "gravel_bot_modlog_last_success_timestamp_seconds") {
		t.Errorf("metrics:\n%s", m)
	}
}

func TestRestartResumesFromItsOwnPosts(t *testing.T) {
	r := newRig(t)
	r.pass(t) // start
	r.hub.add(kick("76561190000000001", "one", "ok"))
	r.pass(t)
	r.hub.add(kick("76561190000000002", "two", "ok")) // while the bot is down
	r.hub.add(kick("76561190000000003", "three", "ok"))
	r.restart(t)
	if res := r.pass(t); res.Posted != 2 {
		t.Fatalf("after a restart: %+v", res)
	}
	posts := r.discord.all()
	if len(posts) != 3 || !strings.Contains(posts[1], "two") || !strings.Contains(posts[2], "three") {
		t.Errorf("posts = %q", posts)
	}
}

func TestStaleEntriesAndRefusals(t *testing.T) {
	r := newRig(t)
	r.pass(t)

	// An entry the hub never finished posts after StaleAfter, as it stands.
	r.hub.add(&hubv1.AuditEntry{Action: "kick", TargetSubject: "76561190000000001", Reason: "x", UserName: "Jo", At: timestamppb.New(time.Now().Add(-modlog.StaleAfter - time.Second))})
	if res := r.pass(t); res.Posted != 1 || !strings.Contains(r.discord.all()[0], "no result recorded") {
		t.Errorf("stale: %+v %q", res, r.discord.all())
	}

	// Discord refuses: counted, nothing lost; the entry posts once allowed.
	r.hub.add(kick("76561190000000002", "y", "player_not_found"))
	r.discord.mu.Lock()
	r.discord.forbid = true
	r.discord.mu.Unlock()
	if res := r.pass(t); !res.Forbidden || res.Posted != 0 {
		t.Errorf("forbidden: %+v", res)
	}
	r.discord.mu.Lock()
	r.discord.forbid = false
	r.discord.mu.Unlock()
	if res := r.pass(t); res.Posted != 1 || !strings.Contains(r.discord.all()[1], "player not found") {
		t.Errorf("after the refusal: %+v %q", res, r.discord.all())
	}
	if m := r.metrics(t); !strings.Contains(m, `gravel_bot_modlog_posts_total{result="forbidden"} 1`) {
		t.Errorf("metrics:\n%s", m)
	}

	// No channel: idle.
	r.hub.mu.Lock()
	r.hub.modLog = ""
	r.hub.mu.Unlock()
	if res := r.pass(t); !res.Idle {
		t.Errorf("no channel: %+v", res)
	}
}

func TestLineEscapesAndPingsNoOne(t *testing.T) {
	e := &hubv1.AuditEntry{Id: 7, At: timestamppb.New(time.Date(2026, 10, 10, 20, 0, 0, 0, time.UTC)), ServerId: "wd-1", Action: "move_player",
		TargetSubject: "765`61", Team: "*Lonestar*", UserName: "@everyone", Outcome: "moved_not_respawned"}
	got := modlog.Line(e, "")
	want := "**Move** · wd-1 · `76561` · by **@\u200beveryone** · to \\*Lonestar\\* · moved not respawned\n-# audit 7 · Oct 10, 20:00 UTC"
	if got != want {
		t.Errorf("line =\n%q\nwant\n%q", got, want)
	}
	if l := modlog.Line(&hubv1.AuditEntry{Id: 1, At: timestamppb.Now(), Action: "kick", Reason: "line one\nline two", AppName: "cron"}, "S"); !strings.Contains(l, "line one line two") || !strings.Contains(l, "by cron") {
		t.Errorf("an app's line = %q", l)
	}
}
