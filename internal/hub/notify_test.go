package hub_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gravel-project/gravel/internal/hub"
	"github.com/gravel-project/gravel/internal/identity"
	"github.com/gravel-project/gravel/internal/org"
	"github.com/gravel-project/gravel/internal/store"
	"github.com/gravel-project/gravel/internal/store/storetest"
)

// The gate item of gravel#196: a server.unreachable event reaches an ntfy topic within one
// dispatcher cycle, with no Discord anywhere in the path.
func TestHubNotifiesNtfy(t *testing.T) {
	cfg := testConfig(t)
	st := storetest.Open(t)
	storetest.Reset(t, st)
	t.Cleanup(func() { storetest.Reset(t, st) })

	var mu sync.Mutex
	var got []map[string]any
	ntfy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		mu.Lock()
		got = append(got, m)
		mu.Unlock()
	}))
	defer ntfy.Close()

	ctx := context.Background()
	h, err := hub.New(ctx, hub.Options{Config: cfg, Logger: quiet(), Version: "test", Providers: []identity.Registration{}})
	if err != nil {
		t.Fatal(err)
	}
	o, err := st.GetBuiltinOrganization(ctx)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(org.Settings{Version: org.SettingsVersion, Notifiers: []org.Notifier{
		{Name: "john-phone", Kind: org.NotifierNtfy, URL: ntfy.URL, Topic: "htg-ops", Events: org.NotifierEvents{Kinds: []string{"server"}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpdateOrganizationSettings(ctx, o.ID, raw, time.Now(), store.Actor{Via: store.ViaCLI}, []string{"notifiers"}); err != nil {
		t.Fatal(err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- h.Run(runCtx) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	<-h.Started()

	// The first pass at start makes the notifier's cursor at the log's head.
	waitFor(t, 5*time.Second, "the notifier's cursor", func() bool {
		cs, err := st.NotifierCursors(ctx, o.ID)
		return err == nil && len(cs) == 1
	})
	at := time.Now().UTC()
	if err := st.AppendEvent(ctx, store.Event{OrganizationID: o.ID, At: at, Kind: "server.unreachable", ServerID: "wd-1",
		Summary: "War Dogs #1 stopped answering its polls", Source: "server_health", SourceID: "wd-1:server.unreachable:1"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second+3*time.Second, "the ntfy message", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) > 0
	})
	mu.Lock()
	m := got[0]
	mu.Unlock()
	if m["topic"] != "htg-ops" || !strings.HasSuffix(m["title"].(string), ": server.unreachable") || m["priority"] != 4.0 ||
		!strings.Contains(m["message"].(string), "server.unreachable · wd-1 · War Dogs #1 stopped answering its polls") {
		t.Errorf("the message: %v", m)
	}
}

func waitFor(t *testing.T, d time.Duration, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("no %s within %v", what, d)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
