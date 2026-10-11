package recaps_test

import (
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
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/gravel-project/gravel/discord/bot"
	"github.com/gravel-project/gravel/discord/hubclient"
	"github.com/gravel-project/gravel/discord/modules/recaps"
	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
	"github.com/gravel-project/gravel/gen/gravel/hub/v1/hubv1connect"
)

const (
	appID   = "1558123590786748516"
	channel = "1300000000000000100"
	locked  = "1300000000000000101"
	someone = "1300000000000000999"
	server  = "htg-wardogs-1"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

var chicago, _ = time.LoadLocation("America/Chicago")

// Wednesday 2026-10-14, 18:00–22:00 Chicago: 23:00–03:00 UTC.
var (
	winStart = time.Date(2026, 10, 14, 18, 0, 0, 0, chicago)
	winEnd   = time.Date(2026, 10, 14, 22, 0, 0, 0, chicago)
)

func rule() *hubv1.DiscordRecap {
	return &hubv1.DiscordRecap{Server: server, Channel: channel, Title: "Wardogs Wednesday", Floor: 20, Note: "See you next week.",
		Hours: []*hubv1.DiscordSeedingHours{{Days: []string{"wed"}, From: "18:00", To: "22:00"}}}
}

func fullRecap() *hubv1.GetSessionRecapResponse {
	ended := timestamppb.New(winStart.Add(2 * time.Hour))
	return &hubv1.GetSessionRecapResponse{PeakPlayers: 24, Minutes: 230, MinutesAtFloor: 95, Players: 31, Floor: 20,
		Matches: []*hubv1.RecapMatch{
			{Id: "1", StartedAt: timestamppb.New(winStart), EndedAt: ended, Map: "Ozeti", Players: 20,
				Teams: []*hubv1.TeamScore{{Name: "Lonestar", Score: 1200}, {Name: "Valkyra", Score: 900}}},
			{Id: "2", StartedAt: ended, Map: "Europe", Players: 24},
		},
		Top: []*hubv1.BoardEntry{{Rank: 1, Name: "Jo", Kills: 31}, {Rank: 2, Name: "Brave Otter", Pseudonymous: true, Kills: 1}},
	}
}

// fakeHub is the hub as the bot's app sees it: the settings, the servers and the recaps.
type fakeHub struct {
	hubv1connect.UnimplementedOrganizationServiceHandler
	hubv1connect.UnimplementedServerServiceHandler
	hubv1connect.UnimplementedStatsServiceHandler
	srv *httptest.Server

	mu       sync.Mutex
	rules    []*hubv1.DiscordRecap
	recap    *hubv1.GetSessionRecapResponse
	recapErr error
	asked    []*hubv1.GetSessionRecapRequest
	down     bool
}

func newFakeHub(t *testing.T) *fakeHub {
	t.Helper()
	f := &fakeHub{rules: []*hubv1.DiscordRecap{rule()}, recap: fullRecap()}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /oauth/token", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"tok","token_type":"Bearer","expires_in":3600}`))
	})
	for _, reg := range []func() (string, http.Handler){
		func() (string, http.Handler) { return hubv1connect.NewOrganizationServiceHandler(f) },
		func() (string, http.Handler) { return hubv1connect.NewServerServiceHandler(f) },
		func() (string, http.Handler) { return hubv1connect.NewStatsServiceHandler(f) },
	} {
		path, h := reg()
		mux.Handle(path, h)
	}
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeHub) GetOrganizationSettings(context.Context, *connect.Request[hubv1.GetOrganizationSettingsRequest]) (*connect.Response[hubv1.GetOrganizationSettingsResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("down"))
	}
	return connect.NewResponse(&hubv1.GetOrganizationSettingsResponse{Settings: &hubv1.OrganizationSettings{Version: 1,
		Stats: &hubv1.StatsSettings{Timezone: "America/Chicago"}, Discord: &hubv1.DiscordSettings{GuildId: "1558121195340042350", Recaps: f.rules}}}), nil
}

func (f *fakeHub) ListServers(context.Context, *connect.Request[hubv1.ListServersRequest]) (*connect.Response[hubv1.ListServersResponse], error) {
	return connect.NewResponse(&hubv1.ListServersResponse{Servers: []*hubv1.Server{{Id: server, Name: "HTG WARDOGS | NA WEST | #1"}}}), nil
}

func (f *fakeHub) GetSessionRecap(_ context.Context, req *connect.Request[hubv1.GetSessionRecapRequest]) (*connect.Response[hubv1.GetSessionRecapResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, req.Msg)
	if f.recapErr != nil {
		return nil, f.recapErr
	}
	return connect.NewResponse(f.recap), nil
}

func (f *fakeHub) set(fn func(*fakeHub)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

type message struct {
	ID        string         `json:"id"`
	ChannelID string         `json:"channel_id"`
	Author    map[string]any `json:"author"`
	Content   string         `json:"content"`
	Flags     int            `json:"flags"`
	Timestamp string         `json:"timestamp"`
}

// fakeDiscord is a channel's messages.
type fakeDiscord struct {
	srv      *httptest.Server
	mu       sync.Mutex
	next     int
	messages map[string][]*message
	calls    []string
	mentions []string
}

func newFakeDiscord(t *testing.T) *fakeDiscord {
	t.Helper()
	f := &fakeDiscord{next: 1, messages: map[string][]*message{}}
	mux := http.NewServeMux()
	guard := func(w http.ResponseWriter, r *http.Request) bool {
		f.calls = append(f.calls, r.Method+" "+r.PathValue("channel"))
		if r.PathValue("channel") == locked {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"code":50001,"message":"Missing Access"}`)
			return false
		}
		return true
	}
	mux.HandleFunc("GET /api/channels/{channel}/messages", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if !guard(w, r) {
			return
		}
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		msgs := f.messages[r.PathValue("channel")]
		out := []*message{}
		for i := len(msgs) - 1; i >= 0 && len(out) < limit; i-- {
			out = append(out, msgs[i])
		}
		writeJSON(w, out)
	})
	mux.HandleFunc("POST /api/channels/{channel}/messages", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if !guard(w, r) {
			return
		}
		var body struct {
			Content         string          `json:"content"`
			Flags           int             `json:"flags"`
			AllowedMentions json.RawMessage `json:"allowed_mentions"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mentions = append(f.mentions, string(body.AllowedMentions))
		writeJSON(w, f.add(r.PathValue("channel"), appID, body.Content, body.Flags))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected Discord call %s %s", r.Method, r.URL)
		http.Error(w, "unexpected", http.StatusNotFound)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeDiscord) add(ch, author, content string, flags int) *message {
	m := &message{ID: strconv.Itoa(1400000000000000000 + f.next), ChannelID: ch, Author: map[string]any{"id": author, "username": "u"},
		Content: content, Flags: flags, Timestamp: "2026-10-15T03:05:00Z"}
	f.next++
	f.messages[ch] = append(f.messages[ch], m)
	return m
}

func (f *fakeDiscord) takeCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.calls
	f.calls = nil
	return c
}

func (f *fakeDiscord) posts() []*message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*message(nil), f.messages[channel]...)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

type rig struct {
	hub     *fakeHub
	discord *fakeDiscord
	mod     *recaps.Module
	rt      *bot.Runtime
	now     time.Time
}

func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{hub: newFakeHub(t), discord: newFakeDiscord(t), now: winEnd.Add(-time.Hour)}
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
	bc.Discord.ApplicationID = appID
	bc.Discord.PublicKey = hex.EncodeToString(pub)
	bc.Discord.Token = base64.RawStdEncoding.EncodeToString([]byte(appID)) + ".Xa1b2c.not-a-real-token-signature"
	bc.Discord.Gateway = false
	r.mod = recaps.New(hc, recaps.Config{Interval: time.Hour, PublicURL: "https://app.example.com/"})
	r.mod.SetClock(func() time.Time { return r.now })
	r.rt, err = bot.New(bot.Options{Config: bc, Logger: quiet(), RESTURL: r.discord.srv.URL + "/api"}, r.mod)
	if err != nil {
		t.Fatal(err)
	}
	rt := r.rt
	t.Cleanup(func() { rt.Close(context.Background()) })
}

func (r *rig) pass(t *testing.T) recaps.Result {
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

func TestPostsOnceAfterTheWindow(t *testing.T) {
	r := newRig(t)
	// During the window, and inside the grace after it: the last ended window is last week's,
	// which is past the catch-up, so nothing.
	for _, at := range []time.Time{winEnd.Add(-time.Hour), winEnd, winEnd.Add(2 * time.Minute)} {
		r.now = at
		if res := r.pass(t); len(res.Posted)+len(res.Found)+len(res.Nobody) != 0 {
			t.Fatalf("%s: %+v", at, res)
		}
	}
	if c := r.discord.takeCalls(); len(c) != 0 {
		t.Errorf("no Discord call before the recap is due: %v", c)
	}

	r.now = winEnd.Add(4 * time.Minute)
	if res := r.pass(t); len(res.Posted) != 1 {
		t.Fatalf("after the grace: %+v", res)
	}
	r.hub.mu.Lock()
	asked := r.hub.asked
	r.hub.mu.Unlock()
	if len(asked) != 1 || asked[0].GetServerId() != server || !asked[0].GetFrom().AsTime().Equal(winStart) || !asked[0].GetTo().AsTime().Equal(winEnd) || asked[0].GetFloor() != 20 {
		t.Fatalf("recap asked for: %v", asked)
	}
	posts := r.discord.posts()
	if len(posts) != 1 {
		t.Fatalf("posts: %d", len(posts))
	}
	p := posts[0]
	link := "https://app.example.com/servers/htg-wardogs-1/recap?" + url.Values{"from": {"2026-10-14T23:00:00Z"}, "to": {"2026-10-15T03:00:00Z"}, "floor": {"20"}}.Encode()
	for _, want := range []string{
		"## Wardogs Wednesday recap",
		"**HTG WARDOGS \\| NA WEST \\| \\#1** · <t:" + strconv.FormatInt(winStart.Unix(), 10) + ":f> to <t:" + strconv.FormatInt(winEnd.Unix(), 10) + ":t>",
		"Peak **24** players · **1h 35m** at 20 or more · **31** players",
		"- Ozeti: Lonestar **1200** · Valkyra **900**\n- Europe\n",
		"1. Jo: **31** kills\n2. Brave Otter: **1** kill\n",
		"See you next week.",
		"[The full recap](" + link + ")",
		"-# recap `htg-wardogs-1` · " + strconv.FormatInt(winStart.Unix(), 10),
	} {
		if !strings.Contains(p.Content, want) {
			t.Errorf("post should contain %q:\n%s", want, p.Content)
		}
	}
	if p.Flags&4 == 0 {
		t.Errorf("the link makes no preview (suppress embeds): flags %d", p.Flags)
	}
	if s, at, ok := recaps.Marker(p.Content); !ok || s != server || at != winStart.Unix() {
		t.Errorf("marker: %s %d %t", s, at, ok)
	}

	if m := r.metrics(t); !strings.Contains(m, `gravel_bot_recaps_total{result="posted",server="htg-wardogs-1"} 1`) {
		t.Errorf("metrics:\n%s", m)
	}

	// The next pass: nothing at all.
	r.discord.takeCalls()
	if res := r.pass(t); len(res.Posted)+len(res.Found) != 0 {
		t.Errorf("again: %+v", res)
	}
	if c := r.discord.takeCalls(); len(c) != 0 {
		t.Errorf("a recapped window costs no Discord call: %v", c)
	}

	// A restart finds the post; someone else's message with the same marker is never taken for it.
	r.discord.mu.Lock()
	r.discord.add(channel, someone, p.Content, 0)
	r.discord.mu.Unlock()
	r.restart(t)
	r.now = winEnd.Add(3 * time.Hour)
	if res := r.pass(t); len(res.Found) != 1 || len(res.Posted) != 0 {
		t.Errorf("after a restart: %+v", res)
	}
	if n := len(r.discord.posts()); n != 2 {
		t.Errorf("no second post: %d messages", n)
	}
	r.discord.mu.Lock()
	for _, am := range r.discord.mentions {
		if am != `{"parse":[],"roles":[],"users":[],"replied_user":false}` {
			t.Errorf("a post allows no mention: %s", am)
		}
	}
	r.discord.mu.Unlock()
}

func TestOtherAuthorsMarkerIsIgnored(t *testing.T) {
	r := newRig(t)
	r.discord.mu.Lock()
	r.discord.add(channel, someone, "-# recap `htg-wardogs-1` · "+strconv.FormatInt(winStart.Unix(), 10), 0)
	r.discord.mu.Unlock()
	r.now = winEnd.Add(5 * time.Minute)
	if res := r.pass(t); len(res.Posted) != 1 {
		t.Errorf("a member's copy of the marker doesn't stop the recap: %+v", res)
	}
}

func TestNobodyCameNoPost(t *testing.T) {
	r := newRig(t)
	r.hub.set(func(f *fakeHub) { f.recap = &hubv1.GetSessionRecapResponse{} })
	r.now = winEnd.Add(5 * time.Minute)
	if res := r.pass(t); len(res.Nobody) != 1 || len(res.Posted) != 0 {
		t.Fatalf("nobody: %+v", res)
	}
	if n := len(r.discord.posts()); n != 0 {
		t.Errorf("nothing posted: %d", n)
	}
	r.hub.set(func(f *fakeHub) { f.asked = nil })
	r.pass(t)
	if len(r.hub.asked) != 0 {
		t.Error("a window nobody came to is not read again")
	}
	if m := r.metrics(t); !strings.Contains(m, `gravel_bot_recaps_total{result="nobody",server="htg-wardogs-1"} 1`) {
		t.Errorf("metrics:\n%s", m)
	}
}

func TestNoMatchStarted(t *testing.T) {
	r := newRig(t)
	r.hub.set(func(f *fakeHub) {
		f.recap = &hubv1.GetSessionRecapResponse{PeakPlayers: 12, Minutes: 45, Players: 14}
	})
	r.now = winEnd.Add(5 * time.Minute)
	r.pass(t)
	posts := r.discord.posts()
	if len(posts) != 1 || !strings.Contains(posts[0].Content, "Peak **12** players · **0 minutes** at 20 or more · **14** players") ||
		!strings.Contains(posts[0].Content, "No match started.") || strings.Contains(posts[0].Content, "Top by kills") {
		t.Errorf("under the floor all night: %v", posts)
	}
}

func TestFailuresRetry(t *testing.T) {
	r := newRig(t)
	r.hub.set(func(f *fakeHub) { f.recapErr = connect.NewError(connect.CodeUnavailable, errors.New("busy")) })
	r.now = winEnd.Add(5 * time.Minute)
	if res := r.pass(t); len(res.Failed) != 1 {
		t.Fatalf("hub failing: %+v", res)
	}
	r.hub.set(func(f *fakeHub) { f.recapErr = nil })
	r.now = winEnd.Add(6 * time.Minute)
	if res := r.pass(t); len(res.Posted) != 1 {
		t.Errorf("retried: %+v", res)
	}

	r.hub.set(func(f *fakeHub) { f.rules[0].Channel = locked })
	r.now = winEnd.Add(7 * 24 * time.Hour).Add(5 * time.Minute) // next week
	if res := r.pass(t); len(res.Failed) != 1 {
		t.Errorf("a forbidden channel: %+v", res)
	}
	if m := r.metrics(t); !strings.Contains(m, `gravel_bot_recaps_total{result="forbidden",server="htg-wardogs-1"} 1`) ||
		!strings.Contains(m, `gravel_bot_recaps_total{result="error",server="htg-wardogs-1"} 1`) {
		t.Errorf("metrics:\n%s", m)
	}

	r.hub.set(func(f *fakeHub) { f.down = true })
	if _, err := r.mod.Pass(context.Background()); err == nil {
		t.Error("the hub away is an error for the pass")
	}
}

func TestCatchUpAndIdle(t *testing.T) {
	r := newRig(t)
	r.now = winEnd.Add(CatchUpPlus(time.Minute))
	if res := r.pass(t); len(res.Posted) != 0 || len(res.Pending) != 1 {
		t.Errorf("a window that ended over a day ago is not posted late: %+v", res)
	}
	r.now = winEnd.Add(23 * time.Hour)
	if res := r.pass(t); len(res.Posted) != 1 {
		t.Errorf("within the catch-up it is: %+v", res)
	}
	r.hub.set(func(f *fakeHub) { f.rules = nil })
	if res := r.pass(t); !res.Idle {
		t.Errorf("no rule: %+v", res)
	}
}

func CatchUpPlus(d time.Duration) time.Duration { return recaps.CatchUp + d }

func TestRender(t *testing.T) {
	rc := fullRecap()
	for i := range 10 {
		rc.Matches = append(rc.Matches, &hubv1.RecapMatch{Map: "M" + strconv.Itoa(i)})
	}
	rc.Top[0].Name = "@everyone [x](https://evil.example)"
	got := recaps.Render(recaps.Post{Title: "Night *one*", Server: server, Start: winStart, End: winEnd, Recap: rc})
	for _, want := range []string{"## Night \\*one\\* recap", "**htg-wardogs-1** ·", "Peak **24** players · **31** players\n", "…and 4 more\n", "@\u200beveryone \\[x\\](https://evil.example)"} {
		if !strings.Contains(got, want) {
			t.Errorf("render should contain %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "at 0 or more") || strings.Contains(got, "full recap") {
		t.Errorf("no floor, no link:\n%s", got)
	}
	if _, _, ok := recaps.Marker("-# recap `HTG 1` · 12"); ok {
		t.Error("a marker names a server id")
	}
}
