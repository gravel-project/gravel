package moderation_test

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

	"github.com/gravel-project/gravel/discord/bot"
	"github.com/gravel-project/gravel/discord/modules/moderation"
	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
)

const appID = "1558123590786748516"

// fakeHub is the hub's servers and moderation in memory; it records every action with who it was
// for, and answers with err when set.
type fakeHub struct {
	mu    sync.Mutex
	done  []string
	err   error
	games []*hubv1.Game
}

func (f *fakeHub) Games(context.Context) ([]*hubv1.Game, error) { return f.games, nil }

func (f *fakeHub) Servers(context.Context) ([]*hubv1.Server, error) {
	return []*hubv1.Server{{Id: "wd-1", Name: "War Dogs #1", GameId: "wardogs"}}, nil
}

func (f *fakeHub) Players(context.Context, string) ([]*hubv1.ServerPlayer, error) {
	return []*hubv1.ServerPlayer{
		{Name: "Stranger", Subject: "76561190000000002"},
		{Name: "Strawberry", Subject: "76561190000000003"},
		{Name: "Jo", Subject: "76561190000000001"},
	}, nil
}

func (f *fakeHub) Do(_ context.Context, a moderation.Action, by *hubv1.Actor) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.done = append(f.done, a.Kind+" "+a.ServerID+" "+a.Subject+" "+a.Text+" for "+by.GetProvider()+":"+by.GetSubject())
	return nil
}

func (f *fakeHub) actions() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.done...)
}

// fakeDiscord records command registration and the bot's edits to its interaction responses.
type fakeDiscord struct {
	srv   *httptest.Server
	mu    sync.Mutex
	cmds  []map[string]any
	edits []map[string]any
}

func newFakeDiscord(t *testing.T) *fakeDiscord {
	t.Helper()
	f := &fakeDiscord{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/commands"):
			var cmds []map[string]any
			_ = json.Unmarshal(body, &cmds)
			f.mu.Lock()
			f.cmds = cmds
			f.mu.Unlock()
			for i := range cmds {
				cmds[i]["id"], cmds[i]["application_id"], cmds[i]["version"] = strconv.Itoa(100+i), appID, "1"
			}
			_ = json.NewEncoder(w).Encode(cmds)
		case r.Method == http.MethodPatch && strings.Contains(r.URL.Path, "/webhooks/"+appID+"/"):
			var edit map[string]any
			_ = json.Unmarshal(body, &edit)
			f.mu.Lock()
			f.edits = append(f.edits, edit)
			f.mu.Unlock()
			_, _ = io.WriteString(w, `{"id":"5","channel_id":"7","content":"","author":{"id":"`+appID+`","username":"bot","discriminator":"0"},"timestamp":"2026-10-10T20:00:00Z"}`)
		default:
			http.Error(w, "unexpected: "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
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
	handler http.Handler
	hub     *fakeHub
	discord *fakeDiscord
	rt      *bot.Runtime
}

func newRig(t *testing.T) *rig {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cfg := bot.Default()
	cfg.Discord.ApplicationID, cfg.Discord.PublicKey, cfg.Discord.Gateway = appID, hex.EncodeToString(pub), false
	cfg.Discord.Token = base64.RawStdEncoding.EncodeToString([]byte(appID)) + ".Xa1b2c.not-a-real-token-signature"
	cfg.Hub.URL, cfg.Hub.ClientID, cfg.Hub.ClientSecret = "http://hub.invalid", "gravel_bot", "s3cret"
	cfg.Server.Listen, cfg.Server.InternalListen = "127.0.0.1:0", "127.0.0.1:0"
	r := &rig{t: t, priv: priv, hub: &fakeHub{games: []*hubv1.Game{{Id: "wardogs", IdentityProvider: "steam"}}}, discord: newFakeDiscord(t)}
	mod := moderation.NewWithHub(r.hub, moderation.Config{Command: "wd", AccountURL: "https://app.example.com/account"})
	rt, err := bot.New(bot.Options{Config: cfg, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Version: "test", RESTURL: r.discord.srv.URL + "/api/"}, mod)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rt.Close(context.Background()) })
	r.handler, r.rt = rt.Handler(), rt
	return r
}

// post sends a signed interaction and returns Discord's view of the HTTP answer.
func (r *rig) post(body string) map[string]any {
	r.t.Helper()
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

func member(user string) string {
	return `"guild_id":"9","channel_id":"7","member":{"user":{"id":"` + user + `","username":"u` + user + `","discriminator":"0","avatar":null},"roles":[],"joined_at":"2026-10-09T12:00:00Z","permissions":"1099511627775"}`
}

func slash(user, sub string, opts map[string]string) string {
	var o []string
	for k, v := range opts {
		o = append(o, `{"type":3,"name":"`+k+`","value":`+strconv.Quote(v)+`}`)
	}
	return `{"type":2,"id":"900","application_id":"` + appID + `","token":"itoken","version":1,` + member(user) +
		`,"data":{"id":"100","name":"wd","type":1,"options":[{"type":1,"name":"` + sub + `","options":[` + strings.Join(o, ",") + `]}]}}`
}

func click(user, customID string) string {
	return `{"type":3,"id":"901","application_id":"` + appID + `","token":"ctoken","version":1,` + member(user) +
		`,"message":{"id":"5","channel_id":"7","content":"","author":{"id":"` + appID + `","username":"bot","discriminator":"0"},"timestamp":"2026-10-10T20:00:00Z"},` +
		`"data":{"custom_id":"` + customID + `","component_type":2}}`
}

// buttons are the custom ids an edit carries.
func buttons(edit map[string]any) []string {
	var out []string
	rows, _ := edit["components"].([]any)
	for _, row := range rows {
		cs, _ := row.(map[string]any)["components"].([]any)
		for _, c := range cs {
			out = append(out, c.(map[string]any)["custom_id"].(string))
		}
	}
	return out
}

func TestKickIsConfirmedThenSentForTheModerator(t *testing.T) {
	r := newRig(t)

	// The command defers privately, then asks.
	if a := r.post(slash("42", "kick", map[string]string{"player": "stranger", "reason": "afk in spawn"})); a["type"] != float64(5) || a["data"].(map[string]any)["flags"] != float64(64) {
		t.Fatalf("answer = %v", a)
	}
	ask := r.discord.edit(t, 1)
	if c, _ := ask["content"].(string); !strings.Contains(c, "Kick **Stranger** (`76561190000000002`) from **War Dogs #1**?") || !strings.Contains(c, "Reason: afk in spawn") {
		t.Errorf("question = %q", c)
	}
	ids := buttons(ask)
	if len(ids) != 2 || !strings.HasPrefix(ids[0], "/moderation/confirm/") || !strings.HasPrefix(ids[1], "/moderation/cancel/") {
		t.Fatalf("buttons = %v", ids)
	}
	if len(r.hub.actions()) != 0 {
		t.Fatal("sent before Confirm")
	}

	// Someone else's click is refused, privately, and sends nothing.
	if a := r.post(click("43", ids[0])); a["type"] != float64(4) || !strings.Contains(a["data"].(map[string]any)["content"].(string), "only the moderator who ran") {
		t.Errorf("another user's click = %v", a)
	}

	// The moderator confirms: the hub gets the call on their behalf, and the message says so.
	if a := r.post(click("42", ids[0])); a["type"] != float64(6) {
		t.Fatalf("confirm answer = %v", a)
	}
	done := r.discord.edit(t, 2)
	if c, _ := done["content"].(string); !strings.Contains(c, "Kicked Stranger on War Dogs #1") || len(buttons(done)) != 0 {
		t.Errorf("result = %v", done)
	}
	if got := r.hub.actions(); len(got) != 1 || got[0] != "kick wd-1 76561190000000002 afk in spawn for discord:42" {
		t.Errorf("hub calls = %v", got)
	}

	// A confirmation is used once.
	if a := r.post(click("42", ids[0])); !strings.Contains(a["data"].(map[string]any)["content"].(string), "expired") {
		t.Errorf("a second confirm = %v", a)
	}
}

func TestRefusalsAndCancel(t *testing.T) {
	r := newRig(t)

	// An ambiguous name never reaches a confirmation.
	r.post(slash("42", "ban", map[string]string{"player": "str", "reason": "x"}))
	if c, _ := r.discord.edit(t, 1)["content"].(string); !strings.Contains(c, `"str" matches Stranger, Strawberry`) {
		t.Errorf("ambiguous = %q", c)
	}

	// Cancel sends nothing.
	r.post(slash("42", "broadcast", map[string]string{"text": "restart in 5"}))
	ids := buttons(r.discord.edit(t, 2))
	if a := r.post(click("42", ids[1])); a["type"] != float64(7) || !strings.Contains(a["data"].(map[string]any)["content"].(string), "Cancelled") {
		t.Errorf("cancel = %v", a)
	}

	// The hub says no: the moderator is told what to do about it.
	r.hub.mu.Lock()
	r.hub.err = connect.NewError(connect.CodePermissionDenied, errors.New("on_behalf_of is not a moderator"))
	r.hub.mu.Unlock()
	r.post(slash("44", "ban", map[string]string{"player": "76561190000000099", "reason": "cheating"}))
	ask := r.discord.edit(t, 3)
	if c, _ := ask["content"].(string); !strings.Contains(c, "Ban **76561190000000099** from **War Dogs #1**?") {
		t.Errorf("an offline ban's question = %q", c)
	}
	r.post(click("44", buttons(ask)[0]))
	if c, _ := r.discord.edit(t, 4)["content"].(string); !strings.Contains(c, "doesn't have you as a moderator") || !strings.Contains(c, "https://app.example.com/account") {
		t.Errorf("refused = %q", c)
	}
	if len(r.hub.actions()) != 0 {
		t.Errorf("hub calls = %v", r.hub.actions())
	}
}

// The command is named by the config, has a subcommand per action, is hidden from members without
// Moderate Members, and works only in a guild.
func TestCommandRegistration(t *testing.T) {
	r := newRig(t)
	cmds := r.rt.Commands()
	if len(cmds) != 1 {
		t.Fatalf("commands = %d", len(cmds))
	}
	raw, err := json.Marshal(cmds[0])
	if err != nil {
		t.Fatal(err)
	}
	var c struct {
		Name     string `json:"name"`
		Perms    string `json:"default_member_permissions"`
		Contexts []int  `json:"contexts"`
		Options  []struct {
			Name    string `json:"name"`
			Type    int    `json:"type"`
			Options []struct {
				Name      string `json:"name"`
				Required  bool   `json:"required"`
				MaxLength int    `json:"max_length"`
			} `json:"options"`
		} `json:"options"`
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	var subs []string
	for _, o := range c.Options {
		subs = append(subs, o.Name)
		if o.Type != 1 {
			t.Errorf("%s is not a subcommand", o.Name)
		}
	}
	if c.Name != "wd" || strings.Join(subs, ",") != "kick,ban,unban,message,broadcast" || c.Perms != "1099511627776" || len(c.Contexts) != 1 || c.Contexts[0] != 0 {
		t.Errorf("command = %s", raw)
	}
	if kick := c.Options[0].Options; kick[1].Name != "reason" || !kick[1].Required || kick[1].MaxLength != moderation.MaxText {
		t.Errorf("kick options = %+v", kick)
	}
}
