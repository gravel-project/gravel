package servercards_test

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
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/disgoorg/disgo/discord"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/gravel-project/gravel/discord/bot"
	"github.com/gravel-project/gravel/discord/hubclient"
	"github.com/gravel-project/gravel/discord/modules/servercards"
	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
	"github.com/gravel-project/gravel/gen/gravel/hub/v1/hubv1connect"
)

const (
	appID     = "1558123590786748516"
	guildID   = "1558121195340042350"
	channel   = "1300000000000000100"
	locked    = "1300000000000000101" // a channel the bot may not use
	someone   = "1300000000000000999" // a member, not the bot
	serverID  = "htg-wardogs-1"
	otherID   = "htg-wardogs-2"
	matchNote = "Matches start at 20 players."
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

var observed = time.Date(2026, 10, 10, 12, 30, 15, 0, time.UTC)

func live(players int32) *hubv1.Server {
	return &hubv1.Server{Id: serverID, Name: "HTG War Dogs", GameId: "wardogs", Status: &hubv1.ServerStatus{
		State: "ok", Reachable: true, ObservedAt: timestamppb.New(observed), Build: "CL-509546",
		Players: players, MaxPlayers: 64, Map: "Ozeti", Lighting: "Day",
		Teams: []*hubv1.TeamScore{{Name: "Alpha", Color: "#4CB1EF", Score: 1200}, {Name: "Bravo", Score: 900}},
	}}
}

// text is every text display of a card, joined by newlines.
func text(cs []discord.LayoutComponent) string {
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

func accent(cs []discord.LayoutComponent) int { return cs[0].(discord.ContainerComponent).AccentColor }

func TestRender(t *testing.T) {
	card := servercards.Render(live(23), matchNote)
	got := text(card)
	for _, want := range []string{"## HTG War Dogs", "**23/64** players · Ozeti (Day)", "Alpha **1200** · Bravo **900**", matchNote,
		"-# `htg-wardogs-1` · Live · updated <t:" + strconv.FormatInt(observed.Unix()-15, 10) + ":R>"} {
		if !strings.Contains(got, want) {
			t.Errorf("live card should contain %q:\n%s", want, got)
		}
	}
	if accent(card) == 0 || servercards.CardServer(card) != serverID {
		t.Errorf("live card: accent %x, server %q", accent(card), servercards.CardServer(card))
	}

	// A field the game does not report is left out.
	bare := &hubv1.Server{Id: serverID, Name: "Bare", Status: &hubv1.ServerStatus{State: "ok", Reachable: true, Players: 3}}
	if got := text(servercards.Render(bare, "")); !strings.Contains(got, "**3** players\n-# `htg-wardogs-1` · Live") || strings.Contains(got, "updated") {
		t.Errorf("bare card:\n%s", got)
	}

	down := live(23)
	down.Status.State, down.Status.Reachable = "unreachable", false
	got = text(servercards.Render(down, matchNote))
	if !strings.Contains(got, "**Unreachable** since <t:"+strconv.FormatInt(observed.Unix(), 10)+":R>.") || strings.Contains(got, "23/64") || strings.Contains(got, "Alpha") || !strings.Contains(got, "· Unreachable") {
		t.Errorf("unreachable card shows the state, not stale numbers:\n%s", got)
	}

	refused := live(23)
	refused.Status.State = "credential_refused"
	if got := text(servercards.Render(refused, "")); !strings.Contains(got, "Status unavailable") || !strings.Contains(got, "Unavailable (credential\\_refused)") || strings.Contains(got, "players") {
		t.Errorf("a failure shows its state word only:\n%s", got)
	}

	fresh := &hubv1.Server{Id: serverID, Name: "New", Status: &hubv1.ServerStatus{State: "unknown"}}
	if c := servercards.Render(fresh, ""); !strings.Contains(text(c), "Waiting for the first look") || accent(c) != 0 {
		t.Errorf("unobserved card: %x\n%s", accent(c), text(c))
	}

	// Names are the game's or the host's: never markdown, never a mention.
	odd := live(1)
	odd.Name = "@everyone *bold* `x`\n# big"
	odd.Status.Teams = []*hubv1.TeamScore{{Name: "<@&1>", Score: 1}}
	got = text(servercards.Render(odd, ""))
	if !strings.Contains(got, "## @\u200beveryone \\*bold\\* \\`x\\` \\# big") || !strings.Contains(got, "<@\u200b&1\\>") {
		t.Errorf("escaped names:\n%s", got)
	}
}

func TestCardServer(t *testing.T) {
	if got := servercards.CardServer(nil); got != "" {
		t.Errorf("no components: %q", got)
	}
	other := []discord.LayoutComponent{discord.NewContainer(discord.NewTextDisplay("-# not a card"))}
	if got := servercards.CardServer(other); got != "" {
		t.Errorf("a container that is not a card: %q", got)
	}
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

// fakeDiscord is Discord's REST API for channels: list, post and edit messages.
type fakeDiscord struct {
	srv *httptest.Server

	mu       sync.Mutex
	next     int
	messages map[string][]*message // channel → newest last
	calls    []string              // "GET 100", "POST 100", "PATCH 100/1"
	mentions []string              // the allowed_mentions of every post and edit
	failAll  bool
}

func newFakeDiscord(t *testing.T) *fakeDiscord {
	t.Helper()
	f := &fakeDiscord{next: 1, messages: map[string][]*message{}}
	mux := http.NewServeMux()
	guard := func(w http.ResponseWriter, r *http.Request) bool {
		f.calls = append(f.calls, r.Method+" "+strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/api/channels/"), "1300000000000000"))
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
			Flags           int             `json:"flags"`
			Components      json.RawMessage `json:"components"`
			AllowedMentions json.RawMessage `json:"allowed_mentions"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mentions = append(f.mentions, string(body.AllowedMentions))
		m := f.add(r.PathValue("channel"), appID, body.Flags, body.Components)
		writeJSON(w, m)
	})
	mux.HandleFunc("PATCH /api/channels/{channel}/messages/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if !guard(w, r) {
			return
		}
		for _, m := range f.messages[r.PathValue("channel")] {
			if m.ID == r.PathValue("id") {
				var body struct {
					Flags           int             `json:"flags"`
					Components      json.RawMessage `json:"components"`
					AllowedMentions json.RawMessage `json:"allowed_mentions"`
				}
				_ = json.NewDecoder(r.Body).Decode(&body)
				f.mentions = append(f.mentions, string(body.AllowedMentions))
				m.Flags, m.Components = body.Flags, body.Components
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
func (f *fakeDiscord) add(ch, author string, flags int, components json.RawMessage) *message {
	m := &message{ID: strconv.Itoa(1400000000000000000 + f.next), ChannelID: ch, Author: map[string]any{"id": author, "username": "u"},
		Flags: flags, Components: components, Timestamp: "2026-10-10T12:00:00Z"}
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

func (f *fakeDiscord) cards(ch string) []*message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*message(nil), f.messages[ch]...)
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

// fakeHub is the hub as the bot's app sees it: the settings and the server list.
type fakeHub struct {
	hubv1connect.UnimplementedOrganizationServiceHandler
	hubv1connect.UnimplementedServerServiceHandler
	srv *httptest.Server

	mu      sync.Mutex
	cards   []*hubv1.DiscordServerCard
	servers []*hubv1.Server
	lists   int
	down    bool
}

func newFakeHub(t *testing.T) *fakeHub {
	t.Helper()
	f := &fakeHub{cards: []*hubv1.DiscordServerCard{{Server: serverID, Channel: channel, Note: matchNote}}, servers: []*hubv1.Server{live(23)}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /oauth/token", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"tok","token_type":"Bearer","expires_in":3600}`))
	})
	path, h := hubv1connect.NewOrganizationServiceHandler(f)
	mux.Handle(path, h)
	path, h = hubv1connect.NewServerServiceHandler(f)
	mux.Handle(path, h)
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
		Discord: &hubv1.DiscordSettings{GuildId: guildID, ServerCards: f.cards}}}), nil
}

func (f *fakeHub) ListServers(context.Context, *connect.Request[hubv1.ListServersRequest]) (*connect.Response[hubv1.ListServersResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lists++
	return connect.NewResponse(&hubv1.ListServersResponse{Servers: f.servers}), nil
}

func (f *fakeHub) set(fn func(*fakeHub)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

type rig struct {
	discord *fakeDiscord
	hub     *fakeHub
	mod     *servercards.Module
	rt      *bot.Runtime
}

func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{discord: newFakeDiscord(t), hub: newFakeHub(t)}
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
	bc.Discord.ApplicationID = appID
	bc.Discord.PublicKey = hex.EncodeToString(pub)
	bc.Discord.Token = base64.RawStdEncoding.EncodeToString([]byte(appID)) + ".Xa1b2c.not-a-real-token-signature"
	bc.Discord.Gateway = false
	r.mod = servercards.New(hc, servercards.Config{Interval: time.Hour})
	r.rt, err = bot.New(bot.Options{Config: bc, Logger: quiet(), RESTURL: r.discord.srv.URL + "/api"}, r.mod)
	if err != nil {
		t.Fatal(err)
	}
	rt := r.rt
	t.Cleanup(func() { rt.Close(context.Background()) })
}

func (r *rig) pass(t *testing.T) servercards.Result {
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

// shown is the text of the one card in the channel.
func (r *rig) shown(t *testing.T) string {
	t.Helper()
	msgs := r.discord.cards(channel)
	if len(msgs) != 1 {
		t.Fatalf("want one message in the channel, got %d", len(msgs))
	}
	var m discord.Message
	raw, _ := json.Marshal(msgs[0])
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return text(m.Components)
}

func equal(a, b []string) bool { return strings.Join(a, ",") == strings.Join(b, ",") }

func TestPostThenEditInPlace(t *testing.T) {
	r := newRig(t)
	if res := r.pass(t); res.Cards != 1 || res.Posted != 1 {
		t.Fatalf("first pass posts: %+v", res)
	}
	if calls := r.discord.takeCalls(); !equal(calls, []string{"GET 100/messages", "POST 100/messages"}) {
		t.Errorf("first pass looks for a card, finds none, posts: %v", calls)
	}
	if got := r.shown(t); !strings.Contains(got, "**23/64** players") || !strings.Contains(got, matchNote) {
		t.Errorf("card:\n%s", got)
	}
	for _, m := range r.discord.mentions {
		if m != `{"parse":[],"roles":[],"users":[],"replied_user":false}` {
			t.Errorf("a card pings no one: %s", m)
		}
	}

	// Nothing changed, or only within the minute: no call to Discord at all.
	r.hub.set(func(f *fakeHub) { f.servers[0].Status.ObservedAt = timestamppb.New(observed.Add(20 * time.Second)) })
	if res := r.pass(t); res.Unchanged != 1 || res.Posted+res.Edited != 0 {
		t.Errorf("same card: %+v", res)
	}
	if calls := r.discord.takeCalls(); len(calls) != 0 {
		t.Errorf("an unchanged card costs nothing: %v", calls)
	}

	// A change is one edit of the same message.
	r.hub.set(func(f *fakeHub) { f.servers[0].Status.Players = 31 })
	if res := r.pass(t); res.Edited != 1 {
		t.Errorf("changed card: %+v", res)
	}
	if calls := r.discord.takeCalls(); len(calls) != 1 || !strings.HasPrefix(calls[0], "PATCH 100/messages/") {
		t.Errorf("one edit: %v", calls)
	}
	if got := r.shown(t); !strings.Contains(got, "**31/64** players") {
		t.Errorf("edited card:\n%s", got)
	}

	// Unreachable: the same message shows the degraded state.
	r.hub.set(func(f *fakeHub) { f.servers[0].Status.State, f.servers[0].Status.Reachable = "unreachable", false })
	r.pass(t)
	if got := r.shown(t); !strings.Contains(got, "**Unreachable** since") || strings.Contains(got, "/64") {
		t.Errorf("degraded card:\n%s", got)
	}
	m := r.metrics(t)
	for _, want := range []string{`gravel_bot_servercards_updates_total{result="posted"} 1`, `gravel_bot_servercards_updates_total{result="edited"} 2`} {
		if !strings.Contains(m, want) {
			t.Errorf("metrics should contain %s", want)
		}
	}
	if strings.Contains(m, "gravel_bot_servercards_last_success_timestamp_seconds 0\n") {
		t.Error("a pass that updated every card is a success")
	}
}

func TestRestartFindsTheCard(t *testing.T) {
	r := newRig(t)
	// Messages that are not this server's card: someone's, and the bot's card for another server.
	r.discord.mu.Lock()
	r.discord.add(channel, someone, 0, json.RawMessage(`[]`))
	r.discord.mu.Unlock()
	r.hub.set(func(f *fakeHub) {
		f.cards = append(f.cards, &hubv1.DiscordServerCard{Server: otherID, Channel: channel})
		other := live(5)
		other.Id = otherID
		f.servers = append(f.servers, other)
	})
	if res := r.pass(t); res.Posted != 2 {
		t.Fatalf("two cards posted: %+v", res)
	}
	r.discord.takeCalls()

	r.restart(t)
	r.hub.set(func(f *fakeHub) { f.servers[0].Status.Players = 40 })
	res := r.pass(t)
	if res.Posted != 0 || res.Edited != 2 {
		t.Errorf("a restart edits what is there: %+v", res)
	}
	if msgs := r.discord.cards(channel); len(msgs) != 3 {
		t.Errorf("no duplicate: %d messages", len(msgs))
	}
	calls := r.discord.takeCalls()
	if len(calls) != 4 || calls[1] != "PATCH 100/messages/1400000000000000002" || calls[3] != "PATCH 100/messages/1400000000000000003" {
		t.Errorf("each card found and edited in place: %v", calls)
	}
}

func TestDeletedCardIsPostedAgain(t *testing.T) {
	r := newRig(t)
	r.pass(t)
	r.discord.mu.Lock()
	r.discord.messages[channel] = nil // a moderator deletes it
	r.discord.mu.Unlock()
	r.discord.takeCalls()
	r.hub.set(func(f *fakeHub) { f.servers[0].Status.Players = 24 })
	if res := r.pass(t); res.Posted != 1 || res.Failed != 0 {
		t.Errorf("deleted card: %+v", res)
	}
	if calls := r.discord.takeCalls(); len(calls) != 2 || !strings.HasPrefix(calls[0], "PATCH") || calls[1] != "POST 100/messages" {
		t.Errorf("the edit finds it gone, then it is posted: %v", calls)
	}
	if got := r.shown(t); !strings.Contains(got, "**24/64**") {
		t.Errorf("reposted card:\n%s", got)
	}
}

func TestFailuresAreCountedNotFatal(t *testing.T) {
	r := newRig(t)
	r.hub.set(func(f *fakeHub) {
		f.cards = []*hubv1.DiscordServerCard{{Server: serverID, Channel: locked}, {Server: "gone-server", Channel: channel}}
	})
	res := r.pass(t)
	if res.Forbidden != 1 || res.Unknown != 1 || res.Posted != 0 {
		t.Errorf("forbidden channel and unknown server: %+v", res)
	}
	if calls := r.discord.takeCalls(); !equal(calls, []string{"GET 101/messages"}) {
		t.Errorf("an unknown server costs no Discord call: %v", calls)
	}
	if n := len(r.discord.cards(locked)); n != 0 {
		t.Errorf("nothing lands in a channel the bot may not use: %d", n)
	}
	if m := r.metrics(t); !strings.Contains(m, `gravel_bot_servercards_updates_total{result="forbidden"} 1`) || !strings.Contains(m, "gravel_bot_servercards_last_success_timestamp_seconds 0\n") {
		t.Errorf("forbidden is counted and the pass is not a success:\n%s", m)
	}

	// Discord failing: an error per card, the next pass tries again.
	r.hub.set(func(f *fakeHub) { f.cards = []*hubv1.DiscordServerCard{{Server: serverID, Channel: channel}} })
	r.discord.mu.Lock()
	r.discord.failAll = true
	r.discord.mu.Unlock()
	if res := r.pass(t); res.Failed != 1 {
		t.Errorf("Discord down: %+v", res)
	}
	r.discord.mu.Lock()
	r.discord.failAll = false
	r.discord.mu.Unlock()
	if res := r.pass(t); res.Posted != 1 {
		t.Errorf("Discord back: %+v", res)
	}

	// The hub away is the pass's error.
	r.hub.set(func(f *fakeHub) { f.down = true })
	if _, err := r.mod.Pass(context.Background()); err == nil {
		t.Error("hub down is an error")
	}
}

func TestIdleWithoutCards(t *testing.T) {
	r := newRig(t)
	r.hub.set(func(f *fakeHub) { f.cards = nil })
	if res := r.pass(t); !res.Idle {
		t.Errorf("no card placed: %+v", res)
	}
	if r.hub.lists != 0 || len(r.discord.takeCalls()) != 0 {
		t.Errorf("idle reads no servers and calls no Discord")
	}
}

func TestRunUpdatesUntilCancelled(t *testing.T) {
	r := newRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.mod.Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for len(r.discord.cards(channel)) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("run: %v", err)
	}
	if len(r.discord.cards(channel)) != 1 {
		t.Error("the first pass runs at start")
	}
}
