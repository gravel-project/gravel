package linkedroles_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/disgoorg/disgo/discord"

	"github.com/gravel-project/gravel/discord/bot"
	"github.com/gravel-project/gravel/discord/modules/linkedroles"
)

const appID = "1558123590786748516"

// fakeDiscord holds one application's metadata records.
type fakeDiscord struct {
	srv *httptest.Server

	mu      sync.Mutex
	records []map[string]any
	gets    int
	puts    int
	fail    int // answer this many calls with 500 first
}

func newFakeDiscord(t *testing.T) *fakeDiscord {
	t.Helper()
	f := &fakeDiscord{records: []map[string]any{}}
	path := "/api/applications/" + appID + "/role-connections/metadata"
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.fail > 0 {
			f.fail--
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"code":0,"message":"boom"}`))
			return
		}
		switch {
		case r.URL.Path == path && r.Method == http.MethodGet:
			f.gets++
		case r.URL.Path == path && r.Method == http.MethodPut:
			f.puts++
			var recs []map[string]any
			if err := json.NewDecoder(r.Body).Decode(&recs); err != nil {
				t.Errorf("put: %v", err)
			}
			f.records = recs
		default:
			t.Errorf("unexpected Discord call %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(f.records)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func newModule(t *testing.T, f *fakeDiscord) (*linkedroles.Module, *bot.Runtime) {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cfg := bot.Default()
	cfg.Discord.ApplicationID = appID
	cfg.Discord.PublicKey = hex.EncodeToString(pub)
	cfg.Discord.Token = base64.RawStdEncoding.EncodeToString([]byte(appID)) + ".Xa1b2c.not-a-real-token-signature"
	cfg.Discord.Gateway = false
	m := linkedroles.New()
	m.SetBackoff(time.Millisecond)
	rt, err := bot.New(bot.Options{Config: cfg, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), RESTURL: f.srv.URL + "/api"}, m)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rt.Close(context.Background()) })
	return m, rt
}

func TestRecordsMatchDiscordTypes(t *testing.T) {
	recs := linkedroles.Records()
	if len(recs) != 5 || recs[0].Key != "steam_linked" || recs[0].Type != discord.ApplicationRoleConnectionMetadataTypeBooleanEqual ||
		recs[4].Key != "supporter_tier" || recs[4].Type != discord.ApplicationRoleConnectionMetadataTypeIntegerGreaterThanOrEqual {
		t.Errorf("records: %+v", recs)
	}
}

func TestSyncIsIdempotent(t *testing.T) {
	f := newFakeDiscord(t)
	m, rt := newModule(t, f)
	ctx := context.Background()
	if changed, err := m.Sync(ctx); err != nil || !changed {
		t.Fatalf("first sync writes: %v %v", changed, err)
	}
	if f.puts != 1 || len(f.records) != 5 || f.records[0]["key"] != "steam_linked" || f.records[0]["type"] != float64(7) {
		t.Errorf("written: %d %v", f.puts, f.records)
	}
	if changed, err := m.Sync(ctx); err != nil || changed || f.puts != 1 {
		t.Errorf("an unchanged schema is not rewritten: %v %v %d", changed, err, f.puts)
	}
	f.records[1]["description"] = "edited in the portal"
	if changed, err := m.Sync(ctx); err != nil || !changed || f.puts != 2 || f.records[1]["description"] != "Has linked an Xbox account" {
		t.Errorf("a drifted schema is restored: %v %v %d %v", changed, err, f.puts, f.records[1])
	}

	if err := m.Run(ctx); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	rt.InternalHandler().ServeHTTP(rec, httptest.NewRequestWithContext(ctx, http.MethodGet, "/metrics", nil))
	if !strings.Contains(rec.Body.String(), "gravel_bot_linked_roles_schema_registered 1") {
		t.Errorf("metric:\n%s", rec.Body.String())
	}
}

func TestRunRetries(t *testing.T) {
	f := newFakeDiscord(t)
	f.fail = 3
	m, _ := newModule(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if f.puts != 1 || len(f.records) != 5 {
		t.Errorf("registered after the failures: %d %v", f.puts, f.records)
	}

	// Discord away for good: Run gives up when the bot stops, without an error.
	f.fail = 1 << 30
	stop, cancel2 := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel2()
	if err := m.Run(stop); err != nil {
		t.Errorf("stopping while retrying: %v", err)
	}
}
