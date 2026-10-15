package events_test

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
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/gravel-project/gravel/discord/bot"
	"github.com/gravel-project/gravel/discord/modules/events"
	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
)

const (
	appID   = "1558123590786748516"
	channel = "1558200000000000002"
)

var ranks = map[string]int{"info": 0, "notice": 1, "warning": 2, "critical": 3}

// fakeHub is the events log, the settings' feed, the servers and their hosts in memory; it filters
// as ListEvents does and records every action with who it was for.
type fakeHub struct {
	mu    sync.Mutex
	feed  *hubv1.DiscordEvents
	log   []*hubv1.Event // ascending by id
	hosts map[string]bool
	acted []string
	err   error // what Act answers
	reads int
}

func (f *fakeHub) Feed(context.Context) (*hubv1.DiscordEvents, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.feed, nil
}

func (f *fakeHub) Events(_ context.Context, after int64, minSeverity string, kinds []string, limit int) ([]*hubv1.Event, int64, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	var head int64
	if n := len(f.log); n > 0 {
		head = f.log[n-1].GetId()
	}
	if after == 0 {
		if head == 0 {
			return nil, 0, 0, nil
		}
		return []*hubv1.Event{f.log[len(f.log)-1]}, head, head, nil
	}
	floor := ranks[minSeverity]
	if minSeverity == "" {
		floor = ranks["warning"]
	}
	var out []*hubv1.Event
	next := after
	for _, e := range f.log {
		if e.GetId() <= after || ranks[e.GetSeverity()] < floor || len(out) == limit {
			continue
		}
		if len(kinds) > 0 && !slices.ContainsFunc(kinds, func(k string) bool { return e.GetKind() == k || strings.HasPrefix(e.GetKind(), k+".") }) {
			continue
		}
		out = append(out, e)
		next = e.GetId()
	}
	return out, next, head, nil
}

func (f *fakeHub) Servers(context.Context) (map[string]string, error) {
	return map[string]string{"htg-cs2-1": "HTG CS2 #1", "wd-1": "War Dogs #1"}, nil
}

func (f *fakeHub) HasHost(_ context.Context, id string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hosts[id], nil
}

func (f *fakeHub) Act(_ context.Context, action, server string, by *hubv1.Actor) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.acted = append(f.acted, action+" "+server+" for "+by.GetProvider()+":"+by.GetSubject())
	return nil
}

// add appends an event of kind and severity about a server.
func (f *fakeHub) add(kind, severity, server, summary string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log = append(f.log, &hubv1.Event{Id: int64(len(f.log) + 1), Kind: kind, Severity: severity, ServerId: server, Summary: summary,
		At: timestamppb.New(time.Date(2026, 10, 15, 20, 0, len(f.log), 0, time.UTC))})
}

func (f *fakeHub) actions() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.acted)
}

// post is a message the bot posted: its content and the custom ids of its buttons.
type post struct {
	content string
	buttons []string
}

// fakeDiscord is the events channel and the bot's edits to its interaction responses; forbid
// refuses the channel with 403.
type fakeDiscord struct {
	srv    *httptest.Server
	mu     sync.Mutex
	posts  []post
	edits  []map[string]any
	forbid bool
}

func newFakeDiscord(t *testing.T) *fakeDiscord {
	t.Helper()
	f := &fakeDiscord{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPatch && strings.Contains(r.URL.Path, "/webhooks/"+appID+"/"):
			var edit map[string]any
			_ = json.Unmarshal(body, &edit)
			f.edits = append(f.edits, edit)
			_, _ = io.WriteString(w, `{"id":"5","channel_id":"7","content":"","author":{"id":"`+appID+`","username":"bot","discriminator":"0"},"timestamp":"2026-10-10T20:00:00Z"}`)
		case f.forbid:
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"code":50001,"message":"Missing Access"}`)
		case !strings.HasSuffix(r.URL.Path, "/channels/"+channel+"/messages"):
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		case r.Method == http.MethodGet:
			var msgs []map[string]any // newest first, as Discord answers
			for i := len(f.posts) - 1; i >= 0; i-- {
				msgs = append(msgs, map[string]any{"id": strconv.Itoa(1000 + i), "channel_id": channel, "content": f.posts[i].content,
					"author": map[string]any{"id": appID, "username": "bot", "discriminator": "0"}, "timestamp": "2026-10-10T20:00:00Z"})
			}
			msgs = append(msgs, map[string]any{"id": "999", "channel_id": channel, "content": "-# event 999999 (someone else's)",
				"author": map[string]any{"id": "42", "username": "jo", "discriminator": "0"}, "timestamp": "2026-10-10T20:00:00Z"})
			_ = json.NewEncoder(w).Encode(msgs)
		case r.Method == http.MethodPost:
			var m struct {
				Content         string         `json:"content"`
				AllowedMentions map[string]any `json:"allowed_mentions"`
				Components      []struct {
					Components []struct {
						CustomID string `json:"custom_id"`
					} `json:"components"`
				} `json:"components"`
			}
			_ = json.Unmarshal(body, &m)
			if m.AllowedMentions == nil {
				http.Error(w, "a post without allowed_mentions", http.StatusBadRequest)
				return
			}
			p := post{content: m.Content}
			for _, row := range m.Components {
				for _, c := range row.Components {
					p.buttons = append(p.buttons, c.CustomID)
				}
			}
			f.posts = append(f.posts, p)
			_, _ = io.WriteString(w, `{"id":"`+strconv.Itoa(1000+len(f.posts)-1)+`","channel_id":"`+channel+`","content":"","author":{"id":"`+appID+`","username":"bot","discriminator":"0"},"timestamp":"2026-10-10T20:00:00Z"}`)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeDiscord) all() []post {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.posts)
}

// edit waits for the n-th edit of an interaction response (1-based).
func (f *fakeDiscord) edit(t *testing.T, n int) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		f.mu.Lock()
		if len(f.edits) >= n {
			e := f.edits[n-1]
			f.mu.Unlock()
			return e
		}
		f.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatalf("no edit %d", n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

type rig struct {
	t       *testing.T
	priv    ed25519.PrivateKey
	hub     *fakeHub
	discord *fakeDiscord
	mod     *events.Module
	handler http.Handler
}

func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{t: t, discord: newFakeDiscord(t), hub: &fakeHub{feed: &hubv1.DiscordEvents{Channel: channel}, hosts: map[string]bool{"htg-cs2-1": true}}}
	r.restart()
	return r
}

// restart builds the module and the runtime afresh, as a bot restart does.
func (r *rig) restart() {
	r.t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		r.t.Fatal(err)
	}
	cfg := bot.Default()
	cfg.Discord.ApplicationID, cfg.Discord.PublicKey, cfg.Discord.Gateway = appID, hex.EncodeToString(pub), false
	cfg.Discord.Token = base64.RawStdEncoding.EncodeToString([]byte(appID)) + ".Xa1b2c.not-a-real-token-signature"
	cfg.Hub.URL, cfg.Hub.ClientID, cfg.Hub.ClientSecret = "http://hub.invalid", "gravel_bot", "s3cret"
	cfg.Server.Listen, cfg.Server.InternalListen = "127.0.0.1:0", "127.0.0.1:0"
	r.mod = events.NewWithHub(r.hub, events.Config{Interval: time.Hour, PublicURL: "https://hub.example.org/"})
	rt, err := bot.New(bot.Options{Config: cfg, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Version: "test", RESTURL: r.discord.srv.URL + "/api/"}, r.mod)
	if err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() { rt.Close(context.Background()) })
	r.priv, r.handler = priv, rt.Handler()
}

func (r *rig) pass() events.Result {
	r.t.Helper()
	res, err := r.mod.Pass(context.Background())
	if err != nil {
		r.t.Fatal(err)
	}
	return res
}

// click sends a signed button click by user and returns Discord's view of the HTTP answer.
func (r *rig) click(user, customID string) map[string]any {
	r.t.Helper()
	body := `{"type":3,"id":"901","application_id":"` + appID + `","token":"ctoken","version":1,"guild_id":"9","channel_id":"` + channel + `",` +
		`"member":{"user":{"id":"` + user + `","username":"u` + user + `","discriminator":"0","avatar":null},"roles":[],"joined_at":"2026-10-09T12:00:00Z","permissions":"1099511627775"},` +
		`"message":{"id":"5","channel_id":"` + channel + `","content":"","author":{"id":"` + appID + `","username":"bot","discriminator":"0"},"timestamp":"2026-10-10T20:00:00Z"},` +
		`"data":{"custom_id":"` + customID + `","component_type":2}}`
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/interactions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Signature-Timestamp", ts)
	req.Header.Set("X-Signature-Ed25519", hex.EncodeToString(ed25519.Sign(r.priv, []byte(ts+body))))
	rec := httptest.NewRecorder()
	r.handler.ServeHTTP(rec, req)
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		r.t.Fatalf("answer %d: %s", rec.Code, rec.Body.String())
	}
	return out
}

// The feed starts at the log's head, posts each event the filter takes once and in order, puts
// Restart and Park on a cloud-VM server's trouble only, and after a restart carries on from its
// own last post.
func TestFeed(t *testing.T) {
	r := newRig(t)
	r.hub.add("server.unreachable", "warning", "wd-1", "War Dogs #1 stopped answering its polls")
	r.hub.add("backup.failed", "critical", "", "the last base backup failed")
	if res := r.pass(); res.Posted != 0 || len(r.discord.all()) != 0 {
		t.Fatalf("turning it on posted history: %+v %v", res, r.discord.all())
	}

	r.hub.add("server.unreachable", "warning", "htg-cs2-1", "HTG CS2 #1 stopped answering its polls")
	r.hub.add("server.recovered", "info", "wd-1", "War Dogs #1 is answering again") // under warning
	r.hub.add("server.unreachable", "warning", "wd-1", "War Dogs #1 stopped answering its polls")
	r.hub.add("host.failing", "warning", "htg-cs2-1", "a step on the host keeps failing: quota")
	if res := r.pass(); res.Posted != 3 {
		t.Fatalf("posted %+v", res)
	}
	got := r.discord.all()
	if len(got) != 3 || !strings.Contains(got[0].content, "-# event 3 · server.unreachable") || !strings.Contains(got[1].content, "-# event 5 ") || !strings.Contains(got[2].content, "-# event 6 ") {
		t.Fatalf("posts = %+v", got)
	}
	if want := []string{"/events/ask/restart/htg-cs2-1", "/events/ask/park/htg-cs2-1"}; !slices.Equal(got[0].buttons, want) || !slices.Equal(got[2].buttons, want) {
		t.Errorf("a cloud-VM server's trouble has %v and %v", got[0].buttons, got[2].buttons)
	}
	if len(got[1].buttons) != 0 {
		t.Errorf("an external server's trouble has buttons: %v", got[1].buttons)
	}
	if c := got[0].content; !strings.HasPrefix(c, "🟠 **Warning** · HTG CS2 #1 stopped answering its polls · [events](<https://hub.example.org/admin/events?server=htg-cs2-1>)") {
		t.Errorf("line = %q", c)
	}
	if res := r.pass(); res.Posted != 0 {
		t.Errorf("a second pass posted %d", res.Posted)
	}

	// A restart finds its place from its own posts, not someone else's marker.
	r.restart()
	r.hub.add("backup.failed", "critical", "", "the last base backup failed")
	if res := r.pass(); res.Posted != 1 || !strings.HasPrefix(r.discord.all()[3].content, "🔴 **Critical** · the last base backup failed") {
		t.Errorf("after a restart: %+v %v", res, r.discord.all())
	}

	// The settings' filter: only the kinds named, from notice up.
	r.hub.mu.Lock()
	r.hub.feed = &hubv1.DiscordEvents{Channel: channel, MinSeverity: "notice", Kinds: []string{"backup"}}
	r.hub.mu.Unlock()
	r.hub.add("server.unreachable", "warning", "wd-1", "War Dogs #1 stopped answering its polls")
	r.hub.add("backup.stale", "warning", "", "no base backup has succeeded for over 26 hours")
	if res := r.pass(); res.Posted != 1 || !strings.Contains(r.discord.all()[4].content, "backup.stale") {
		t.Errorf("filtered: %+v %v", res, r.discord.all())
	}
}

// No channel: idle, nothing read past the settings. A channel the bot may not post in: the pass
// says so and the event waits.
func TestIdleAndForbidden(t *testing.T) {
	r := newRig(t)
	r.hub.feed = nil
	if res := r.pass(); !res.Idle || r.hub.reads != 0 {
		t.Errorf("idle: %+v, %d reads", res, r.hub.reads)
	}
	r.hub.feed = &hubv1.DiscordEvents{Channel: channel}
	r.pass()
	r.hub.add("backup.failed", "critical", "", "the last base backup failed")
	r.discord.forbid = true
	if res := r.pass(); !res.Forbidden || res.Posted != 0 {
		t.Errorf("forbidden: %+v", res)
	}
	r.discord.forbid = false
	if res := r.pass(); res.Posted != 1 {
		t.Errorf("after access came back: %+v", res)
	}
}

// Restart asks the clicker privately, then goes to the hub on their behalf; Park the same; the
// hub's refusal of a member who isn't an operator is said plainly; Cancel does nothing.
func TestButtons(t *testing.T) {
	r := newRig(t)
	ask := r.click("42", "/events/ask/restart/htg-cs2-1")
	data, _ := ask["data"].(map[string]any)
	if ask["type"] != float64(4) || data["flags"] != float64(64) || !strings.Contains(data["content"].(string), "Restart **htg-cs2-1**'s machine?") {
		t.Fatalf("ask = %v", ask)
	}
	if len(r.hub.actions()) != 0 {
		t.Fatal("acted before Confirm")
	}
	if a := r.click("42", "/events/do/restart/htg-cs2-1"); a["type"] != float64(6) {
		t.Fatalf("confirm = %v", a)
	}
	if c, _ := r.discord.edit(t, 1)["content"].(string); !strings.Contains(c, "Restarting **htg-cs2-1**'s machine") {
		t.Errorf("result = %q", c)
	}
	r.click("43", "/events/do/park/htg-cs2-1")
	r.discord.edit(t, 2)
	if got := r.hub.actions(); !slices.Equal(got, []string{"restart htg-cs2-1 for discord:42", "park htg-cs2-1 for discord:43"}) {
		t.Errorf("hub calls = %v", got)
	}

	r.hub.err = connect.NewError(connect.CodePermissionDenied, errors.New("not allowed"))
	r.click("44", "/events/do/park/htg-cs2-1")
	if c, _ := r.discord.edit(t, 3)["content"].(string); !strings.Contains(c, "only lets an operator of this server's game") {
		t.Errorf("a non-operator's click = %q", c)
	}
	r.hub.err = connect.NewError(connect.CodeFailedPrecondition, errors.New("the host is parking"))
	r.click("42", "/events/do/restart/htg-cs2-1")
	if c, _ := r.discord.edit(t, 4)["content"].(string); c != "Nothing done: the host is parking." {
		t.Errorf("a refusal = %q", c)
	}
	if a := r.click("42", "/events/cancel"); a["type"] != float64(7) || !strings.Contains(a["data"].(map[string]any)["content"].(string), "nothing was done") {
		t.Errorf("cancel = %v", a)
	}
	if a := r.click("42", "/events/ask/destroy/htg-cs2-1"); !strings.Contains(a["data"].(map[string]any)["content"].(string), "not one this bot made") {
		t.Errorf("an unknown action = %v", a)
	}
}

func TestLine(t *testing.T) {
	e := &hubv1.Event{Id: 7, Kind: "action.host_park", Severity: "info", ServerId: "htg-cs2-1", Summary: "parked *now* @everyone", ActorName: "Jo_",
		At: timestamppb.New(time.Date(2026, 10, 15, 20, 5, 0, 0, time.UTC))}
	if got, want := events.Line(e, "HTG CS2 #1", ""), "⚪ Info · parked \\*now\\* @\u200beveryone · HTG CS2 #1 · by Jo\\_\n-# event 7 · action.host_park · Oct 15, 20:05 UTC"; got != want {
		t.Errorf("Line = %q\nwant   %q", got, want)
	}
}
