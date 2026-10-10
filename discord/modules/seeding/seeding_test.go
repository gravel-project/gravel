package seeding_test

import (
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

	"github.com/gravel-project/gravel/discord/bot"
	"github.com/gravel-project/gravel/discord/hubclient"
	"github.com/gravel-project/gravel/discord/modules/seeding"
	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
	"github.com/gravel-project/gravel/gen/gravel/hub/v1/hubv1connect"
)

const (
	appID    = "1558123590786748516"
	guildID  = "1558121195340042350"
	channel  = "1300000000000000100"
	locked   = "1300000000000000101" // a channel the bot may not use
	someone  = "1300000000000000999" // a member, not the bot
	role     = "1300000000000000004" // the seeder role
	serverID = "htg-wardogs-1"
	note     = "Join from the server browser: HTG WARDOGS."
	interval = 30 * time.Second
)

func chicago(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

// htgRule is HTG's War Dogs rule: Wednesday, Saturday and Sunday evenings in Chicago.
func htgRule() *hubv1.DiscordSeeding {
	return &hubv1.DiscordSeeding{Server: serverID, Channel: channel, Role: role, Below: 20,
		Hours: []*hubv1.DiscordSeedingHours{{Days: []string{"wed", "sat", "sun"}, From: "18:00", To: "23:00"}}, Cooldown: "2h", Note: note}
}

func steamID(i int) string { return fmt.Sprintf("765611980000000%02d", i) }

// message is one message the fake keeps.
type message struct {
	ID        string         `json:"id"`
	ChannelID string         `json:"channel_id"`
	Author    map[string]any `json:"author"`
	Content   string         `json:"content"`
	Timestamp string         `json:"timestamp"`
	Type      int            `json:"type"`
}

// fakeDiscord is Discord's REST API for channels: list, post and edit messages.
type fakeDiscord struct {
	srv *httptest.Server

	mu         sync.Mutex
	next       int
	messages   map[string][]*message // channel → newest last
	calls      []string              // "GET", "POST", "PATCH"
	mentions   []string              // the allowed_mentions of every post and edit, in order
	failAll    bool
	forbidPost bool // the bot may read the channel but not post in it
	failEdit   bool // edits fail: the bot stopped between recording and editing
}

func (f *fakeDiscord) set(fn func(*fakeDiscord)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func newFakeDiscord(t *testing.T) *fakeDiscord {
	t.Helper()
	f := &fakeDiscord{next: 1, messages: map[string][]*message{}}
	mux := http.NewServeMux()
	guard := func(w http.ResponseWriter, r *http.Request) bool {
		f.calls = append(f.calls, r.Method)
		switch {
		case f.failAll:
			discordError(w, http.StatusInternalServerError, 0, "boom")
			return false
		case r.PathValue("channel") == locked:
			discordError(w, http.StatusForbidden, 50001, "Missing Access")
			return false
		}
		return true
	}
	var body struct {
		Content         string          `json:"content"`
		AllowedMentions json.RawMessage `json:"allowed_mentions"`
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
		if f.forbidPost {
			discordError(w, http.StatusForbidden, 50013, "Missing Permissions")
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mentions = append(f.mentions, string(body.AllowedMentions))
		writeJSON(w, f.add(r.PathValue("channel"), appID, body.Content))
	})
	mux.HandleFunc("PATCH /api/channels/{channel}/messages/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if !guard(w, r) {
			return
		}
		if f.failEdit {
			discordError(w, http.StatusInternalServerError, 0, "boom")
			return
		}
		for _, m := range f.messages[r.PathValue("channel")] {
			if m.ID == r.PathValue("id") {
				_ = json.NewDecoder(r.Body).Decode(&body)
				f.mentions = append(f.mentions, string(body.AllowedMentions))
				m.Content = body.Content
				writeJSON(w, m)
				return
			}
		}
		discordError(w, http.StatusNotFound, 10008, "Unknown Message")
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
func (f *fakeDiscord) add(ch, author, content string) *message {
	m := &message{ID: strconv.Itoa(1400000000000000000 + f.next), ChannelID: ch, Author: map[string]any{"id": author, "username": "u"},
		Content: content, Timestamp: "2026-10-14T23:00:00Z"}
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

// posts are the rules' channel's message contents, oldest first.
func (f *fakeDiscord) posts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, m := range f.messages[channel] {
		out = append(out, m.Content)
	}
	return out
}

func discordError(w http.ResponseWriter, status, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"code": code, "message": msg})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// fakeHub is the hub as the bot's app sees it: the settings, the servers, who is on, and
// RecordParticipation, which keeps each key once as the hub does.
type fakeHub struct {
	hubv1connect.UnimplementedOrganizationServiceHandler
	hubv1connect.UnimplementedServerServiceHandler
	hubv1connect.UnimplementedStatsServiceHandler
	srv *httptest.Server

	mu          sync.Mutex
	rules       []*hubv1.DiscordSeeding
	timezone    string
	servers     []*hubv1.Server
	players     map[string][]*hubv1.ServerPlayer
	recorded    map[string]*hubv1.ParticipationEvent
	recordCalls int
	statsDown   bool
	playersDown bool
	refuse      func(*hubv1.ParticipationEvent) bool // an event the hub calls invalid
}

func newFakeHub(t *testing.T) *fakeHub {
	t.Helper()
	f := &fakeHub{rules: []*hubv1.DiscordSeeding{htgRule()}, timezone: "America/Chicago", servers: []*hubv1.Server{server(serverID, 0)},
		players: map[string][]*hubv1.ServerPlayer{}, recorded: map[string]*hubv1.ParticipationEvent{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /oauth/token", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"tok","token_type":"Bearer","expires_in":3600}`))
	})
	mux.Handle(hubv1connect.NewOrganizationServiceHandler(f))
	mux.Handle(hubv1connect.NewServerServiceHandler(f))
	mux.Handle(hubv1connect.NewStatsServiceHandler(f))
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func server(id string, players int32) *hubv1.Server {
	return &hubv1.Server{Id: id, Name: "HTG War Dogs", GameId: "wardogs", Status: &hubv1.ServerStatus{State: "ok", Reachable: true, Players: players, MaxPlayers: 64}}
}

func (f *fakeHub) GetOrganizationSettings(context.Context, *connect.Request[hubv1.GetOrganizationSettingsRequest]) (*connect.Response[hubv1.GetOrganizationSettingsResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return connect.NewResponse(&hubv1.GetOrganizationSettingsResponse{Settings: &hubv1.OrganizationSettings{Version: 1,
		Discord: &hubv1.DiscordSettings{GuildId: guildID, Seeding: f.rules}, Stats: &hubv1.StatsSettings{Timezone: f.timezone}}}), nil
}

func (f *fakeHub) ListServers(context.Context, *connect.Request[hubv1.ListServersRequest]) (*connect.Response[hubv1.ListServersResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return connect.NewResponse(&hubv1.ListServersResponse{Servers: f.servers}), nil
}

func (f *fakeHub) ListServerPlayers(_ context.Context, req *connect.Request[hubv1.ListServerPlayersRequest]) (*connect.Response[hubv1.ListServerPlayersResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.playersDown {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("servers:read"))
	}
	return connect.NewResponse(&hubv1.ListServerPlayersResponse{Players: f.players[req.Msg.GetServerId()]}), nil
}

func (f *fakeHub) RecordParticipation(_ context.Context, req *connect.Request[hubv1.RecordParticipationRequest]) (*connect.Response[hubv1.RecordParticipationResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recordCalls++
	if f.statsDown {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("down"))
	}
	if n := len(req.Msg.GetEvents()); n < 1 || n > 100 {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("%d events", n))
	}
	for i, e := range req.Msg.GetEvents() {
		if f.refuse != nil && f.refuse(e) {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("events[%d]: refused", i))
		}
	}
	out := &hubv1.RecordParticipationResponse{}
	for _, e := range req.Msg.GetEvents() {
		outcome := "duplicate"
		if _, dup := f.recorded[e.GetKey()]; !dup {
			f.recorded[e.GetKey()], outcome = e, "recorded"
		}
		out.Results = append(out.Results, &hubv1.ParticipationResult{Key: e.GetKey(), Outcome: outcome})
	}
	return connect.NewResponse(out), nil
}

func (f *fakeHub) set(fn func(*fakeHub)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

// on puts these players (by number) on the server, its count with them.
func (f *fakeHub) on(ids ...int) {
	f.set(func(f *fakeHub) {
		var ps []*hubv1.ServerPlayer
		for _, i := range ids {
			ps = append(ps, &hubv1.ServerPlayer{Name: fmt.Sprintf("p%d", i), Provider: "steam", Subject: steamID(i)})
		}
		f.players[serverID] = ps
		f.servers[0].Status.Players = int32(len(ids)) //nolint:gosec // a test's handful
	})
}

// full sets the server's count to the floor alone, whoever the test tracks on it.
func (f *fakeHub) full() { f.set(func(f *fakeHub) { f.servers[0].Status.Players = 20 }) }

func (f *fakeHub) events() map[string]*hubv1.ParticipationEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]*hubv1.ParticipationEvent{}
	for k, v := range f.recorded {
		out[k] = v
	}
	return out
}

type rig struct {
	discord *fakeDiscord
	hub     *fakeHub
	mod     *seeding.Module
	rt      *bot.Runtime

	mu  sync.Mutex
	now time.Time
}

func newRig(t *testing.T, start time.Time) *rig {
	t.Helper()
	r := &rig{discord: newFakeDiscord(t), hub: newFakeHub(t), now: start}
	r.restart(t)
	return r
}

// restart builds a fresh module and runtime against the same fakes: a bot restart.
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
	r.mod = seeding.New(hc, seeding.Config{Interval: interval})
	r.mod.SetClock(func() time.Time {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.now
	})
	r.rt, err = bot.New(bot.Options{Config: bc, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), RESTURL: r.discord.srv.URL + "/api"}, r.mod)
	if err != nil {
		t.Fatal(err)
	}
	rt := r.rt
	t.Cleanup(func() { rt.Close(context.Background()) })
}

// at moves the clock to t and runs a pass.
func (r *rig) at(t *testing.T, at time.Time) seeding.Result {
	t.Helper()
	r.mu.Lock()
	r.now = at
	r.mu.Unlock()
	res, err := r.mod.Pass(context.Background())
	if err != nil {
		t.Fatalf("pass at %s: %v", at, err)
	}
	return res
}

// passes runs a pass every interval from (excluded) the clock to until (included).
func (r *rig) passes(t *testing.T, until time.Time) {
	t.Helper()
	r.mu.Lock()
	next := r.now.Add(interval)
	r.mu.Unlock()
	for ; !next.After(until); next = next.Add(interval) {
		r.at(t, next)
	}
}

func (r *rig) metrics(t *testing.T) string {
	t.Helper()
	rec := httptest.NewRecorder()
	r.rt.InternalHandler().ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/metrics", nil))
	return rec.Body.String()
}

func (r *rig) wantMetrics(t *testing.T, want ...string) {
	t.Helper()
	m := r.metrics(t)
	for _, w := range want {
		if !strings.Contains(m, w+"\n") {
			t.Errorf("metrics should contain %s", w)
		}
	}
}

// wednesday is 2026-10-14, a Wednesday, at hh:mm in Chicago.
func wednesday(t *testing.T, hh, mm int) time.Time {
	return time.Date(2026, 10, 14, hh, mm, 0, 0, chicago(t))
}

func TestCallsOnlyInsideAWindow(t *testing.T) {
	r := newRig(t, wednesday(t, 17, 59))
	if res := r.at(t, wednesday(t, 17, 59)); res.Called != 0 || res.Rules != 1 {
		t.Fatalf("a minute before the window: %+v", res)
	}
	if posts := r.discord.posts(); len(posts) != 0 {
		t.Fatalf("nothing is posted outside a window: %v", posts)
	}
	if calls := r.discord.takeCalls(); !slices.Equal(calls, []string{"GET"}) {
		t.Errorf("the first pass reads the channel for its last call, and posts nothing: %v", calls)
	}
	// Thursday evening and Wednesday after 23:00 are outside too.
	r.at(t, time.Date(2026, 10, 15, 19, 0, 0, 0, chicago(t)))
	r.at(t, time.Date(2026, 10, 14, 23, 0, 0, 0, chicago(t)).Add(24*time.Hour*7))
	if posts := r.discord.posts(); len(posts) != 0 {
		t.Fatalf("still nothing: %v", posts)
	}
}

func TestCallPingsOnlyTheRole(t *testing.T) {
	r := newRig(t, wednesday(t, 18, 0))
	r.hub.on(1, 2, 3, 4, 5, 6, 7)
	if res := r.at(t, wednesday(t, 18, 0)); res.Called != 1 {
		t.Fatalf("under the floor at the window's start: %+v", res)
	}
	posts := r.discord.posts()
	start := wednesday(t, 18, 0).Unix()
	want := "<@&" + role + "> **HTG War Dogs** is at 7/20: join to get the match going.\n" + note + "\n-# seeding htg-wardogs-1 · " + strconv.FormatInt(start, 10) + " · open"
	if len(posts) != 1 || posts[0] != want {
		t.Fatalf("the call:\n%q\nwant\n%q", posts, want)
	}
	if got := r.discord.mentions; len(got) != 1 || got[0] != `{"parse":[],"roles":["`+role+`"],"users":[],"replied_user":false}` {
		t.Errorf("a call pings the seeder role and nothing else: %v", got)
	}
	// While the session is open, no second call, whatever the count does.
	r.hub.on(1)
	r.passes(t, wednesday(t, 20, 30))
	if posts := r.discord.posts(); len(posts) != 1 {
		t.Errorf("one call per session: %v", posts)
	}
	r.wantMetrics(t, `gravel_bot_seeding_calls_total{result="posted",server="htg-wardogs-1"} 1`, `gravel_bot_seeding_sessions_total{outcome="started",server="htg-wardogs-1"} 1`)
}

func TestNoCallWhenNotLiveOrAtTheFloor(t *testing.T) {
	r := newRig(t, wednesday(t, 18, 0))
	r.hub.full()
	if res := r.at(t, wednesday(t, 18, 0)); res.Called != 0 {
		t.Errorf("at the floor: %+v", res)
	}
	r.hub.set(func(f *fakeHub) {
		f.servers[0].Status.Players, f.servers[0].Status.State, f.servers[0].Status.Reachable = 0, "unreachable", false
	})
	if res := r.at(t, wednesday(t, 18, 1)); res.Called != 0 {
		t.Errorf("unreachable: %+v", res)
	}
	r.hub.set(func(f *fakeHub) { f.servers[0].Status.State, f.servers[0].Status.Reachable = "ok", true })
	if res := r.at(t, wednesday(t, 18, 2)); res.Called != 1 {
		t.Errorf("live and empty: %+v", res)
	}
}

func TestCooldownBetweenCalls(t *testing.T) {
	r := newRig(t, wednesday(t, 18, 0))
	r.hub.on(1)
	r.at(t, wednesday(t, 18, 0)) // call 1
	r.hub.full()
	if res := r.at(t, wednesday(t, 18, 10)); res.Reached != 1 {
		t.Fatalf("reached: %+v", res)
	}
	r.hub.on(1, 2) // the match ended, the server emptied
	for _, at := range []time.Time{wednesday(t, 18, 11), wednesday(t, 19, 0), wednesday(t, 19, 59)} {
		if res := r.at(t, at); res.Called != 0 {
			t.Errorf("inside the cooldown at %s: %+v", at.Format(time.Kitchen), res)
		}
	}
	if res := r.at(t, wednesday(t, 20, 0)); res.Called != 1 {
		t.Errorf("the cooldown ran out: %+v", res)
	}
	if posts := r.discord.posts(); len(posts) != 2 || !strings.Contains(posts[1], "is at 2/20") {
		t.Errorf("two calls, two hours apart: %v", posts)
	}
}

func TestCallAtStart(t *testing.T) {
	// Two windows half an hour apart: a call late in the first leaves a cooldown into the second.
	run := func(t *testing.T, atStart bool) []string {
		r := newRig(t, wednesday(t, 19, 50))
		r.hub.set(func(f *fakeHub) {
			f.rules[0].Hours = []*hubv1.DiscordSeedingHours{{Days: []string{"wed"}, From: "18:00", To: "20:00"}, {Days: []string{"wed"}, From: "20:30", To: "23:00"}}
			f.rules[0].CallAtStart = atStart
		})
		r.hub.on(1)
		r.at(t, wednesday(t, 19, 50))
		if res := r.at(t, wednesday(t, 20, 0)); res.Closed != 1 {
			t.Fatalf("the first window closes the session: %+v", res)
		}
		r.at(t, wednesday(t, 20, 29))
		r.at(t, wednesday(t, 20, 30))
		r.hub.full()
		r.at(t, wednesday(t, 20, 40))
		r.hub.on(1)
		r.at(t, wednesday(t, 20, 45)) // inside the cooldown from either call
		return r.discord.posts()
	}
	if posts := run(t, false); len(posts) != 1 {
		t.Errorf("without call_at_start the second window waits out the cooldown: %d posts", len(posts))
	}
	posts := run(t, true)
	if len(posts) != 2 || !strings.Contains(posts[1], "started: 20/20") || !strings.HasSuffix(posts[1], "· "+strconv.FormatInt(wednesday(t, 20, 30).Unix(), 10)+" · reached") {
		t.Errorf("with call_at_start, one call at the second window's start and none after it:\n%s", strings.Join(posts, "\n---\n"))
	}
}

func TestSessionCountsSeedersAndRecordsThemOnce(t *testing.T) {
	r := newRig(t, wednesday(t, 18, 0))
	start := wednesday(t, 18, 0)
	r.hub.on(1, 2)
	r.at(t, start) // the call: nobody is credited for the time before it
	r.passes(t, start.Add(5*time.Minute))
	r.hub.on(1, 2, 3)                                    // 3 joins
	r.passes(t, start.Add(5*time.Minute+30*time.Second)) // 3 is on for one pass: 30 s, under a minute
	r.hub.on(1, 2)                                       // 3 leaves
	r.passes(t, start.Add(10*time.Minute))
	r.hub.on(1, 4) // 2 leaves, 4 joins
	r.passes(t, start.Add(12*time.Minute))
	r.hub.on(1, 2, 4) // 2 rejoins
	r.passes(t, start.Add(14*time.Minute))
	// The bot stalls for ten minutes: the gap counts as twice the interval, no more.
	r.at(t, start.Add(24*time.Minute))
	r.hub.on(1, 2, 4)
	r.hub.full() // full
	if res := r.at(t, start.Add(24*time.Minute+30*time.Second)); res.Reached != 1 || res.Recorded != 3 {
		t.Fatalf("reaching the floor ends the session and records it: %+v", res)
	}
	ref := "seeding:htg-wardogs-1:" + strconv.FormatInt(start.Unix(), 10)
	want := map[int]int32{
		1: 14*60 + 60 + 30, // 14 minutes, the capped gap, the last pass
		2: 10*60 + 2*60 + 60 + 30,
		4: 4*60 + 60 + 30,
	}
	got := r.hub.events()
	if len(got) != len(want) {
		t.Fatalf("one event per seeder of a minute or more (not 3): %v", got)
	}
	for i, secs := range want {
		e := got[ref+":steam:"+steamID(i)]
		if e == nil {
			t.Errorf("player %d: no event under key %s:steam:%s", i, ref, steamID(i))
			continue
		}
		if e.GetKind() != "seeding" || e.GetDurationSeconds() != secs || e.GetServerId() != serverID || e.GetSession() != ref ||
			!e.GetOccurredAt().AsTime().Equal(start) || e.GetIdentity().GetProvider() != "steam" || e.GetIdentity().GetSubject() != steamID(i) {
			t.Errorf("player %d: %v (want %d s)", i, e, secs)
		}
	}
	posts := r.discord.posts()
	reached := start.Add(24*time.Minute + 30*time.Second).Unix()
	if len(posts) != 1 || posts[0] != fmt.Sprintf("**HTG War Dogs** started: 20/20 at <t:%d:t>. Thanks for seeding!\n-# seeding htg-wardogs-1 · %d · reached", reached, start.Unix()) {
		t.Errorf("the same message says it started: %q", posts)
	}
	if last := r.discord.mentions[len(r.discord.mentions)-1]; last != `{"parse":[],"roles":[],"users":[],"replied_user":false}` {
		t.Errorf("an edit pings no one: %s", last)
	}
	r.wantMetrics(t, `gravel_bot_seeding_sessions_total{outcome="reached",server="htg-wardogs-1"} 1`,
		fmt.Sprintf(`gravel_bot_seeding_recorded_seconds_total{server="htg-wardogs-1"} %d`, want[1]+want[2]+want[4]))
	// The next passes record nothing more.
	r.passes(t, start.Add(26*time.Minute))
	if n := len(r.hub.events()); n != 3 {
		t.Errorf("recorded once: %d", n)
	}
}

func TestWindowEndClosesTheCall(t *testing.T) {
	r := newRig(t, wednesday(t, 22, 50))
	r.hub.on(1, 2)
	r.at(t, wednesday(t, 22, 50))
	r.passes(t, wednesday(t, 22, 59)) // 9 minutes
	// The next pass is late, past 23:00: only the time up to the window's end counts.
	if res := r.at(t, wednesday(t, 23, 0).Add(20*time.Second)); res.Closed != 1 || res.Recorded != 2 {
		t.Fatalf("the window closes the session: %+v", res)
	}
	for k, e := range r.hub.events() {
		if e.GetDurationSeconds() != 10*60 {
			t.Errorf("%s: %d s, want 600", k, e.GetDurationSeconds())
		}
	}
	posts := r.discord.posts()
	if len(posts) != 1 || posts[0] != fmt.Sprintf("**HTG War Dogs**: the seeding window closed at 2/20.\n-# seeding htg-wardogs-1 · %d · closed", wednesday(t, 22, 50).Unix()) {
		t.Errorf("the call says the window closed: %q", posts)
	}
	if res := r.at(t, wednesday(t, 23, 5)); res.Called != 0 {
		t.Errorf("no call after the window: %+v", res)
	}
	r.wantMetrics(t, `gravel_bot_seeding_sessions_total{outcome="window_closed",server="htg-wardogs-1"} 1`)
}

func TestRecordingIsRetriedWithoutDuplicates(t *testing.T) {
	r := newRig(t, wednesday(t, 18, 0))
	r.hub.on(1, 2)
	r.at(t, wednesday(t, 18, 0))
	r.passes(t, wednesday(t, 18, 5))
	r.hub.set(func(f *fakeHub) { f.statsDown = true })
	r.hub.full()
	if res := r.at(t, wednesday(t, 18, 6)); res.Reached != 1 || res.RecordFailed != 1 || res.Recorded != 0 {
		t.Fatalf("the hub is away at the end: %+v", res)
	}
	if res := r.at(t, wednesday(t, 18, 7)); res.RecordFailed != 1 {
		t.Errorf("still away: %+v", res)
	}
	r.hub.set(func(f *fakeHub) { f.statsDown = false })
	if res := r.at(t, wednesday(t, 18, 8)); res.Recorded != 2 || res.Duplicates != 0 {
		t.Errorf("the hub is back: %+v", res)
	}
	if res := r.at(t, wednesday(t, 18, 9)); res.Recorded+res.Duplicates != 0 {
		t.Errorf("nothing left to send: %+v", res)
	}
	r.wantMetrics(t, `gravel_bot_seeding_record_errors_total{server="htg-wardogs-1"} 2`)
	if posts := r.discord.posts(); len(posts) != 1 || !strings.Contains(posts[0], "started: 20/20") {
		t.Errorf("the call is edited whatever the hub does: %v", posts)
	}
}

func TestARestartBetweenRecordingAndEditingCountsOnce(t *testing.T) {
	r := newRig(t, wednesday(t, 18, 0))
	start := wednesday(t, 18, 0)
	r.hub.on(1, 2)
	r.at(t, start)
	r.passes(t, start.Add(5*time.Minute))
	r.hub.full()
	r.discord.set(func(f *fakeDiscord) { f.failEdit = true })
	if res := r.at(t, start.Add(5*time.Minute+30*time.Second)); res.Recorded != 2 || res.Failed != 1 {
		t.Fatalf("recorded, then the edit failed: %+v", res)
	}
	first := r.hub.events()
	// The bot stops before the edit lands: the call still reads open.
	r.discord.set(func(f *fakeDiscord) { f.failEdit = false })
	r.restart(t)
	r.hub.on(1, 2)
	if res := r.at(t, start.Add(6*time.Minute)); res.Resumed != 1 {
		t.Fatalf("the open call resumes: %+v", res)
	}
	r.passes(t, start.Add(8*time.Minute))
	r.hub.full()
	if res := r.at(t, start.Add(8*time.Minute+30*time.Second)); res.Reached != 1 || res.Recorded != 0 || res.Duplicates != 2 {
		t.Errorf("the resumed session records under the same keys, which the hub keeps once: %+v", res)
	}
	if got := r.hub.events(); len(got) != 2 || got["seeding:htg-wardogs-1:"+strconv.FormatInt(start.Unix(), 10)+":steam:"+steamID(1)] != first["seeding:htg-wardogs-1:"+strconv.FormatInt(start.Unix(), 10)+":steam:"+steamID(1)] {
		t.Errorf("each seeder once, as first recorded: %v", got)
	}
	if posts := r.discord.posts(); len(posts) != 1 || !strings.Contains(posts[0], "started: 20/20") {
		t.Errorf("one call, edited: %q", posts)
	}
}

func TestAnInvalidEventIsDroppedAlone(t *testing.T) {
	r := newRig(t, wednesday(t, 18, 0))
	r.hub.on(1, 2, 3)
	r.hub.set(func(f *fakeHub) {
		f.refuse = func(e *hubv1.ParticipationEvent) bool { return e.GetIdentity().GetSubject() == steamID(2) }
	})
	r.at(t, wednesday(t, 18, 0))
	r.passes(t, wednesday(t, 18, 5))
	r.hub.full()
	if res := r.at(t, wednesday(t, 18, 6)); res.Recorded != 2 || res.Dropped != 1 || res.RecordFailed != 0 {
		t.Errorf("one refused event costs only itself: %+v", res)
	}
	if res := r.at(t, wednesday(t, 18, 7)); res.Recorded+res.Dropped != 0 {
		t.Errorf("and is not retried: %+v", res)
	}
}

func TestRestartResumesAnOpenCall(t *testing.T) {
	r := newRig(t, wednesday(t, 18, 0))
	start := wednesday(t, 18, 0)
	r.hub.on(1, 2)
	r.at(t, start)
	r.passes(t, start.Add(5*time.Minute)) // five minutes counted, then lost to the restart
	// Other authors' markers are not the bot's calls, even newer and open.
	r.discord.mu.Lock()
	r.discord.add(channel, someone, "-# seeding htg-wardogs-1 · "+strconv.FormatInt(start.Add(time.Hour).Unix(), 10)+" · open")
	r.discord.mu.Unlock()
	r.restart(t)
	r.discord.takeCalls()
	if res := r.at(t, start.Add(10*time.Minute)); res.Resumed != 1 || res.Called != 0 {
		t.Fatalf("the open call resumes: %+v", res)
	}
	if calls := r.discord.takeCalls(); !slices.Equal(calls, []string{"GET"}) {
		t.Errorf("it reads the channel and posts nothing: %v", calls)
	}
	r.passes(t, start.Add(13*time.Minute))
	r.hub.full()
	if res := r.at(t, start.Add(13*time.Minute+30*time.Second)); res.Reached != 1 || res.Recorded != 2 {
		t.Fatalf("the resumed session ends at the floor: %+v", res)
	}
	for k, e := range r.hub.events() { // counted from the restart: 3.5 minutes
		if !strings.HasPrefix(k, "seeding:htg-wardogs-1:"+strconv.FormatInt(start.Unix(), 10)+":") || e.GetDurationSeconds() != 210 {
			t.Errorf("%s: %d s", k, e.GetDurationSeconds())
		}
	}
	posts := r.discord.posts()
	if len(posts) != 2 || !strings.Contains(posts[0], "started: 20/20") {
		t.Errorf("the bot's own call is edited, the other message left alone: %q", posts)
	}
	// After another restart, the cooldown still counts from that call.
	r.restart(t)
	r.hub.on(1)
	if res := r.at(t, start.Add(time.Hour)); res.Called != 0 || res.Resumed != 0 {
		t.Errorf("a reached call is not resumed and its cooldown holds: %+v", res)
	}
	if res := r.at(t, start.Add(2*time.Hour)); res.Called != 1 {
		t.Errorf("a call once the cooldown from the last one ran out: %+v", res)
	}
}

func TestRestartClosesAStaleCall(t *testing.T) {
	r := newRig(t, wednesday(t, 22, 0))
	r.hub.on(1)
	r.at(t, wednesday(t, 22, 0))
	// The bot is down past the window's end.
	r.restart(t)
	if res := r.at(t, wednesday(t, 23, 30)); res.Closed != 1 || res.Resumed != 0 || res.Called != 0 {
		t.Fatalf("a call whose window closed is closed: %+v", res)
	}
	posts := r.discord.posts()
	if len(posts) != 1 || !strings.HasPrefix(posts[0], "**HTG War Dogs**: the seeding window closed at 1/20.\n") || !strings.HasSuffix(posts[0], " · closed") {
		t.Errorf("stale call: %q", posts)
	}
	// A call left open from an earlier window is closed even when a new window is open.
	r.discord.mu.Lock()
	r.discord.messages[channel][0].Content = seeding.CallText(serverID, "HTG War Dogs", 1, 1, 20, "", wednesday(t, 22, 0))
	r.discord.mu.Unlock()
	r.restart(t)
	saturday := time.Date(2026, 10, 17, 18, 0, 0, 0, chicago(t))
	if res := r.at(t, saturday); res.Closed != 1 || res.Called != 1 {
		t.Errorf("the old call closes and the new window calls: %+v", res)
	}
	if posts := r.discord.posts(); len(posts) != 2 || !strings.Contains(posts[0], "closed") || !strings.Contains(posts[1], "is at 1/20") {
		t.Errorf("posts: %q", posts)
	}
}

func TestNeverPostsHistoryOnFirstEnable(t *testing.T) {
	r := newRig(t, wednesday(t, 17, 0))
	r.hub.on(1)
	r.at(t, wednesday(t, 17, 0))
	r.at(t, wednesday(t, 17, 30))
	if posts := r.discord.posts(); len(posts) != 0 {
		t.Errorf("turning it on outside a window posts nothing: %v", posts)
	}
}

func TestDaylightSavingWeek(t *testing.T) {
	// Sunday 2026-11-01 ends daylight saving in Chicago: the window stays 18:00–23:00 local, which
	// is 00:00–05:00 UTC on Monday, an hour later in UTC than the Saturday before.
	r := newRig(t, time.Date(2026, 11, 1, 23, 30, 0, 0, time.UTC))
	r.hub.on(1)
	if res := r.at(t, time.Date(2026, 11, 1, 23, 30, 0, 0, time.UTC)); res.Called != 0 {
		t.Errorf("17:30 CST is before the window: %+v", res)
	}
	if res := r.at(t, time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)); res.Called != 1 {
		t.Errorf("18:00 CST is the window's start: %+v", res)
	}
	if res := r.at(t, time.Date(2026, 11, 2, 5, 0, 0, 0, time.UTC)); res.Closed != 1 {
		t.Errorf("23:00 CST closes it: %+v", res)
	}
	// In UTC (no stats.timezone) the same rule is 18:00–23:00 UTC.
	u := newRig(t, time.Date(2026, 11, 1, 18, 0, 0, 0, time.UTC))
	u.hub.set(func(f *fakeHub) { f.timezone = "" })
	u.hub.on(1)
	if res := u.at(t, time.Date(2026, 11, 1, 18, 0, 0, 0, time.UTC)); res.Called != 1 {
		t.Errorf("UTC when unset: %+v", res)
	}
}

func TestForbiddenChannelAndUnknownServer(t *testing.T) {
	r := newRig(t, wednesday(t, 18, 0))
	r.hub.set(func(f *fakeHub) {
		f.rules[0].Channel = locked
		other := htgRule()
		other.Server = "htg-wardogs-9" // the hub lists no such server
		f.rules = append(f.rules, other)
	})
	r.hub.on(1)
	res := r.at(t, wednesday(t, 18, 0))
	if res.Forbidden != 1 || res.Unknown != 1 || res.Called != 0 {
		t.Fatalf("a refused channel and an unknown server are counted: %+v", res)
	}
	// The locked channel could not be read, so the next pass reads it again; once readable but not
	// writable, the call is refused and counted.
	r.at(t, wednesday(t, 18, 1))
	if posts := r.discord.posts(); len(posts) != 0 {
		t.Errorf("the unknown server gets no call: %v", posts)
	}
	r.discord.set(func(f *fakeDiscord) { f.failAll = true })
	r.hub.set(func(f *fakeHub) { f.rules = f.rules[:1]; f.rules[0].Channel = channel })
	if res := r.at(t, wednesday(t, 18, 2)); res.Failed != 1 {
		t.Errorf("Discord failing: %+v", res)
	}
	r.discord.set(func(f *fakeDiscord) { f.failAll = false })
	if res := r.at(t, wednesday(t, 18, 3)); res.Called != 1 {
		t.Errorf("Discord back: %+v", res)
	}
}

func TestCallRefusedIsCounted(t *testing.T) {
	r := newRig(t, wednesday(t, 18, 0))
	r.at(t, wednesday(t, 17, 0)) // the channel was read before the window
	r.discord.set(func(f *fakeDiscord) { f.forbidPost = true })
	r.hub.on(1)
	if res := r.at(t, wednesday(t, 18, 0)); res.Forbidden != 1 || res.Called != 0 {
		t.Fatalf("a refused call: %+v", res)
	}
	r.wantMetrics(t, `gravel_bot_seeding_calls_total{result="forbidden",server="htg-wardogs-1"} 1`)
	r.discord.set(func(f *fakeDiscord) { f.forbidPost = false })
	if res := r.at(t, wednesday(t, 18, 1)); res.Called != 1 {
		t.Errorf("retried at the next pass, no cooldown from a call that never went out: %+v", res)
	}
}

func TestPlayersUnreadable(t *testing.T) {
	r := newRig(t, wednesday(t, 18, 0))
	r.hub.on(1)
	r.at(t, wednesday(t, 18, 0))
	r.hub.set(func(f *fakeHub) { f.playersDown = true })
	if res := r.at(t, wednesday(t, 18, 0).Add(interval)); res.Failed != 1 {
		t.Errorf("who is on cannot be read: %+v", res)
	}
	r.hub.set(func(f *fakeHub) { f.playersDown = false })
	r.passes(t, wednesday(t, 18, 3))
	r.hub.full()
	r.at(t, wednesday(t, 18, 3).Add(interval))
	// 3.5 minutes on the clock; the failed pass's 30 s is counted at the next, capped at a minute.
	if e := r.hub.events()["seeding:htg-wardogs-1:"+strconv.FormatInt(wednesday(t, 18, 0).Unix(), 10)+":steam:"+steamID(1)]; e == nil || e.GetDurationSeconds() != 210 {
		t.Errorf("seeder 1: %v", e)
	}
}

func TestIdleWithoutRules(t *testing.T) {
	r := newRig(t, wednesday(t, 18, 0))
	r.hub.set(func(f *fakeHub) { f.rules = nil })
	if res := r.at(t, wednesday(t, 18, 0)); !res.Idle {
		t.Errorf("no rule: %+v", res)
	}
	if calls := r.discord.takeCalls(); len(calls) != 0 {
		t.Errorf("an idle pass touches nothing: %v", calls)
	}
	// A rule removed while its call is open: the call closes.
	r.hub.set(func(f *fakeHub) { f.rules = []*hubv1.DiscordSeeding{htgRule()} })
	r.hub.on(1)
	r.at(t, wednesday(t, 18, 1))
	r.hub.set(func(f *fakeHub) { f.rules = nil })
	if res := r.at(t, wednesday(t, 18, 2)); res.Idle || res.Closed != 1 {
		t.Errorf("the removed rule's call closes: %+v", res)
	}
	if res := r.at(t, wednesday(t, 18, 3)); !res.Idle {
		t.Errorf("then idle: %+v", res)
	}
}

func TestRuleWithoutHoursNeverCalls(t *testing.T) {
	r := newRig(t, wednesday(t, 18, 0))
	r.hub.set(func(f *fakeHub) { f.rules[0].Hours = nil })
	r.hub.on(1)
	for d := range 7 {
		r.at(t, wednesday(t, 20, 0).Add(time.Duration(d)*24*time.Hour))
	}
	if posts := r.discord.posts(); len(posts) != 0 {
		t.Errorf("no window, no call: %v", posts)
	}
}

func TestTexts(t *testing.T) {
	start := time.Unix(1760482800, 0)
	got := seeding.CallText(serverID, "@everyone *HTG* <@&1>", 42, 3, 20, "", start)
	if !strings.HasPrefix(got, "<@&42> **@\u200beveryone \\*HTG\\* <@\u200b&1\\>** is at 3/20") {
		t.Errorf("a name never pings or formats: %q", got)
	}
	server, at, state, ok := seeding.Marker(got)
	if !ok || server != serverID || !at.Equal(start) || state != "open" {
		t.Errorf("marker: %q %s %q %t", server, at, state, ok)
	}
	if _, _, _, ok := seeding.Marker("-# seeding HTG · 1 · open"); ok {
		t.Error("not a server id")
	}
	if _, _, _, ok := seeding.Marker("-# seeding htg-wardogs-1 · 1 · paused"); ok {
		t.Error("not a state")
	}
	if got := seeding.ClosedText(serverID, "", nil, 20, start); !strings.HasPrefix(got, "**htg-wardogs-1**: the seeding window closed.\n") {
		t.Errorf("no name, no count: %q", got)
	}
}
