package matchresults_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/disgoorg/disgo/discord"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/gravel-project/gravel/discord/bot"
	"github.com/gravel-project/gravel/discord/hubclient"
	"github.com/gravel-project/gravel/discord/modules/matchresults"
	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
	"github.com/gravel-project/gravel/gen/gravel/hub/v1/hubv1connect"
)

const (
	appID   = "1558123590786748516"
	channel = "1300000000000000100"
	locked  = "1300000000000000101" // a channel the bot may not use
	someone = "1300000000000000999" // a member, not the bot
	cs1     = "htg-cs2-1"
	cs2     = "htg-cs2-2"
)

// start is the module's first pass.
var start = time.Date(2026, 10, 14, 23, 0, 0, 0, time.UTC)

func id(n int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012d", n) }

// series is a finished best-of-three on a server, ended at the given time.
func series(n int, server string, ended time.Time) *hubv1.MatchResult {
	return &hubv1.MatchResult{Id: id(n), ServerId: server, LeaseId: "11111111-0000-4000-8000-000000000001", Team1Name: "Lonestar", Team2Name: "Valkyra",
		NumMaps: 3, Team1Score: 2, Team2Score: 1, Winner: "team1", StartedAt: timestamppb.New(ended.Add(-2 * time.Hour)), EndedAt: timestamppb.New(ended),
		Maps: []*hubv1.MatchResultMap{
			{MapNumber: 1, MapName: "de_inferno", Team1Score: 9, Team2Score: 13, Winner: "team2"},
			{MapNumber: 0, MapName: "de_nuke", Team1Score: 13, Team2Score: 9, Winner: "team1", HasDemo: true},
			{MapNumber: 2, MapName: "de_mirage", Team1Score: 16, Team2Score: 14, Winner: "team1"},
		},
		Players: []*hubv1.MatchResultPlayer{
			{MapNumber: 0, Name: "Jo", UserId: "22222222-0000-4000-8000-000000000001", Team: "team1", Kills: 20, Deaths: 10},
			{MapNumber: 1, Name: "Jo", UserId: "22222222-0000-4000-8000-000000000001", Team: "team1", Kills: 15, Deaths: 12},
			{MapNumber: 0, Name: "Brave Otter", Pseudonymous: true, Team: "team2", Kills: 25, Deaths: 9},
			{MapNumber: 2, Name: "Brave Otter", Pseudonymous: true, Team: "team2", Kills: 5, Deaths: 20},
			{MapNumber: 0, Name: "Sam", UserId: "22222222-0000-4000-8000-000000000002", Team: "team1", Kills: 29, Deaths: 8},
			{MapNumber: 1, Name: "Quiet Heron", Pseudonymous: true, Team: "team2", Kills: 28, Deaths: 30},
		}}
}

// summary is a series as ListMatchResults gives it: no maps, no players.
func summary(r *hubv1.MatchResult) *hubv1.MatchResult {
	s := proto.Clone(r).(*hubv1.MatchResult)
	s.Maps, s.Players = nil, nil
	return s
}

// fakeHub is the hub as the bot's app sees it: the settings and the match results.
type fakeHub struct {
	hubv1connect.UnimplementedOrganizationServiceHandler
	hubv1connect.UnimplementedStatsServiceHandler
	srv *httptest.Server

	mu       sync.Mutex
	settings *hubv1.DiscordMatchResults
	results  []*hubv1.MatchResult
	listErr  error
	listed   []*hubv1.ListMatchResultsRequest
	got      []string
	down     bool
}

func newFakeHub(t *testing.T) *fakeHub {
	t.Helper()
	f := &fakeHub{settings: &hubv1.DiscordMatchResults{Channel: channel}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /oauth/token", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"tok","token_type":"Bearer","expires_in":3600}`))
	})
	for _, reg := range []func() (string, http.Handler){
		func() (string, http.Handler) { return hubv1connect.NewOrganizationServiceHandler(f) },
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
		Discord: &hubv1.DiscordSettings{GuildId: "1558121195340042350", MatchResults: f.settings}}}), nil
}

// ListMatchResults filters as the hub does: the server, ended only, ended after; newest first.
func (f *fakeHub) ListMatchResults(_ context.Context, req *connect.Request[hubv1.ListMatchResultsRequest]) (*connect.Response[hubv1.ListMatchResultsResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listed = append(f.listed, req.Msg)
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := &hubv1.ListMatchResultsResponse{}
	for _, r := range f.results {
		switch {
		case req.Msg.GetServerId() != "" && r.GetServerId() != req.Msg.GetServerId():
		case req.Msg.GetEndedOnly() && r.GetEndedAt() == nil:
		case req.Msg.GetEndedAfter() != nil && r.GetEndedAt() != nil && !r.GetEndedAt().AsTime().After(req.Msg.GetEndedAfter().AsTime()):
		default:
			out.Results = append(out.Results, summary(r))
		}
	}
	slices.SortFunc(out.Results, func(a, b *hubv1.MatchResult) int { return b.GetStartedAt().AsTime().Compare(a.GetStartedAt().AsTime()) })
	return connect.NewResponse(out), nil
}

func (f *fakeHub) GetMatchResult(_ context.Context, req *connect.Request[hubv1.GetMatchResultRequest]) (*connect.Response[hubv1.GetMatchResultResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, req.Msg.GetId())
	for _, r := range f.results {
		if r.GetId() == req.Msg.GetId() {
			return connect.NewResponse(&hubv1.GetMatchResultResponse{Result: r}), nil
		}
	}
	return nil, connect.NewError(connect.CodeNotFound, errors.New("no such result"))
}

func (f *fakeHub) set(fn func(*fakeHub)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

// message is one message the fake keeps.
type message struct {
	ID         string          `json:"id"`
	ChannelID  string          `json:"channel_id"`
	Author     map[string]any  `json:"author"`
	Flags      int             `json:"flags"`
	Content    string          `json:"content"`
	Components json.RawMessage `json:"components"`
	Timestamp  string          `json:"timestamp"`
	Type       int             `json:"type"`
}

// fakeDiscord is Discord's REST API for channels: list and post messages.
type fakeDiscord struct {
	srv *httptest.Server

	mu       sync.Mutex
	next     int
	messages map[string][]*message // channel → newest last
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
			Components      json.RawMessage `json:"components"`
			AllowedMentions json.RawMessage `json:"allowed_mentions"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mentions = append(f.mentions, string(body.AllowedMentions))
		writeJSON(w, f.add(r.PathValue("channel"), appID, body.Flags, body.Components))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected Discord call %s %s", r.Method, r.URL)
		http.Error(w, "unexpected", http.StatusNotFound)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// add stores a message; the caller holds the lock.
func (f *fakeDiscord) add(ch, author string, flags int, components json.RawMessage) *message {
	m := &message{ID: strconv.Itoa(1400000000000000000 + f.next), ChannelID: ch, Author: map[string]any{"id": author, "username": "u"},
		Flags: flags, Components: components, Timestamp: "2026-10-15T00:00:00Z"}
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

// text is a message's text displays, joined by newlines.
func text(t *testing.T, m *message) string {
	t.Helper()
	var dm discord.Message
	raw, _ := json.Marshal(m)
	if err := json.Unmarshal(raw, &dm); err != nil {
		t.Fatal(err)
	}
	return texts(dm.Components)
}

// marked is the result a message's marker names.
func marked(t *testing.T, m *message) string {
	t.Helper()
	var dm discord.Message
	raw, _ := json.Marshal(m)
	if err := json.Unmarshal(raw, &dm); err != nil {
		t.Fatal(err)
	}
	got, _ := matchresults.Marker(dm.Components)
	return got
}

func texts(cs []discord.LayoutComponent) string {
	var out []string
	for _, lc := range cs {
		if box, ok := lc.(discord.ContainerComponent); ok {
			for _, sc := range box.Components {
				if t, ok := sc.(discord.TextDisplayComponent); ok {
					out = append(out, t.Content)
				}
			}
		}
	}
	return strings.Join(out, "\n")
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// logs is the runtime's log, safe to read while the module writes.
type logs struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logs) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logs) count(s string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Count(l.buf.String(), s)
}

type rig struct {
	hub     *fakeHub
	discord *fakeDiscord
	mod     *matchresults.Module
	rt      *bot.Runtime
	logs    *logs
	now     time.Time
}

func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{hub: newFakeHub(t), discord: newFakeDiscord(t), now: start}
	r.restart(t)
	return r
}

// restart is a new process: a new module and runtime against the same hub and Discord.
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
	r.logs = &logs{}
	r.mod = matchresults.New(hc, matchresults.Config{Interval: time.Hour, PublicURL: "https://app.example.com/"})
	r.mod.SetClock(func() time.Time { return r.now })
	r.rt, err = bot.New(bot.Options{Config: bc, Logger: slog.New(slog.NewTextHandler(r.logs, nil)), RESTURL: r.discord.srv.URL + "/api"}, r.mod)
	if err != nil {
		t.Fatal(err)
	}
	rt := r.rt
	t.Cleanup(func() { rt.Close(context.Background()) })
}

func (r *rig) pass(t *testing.T) matchresults.Result {
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

func (r *rig) add(results ...*hubv1.MatchResult) {
	r.hub.set(func(f *fakeHub) { f.results = append(f.results, results...) })
}

func TestPostsAFinishedResultOnce(t *testing.T) {
	r := newRig(t)
	if res := r.pass(t); res.Idle || len(res.Posted)+len(res.Found)+len(res.Passed) != 0 || res.Failed {
		t.Fatalf("nothing yet: %+v", res)
	}
	if c := r.discord.takeCalls(); len(c) != 0 {
		t.Errorf("no result, no Discord call: %v", c)
	}
	running := series(2, cs1, start.Add(time.Hour))
	running.EndedAt, running.Winner = nil, ""
	r.add(series(1, cs1, start.Add(10*time.Minute)), running)
	r.now = start.Add(11 * time.Minute)
	if res := r.pass(t); !slices.Equal(res.Posted, []string{id(1)}) {
		t.Fatalf("the finished one is posted, the running one isn't: %+v", res)
	}
	r.hub.mu.Lock()
	asked := r.hub.listed[len(r.hub.listed)-1]
	r.hub.mu.Unlock()
	if asked.GetServerId() != "" || !asked.GetEndedOnly() || !asked.GetEndedAfter().AsTime().Equal(r.now.Add(-matchresults.CatchUp)) {
		t.Errorf("listed with: %v", asked)
	}
	posts := r.discord.posts()
	if len(posts) != 1 {
		t.Fatalf("posts: %d", len(posts))
	}
	p := posts[0]
	if p.Flags&int(discord.MessageFlagIsComponentsV2) == 0 {
		t.Errorf("a Components V2 message: flags %d", p.Flags)
	}
	got := text(t, p)
	for _, want := range []string{
		"## Lonestar 2–1 Valkyra",
		"**Lonestar** wins",
		"de\\_nuke 13–9\nde\\_inferno 9–13\nde\\_mirage 16–14",
		"**Top by kills**\n1. Jo: **35** kills\n2. Brave Otter: **30** kills\n3. Sam: **29** kills",
		"[Full result](https://app.example.com/results/" + id(1) + ")",
		"-# result `" + id(1) + "`",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("post should contain %q:\n%s", want, got)
		}
	}
	r.discord.mu.Lock()
	for _, am := range r.discord.mentions {
		if am != `{"parse":[],"roles":[],"users":[],"replied_user":false}` {
			t.Errorf("a post allows no mention: %s", am)
		}
	}
	r.discord.mu.Unlock()
	if m := r.metrics(t); !strings.Contains(m, `gravel_bot_match_results_total{result="posted"} 1`) ||
		!strings.Contains(m, `gravel_bot_match_results_total{result="error"} 0`) || !strings.Contains(m, "gravel_bot_match_results_last_success_timestamp_seconds ") {
		t.Errorf("metrics:\n%s", m)
	}

	// The next pass: nothing at all from Discord, and the result isn't read again.
	r.discord.takeCalls()
	r.hub.set(func(f *fakeHub) { f.got = nil })
	r.now = start.Add(12 * time.Minute)
	if res := r.pass(t); len(res.Posted)+len(res.Found) != 0 {
		t.Errorf("again: %+v", res)
	}
	if c := r.discord.takeCalls(); len(c) != 0 {
		t.Errorf("a posted result costs no Discord call: %v", c)
	}
	if len(r.hub.got) != 0 {
		t.Errorf("nor a hub read: %v", r.hub.got)
	}
}

func TestNoRepostAfterARestart(t *testing.T) {
	r := newRig(t)
	r.pass(t)
	r.add(series(1, cs1, start.Add(10*time.Minute)))
	r.now = start.Add(11 * time.Minute)
	if res := r.pass(t); len(res.Posted) != 1 {
		t.Fatalf("posted: %+v", res)
	}
	// While the bot was away: one more series ended, and members talked in the channel; one copied
	// the marker, which is never taken for the bot's.
	r.add(series(2, cs1, start.Add(30*time.Minute)), series(3, cs1, start.Add(40*time.Minute)))
	r.discord.mu.Lock()
	r.discord.add(channel, someone, 1<<15, r.discord.messages[channel][0].Components)
	r.discord.mu.Unlock()
	r.now = start.Add(2 * time.Hour)
	r.restart(t)
	res := r.pass(t)
	if !slices.Equal(res.Found, []string{id(1)}) || !slices.Equal(res.Posted, []string{id(2), id(3)}) {
		t.Fatalf("after a restart: %+v", res)
	}
	if n := len(r.discord.posts()); n != 4 {
		t.Errorf("one post each, the member's copy aside: %d messages", n)
	}
	// A series older than the newest one the channel holds was handled before; it is never posted.
	r.restart(t)
	r.add(series(4, cs1, start.Add(20*time.Minute)))
	r.now = start.Add(3 * time.Hour)
	if res := r.pass(t); len(res.Posted) != 0 || !slices.Contains(res.Passed, id(4)) {
		t.Errorf("an older one: %+v", res)
	}
}

func TestNoHistoryOnFirstLook(t *testing.T) {
	r := newRig(t)
	r.add(series(1, cs1, start.Add(-3*time.Hour)), series(2, cs2, start.Add(-time.Minute)))
	if res := r.pass(t); len(res.Posted) != 0 || len(res.Passed) != 2 {
		t.Fatalf("what ended before the module started is history: %+v", res)
	}
	if n := len(r.discord.posts()); n != 0 {
		t.Errorf("no history posted: %d", n)
	}
	r.discord.takeCalls()
	r.now = start.Add(time.Minute)
	r.pass(t)
	if c := r.discord.takeCalls(); len(c) != 0 {
		t.Errorf("history passed over costs no further call: %v", c)
	}
	r.add(series(3, cs1, start.Add(5*time.Minute)))
	r.now = start.Add(6 * time.Minute)
	if res := r.pass(t); !slices.Equal(res.Posted, []string{id(3)}) {
		t.Errorf("what ends after it is posted: %+v", res)
	}
}

func TestServersFilter(t *testing.T) {
	r := newRig(t)
	r.hub.set(func(f *fakeHub) { f.settings.Servers = []string{cs1} })
	r.pass(t)
	r.add(series(1, cs2, start.Add(5*time.Minute)), series(2, cs1, start.Add(6*time.Minute)))
	r.now = start.Add(10 * time.Minute)
	if res := r.pass(t); !slices.Equal(res.Posted, []string{id(2)}) {
		t.Errorf("only the listed server's: %+v", res)
	}
	r.hub.mu.Lock()
	for _, l := range r.hub.listed {
		if l.GetServerId() != cs1 {
			t.Errorf("listed for the server: %v", l)
		}
	}
	r.hub.mu.Unlock()
	// Every server again: the other server's result is posted now.
	r.hub.set(func(f *fakeHub) { f.settings.Servers = nil })
	r.now = start.Add(11 * time.Minute)
	if res := r.pass(t); !slices.Equal(res.Posted, []string{id(1)}) {
		t.Errorf("every server: %+v", res)
	}
}

func TestFiveAPassOldestFirst(t *testing.T) {
	r := newRig(t)
	r.pass(t)
	for i := range 7 {
		r.add(series(i+1, cs1, start.Add(time.Duration(10-i)*time.Minute))) // 1 ended last
	}
	r.now = start.Add(20 * time.Minute)
	if res := r.pass(t); !slices.Equal(res.Posted, []string{id(7), id(6), id(5), id(4), id(3)}) {
		t.Fatalf("five, oldest first: %+v", res)
	}
	if res := r.pass(t); !slices.Equal(res.Posted, []string{id(2), id(1)}) {
		t.Errorf("the rest next pass: %+v", res)
	}
	var order []string
	for _, p := range r.discord.posts() {
		order = append(order, marked(t, p))
	}
	if !slices.Equal(order, []string{id(7), id(6), id(5), id(4), id(3), id(2), id(1)}) {
		t.Errorf("the channel in order: %v", order)
	}
}

func TestForbiddenChannel(t *testing.T) {
	r := newRig(t)
	r.hub.set(func(f *fakeHub) { f.settings.Channel = locked })
	r.pass(t)
	r.add(series(1, cs1, start.Add(5*time.Minute)))
	r.now = start.Add(6 * time.Minute)
	for range 3 {
		if res := r.pass(t); !res.Failed || len(res.Posted) != 0 {
			t.Fatalf("a forbidden channel: %+v", res)
		}
	}
	if n := r.logs.count("the bot may not use the channel"); n != 1 {
		t.Errorf("logged once, not every pass: %d", n)
	}
	if m := r.metrics(t); !strings.Contains(m, `gravel_bot_match_results_total{result="error"} 3`) {
		t.Errorf("metrics:\n%s", m)
	}
	// Allowed again: the result goes out.
	r.hub.set(func(f *fakeHub) { f.settings.Channel = channel })
	if res := r.pass(t); !slices.Equal(res.Posted, []string{id(1)}) {
		t.Errorf("allowed again: %+v", res)
	}
}

func TestHubFailures(t *testing.T) {
	r := newRig(t)
	r.hub.set(func(f *fakeHub) {
		f.listErr = connect.NewError(connect.CodePermissionDenied, errors.New("stats are private"))
	})
	for range 2 {
		if res := r.pass(t); !res.Failed {
			t.Fatalf("denied: %+v", res)
		}
	}
	if n := r.logs.count("give its app stats:read or make stats public"); n != 1 {
		t.Errorf("the missing scope is named once: %d", n)
	}
	r.hub.set(func(f *fakeHub) { f.listErr = nil })
	r.add(series(1, cs1, start.Add(5*time.Minute)))
	r.now = start.Add(6 * time.Minute)
	if res := r.pass(t); len(res.Posted) != 1 || res.Failed {
		t.Errorf("recovered: %+v", res)
	}
	r.hub.set(func(f *fakeHub) { f.down = true })
	if _, err := r.mod.Pass(context.Background()); err == nil {
		t.Error("the settings unreadable is an error for the pass")
	}
}

func TestNoChannelIsIdle(t *testing.T) {
	r := newRig(t)
	r.hub.set(func(f *fakeHub) { f.settings = nil })
	r.add(series(1, cs1, start.Add(-time.Minute)))
	if res := r.pass(t); !res.Idle {
		t.Errorf("no channel: %+v", res)
	}
	r.hub.set(func(f *fakeHub) { f.settings = &hubv1.DiscordMatchResults{Servers: []string{cs1}} })
	if res := r.pass(t); !res.Idle {
		t.Errorf("servers without a channel: %+v", res)
	}
	if c := r.discord.takeCalls(); len(c) != 0 {
		t.Errorf("idle makes no Discord call: %v", c)
	}
	if len(r.hub.listed) != 0 {
		t.Errorf("nor a hub read: %v", r.hub.listed)
	}
	if n := r.logs.count("match results idle"); n != 1 {
		t.Errorf("idle is said once: %d", n)
	}
}

func TestRender(t *testing.T) {
	r := series(1, cs1, start)
	r.Team1Name, r.Team2Name = "@everyone *Bold*", "[x](https://evil.example) <@123>"
	r.Players[0].Name = "__Jo__"
	r.Players[1].Name = "__Jo__"
	got := texts(matchresults.Render(r, ""))
	for _, want := range []string{
		"## @\u200beveryone \\*Bold\\* 2–1 \\[x\\](https://evil.example) \\<@\u200b123\\>",
		"**@\u200beveryone \\*Bold\\*** wins",
		"1. \\_\\_Jo\\_\\_: **35** kills",
		"-# result `" + id(1) + "`",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("render should contain %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Full result") {
		t.Errorf("no public URL, no link:\n%s", got)
	}

	draw := &hubv1.MatchResult{Id: id(2), Team1Score: 1, Team2Score: 1, Winner: "none", Maps: []*hubv1.MatchResultMap{{MapName: "", Team1Score: 15, Team2Score: 15}},
		Players: []*hubv1.MatchResultPlayer{{Name: "B", Kills: 1, Deaths: 3}, {Name: "A", Kills: 1, Deaths: 3}, {Name: "C", Kills: 1, Deaths: 1}, {Name: "D"}}}
	got = texts(matchresults.Render(draw, "https://app.example.com/results/"+id(2)))
	for _, want := range []string{"## Team 1 1–1 Team 2", "A draw", "Unknown map 15–15", "1. C: **1** kill\n2. A: **1** kill\n3. B: **1** kill", "[Full result](https://app.example.com/results/" + id(2) + ")"} {
		if !strings.Contains(got, want) {
			t.Errorf("draw should contain %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "D:") {
		t.Errorf("three players only:\n%s", got)
	}
	if mid, ok := matchresults.Marker(matchresults.Render(draw, "")); !ok || mid != id(2) {
		t.Errorf("marker: %q %t", mid, ok)
	}
	if _, ok := matchresults.Marker([]discord.LayoutComponent{discord.NewTextDisplay("-# result `not-an-id`")}); ok {
		t.Error("a marker names a result id")
	}
	if top := matchresults.Top(nil, 3); len(top) != 0 {
		t.Errorf("nobody: %v", top)
	}
}
