package news_test

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
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/gravel-project/gravel/discord/bot"
	"github.com/gravel-project/gravel/discord/hubclient"
	"github.com/gravel-project/gravel/discord/modules/news"
	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
	"github.com/gravel-project/gravel/gen/gravel/hub/v1/hubv1connect"
)

const (
	appID   = "1558123590786748516"
	channel = "1556656220243239072"
	other   = "1556656220243239073"
	locked  = "1300000000000000101"
	someone = "1300000000000000999"
	// webhook is the Worker's #cs-news webhook, which posted before the bot.
	webhook = "1556658583796449343"
)

// latest is the three latest announcements Steam sent on 2026-10-05, newest first, for a test to change.
func latest(t *testing.T) []map[string]any {
	t.Helper()
	b, err := os.ReadFile("testdata/steam-news.json")
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		AppNews struct {
			NewsItems []map[string]any `json:"newsitems"`
		} `json:"appnews"`
	}
	if err := json.Unmarshal(b, &body); err != nil {
		t.Fatal(err)
	}
	return body.AppNews.NewsItems
}

// fixture is a fixture's items as the Steam client reads them.
func fixture(t *testing.T, name string) []news.Item {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(b) }))
	t.Cleanup(srv.Close)
	items, err := news.NewSteam(srv.URL, srv.Client()).News(context.Background(), 730, []string{news.DefaultFeed})
	if err != nil {
		t.Fatal(err)
	}
	return items
}

func item(base map[string]any, changes map[string]any) map[string]any {
	out := maps.Clone(base)
	maps.Copy(out, changes)
	return out
}

func date(it map[string]any) time.Time { return time.Unix(int64(it["date"].(float64)), 0).UTC() }

// fakeSteam is Steam's news API: each app's items, or a status for every call.
type fakeSteam struct {
	srv        *httptest.Server
	mu         sync.Mutex
	items      map[string][]map[string]any // by appid
	status     int
	retryAfter string
	body       string // replaces the answer when set
	calls      []url.Values
}

func newFakeSteam(t *testing.T) *fakeSteam {
	t.Helper()
	f := &fakeSteam{items: map[string][]map[string]any{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ISteamNews/GetNewsForApp/v2/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		q := r.URL.Query()
		f.calls = append(f.calls, q)
		if f.status != 0 && f.status != http.StatusOK {
			if f.retryAfter != "" {
				w.Header().Set("Retry-After", f.retryAfter)
			}
			http.Error(w, "{}", f.status)
			return
		}
		if f.body != "" {
			_, _ = io.WriteString(w, f.body)
			return
		}
		list := f.items[q.Get("appid")]
		if list == nil {
			list = []map[string]any{}
		}
		writeJSON(w, map[string]any{"appnews": map[string]any{"appid": q.Get("appid"), "newsitems": list, "count": len(list)}})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected Steam call %s %s", r.Method, r.URL)
		http.NotFound(w, r)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeSteam) set(fn func(*fakeSteam)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *fakeSteam) takeCalls() []url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.calls
	f.calls = nil
	return c
}

type embed struct {
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	URL         string `json:"url,omitempty"`
	Timestamp   string `json:"timestamp,omitempty"`
	Color       int    `json:"color,omitempty"`
	Footer      *struct {
		Text string `json:"text"`
	} `json:"footer,omitempty"`
	Image *struct {
		URL string `json:"url"`
	} `json:"image,omitempty"`
}

type message struct {
	ID        string         `json:"id"`
	ChannelID string         `json:"channel_id"`
	Author    map[string]any `json:"author"`
	WebhookID string         `json:"webhook_id,omitempty"`
	Content   string         `json:"content"`
	Embeds    []embed        `json:"embeds"`
	Timestamp string         `json:"timestamp"`
}

// fakeDiscord is the channels' messages.
type fakeDiscord struct {
	srv      *httptest.Server
	mu       sync.Mutex
	next     int
	messages map[string][]*message
	calls    []string
	mentions []string
	failPost []int // the statuses the next posts answer with
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
		if len(f.failPost) > 0 {
			status := f.failPost[0]
			f.failPost = f.failPost[1:]
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, `{"message":"nope","code":0}`)
			return
		}
		var body struct {
			Content         string          `json:"content"`
			Embeds          []embed         `json:"embeds"`
			AllowedMentions json.RawMessage `json:"allowed_mentions"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mentions = append(f.mentions, string(body.AllowedMentions))
		writeJSON(w, f.add(r.PathValue("channel"), appID, "", body.Content, body.Embeds...))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected Discord call %s %s", r.Method, r.URL)
		http.Error(w, "unexpected", http.StatusNotFound)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeDiscord) add(ch, author, webhookID, content string, embeds ...embed) *message {
	m := &message{ID: strconv.Itoa(1400000000000000000 + f.next), ChannelID: ch, Author: map[string]any{"id": author, "username": "u"},
		WebhookID: webhookID, Content: content, Embeds: embeds, Timestamp: "2026-10-12T12:00:00Z"}
	if m.Embeds == nil {
		m.Embeds = []embed{}
	}
	f.next++
	f.messages[ch] = append(f.messages[ch], m)
	return m
}

// worker adds a post the Worker's webhook made for an item.
func (f *fakeDiscord) worker(ch string, it map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.add(ch, webhook, webhook, "", embed{Title: it["title"].(string), URL: news.Link(730, it["gid"].(string)), Timestamp: date(it).Format(time.RFC3339)})
}

func (f *fakeDiscord) takeCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.calls
	f.calls = nil
	return c
}

// posts are the bot's posts in ch.
func (f *fakeDiscord) posts(ch string) []*message {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*message
	for _, m := range f.messages[ch] {
		if m.Author["id"] == appID {
			out = append(out, m)
		}
	}
	return out
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

// fakeHub is the hub as the bot's app sees it: the settings' news feeds.
type fakeHub struct {
	hubv1connect.UnimplementedOrganizationServiceHandler
	srv *httptest.Server

	mu    sync.Mutex
	feeds []*hubv1.DiscordNewsFeed
	down  bool
}

func newFakeHub(t *testing.T, feeds []*hubv1.DiscordNewsFeed) *fakeHub {
	t.Helper()
	f := &fakeHub{feeds: feeds}
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
	d := &hubv1.DiscordSettings{GuildId: "1558121195340042350"}
	if len(f.feeds) > 0 {
		d.News = &hubv1.DiscordNews{Feeds: f.feeds}
	}
	return connect.NewResponse(&hubv1.GetOrganizationSettingsResponse{Settings: &hubv1.OrganizationSettings{Version: 1, Discord: d}}), nil
}

func (f *fakeHub) set(fn func(*fakeHub)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

type rig struct {
	hub     *fakeHub
	steam   *fakeSteam
	discord *fakeDiscord
	mod     *news.Module
	rt      *bot.Runtime
	log     *logs
	now     time.Time
}

func newRig(t *testing.T, feeds ...*hubv1.DiscordNewsFeed) *rig {
	t.Helper()
	if len(feeds) == 0 {
		feeds = []*hubv1.DiscordNewsFeed{{AppId: 730, Channel: channel, Label: "Counter-Strike 2", Color: 0xde9b35}}
	}
	r := &rig{hub: newFakeHub(t, feeds), steam: newFakeSteam(t), discord: newFakeDiscord(t), log: &logs{}}
	r.restart(t)
	return r
}

// restart is a new process: a new module and runtime against the same hub, Steam and channels.
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
	r.mod = news.New(hc, news.Config{Interval: time.Hour, Steam: news.NewSteam(r.steam.srv.URL, r.steam.srv.Client())})
	r.mod.SetClock(func() time.Time { return r.now })
	logger := slog.New(slog.NewTextHandler(r.log, nil))
	r.rt, err = bot.New(bot.Options{Config: bc, Logger: logger, RESTURL: r.discord.srv.URL + "/api"}, r.mod)
	if err != nil {
		t.Fatal(err)
	}
	rt := r.rt
	t.Cleanup(func() { rt.Close(context.Background()) })
}

func (r *rig) pass() news.Result {
	res, err := r.mod.Pass(context.Background())
	if err != nil {
		panic("pass: " + err.Error()) // every test's hub is up unless it says otherwise
	}
	return res
}

// warm is a first pass a week before r.now with no news: the bot was running before the items
// Steam lists next, so they are all new.
func (r *rig) warm(t *testing.T) {
	t.Helper()
	at := r.now
	r.now = at.Add(-7 * 24 * time.Hour)
	r.steam.set(func(f *fakeSteam) { f.items = map[string][]map[string]any{} })
	if res := r.pass(); len(res.Posted)+len(res.Failed)+len(res.Waiting) != 0 {
		t.Fatalf("warm: %+v", res)
	}
	r.now = at
}

func (r *rig) serve(app string, items ...map[string]any) {
	r.steam.set(func(f *fakeSteam) { f.items[app] = items })
}

func (r *rig) metrics(t *testing.T) string {
	t.Helper()
	rec := httptest.NewRecorder()
	r.rt.InternalHandler().ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/metrics", nil))
	return rec.Body.String()
}

func gids(items ...map[string]any) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it["gid"].(string)
	}
	return out
}

func TestAFreshChannelGetsNoBacklog(t *testing.T) {
	r := newRig(t)
	items := latest(t)
	r.now = date(items[0]).Add(3 * 24 * time.Hour)
	r.serve("730", items...)
	if res := r.pass(); len(res.Posted) != 0 || len(res.Failed) != 0 {
		t.Fatalf("the first pass posts no backlog: %+v", res)
	}
	if c := r.discord.takeCalls(); !slices.Equal(c, []string{"GET " + channel}) {
		t.Errorf("one channel read and no post: %v", c)
	}
	// A restart within a day of the newest: only that one, the rest are older than a day.
	r.restart(t)
	r.now = date(items[0]).Add(2 * time.Hour)
	if res := r.pass(); !slices.Equal(res.Posted, gids(items[0])) {
		t.Fatalf("only the last day's: %+v", res)
	}
	if res := r.pass(); len(res.Posted) != 0 {
		t.Errorf("then nothing: %+v", res)
	}
	q := r.steam.takeCalls()
	if len(q) != 3 || q[0].Get("appid") != "730" || q[0].Get("count") != "10" || q[0].Get("feeds") != "steam_community_announcements" {
		t.Errorf("Steam asked: %v", q)
	}
}

func TestANewItemIsPostedOnce(t *testing.T) {
	r := newRig(t)
	items := latest(t)
	r.now = date(items[0]).Add(3 * 24 * time.Hour)
	// The Worker posted the two older ones through its webhook.
	r.discord.worker(channel, items[2])
	r.discord.worker(channel, items[1])
	r.serve("730", items...)
	if res := r.pass(); !slices.Equal(res.Posted, gids(items[0])) {
		t.Fatalf("the new one: %+v", res)
	}
	posts := r.discord.posts(channel)
	if len(posts) != 1 || len(posts[0].Embeds) != 1 || posts[0].Embeds[0].URL != "https://store.steampowered.com/news/app/730/view/"+items[0]["gid"].(string) {
		t.Fatalf("one embed linking the item: %+v", posts)
	}
	e := posts[0].Embeds[0]
	if e.Color != 0xde9b35 || e.Footer == nil || e.Footer.Text != "Counter-Strike 2 · Steam announcement" || e.Timestamp != date(items[0]).Format(time.RFC3339) {
		t.Errorf("the Worker's look: %+v", e)
	}
	if posts[0].Content != "" {
		t.Errorf("no content: %q", posts[0].Content)
	}
	if m := r.discord.mentions[0]; !strings.Contains(m, `"parse":[]`) || !strings.Contains(m, `"roles":[]`) || !strings.Contains(m, `"users":[]`) {
		t.Errorf("no mention allowed: %s", m)
	}
	if res := r.pass(); len(res.Posted) != 0 {
		t.Errorf("not again: %+v", res)
	}
	r.restart(t)
	if res := r.pass(); len(res.Posted) != 0 {
		t.Errorf("not after a restart: %+v", res)
	}
	if n := len(r.discord.posts(channel)); n != 1 {
		t.Errorf("posts: %d", n)
	}
}

// The Worker seeded what Steam listed when it started and posted only what came after, so the
// channel lacks older items: taking over must not post them.
func TestTakingOverPostsNothingOlderThanTheChannel(t *testing.T) {
	r := newRig(t)
	items := latest(t)
	r.now = date(items[0]).Add(time.Hour)
	r.discord.worker(channel, items[1])
	r.serve("730", items...)
	if res := r.pass(); !slices.Equal(res.Posted, gids(items[0])) {
		t.Fatalf("only what is newer than the channel's newest: %+v", res)
	}
	// Nor later, while the process runs: the first look set where the feed starts.
	if res := r.pass(); len(res.Posted) != 0 {
		t.Errorf("the next pass: %+v", res)
	}
	// After a restart, the newest in the channel is the bot's own post.
	r.restart(t)
	if res := r.pass(); len(res.Posted) != 0 {
		t.Errorf("after a restart: %+v", res)
	}
	newer := item(items[0], map[string]any{"gid": "1845383656390000", "date": float64(date(items[0]).Unix() + 60)})
	r.serve("730", newer, items[0], items[1], items[2])
	if res := r.pass(); !slices.Equal(res.Posted, []string{"1845383656390000"}) {
		t.Errorf("a new one: %+v", res)
	}
}

func TestSeveralGoOutOldestFirst(t *testing.T) {
	r := newRig(t)
	items := latest(t)
	r.now = date(items[0]).Add(time.Hour)
	r.warm(t)
	r.serve("730", items...)
	if res := r.pass(); !slices.Equal(res.Posted, gids(items[2], items[1], items[0])) {
		t.Fatalf("oldest first: %+v", res)
	}
	var stamps []string
	for _, p := range r.discord.posts(channel) {
		stamps = append(stamps, p.Embeds[0].Timestamp)
	}
	want := []string{date(items[2]).Format(time.RFC3339), date(items[1]).Format(time.RFC3339), date(items[0]).Format(time.RFC3339)}
	if !slices.Equal(stamps, want) {
		t.Errorf("timestamps %v, want %v", stamps, want)
	}
}

func TestAtMostFiveAPass(t *testing.T) {
	r := newRig(t)
	base := latest(t)[0]
	r.now = date(base).Add(time.Hour)
	r.warm(t)
	var many []map[string]any
	for n := range 8 {
		many = append(many, item(base, map[string]any{"gid": strconv.Itoa(100 + n), "date": float64(date(base).Unix() - 1000 + int64(n))}))
	}
	r.serve("730", many...)
	if res := r.pass(); !slices.Equal(res.Posted, []string{"100", "101", "102", "103", "104"}) {
		t.Fatalf("five: %+v", res)
	}
	if res := r.pass(); !slices.Equal(res.Posted, []string{"105", "106", "107"}) {
		t.Errorf("the rest next pass: %+v", res)
	}
}

func TestAFailedPostIsRetriedNextPass(t *testing.T) {
	r := newRig(t)
	items := latest(t)
	r.now = date(items[0]).Add(time.Hour)
	r.warm(t)
	// The oldest goes out; then the next post fails, and the newest waits behind it.
	r.serve("730", items[2])
	if res := r.pass(); !slices.Equal(res.Posted, gids(items[2])) {
		t.Fatalf("the first: %+v", res)
	}
	r.serve("730", items...)
	r.discord.set(func(f *fakeDiscord) { f.failPost = []int{http.StatusInternalServerError} })
	if res := r.pass(); len(res.Posted) != 0 || !slices.Equal(res.Failed, []string{"730/" + channel}) {
		t.Fatalf("a failed post: %+v", res)
	}
	if res := r.pass(); !slices.Equal(res.Posted, gids(items[1], items[0])) {
		t.Fatalf("retried, in order: %+v", res)
	}
	if n := len(r.discord.posts(channel)); n != 3 {
		t.Errorf("posts: %d", n)
	}
	m := r.metrics(t)
	for _, want := range []string{`gravel_bot_news_posts_total{app="730",result="posted"} 3`, `gravel_bot_news_posts_total{app="730",result="failed"} 1`,
		`gravel_bot_news_last_success_timestamp_seconds{app="730"}`} {
		if !strings.Contains(m, want) {
			t.Errorf("metrics lack %s", want)
		}
	}
	if n := r.log.count("news: a post failed, will retry"); n != 1 {
		t.Errorf("the failure logged once: %d", n)
	}
}

func (f *fakeDiscord) set(fn func(*fakeDiscord)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func TestASteamErrorPostsNothing(t *testing.T) {
	r := newRig(t)
	items := latest(t)
	r.now = date(items[0]).Add(time.Hour)
	r.serve("730", items...)
	r.steam.set(func(f *fakeSteam) { f.status = http.StatusServiceUnavailable })
	if res := r.pass(); len(res.Posted) != 0 || !slices.Equal(res.Failed, []string{"730/" + channel}) {
		t.Fatalf("a 503: %+v", res)
	}
	r.steam.set(func(f *fakeSteam) { f.status, f.body = 0, `{"response":{}}` })
	if res := r.pass(); len(res.Posted) != 0 || len(res.Failed) != 1 {
		t.Fatalf("no newsitems: %+v", res)
	}
	r.steam.set(func(f *fakeSteam) { f.body = `{"appnews":` })
	if res := r.pass(); len(res.Failed) != 1 {
		t.Fatalf("a cut answer: %+v", res)
	}
	if c := r.discord.takeCalls(); len(c) != 0 {
		t.Errorf("no Discord call: %v", c)
	}
	if n := r.log.count("news: Steam failed, will retry"); n != 1 {
		t.Errorf("logged once while it lasts: %d", n)
	}
	if strings.Contains(r.metrics(t), "gravel_bot_news_last_success_timestamp_seconds{") {
		t.Error("no success yet")
	}
	// Steam back: the channel was never read, so this is still the first look.
	r.steam.set(func(f *fakeSteam) { f.body = "" })
	if res := r.pass(); !slices.Equal(res.Posted, gids(items[0])) {
		t.Errorf("back: %+v", res)
	}
}

func TestOtherAppsAndFeedsAreIgnored(t *testing.T) {
	r := newRig(t)
	items := latest(t)
	r.now = date(items[0]).Add(time.Hour)
	r.warm(t)
	r.serve("730", item(items[0], map[string]any{"appid": float64(570)}), item(items[1], map[string]any{"feedname": "pcgamer"}),
		item(items[2], map[string]any{"gid": float64(1844751498232642)}), item(items[2], map[string]any{"gid": "not-a-gid"}))
	if res := r.pass(); len(res.Posted) != 0 || len(res.Failed) != 0 {
		t.Errorf("nothing: %+v", res)
	}
}

func TestDedupeCountsAnyAuthorButOnlyThisApp(t *testing.T) {
	r := newRig(t)
	items := latest(t)
	r.now = date(items[0]).Add(time.Hour)
	r.warm(t)
	r.discord.set(func(f *fakeDiscord) {
		// A member pasted the newest; Discord unfurled it.
		f.add(channel, someone, "", "patch!", embed{URL: news.Link(730, items[0]["gid"].(string)) + "?l=english"})
		// Another game's announcement with the middle one's gid is not this one.
		f.add(channel, someone, "", "", embed{URL: news.Link(570, items[1]["gid"].(string))})
	})
	r.serve("730", items...)
	if res := r.pass(); !slices.Equal(res.Posted, gids(items[2], items[1])) {
		t.Errorf("the pasted one counts, the other app's doesn't: %+v", res)
	}
	got, newest := news.Posted(nil, 730)
	if len(got) != 0 || !newest.IsZero() {
		t.Errorf("an empty channel: %v %v", got, newest)
	}
}

func TestSteamThrottlingIsWaitedOut(t *testing.T) {
	gta := &hubv1.DiscordNewsFeed{AppId: 271590, Channel: other, Label: "Grand Theft Auto V"}
	r := newRig(t, &hubv1.DiscordNewsFeed{AppId: 730, Channel: channel, Label: "Counter-Strike 2"}, gta)
	items := latest(t)
	r.now = date(items[0]).Add(time.Hour)
	r.serve("730", items[0])
	r.steam.set(func(f *fakeSteam) { f.status, f.retryAfter = http.StatusTooManyRequests, "120" })
	if res := r.pass(); !slices.Equal(res.Waiting, []string{"730/" + channel, "271590/" + other}) {
		t.Fatalf("both wait: %+v", res)
	}
	if q := r.steam.takeCalls(); len(q) != 1 {
		t.Errorf("Steam asked once, then left alone: %d", len(q))
	}
	r.steam.set(func(f *fakeSteam) { f.status = 0 })
	r.now = r.now.Add(time.Minute)
	if res := r.pass(); len(res.Waiting) != 2 || len(r.steam.takeCalls()) != 0 {
		t.Fatalf("within the Retry-After: %+v", res)
	}
	r.now = r.now.Add(61 * time.Second)
	if res := r.pass(); !slices.Equal(res.Posted, gids(items[0])) || len(res.Waiting) != 0 {
		t.Fatalf("after it: %+v", res)
	}
	if n := r.log.count("news: Steam is rate-limiting the bot; waiting"); n != 1 {
		t.Errorf("logged once: %d", n)
	}
}

func TestAForbiddenChannelIsLoggedOnce(t *testing.T) {
	r := newRig(t, &hubv1.DiscordNewsFeed{AppId: 730, Channel: locked, Label: "Counter-Strike 2"})
	items := latest(t)
	r.now = date(items[0]).Add(time.Hour)
	r.serve("730", items...)
	for range 3 {
		if res := r.pass(); !slices.Equal(res.Failed, []string{"730/" + locked}) {
			t.Fatalf("forbidden: %+v", res)
		}
	}
	if n := r.log.count("news: the bot may not use the channel"); n != 1 {
		t.Errorf("logged once: %d", n)
	}
	if strings.Contains(r.metrics(t), "gravel_bot_news_last_success_timestamp_seconds{") {
		t.Error("no success")
	}
}

func TestTwoFeedsPostToTheirOwnChannels(t *testing.T) {
	gta := &hubv1.DiscordNewsFeed{AppId: 271590, Channel: other, Label: "Grand Theft Auto V", Feeds: []string{news.DefaultFeed, "steam_updates"}}
	r := newRig(t, &hubv1.DiscordNewsFeed{AppId: 730, Channel: channel, Label: "Counter-Strike 2"}, gta)
	items := latest(t)
	r.now = date(items[0]).Add(time.Hour)
	r.warm(t)
	r.serve("730", items[0])
	r.serve("271590", item(items[1], map[string]any{"appid": float64(271590), "gid": "5000000000000001", "title": "GTA Online"}),
		item(items[2], map[string]any{"appid": float64(271590), "gid": "5000000000000002", "feedname": "steam_updates"}))
	res := r.pass()
	if !slices.Equal(res.Posted, []string{items[0]["gid"].(string), "5000000000000002", "5000000000000001"}) {
		t.Fatalf("each feed: %+v", res)
	}
	cs, gtaPosts := r.discord.posts(channel), r.discord.posts(other)
	if len(cs) != 1 || cs[0].Embeds[0].Footer.Text != "Counter-Strike 2 · Steam announcement" || cs[0].Embeds[0].Color != news.DefaultColor {
		t.Errorf("CS2's channel: %+v", cs)
	}
	if len(gtaPosts) != 2 || gtaPosts[1].Embeds[0].URL != "https://store.steampowered.com/news/app/271590/view/5000000000000001" ||
		gtaPosts[0].Embeds[0].Footer.Text != "Grand Theft Auto V · Steam announcement" {
		t.Errorf("GTA's channel: %+v", gtaPosts)
	}
	var feeds []string
	for _, q := range r.steam.takeCalls() {
		feeds = append(feeds, q.Get("appid")+":"+q.Get("feeds"))
	}
	if !slices.Contains(feeds, "271590:steam_community_announcements,steam_updates") {
		t.Errorf("GTA's feeds asked for: %v", feeds)
	}
	m := r.metrics(t)
	for _, want := range []string{`gravel_bot_news_posts_total{app="730",result="posted"} 1`, `gravel_bot_news_posts_total{app="271590",result="posted"} 2`,
		`gravel_bot_news_posts_total{app="271590",result="failed"} 0`, `gravel_bot_news_last_success_timestamp_seconds{app="271590"}`,
		`gravel_bot_news_last_success_timestamp_seconds{app="730"}`} {
		if !strings.Contains(m, want) {
			t.Errorf("metrics lack %s", want)
		}
	}
}

func TestSteamClient(t *testing.T) {
	items := fixture(t, "steam-news.json")
	if len(items) != 3 || items[0].GID != "1845383656387875" || items[0].AppID != 730 || items[0].FeedName != news.DefaultFeed ||
		!items[0].Date.Equal(time.Unix(1790979853, 0)) || !strings.HasPrefix(items[0].Contents, `[p]\[ MISC ][/p]`) {
		t.Errorf("items: %+v", items[0])
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", time.Now().Add(3*time.Minute).UTC().Format(http.TimeFormat))
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	_, err := news.NewSteam(srv.URL+"/", srv.Client()).News(context.Background(), 730, []string{news.DefaultFeed})
	var te *news.ThrottledError
	if !errors.As(err, &te) || te.RetryAfter < 2*time.Minute || te.RetryAfter > 3*time.Minute {
		t.Errorf("an HTTP-date Retry-After: %v", err)
	}
}

func TestRunPassesAtStart(t *testing.T) {
	r := newRig(t)
	items := latest(t)
	r.now = date(items[0]).Add(time.Hour)
	r.serve("730", items...)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.mod.Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for len(r.discord.posts(channel)) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n := len(r.discord.posts(channel)); n != 1 {
		t.Errorf("the first pass runs at start: %d posts", n)
	}
	if n := r.log.count("news: posted"); n != 1 {
		t.Errorf("logged: %d", n)
	}
}

func TestIdleWithoutAFeedAndTheHubDown(t *testing.T) {
	r := newRig(t)
	r.hub.set(func(f *fakeHub) { f.feeds = nil })
	for range 3 {
		if res := r.pass(); !res.Idle {
			t.Fatalf("idle: %+v", res)
		}
	}
	if n := r.log.count("news idle: the organization settings list no feed"); n != 1 {
		t.Errorf("idle logged once: %d", n)
	}
	if c := r.discord.takeCalls(); len(c) != 0 || len(r.steam.takeCalls()) != 0 {
		t.Errorf("no call while idle: %v", c)
	}
	r.hub.set(func(f *fakeHub) { f.down = true })
	if _, err := r.mod.Pass(context.Background()); err == nil || !strings.Contains(err.Error(), "read the settings") {
		t.Errorf("the hub down: %v", err)
	}
}

func TestSettingsChangesApplyAtTheNextPass(t *testing.T) {
	r := newRig(t)
	items := latest(t)
	r.now = date(items[0]).Add(time.Hour)
	r.warm(t)
	// The feed leaves the settings and comes back: it starts afresh, a day back from now.
	r.hub.set(func(f *fakeHub) { f.feeds = nil })
	if res := r.pass(); !res.Idle {
		t.Fatalf("removed: %+v", res)
	}
	r.hub.set(func(f *fakeHub) {
		f.feeds = []*hubv1.DiscordNewsFeed{{AppId: 730, Channel: channel, Label: "CS2", Color: 0x123456}}
	})
	r.serve("730", items...)
	if res := r.pass(); !slices.Equal(res.Posted, gids(items[0])) {
		t.Fatalf("added again: only the last day's: %+v", res)
	}
	e := r.discord.posts(channel)[0].Embeds[0]
	if e.Footer.Text != "CS2 · Steam announcement" || e.Color != 0x123456 {
		t.Errorf("the settings' label and color: %+v", e)
	}
	// A second channel for the same game is a feed of its own, with its own start.
	r.hub.set(func(f *fakeHub) {
		f.feeds = append(f.feeds, &hubv1.DiscordNewsFeed{AppId: 730, Channel: other, Label: "CS2"})
	})
	if res := r.pass(); !slices.Equal(res.Posted, gids(items[0])) {
		t.Errorf("the new channel gets the last day's: %+v", res)
	}
	if n := len(r.discord.posts(other)); n != 1 {
		t.Errorf("posts in the new channel: %d", n)
	}
}

func TestFeedsDefaults(t *testing.T) {
	got := news.Feeds(&hubv1.DiscordNews{Feeds: []*hubv1.DiscordNewsFeed{
		{AppId: 730, Channel: channel, Label: "CS2"},
		{AppId: 730, Channel: "#cs-news", Label: "bad channel"},
		{AppId: 570, Channel: other, Label: "Dota 2", Feeds: []string{"steam_updates"}, Color: 0xff0000},
	}})
	if len(got) != 2 || got[0].Feeds[0] != news.DefaultFeed || got[0].Color != news.DefaultColor || got[0].Channel.String() != channel ||
		got[1].Feeds[0] != "steam_updates" || got[1].Color != 0xff0000 {
		t.Errorf("feeds: %+v", got)
	}
	if news.Feeds(nil) != nil {
		t.Error("no settings, no feeds")
	}
}
