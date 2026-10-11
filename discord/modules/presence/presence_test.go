package presence_test

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

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/disgo/gateway"
	"github.com/disgoorg/snowflake/v2"

	"github.com/gravel-project/gravel/discord/bot"
	"github.com/gravel-project/gravel/discord/modules/presence"
	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
)

const (
	appID   = "1558123590786748516"
	guildID = "1558121195340042350"
	channel = "1300000000000000100"
	locked  = "1300000000000000101" // a channel the bot may not use
	someone = "1300000000000000999" // a member, not the bot
	cs2App  = "1158877933042143272"
	note    = "Counts members who share their activity on Discord."
)

var (
	guild = snowflake.MustParse(guildID)
	at    = time.Date(2026, 10, 11, 1, 2, 3, 0, time.UTC)
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func games() []*hubv1.DiscordPresenceGame {
	return []*hubv1.DiscordPresenceGame{
		{Game: "wardogs", Label: "War Dogs", Activities: []string{"WARDOGS"}},
		{Game: "cs2", Label: "Counter-Strike 2", ApplicationIds: []string{cs2App}},
	}
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

func TestRender(t *testing.T) {
	live := text(presence.Render(presence.View{Games: games(), Counts: []int{3, 0}, Live: true, Note: note}, at))
	for _, want := range []string{"## Who's playing now", "**War Dogs** · 3 playing\n**Counter-Strike 2** · nobody right now", note,
		"-# `presence` · members who share their activity on Discord · changed <t:" + strconv.FormatInt(at.Unix(), 10) + ":R>"} {
		if !strings.Contains(live, want) {
			t.Errorf("live card should contain %q:\n%s", want, live)
		}
	}
	blind := text(presence.Render(presence.View{Games: games(), Live: false}, at))
	if !strings.Contains(blind, "can't see what members are playing") || strings.Contains(blind, "nobody") || strings.Contains(blind, "War Dogs") {
		t.Errorf("without presences the card shows no counts at all, not counts of nobody:\n%s", blind)
	}
	hostile := text(presence.Render(presence.View{Games: []*hubv1.DiscordPresenceGame{{Game: "x", Label: "@everyone *x*"}}, Counts: []int{1}, Live: true, Note: "<@&1>\nhi"}, at))
	if strings.Contains(hostile, "@everyone") || strings.Contains(hostile, "*x*") || strings.Contains(hostile, "\nhi") {
		t.Errorf("a label or note is plain text on one line:\n%s", hostile)
	}
	if !presence.IsCard(presence.Render(presence.View{Live: true}, at)) {
		t.Error("a rendered card is a card")
	}
	if presence.IsCard([]discord.LayoutComponent{discord.NewContainer(discord.NewTextDisplay("-# `htg-wardogs-1` · Live"))}) {
		t.Error("a server card is not a presence card")
	}
}

type message struct {
	ID         string          `json:"id"`
	ChannelID  string          `json:"channel_id"`
	Author     map[string]any  `json:"author"`
	Flags      int             `json:"flags"`
	Components json.RawMessage `json:"components"`
	Timestamp  string          `json:"timestamp"`
}

// fakeDiscord is Discord's REST API as the module and the runtime use it: the gateway URL, the
// application's flags, and a channel's messages.
type fakeDiscord struct {
	srv *httptest.Server

	mu       sync.Mutex
	flags    discord.ApplicationFlags
	next     int
	messages map[string][]*message
	calls    []string // channel calls: "GET 100", "POST 100", "PATCH 100/1400000000000000001"
	mentions []string
	failAll  bool
}

func newFakeDiscord(t *testing.T, flags discord.ApplicationFlags) *fakeDiscord {
	t.Helper()
	f := &fakeDiscord{flags: flags, next: 1, messages: map[string][]*message{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/gateway", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]string{"url": "wss://gateway.invalid"})
	})
	mux.HandleFunc("GET /api/applications/@me", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		writeJSON(w, map[string]any{"id": appID, "name": "Bot", "flags": f.flags})
	})
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
	body := func(r *http.Request) (int, json.RawMessage, string) {
		var b struct {
			Flags           int             `json:"flags"`
			Components      json.RawMessage `json:"components"`
			AllowedMentions json.RawMessage `json:"allowed_mentions"`
		}
		_ = json.NewDecoder(r.Body).Decode(&b)
		return b.Flags, b.Components, string(b.AllowedMentions)
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
		flags, comps, mentions := body(r)
		f.mentions = append(f.mentions, mentions)
		writeJSON(w, f.add(r.PathValue("channel"), appID, flags, comps))
	})
	mux.HandleFunc("PATCH /api/channels/{channel}/messages/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if !guard(w, r) {
			return
		}
		for _, m := range f.messages[r.PathValue("channel")] {
			if m.ID == r.PathValue("id") {
				flags, comps, mentions := body(r)
				f.mentions = append(f.mentions, mentions)
				m.Flags, m.Components = flags, comps
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
		Flags: flags, Components: components, Timestamp: "2026-10-11T00:00:00Z"}
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

func discordError(w http.ResponseWriter, status, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"code": code, "message": msg})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// settings is the Organization settings' Discord section as the hub would serve it.
type settings struct {
	mu   sync.Mutex
	d    *hubv1.DiscordSettings
	down bool
}

func (s *settings) read(context.Context) (*hubv1.DiscordSettings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.down {
		return nil, errors.New("hub down")
	}
	return s.d, nil
}

func (s *settings) set(f func(d *hubv1.DiscordSettings)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(s.d)
}

type rig struct {
	discord  *fakeDiscord
	settings *settings
	mod      *presence.Module
	rt       *bot.Runtime
	gateway  bool
}

func newRig(t *testing.T, flags discord.ApplicationFlags) *rig {
	t.Helper()
	r := &rig{discord: newFakeDiscord(t, flags), gateway: true,
		settings: &settings{d: &hubv1.DiscordSettings{GuildId: guildID, Presence: &hubv1.DiscordPresence{Channel: channel, Note: note, Games: games()}}}}
	r.restart(t)
	return r
}

// restart builds a fresh module and runtime against the same fakes: a bot restart, with the
// gateway on (nothing dials it) so the runtime checks the application's intents.
func (r *rig) restart(t *testing.T) {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	bc := bot.Default()
	bc.Discord.ApplicationID = appID
	bc.Discord.PublicKey = hex.EncodeToString(pub)
	bc.Discord.Token = base64.RawStdEncoding.EncodeToString([]byte(appID)) + ".Xa1b2c.not-a-real-token-signature"
	bc.Discord.Gateway = r.gateway
	r.mod = presence.NewWith(r.settings.read, presence.Config{Interval: time.Hour})
	presence.SetNow(r.mod, func() time.Time { return at })
	r.rt, err = bot.New(bot.Options{Config: bc, Logger: quiet(), RESTURL: r.discord.srv.URL + "/api"}, r.mod)
	if err != nil {
		t.Fatal(err)
	}
	rt := r.rt
	t.Cleanup(func() { rt.Close(context.Background()) })
}

func (r *rig) pass(t *testing.T) presence.Result {
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

// shown is the text of the one message in the channel.
func (r *rig) shown(t *testing.T) string {
	t.Helper()
	r.discord.mu.Lock()
	msgs := append([]*message(nil), r.discord.messages[channel]...)
	r.discord.mu.Unlock()
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

func id(n int) snowflake.ID { return snowflake.ID(1300000000000001000 + n) }

func act(name string, typ discord.ActivityType, app string) discord.Activity {
	return discord.Activity{Name: name, Type: typ, ApplicationID: json.Number(app)}
}

func pres(user int, status discord.OnlineStatus, acts ...discord.Activity) discord.Presence {
	return discord.Presence{PresenceUser: discord.PresenceUser{ID: id(user)}, GuildID: guild, Status: status, Activities: acts}
}

// htgGuild is a guild's presences as the gateway delivers them.
func htgGuild() []discord.Presence {
	return []discord.Presence{
		pres(1, discord.OnlineStatusOnline, act("WARDOGS", discord.ActivityTypeGame, "")),
		pres(2, discord.OnlineStatusIdle, act("Counter-Strike 2 (beta)", discord.ActivityTypeGame, cs2App)),                                // by application id
		pres(3, discord.OnlineStatusOffline, act("WARDOGS", discord.ActivityTypeGame, "")),                                                 // offline: not counted
		pres(4, discord.OnlineStatusOnline, act("Spotify", discord.ActivityTypeListening, "")),                                             // not a game
		pres(5, discord.OnlineStatusDND, act("wardogs", discord.ActivityTypeGame, ""), act("WarDogs ", discord.ActivityTypeCompeting, "")), // once
		pres(6, discord.OnlineStatusOnline, act("WARDOGS", discord.ActivityTypeStreaming, "")),                                             // streaming isn't playing
		pres(7, discord.OnlineStatusOnline, act("Custom Status", discord.ActivityTypeCustom, "")),
	}
}

func TestCountsPostThenEditInPlace(t *testing.T) {
	r := newRig(t, discord.ApplicationFlagGatewayPresenceLimited)
	r.mod.LoadGuild(guild, htgGuild())

	res := r.pass(t)
	if !res.Live || !res.Posted || res.Counts["wardogs"] != 2 || res.Counts["cs2"] != 1 {
		t.Fatalf("first pass: %+v", res)
	}
	if got := r.shown(t); !strings.Contains(got, "**War Dogs** · 2 playing\n**Counter-Strike 2** · 1 playing") {
		t.Errorf("card:\n%s", got)
	}
	if c := r.discord.takeCalls(); strings.Join(c, ",") != "GET 100/messages,POST 100/messages" {
		t.Errorf("first pass looks for its card, then posts: %v", c)
	}
	m := r.metrics(t)
	for _, want := range []string{`gravel_bot_presence_playing{game="wardogs"} 2`, `gravel_bot_presence_playing{game="cs2"} 1`,
		"gravel_bot_presence_members_tracked 3", `gravel_bot_presence_updates_total{result="posted"} 1`,
		`gravel_bot_gateway_intent_missing{intent="guild_presences"} 0`} {
		if !strings.Contains(m, want) {
			t.Errorf("metrics lack %q", want)
		}
	}

	// Nothing changed: no Discord call at all.
	if res := r.pass(t); !res.Unchanged {
		t.Errorf("second pass: %+v", res)
	}
	if c := r.discord.takeCalls(); len(c) != 0 {
		t.Errorf("an unchanged count costs no Discord call: %v", c)
	}

	// Member 1 goes offline, member 2 leaves the guild: the same message is edited.
	r.mod.Observe(pres(1, discord.OnlineStatusOffline))
	r.mod.ForgetMember(guild, id(2))
	if res := r.pass(t); !res.Edited || res.Counts["wardogs"] != 1 || res.Counts["cs2"] != 0 {
		t.Errorf("after leaving: %+v", res)
	}
	if c := r.discord.takeCalls(); strings.Join(c, ",") != "PATCH 100/messages/1400000000000000001" {
		t.Errorf("one edit of the same message: %v", c)
	}
	if got := r.shown(t); !strings.Contains(got, "**War Dogs** · 1 playing\n**Counter-Strike 2** · nobody right now") {
		t.Errorf("edited card:\n%s", got)
	}

	// A member starts playing; a presence for a guild the gateway hasn't delivered is ignored.
	r.mod.Observe(pres(8, discord.OnlineStatusOnline, act("WARDOGS", discord.ActivityTypeGame, "")))
	other := pres(9, discord.OnlineStatusOnline, act("WARDOGS", discord.ActivityTypeGame, ""))
	other.GuildID = snowflake.ID(42)
	r.mod.Observe(other)
	if res := r.pass(t); res.Counts["wardogs"] != 2 {
		t.Errorf("a new player counts, another guild's doesn't: %+v", res)
	}

	// A settings change applies at the next pass, without a presence event.
	r.settings.set(func(d *hubv1.DiscordSettings) {
		d.Presence.Games[1].Activities = []string{"Spotify"}
	})
	if res := r.pass(t); res.Counts["cs2"] != 0 {
		t.Errorf("listening is never playing, whatever the name: %+v", res)
	}
	r.settings.set(func(d *hubv1.DiscordSettings) {
		d.Presence.Games = append(d.Presence.Games, &hubv1.DiscordPresenceGame{Game: "custom", Label: "Custom", Activities: []string{"WARDOGS"}})
	})
	if res := r.pass(t); res.Counts["custom"] != 2 || res.Counts["wardogs"] != 2 {
		t.Errorf("a member may count for two lines: %+v", res)
	}

	r.discord.mu.Lock()
	mentions := r.discord.mentions
	r.discord.mu.Unlock()
	for _, am := range mentions {
		if am != `{"parse":[],"roles":[],"users":[],"replied_user":false}` {
			t.Errorf("every post and edit allows no mention: %s", am)
		}
	}
}

func TestWaitsForTheGuild(t *testing.T) {
	r := newRig(t, discord.ApplicationFlagGatewayPresence)
	if res := r.pass(t); !res.Waiting {
		t.Errorf("before the gateway delivers the guild: %+v", res)
	}
	if c := r.discord.takeCalls(); len(c) != 0 {
		t.Errorf("waiting touches no card: %v", c)
	}
	r.mod.Observe(pres(1, discord.OnlineStatusOnline, act("WARDOGS", discord.ActivityTypeGame, "")))
	if res := r.pass(t); !res.Waiting {
		t.Errorf("an update before the guild is not a view: %+v", res)
	}
	r.mod.LoadGuild(guild, nil)
	if res := r.pass(t); !res.Posted || res.Counts["wardogs"] != 0 {
		t.Errorf("an empty guild is counted: %+v", res)
	}
	r.mod.ForgetGuild(guild)
	if res := r.pass(t); !res.Waiting {
		t.Errorf("a guild lost is waited for again: %+v", res)
	}
	if got := r.shown(t); !strings.Contains(got, "**War Dogs** · nobody right now") {
		t.Errorf("the card is left as it was:\n%s", got)
	}
}

func TestWithoutTheIntent(t *testing.T) {
	r := newRig(t, discord.ApplicationFlagGatewayGuildMemberLimited)
	if r.rt.Intents().Has(gateway.IntentGuildPresences) {
		t.Error("the runtime leaves out an intent the application isn't allowed")
	}
	r.mod.LoadGuild(guild, htgGuild()) // cannot happen without the intent; ignored if it did
	res := r.pass(t)
	if res.Live || !res.Posted || res.Counts != nil {
		t.Fatalf("pass: %+v", res)
	}
	if got := r.shown(t); !strings.Contains(got, "can't see what members are playing") || strings.Contains(got, "playing\n") {
		t.Errorf("card:\n%s", got)
	}
	m := r.metrics(t)
	if !strings.Contains(m, `gravel_bot_gateway_intent_missing{intent="guild_presences"} 1`) || strings.Contains(m, "gravel_bot_presence_playing{") {
		t.Errorf("the gauge says why, and no count is exported:\n%s", m)
	}

	// The toggle turned on, the bot restarted: the same message shows counts.
	r.discord.mu.Lock()
	r.discord.flags = discord.ApplicationFlagGatewayPresenceLimited
	r.discord.mu.Unlock()
	r.restart(t)
	r.mod.LoadGuild(guild, htgGuild())
	if res := r.pass(t); !res.Live || !res.Edited {
		t.Errorf("after the toggle: %+v", res)
	}
	if got := r.shown(t); !strings.Contains(got, "2 playing") {
		t.Errorf("card:\n%s", got)
	}
}

func TestGatewayOff(t *testing.T) {
	r := &rig{discord: newFakeDiscord(t, discord.ApplicationFlagGatewayPresence), settings: &settings{d: &hubv1.DiscordSettings{GuildId: guildID,
		Presence: &hubv1.DiscordPresence{Channel: channel, Games: games()}}}}
	r.restart(t)
	if res := r.pass(t); res.Live || !res.Posted {
		t.Errorf("no gateway, no presences: %+v", res)
	}
}

func TestRestartFindsTheCard(t *testing.T) {
	r := newRig(t, discord.ApplicationFlagGatewayPresence)
	// Someone else's message carrying the marker is never taken for the card.
	r.discord.mu.Lock()
	theirs, _ := json.Marshal(presence.Render(presence.View{Live: true}, at))
	r.discord.add(channel, someone, int(discord.MessageFlagIsComponentsV2), theirs)
	r.discord.mu.Unlock()
	r.mod.LoadGuild(guild, htgGuild())
	if res := r.pass(t); !res.Posted {
		t.Fatalf("first: %+v", res)
	}
	r.restart(t)
	r.mod.LoadGuild(guild, htgGuild())
	r.discord.takeCalls()
	if res := r.pass(t); !res.Edited {
		t.Errorf("after a restart the card is found and edited: %+v", res)
	}
	if c := r.discord.takeCalls(); strings.Join(c, ",") != "GET 100/messages,PATCH 100/messages/1400000000000000002" {
		t.Errorf("calls: %v", c)
	}

	// Deleted since: posted again.
	r.discord.mu.Lock()
	r.discord.messages[channel] = r.discord.messages[channel][:1]
	r.discord.mu.Unlock()
	r.mod.Observe(pres(1, discord.OnlineStatusOffline))
	if res := r.pass(t); !res.Posted {
		t.Errorf("a deleted card is posted again: %+v", res)
	}
}

func TestIdleAndFailures(t *testing.T) {
	r := newRig(t, discord.ApplicationFlagGatewayPresence)
	r.mod.LoadGuild(guild, htgGuild())
	r.pass(t)
	r.settings.set(func(d *hubv1.DiscordSettings) { d.Presence = nil })
	if res := r.pass(t); !res.Idle {
		t.Errorf("no card placed: %+v", res)
	}
	if m := r.metrics(t); strings.Contains(m, "gravel_bot_presence_playing{") {
		t.Errorf("an idle card exports no counts:\n%s", m)
	}

	r.settings.set(func(d *hubv1.DiscordSettings) { d.Presence = &hubv1.DiscordPresence{Channel: locked, Games: games()} })
	r.discord.takeCalls()
	if res := r.pass(t); !res.Forbidden {
		t.Errorf("a channel the bot may not use: %+v", res)
	}
	r.discord.mu.Lock()
	r.discord.failAll = true
	r.discord.mu.Unlock()
	r.settings.set(func(d *hubv1.DiscordSettings) { d.Presence.Channel = channel })
	if res := r.pass(t); !res.Failed {
		t.Errorf("Discord failing: %+v", res)
	}
	m := r.metrics(t)
	for _, want := range []string{`gravel_bot_presence_updates_total{result="forbidden"} 1`, `gravel_bot_presence_updates_total{result="error"} 1`} {
		if !strings.Contains(m, want) {
			t.Errorf("metrics lack %q", want)
		}
	}

	r.settings.mu.Lock()
	r.settings.down = true
	r.settings.mu.Unlock()
	if _, err := r.mod.Pass(context.Background()); err == nil {
		t.Error("the hub away is an error for the pass, never a crash")
	}
}

func TestEventsKeepTheView(t *testing.T) {
	r := newRig(t, discord.ApplicationFlagGatewayPresence)
	gg := discord.GatewayGuild{Presences: htgGuild()}
	gen := &events.GenericGuild{GuildID: guild}
	r.mod.OnEvent(&events.GuildReady{GenericGuild: gen, Guild: gg})
	if res := r.pass(t); res.Counts["wardogs"] != 2 {
		t.Fatalf("guild ready: %+v", res)
	}
	r.mod.OnEvent(&events.PresenceUpdate{EventPresenceUpdate: gateway.EventPresenceUpdate{Presence: pres(5, discord.OnlineStatusOnline)}})
	r.mod.OnEvent(&events.GuildMemberLeave{GuildID: guild, User: discord.User{ID: id(2)}})
	if res := r.pass(t); res.Counts["wardogs"] != 1 || res.Counts["cs2"] != 0 {
		t.Errorf("an update and a leave: %+v", res)
	}
	r.mod.OnEvent(&events.GuildUnavailable{GenericGuild: gen})
	if res := r.pass(t); !res.Waiting {
		t.Errorf("an outage: %+v", res)
	}
	r.mod.OnEvent(&events.GuildAvailable{GenericGuild: gen, Guild: gg})
	if res := r.pass(t); res.Counts["wardogs"] != 2 {
		t.Errorf("back: %+v", res)
	}
	r.mod.OnEvent(&events.GuildLeave{GenericGuild: gen})
	r.mod.OnEvent(&events.GuildJoin{GenericGuild: gen, Guild: discord.GatewayGuild{}})
	if res := r.pass(t); res.Counts["wardogs"] != 0 {
		t.Errorf("left, then joined again with nobody playing: %+v", res)
	}
}

func TestRunUntilCancelled(t *testing.T) {
	r := newRig(t, discord.ApplicationFlagGatewayPresence)
	r.mod.LoadGuild(guild, htgGuild())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.mod.Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		r.discord.mu.Lock()
		n := len(r.discord.messages[channel])
		r.discord.mu.Unlock()
		if n == 1 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("run: %v", err)
	}
	if got := r.shown(t); !strings.Contains(got, "2 playing") {
		t.Errorf("run posts at once:\n%s", got)
	}
}
