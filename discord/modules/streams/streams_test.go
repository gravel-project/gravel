package streams_test

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
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/gravel-project/gravel/discord/bot"
	"github.com/gravel-project/gravel/discord/hubclient"
	"github.com/gravel-project/gravel/discord/modules/streams"
	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
	"github.com/gravel-project/gravel/gen/gravel/hub/v1/hubv1connect"
)

const (
	appID   = "1558123590786748516"
	channel = "1300000000000000100"
	locked  = "1300000000000000101"
	role    = "1300000000000000005"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

var start = time.Date(2026, 10, 14, 23, 0, 0, 0, time.UTC)

func settings() *hubv1.DiscordStreams {
	return &hubv1.DiscordStreams{Channel: channel, Role: role, Note: "Grab the Stream role in #roles.", Streamers: []*hubv1.DiscordStreamer{
		{Platform: "twitch", Login: "hiddentoken", Label: "Hidden Token"},
		{Platform: "kick", Login: "hidden-token"},
	}}
}

// fakeHub is the hub as the bot's app sees it: the settings.
type fakeHub struct {
	hubv1connect.UnimplementedOrganizationServiceHandler
	srv *httptest.Server

	mu      sync.Mutex
	streams *hubv1.DiscordStreams
	down    bool
}

func newFakeHub(t *testing.T) *fakeHub {
	t.Helper()
	f := &fakeHub{streams: settings()}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /oauth/token", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"tok","token_type":"Bearer","expires_in":3600}`))
	})
	path, h := hubv1connect.NewOrganizationServiceHandler(f)
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
		Discord: &hubv1.DiscordSettings{GuildId: "1558121195340042350", Streams: f.streams}}}), nil
}

type message struct {
	ID        string         `json:"id"`
	ChannelID string         `json:"channel_id"`
	Author    map[string]any `json:"author"`
	Content   string         `json:"content"`
	Flags     int            `json:"flags"`
	Timestamp string         `json:"timestamp"`
	Edited    *string        `json:"edited_timestamp"`
}

// fakeDiscord is a channel's messages.
type fakeDiscord struct {
	srv      *httptest.Server
	mu       sync.Mutex
	next     int
	messages map[string][]*message
	calls    []string
	mentions []string
	failPost int // the next posts answer 500
}

func newFakeDiscord(t *testing.T) *fakeDiscord {
	t.Helper()
	f := &fakeDiscord{next: 1, messages: map[string][]*message{}}
	mux := http.NewServeMux()
	guard := func(w http.ResponseWriter, r *http.Request) bool {
		f.calls = append(f.calls, r.Method)
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
		if f.failPost > 0 {
			f.failPost--
			http.Error(w, `{"message":"boom","code":0}`, http.StatusInternalServerError)
			return
		}
		var body struct {
			Content         string          `json:"content"`
			Flags           int             `json:"flags"`
			AllowedMentions json.RawMessage `json:"allowed_mentions"`
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
		var body struct {
			Content         *string         `json:"content"`
			Flags           *int            `json:"flags"`
			AllowedMentions json.RawMessage `json:"allowed_mentions"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mentions = append(f.mentions, string(body.AllowedMentions))
		for _, m := range f.messages[r.PathValue("channel")] {
			if m.ID == r.PathValue("id") {
				if body.Content != nil {
					m.Content = *body.Content
				}
				if body.Flags != nil {
					m.Flags = *body.Flags
				}
				ts := "2026-10-15T03:00:00Z"
				m.Edited = &ts
				writeJSON(w, m)
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"code":10008,"message":"Unknown Message"}`)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected Discord call %s %s", r.Method, r.URL)
		http.Error(w, "unexpected", http.StatusNotFound)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeDiscord) add(ch, author, content string) *message {
	m := &message{ID: strconv.Itoa(1400000000000000000 + f.next), ChannelID: ch, Author: map[string]any{"id": author, "username": "u"},
		Content: content, Timestamp: "2026-10-14T23:01:00Z"}
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

func (f *fakeDiscord) posts() []message {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []message
	for _, m := range f.messages[channel] {
		out = append(out, *m)
	}
	return out
}

func (f *fakeDiscord) lastMentions() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.mentions[len(f.mentions)-1]
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// fakePlatforms are Twitch and Kick: their token endpoints and the two reads.
type fakePlatforms struct {
	srv *httptest.Server

	mu         sync.Mutex
	twitch     []map[string]any // Helix streams
	kick       []map[string]any // Kick channels
	tokens     map[string]int   // tokens issued, by platform
	reads      map[string]int
	refuse     map[string]int // the next reads answer 401, by platform
	throttle   map[string]string
	badSecrets bool
}

func newFakePlatforms(t *testing.T) *fakePlatforms {
	t.Helper()
	f := &fakePlatforms{tokens: map[string]int{}, reads: map[string]int{}, refuse: map[string]int{}, throttle: map[string]string{}}
	mux := http.NewServeMux()
	token := func(platform, id, secret string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			f.mu.Lock()
			defer f.mu.Unlock()
			if err := r.ParseForm(); err != nil || r.PostForm.Get("grant_type") != "client_credentials" || r.PostForm.Get("client_id") != id ||
				r.PostForm.Get("client_secret") != secret || f.badSecrets {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error":"invalid_client"}`)
				return
			}
			f.tokens[platform]++
			writeJSON(w, map[string]any{"access_token": platform + "-tok-" + strconv.Itoa(f.tokens[platform]), "token_type": "bearer", "expires_in": 5000000})
		}
	}
	mux.HandleFunc("POST /twitch/oauth2/token", token("twitch", "tw-id", "tw-secret"))
	mux.HandleFunc("POST /kick/oauth/token", token("kick", "ki-id", "ki-secret"))
	read := func(platform string, w http.ResponseWriter, r *http.Request) bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.reads[platform]++
		if f.refuse[platform] > 0 {
			f.refuse[platform]--
			w.WriteHeader(http.StatusUnauthorized)
			return false
		}
		if r.Header.Get("Authorization") != "Bearer "+platform+"-tok-"+strconv.Itoa(f.tokens[platform]) {
			t.Errorf("%s: authorization %q", platform, r.Header.Get("Authorization"))
		}
		if ra, ok := f.throttle[platform]; ok {
			w.Header().Set("Retry-After", ra)
			w.WriteHeader(http.StatusTooManyRequests)
			return false
		}
		return true
	}
	mux.HandleFunc("GET /twitch/api/helix/streams", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Client-Id") != "tw-id" || r.URL.Query().Get("type") != "live" || !slices.Equal(r.URL.Query()["user_login"], []string{"hiddentoken"}) {
			t.Errorf("twitch: %v %v", r.Header, r.URL.Query())
		}
		if !read("twitch", w, r) {
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		writeJSON(w, map[string]any{"data": f.twitch, "pagination": map[string]any{}})
	})
	mux.HandleFunc("GET /kick/api/public/v1/channels", func(w http.ResponseWriter, r *http.Request) {
		if !slices.Equal(r.URL.Query()["slug"], []string{"hidden-token"}) {
			t.Errorf("kick: %v", r.URL.Query())
		}
		if !read("kick", w, r) {
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		writeJSON(w, map[string]any{"data": f.kick, "message": "OK"})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakePlatforms) set(fn func(*fakePlatforms)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *fakePlatforms) count(m map[string]int, platform string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return m[platform]
}

func twitchLive(id, title string) []map[string]any {
	return []map[string]any{{"id": id, "user_id": "141981764", "user_login": "hiddentoken", "user_name": "HiddenToken", "game_id": "1",
		"game_name": "War Dogs", "type": "live", "title": title, "viewer_count": 12, "started_at": start.Format(time.RFC3339), "language": "en"}}
}

func kickChannel(live bool, startTime string) []map[string]any {
	return []map[string]any{{"broadcaster_user_id": 1, "slug": "hidden-token", "stream_title": "Season 02 night", "category": map[string]any{"id": 1, "name": "War Dogs"},
		"stream": map[string]any{"is_live": live, "start_time": startTime, "viewer_count": 3}}}
}

type rig struct {
	hub       *fakeHub
	discord   *fakeDiscord
	platforms *fakePlatforms
	mod       *streams.Module
	rt        *bot.Runtime
	now       time.Time
	only      []string // the platforms with an application; nil is both
}

func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{hub: newFakeHub(t), discord: newFakeDiscord(t), platforms: newFakePlatforms(t), now: start.Add(time.Minute)}
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
	httpc := streams.HTTPClient()
	var ps []streams.Platform
	if r.only == nil || slices.Contains(r.only, "twitch") {
		ps = append(ps, streams.NewTwitch(streams.App{ClientID: "tw-id", ClientSecret: "tw-secret", TokenURL: r.platforms.srv.URL + "/twitch/oauth2/token", APIURL: r.platforms.srv.URL + "/twitch/api"}, httpc))
	}
	if r.only == nil || slices.Contains(r.only, "kick") {
		ps = append(ps, streams.NewKick(streams.App{ClientID: "ki-id", ClientSecret: "ki-secret", TokenURL: r.platforms.srv.URL + "/kick/oauth/token", APIURL: r.platforms.srv.URL + "/kick/api/"}, httpc))
	}
	r.mod = streams.New(hc, streams.Config{Interval: time.Hour, Platforms: ps})
	r.mod.SetClock(func() time.Time { return r.now })
	r.rt, err = bot.New(bot.Options{Config: bc, Logger: quiet(), RESTURL: r.discord.srv.URL + "/api"}, r.mod)
	if err != nil {
		t.Fatal(err)
	}
	rt := r.rt
	t.Cleanup(func() { rt.Close(context.Background()) })
}

func (r *rig) pass(t *testing.T) streams.Result {
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

func TestPostsOnceAndEditsWhenTheStreamEnds(t *testing.T) {
	r := newRig(t)
	if res := r.pass(t); len(res.Posted)+len(res.Ended)+len(res.Failed)+len(res.Unknown) != 0 {
		t.Fatalf("nobody live: %+v", res)
	}
	if c := r.discord.takeCalls(); !slices.Equal(c, []string{"GET"}) {
		t.Errorf("the channel is searched once, nothing posted: %v", c)
	}

	r.platforms.set(func(f *fakePlatforms) { f.twitch = twitchLive("312345", "Season 02 *preview* @everyone") })
	r.now = start.Add(2 * time.Minute)
	if res := r.pass(t); !slices.Equal(res.Posted, []string{"twitch/hiddentoken"}) {
		t.Fatalf("posted: %+v", res)
	}
	posts := r.discord.posts()
	if len(posts) != 1 {
		t.Fatalf("one post: %+v", posts)
	}
	want := "🔴 **Hidden Token** is live on Twitch\n**Season 02 \\*preview\\* @\u200beveryone** · War Dogs\nhttps://www.twitch.tv/hiddentoken\nGrab the Stream role in \\#roles.\n<@&" + role + ">\n-# stream twitch `hiddentoken` · 312345"
	if posts[0].Content != want {
		t.Errorf("post:\n%s\nwant:\n%s", posts[0].Content, want)
	}
	if m := r.discord.lastMentions(); !strings.Contains(m, `"roles":["`+role+`"]`) || !strings.Contains(m, `"parse":[]`) || !strings.Contains(m, `"users":[]`) {
		t.Errorf("the post pings the role and nobody else: %s", m)
	}

	// Still live, and the same stream listed twice: nothing more.
	r.platforms.set(func(f *fakePlatforms) { f.twitch = append(f.twitch, f.twitch[0]) })
	r.discord.takeCalls()
	for range 3 {
		r.now = r.now.Add(time.Minute)
		if res := r.pass(t); len(res.Posted)+len(res.Resumed)+len(res.Ended) != 0 {
			t.Fatalf("still live: %+v", res)
		}
	}
	if c := r.discord.takeCalls(); len(c) != 0 {
		t.Errorf("no Discord call while the stream runs: %v", c)
	}

	r.platforms.set(func(f *fakePlatforms) { f.twitch = nil })
	r.now = start.Add(2 * time.Hour)
	if res := r.pass(t); !slices.Equal(res.Ended, []string{"twitch/hiddentoken"}) {
		t.Fatalf("ended: %+v", res)
	}
	posts = r.discord.posts()
	want = "**Hidden Token** was live on Twitch · ended <t:" + strconv.FormatInt(r.now.Unix(), 10) + ":R>\n**Season 02 \\*preview\\* @\u200beveryone** · War Dogs\nhttps://www.twitch.tv/hiddentoken\nGrab the Stream role in \\#roles.\n-# stream twitch `hiddentoken` · 312345 · ended"
	if len(posts) != 1 || posts[0].Content != want || posts[0].Flags&4 == 0 {
		t.Errorf("edited to ended, embeds suppressed:\n%s (flags %d)\nwant:\n%s", posts[0].Content, posts[0].Flags, want)
	}
	if m := r.discord.lastMentions(); !strings.Contains(m, `"roles":[]`) {
		t.Errorf("the edit allows no mention: %s", m)
	}
	r.now = r.now.Add(time.Hour)
	if res := r.pass(t); len(res.Posted)+len(res.Resumed)+len(res.Ended) != 0 {
		t.Errorf("after the end: %+v", res)
	}
	m := r.metrics(t)
	for _, want := range []string{`gravel_bot_streams_posts_total{platform="twitch",result="posted"} 1`, `gravel_bot_streams_posts_total{platform="twitch",result="ended"} 1`,
		`gravel_bot_streams_polls_total{platform="kick",result="ok"}`, `gravel_bot_streams_live{platform="twitch"} 0`, "gravel_bot_streams_last_success_timestamp_seconds"} {
		if !strings.Contains(m, want) {
			t.Errorf("metrics lack %s", want)
		}
	}
}

func TestARestartFindsItsPosts(t *testing.T) {
	r := newRig(t)
	r.platforms.set(func(f *fakePlatforms) { f.twitch = twitchLive("312345", "Night one") })
	r.pass(t)
	// Someone else's message with a marker is not the bot's.
	r.discord.mu.Lock()
	r.discord.add(channel, "1300000000000000999", "-# stream twitch `hiddentoken` · 999")
	r.discord.mu.Unlock()

	r.restart(t)
	r.now = r.now.Add(time.Minute)
	if res := r.pass(t); len(res.Posted)+len(res.Resumed) != 0 {
		t.Fatalf("a stream announced before the restart isn't announced again: %+v", res)
	}
	// It ends while the bot is away: the next start marks the found post ended.
	r.platforms.set(func(f *fakePlatforms) { f.twitch = nil })
	r.restart(t)
	r.now = r.now.Add(time.Hour)
	if res := r.pass(t); !slices.Equal(res.Ended, []string{"twitch/hiddentoken"}) {
		t.Fatalf("ended after a restart: %+v", res)
	}
	if posts := r.discord.posts(); !strings.HasPrefix(posts[0].Content, "**Hidden Token** was live on Twitch") || !strings.Contains(posts[0].Content, "**Night one**") ||
		!strings.HasSuffix(posts[0].Content, "· 312345 · ended") || strings.Contains(posts[0].Content, "<@&") {
		t.Errorf("the found post, ended: %s", posts[0].Content)
	}
	// A restart after the end finds the post ended: nothing to do, and a stream within Rejoin of
	// its edit resumes it.
	r.restart(t)
	r.platforms.set(func(f *fakePlatforms) { f.twitch = twitchLive("312400", "Night one, again") })
	r.now = time.Date(2026, 10, 15, 3, 5, 0, 0, time.UTC) // five minutes after the fake's edit time
	if res := r.pass(t); !slices.Equal(res.Resumed, []string{"twitch/hiddentoken"}) {
		t.Fatalf("resumed from a found post: %+v", res)
	}
}

func TestADroppedStreamResumesItsPost(t *testing.T) {
	r := newRig(t)
	r.platforms.set(func(f *fakePlatforms) { f.twitch = twitchLive("1", "First") })
	r.pass(t)
	r.platforms.set(func(f *fakePlatforms) { f.twitch = nil })
	r.now = r.now.Add(time.Minute)
	r.pass(t)
	// Back within Rejoin as a new stream: the post goes live again, with no second ping.
	r.platforms.set(func(f *fakePlatforms) { f.twitch = twitchLive("2", "Second") })
	r.now = r.now.Add(Rejoin - time.Minute)
	if res := r.pass(t); !slices.Equal(res.Resumed, []string{"twitch/hiddentoken"}) || len(res.Posted) != 0 {
		t.Fatalf("resumed: %+v", res)
	}
	posts := r.discord.posts()
	if len(posts) != 1 || !strings.HasPrefix(posts[0].Content, "🔴 **Hidden Token** is live") || !strings.Contains(posts[0].Content, "**Second**") ||
		!strings.HasSuffix(posts[0].Content, "· 2") || strings.Contains(posts[0].Content, "<@&") {
		t.Errorf("resumed post: %+v", posts)
	}
	if m := r.discord.lastMentions(); !strings.Contains(m, `"roles":[]`) {
		t.Errorf("a resume pings no one: %s", m)
	}
	// A new stream between two passes, with no pass in between seeing it end: the same.
	r.platforms.set(func(f *fakePlatforms) { f.twitch = twitchLive("3", "Third") })
	r.now = r.now.Add(time.Minute)
	if res := r.pass(t); !slices.Equal(res.Resumed, []string{"twitch/hiddentoken"}) {
		t.Fatalf("a new stream id while live: %+v", res)
	}
	// Ended, and a new stream long after: a new post and a ping.
	r.platforms.set(func(f *fakePlatforms) { f.twitch = nil })
	r.now = r.now.Add(time.Minute)
	r.pass(t)
	r.platforms.set(func(f *fakePlatforms) { f.twitch = twitchLive("4", "Next week") })
	r.now = r.now.Add(Rejoin + time.Minute)
	if res := r.pass(t); !slices.Equal(res.Posted, []string{"twitch/hiddentoken"}) {
		t.Fatalf("a new stream later: %+v", res)
	}
	if posts := r.discord.posts(); len(posts) != 2 {
		t.Errorf("two posts: %d", len(posts))
	}
}

// Rejoin is the package's, read here for the tests' arithmetic.
const Rejoin = streams.Rejoin

func TestAStreamThatEndsBeforeThePost(t *testing.T) {
	r := newRig(t)
	r.discord.failPost = 1
	r.platforms.set(func(f *fakePlatforms) { f.twitch = twitchLive("1", "Short") })
	if res := r.pass(t); !slices.Equal(res.Failed, []string{"twitch/hiddentoken"}) {
		t.Fatalf("the post failed: %+v", res)
	}
	r.platforms.set(func(f *fakePlatforms) { f.twitch = nil })
	r.now = r.now.Add(time.Minute)
	if res := r.pass(t); len(res.Posted)+len(res.Ended)+len(res.Failed) != 0 {
		t.Fatalf("over before it was posted: %+v", res)
	}
	if posts := r.discord.posts(); len(posts) != 0 {
		t.Errorf("no post for a stream already over: %+v", posts)
	}
	if !strings.Contains(r.metrics(t), `gravel_bot_streams_posts_total{platform="twitch",result="error"} 1`) {
		t.Error("the failure is counted")
	}
}

func TestKickAndItsRateLimit(t *testing.T) {
	r := newRig(t)
	r.platforms.set(func(f *fakePlatforms) { f.kick = kickChannel(true, "2026-10-14T23:00:00Z") })
	res := r.pass(t)
	if !slices.Equal(res.Posted, []string{"kick/hidden-token"}) {
		t.Fatalf("kick posted: %+v", res)
	}
	if p := r.discord.posts()[0].Content; !strings.HasPrefix(p, "🔴 **hidden-token** is live on Kick\n**Season 02 night** · War Dogs\nhttps://kick.com/hidden-token\n") ||
		!strings.HasSuffix(p, "-# stream kick `hidden-token` · "+strconv.FormatInt(start.Unix(), 10)) {
		t.Errorf("kick post: %s", p)
	}
	r.platforms.set(func(f *fakePlatforms) { f.throttle["kick"] = "120" })
	r.now = r.now.Add(time.Minute)
	if res := r.pass(t); !slices.Equal(res.Unknown, []string{"kick/hidden-token"}) || len(res.Ended) != 0 {
		t.Fatalf("throttled: the post stays as it is: %+v", res)
	}
	reads := r.platforms.count(r.platforms.reads, "kick")
	r.platforms.set(func(f *fakePlatforms) { delete(f.throttle, "kick"); f.kick = kickChannel(false, "") })
	r.now = r.now.Add(time.Minute)
	if res := r.pass(t); !slices.Equal(res.Unknown, []string{"kick/hidden-token"}) || r.platforms.count(r.platforms.reads, "kick") != reads {
		t.Fatalf("Kick isn't asked inside its Retry-After: %+v", res)
	}
	if res := r.pass(t); len(res.Unknown) != 1 {
		t.Fatalf("still waiting: %+v", res)
	}
	r.now = r.now.Add(2 * time.Minute)
	if res := r.pass(t); !slices.Equal(res.Ended, []string{"kick/hidden-token"}) {
		t.Fatalf("asked again after Retry-After: %+v", res)
	}
	if !strings.Contains(r.metrics(t), `gravel_bot_streams_polls_total{platform="kick",result="throttled"} 1`) {
		t.Error("the 429 is counted")
	}
}

func TestKickWithoutAStartTime(t *testing.T) {
	r := newRig(t)
	r.platforms.set(func(f *fakePlatforms) { f.kick = kickChannel(true, "") })
	r.pass(t)
	if p := r.discord.posts()[0].Content; !strings.HasSuffix(p, "· "+streams.UnknownID) {
		t.Fatalf("unknown id: %s", p)
	}
	r.platforms.set(func(f *fakePlatforms) { f.kick = kickChannel(false, "") })
	r.now = r.now.Add(time.Minute)
	r.pass(t)
	// Long after, another stream with no start time is a new stream, not the old one resumed.
	r.platforms.set(func(f *fakePlatforms) { f.kick = kickChannel(true, "") })
	r.now = r.now.Add(24 * time.Hour)
	if res := r.pass(t); !slices.Equal(res.Posted, []string{"kick/hidden-token"}) {
		t.Fatalf("a new post: %+v", res)
	}
}

func TestTokens(t *testing.T) {
	r := newRig(t)
	r.pass(t)
	r.pass(t)
	if n := r.platforms.count(r.platforms.tokens, "twitch"); n != 1 {
		t.Errorf("one token while it lasts: %d", n)
	}
	// Twitch revokes the token: one more token, and the read goes through in the same pass.
	r.platforms.set(func(f *fakePlatforms) { f.refuse["twitch"] = 1; f.twitch = twitchLive("1", "x") })
	if res := r.pass(t); !slices.Equal(res.Posted, []string{"twitch/hiddentoken"}) {
		t.Fatalf("after a new token: %+v", res)
	}
	if n := r.platforms.count(r.platforms.tokens, "twitch"); n != 2 {
		t.Errorf("a refused token is replaced: %d", n)
	}
	// Refused twice: the application is wrong. Nothing ends, the problem is counted.
	r.platforms.set(func(f *fakePlatforms) { f.refuse["twitch"] = 2; f.twitch = nil })
	if res := r.pass(t); !slices.Equal(res.Unknown, []string{"twitch/hiddentoken"}) || len(res.Ended) != 0 {
		t.Fatalf("refused: %+v", res)
	}
	// A wrong secret: the token endpoint refuses.
	r.platforms.set(func(f *fakePlatforms) { f.badSecrets = true })
	r.restart(t)
	if res := r.pass(t); len(res.Unknown) != 2 {
		t.Fatalf("bad secrets: %+v", res)
	}
	if m := r.metrics(t); !strings.Contains(m, `gravel_bot_streams_polls_total{platform="twitch",result="unauthorized"} 1`) {
		t.Errorf("unauthorized counted:\n%s", m)
	}
}

func TestAPlatformWithoutAnApplication(t *testing.T) {
	r := newRig(t)
	r.only = []string{"twitch"}
	r.restart(t)
	r.platforms.set(func(f *fakePlatforms) {
		f.twitch = twitchLive("1", "x")
		f.kick = kickChannel(true, "2026-10-14T23:00:00Z")
	})
	res := r.pass(t)
	if !slices.Equal(res.Posted, []string{"twitch/hiddentoken"}) || !slices.Equal(res.Unknown, []string{"kick/hidden-token"}) {
		t.Fatalf("kick skipped: %+v", res)
	}
	if r.platforms.count(r.platforms.reads, "kick") != 0 {
		t.Error("kick isn't asked")
	}
}

func TestIdleLockedAndHubDown(t *testing.T) {
	r := newRig(t)
	r.hub.mu.Lock()
	r.hub.streams = nil
	r.hub.mu.Unlock()
	if res := r.pass(t); !res.Idle {
		t.Errorf("idle: %+v", res)
	}
	r.hub.mu.Lock()
	r.hub.streams = settings()
	r.hub.streams.Channel = locked
	r.hub.mu.Unlock()
	if res := r.pass(t); len(res.Unknown) != 2 || r.platforms.count(r.platforms.reads, "twitch") != 0 {
		t.Errorf("a channel the bot can't read: nothing asked, nothing posted: %+v", res)
	}
	r.hub.mu.Lock()
	r.hub.down = true
	r.hub.mu.Unlock()
	if _, err := r.mod.Pass(context.Background()); err == nil {
		t.Error("the hub down is an error")
	}
}

func TestMarkerAndRender(t *testing.T) {
	s := streams.Stream{Platform: "kick", Login: "hidden-token", ID: "1760482800", Title: "", Game: ""}
	p := streams.Render(streams.Post{Label: "HTG", Stream: s})
	want := "🔴 **HTG** is live on Kick\n**Live now**\nhttps://kick.com/hidden-token\n-# stream kick `hidden-token` · 1760482800"
	if p != want {
		t.Errorf("render:\n%s\nwant:\n%s", p, want)
	}
	platform, login, id, ended, ok := streams.Marker(p)
	if !ok || platform != "kick" || login != "hidden-token" || id != "1760482800" || ended {
		t.Errorf("marker: %s %s %s %t %t", platform, login, id, ended, ok)
	}
	for _, bad := range []string{"-# stream youtube `x` · 1", "-# stream twitch `X` · 1", "-# stream twitch `x` · abc", "stream twitch `x` · 1"} {
		if _, _, _, _, ok := streams.Marker(bad); ok {
			t.Errorf("not a marker: %q", bad)
		}
	}
	if _, _, _, ended, ok := streams.Marker("x\n-# stream twitch `abcd` · 1 · ended"); !ok || !ended {
		t.Error("an ended marker")
	}
}
