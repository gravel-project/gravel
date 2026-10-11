package bot_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/gateway"

	"github.com/gravel-project/gravel/discord/bot"
)

// intentsModule asks for extra intents and records what the registry says it got.
type intentsModule struct {
	want gateway.Intents
	got  bool
}

func (m *intentsModule) Name() string             { return "needy" }
func (m *intentsModule) Intents() gateway.Intents { return m.want }
func (m *intentsModule) Register(r *bot.Registry) error {
	m.got = r.HasIntent(m.want)
	return nil
}

// appAPI answers the gateway URL and the current application with the given flags (or an error
// when status isn't 200), counting the application reads.
type appAPI struct {
	srv    *httptest.Server
	mu     sync.Mutex
	status int
	flags  discord.ApplicationFlags
	reads  int
}

func newAppAPI(t *testing.T, status int, flags discord.ApplicationFlags) *appAPI {
	t.Helper()
	a := &appAPI{status: status, flags: flags}
	a.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/gateway"):
			_, _ = io.WriteString(w, `{"url":"wss://gateway.invalid"}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/applications/@me"):
			a.mu.Lock()
			a.reads++
			a.mu.Unlock()
			if a.status != http.StatusOK {
				w.WriteHeader(a.status)
				_, _ = io.WriteString(w, `{"code":0,"message":"boom"}`)
				return
			}
			_, _ = io.WriteString(w, `{"id":"1558123590786748516","name":"Bot","flags":`+strconv.Itoa(int(a.flags))+`}`)
		default:
			t.Errorf("unexpected Discord call %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	t.Cleanup(a.srv.Close)
	return a
}

func metricsOf(rt *bot.Runtime) string {
	rec := httptest.NewRecorder()
	rt.InternalHandler().ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/metrics", nil))
	return rec.Body.String()
}

func TestModuleIntents(t *testing.T) {
	cases := []struct {
		name    string
		gateway bool
		status  int
		flags   discord.ApplicationFlags
		got     bool
		reads   int
		missing string // the gauge's line, "" when there is none
	}{
		{"allowed", true, http.StatusOK, discord.ApplicationFlagGatewayPresence, true, 1, `gravel_bot_gateway_intent_missing{intent="guild_presences"} 0`},
		{"allowed under 100 guilds", true, http.StatusOK, discord.ApplicationFlagGatewayPresenceLimited | discord.ApplicationFlagGatewayGuildMemberLimited, true, 1, `gravel_bot_gateway_intent_missing{intent="guild_presences"} 0`},
		{"not allowed: left out, the bot still builds", true, http.StatusOK, discord.ApplicationFlagGatewayGuildMemberLimited | discord.ApplicationFlagGatewayMessageContent, false, 1, `gravel_bot_gateway_intent_missing{intent="guild_presences"} 1`},
		{"unreadable: asked for, Discord decides", true, http.StatusInternalServerError, 0, true, 1, ""},
		{"gateway off: nothing to check", false, http.StatusOK, 0, false, 0, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			api := newAppAPI(t, c.status, c.flags)
			cfg := testConfig(newSigner(t), newFakeHub(t))
			cfg.Discord.Gateway = c.gateway
			mod := &intentsModule{want: gateway.IntentGuildPresences}
			rt, err := bot.New(bot.Options{Config: cfg, Logger: quiet(), Version: "test", RESTURL: api.srv.URL + "/api/"}, mod)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { rt.Close(context.Background()) })
			if mod.got != c.got {
				t.Errorf("HasIntent: %t, want %t", mod.got, c.got)
			}
			intents := rt.Intents()
			if !intents.Has(gateway.IntentGuilds | gateway.IntentGuildMembers) {
				t.Errorf("the default intents stay: %d", intents)
			}
			if c.gateway && intents.Has(gateway.IntentGuildPresences) != c.got {
				t.Errorf("the session's intents %d: presences %t, want %t", intents, intents.Has(gateway.IntentGuildPresences), c.got)
			}
			if api.reads != c.reads {
				t.Errorf("application reads: %d, want %d", api.reads, c.reads)
			}
			m := metricsOf(rt)
			if c.missing != "" && !strings.Contains(m, c.missing) {
				t.Errorf("metrics lack %q:\n%s", c.missing, grepLines(m, "intent_missing"))
			}
			if c.missing == "" && strings.Contains(m, "gravel_bot_gateway_intent_missing{") {
				t.Errorf("no intent was checked, so no gauge:\n%s", grepLines(m, "intent_missing"))
			}
		})
	}
}

func TestBaseIntentsAreNotChecked(t *testing.T) {
	// A module asking only for what the runtime already asks for (guild members, privileged) costs
	// no application read: the base is the host's choice, as before modules could ask.
	api := newAppAPI(t, http.StatusOK, 0)
	cfg := testConfig(newSigner(t), newFakeHub(t))
	cfg.Discord.Gateway = true
	mod := &intentsModule{want: gateway.IntentGuildMembers | gateway.IntentGuildVoiceStates}
	rt, err := bot.New(bot.Options{Config: cfg, Logger: quiet(), Version: "test", RESTURL: api.srv.URL + "/api/"}, mod)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rt.Close(context.Background()) })
	if api.reads != 0 || !mod.got || !rt.Intents().Has(gateway.IntentGuildVoiceStates) {
		t.Errorf("reads %d, got %t, intents %d", api.reads, mod.got, rt.Intents())
	}
	// A host that replaces the base without members, then a module that adds them: checked.
	base := gateway.IntentGuilds
	rt2, err := bot.New(bot.Options{Config: cfg, Logger: quiet(), Version: "test", Intents: &base, RESTURL: api.srv.URL + "/api/"}, &intentsModule{want: gateway.IntentGuildMembers})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rt2.Close(context.Background()) })
	if api.reads != 1 || rt2.Intents().Has(gateway.IntentGuildMembers) {
		t.Errorf("members added by a module are checked: reads %d, intents %d", api.reads, rt2.Intents())
	}
}
