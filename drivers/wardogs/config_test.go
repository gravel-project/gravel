package wardogs_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/gravel-project/gravel/drivers"
	"github.com/gravel-project/gravel/games/wardogs/wardogstest"
)

const (
	secretPass  = "s3cret-rcon-pass"
	secretToken = "feed-token-123456"
	current     = "[/Script/WDRCON.WDRCONSettings]\r\nbEnabled=true\r\nPassword=" + secretPass + "\r\nPort=7789\r\n\r\n" +
		"[/Script/WDGame.WDGameSession]\r\nServerName=HTG\r\nServerPassword=join-pass\r\nMaxReservedSlots=0\r\n!DefaultBannedPlayerIds=ClearArray\r\n.DefaultBannedPlayerIds=\"76561190000000009\"\r\n\r\n" +
		"[MatchState.Playing.KOTH]\r\nScorePeriod=24\r\n\r\n" +
		"[WDServerFeed]\r\nUrl=https://ingest.example\r\nToken=" + secretToken + "\r\n"
	// The deployment's document: secrets redacted, the owned sections and the ban list left out.
	desired = "[/Script/WDGame.WDGameSession]\r\nServerName=HTG | NA\r\nServerPassword=<redacted>\r\nMaxReservedSlots=0\r\n\r\n" +
		"[MatchState.Playing.KOTH]\r\nScorePeriod=27\r\n"
)

type configServer struct {
	mu       sync.Mutex
	revision string
	text     string
	sent     []string // validate and put bodies
	ifMatch  []string
	reject   bool
	moveOnce bool // the next PUT answers 412 after the document moved
}

func (c *configServer) handlers() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		"GET /v1/config": func(w http.ResponseWriter, _ *http.Request) {
			c.mu.Lock()
			defer c.mu.Unlock()
			w.Header().Set("ETag", `"`+c.revision+`"`)
			_ = json.NewEncoder(w).Encode(map[string]any{"revision": c.revision, "writable": true, "text": c.text,
				"sections": []map[string]any{{"section": "/Script/WDRCON.WDRCONSettings", "appliesWhen": "next-restart", "allowedKeys": []string{"Password", "Port"},
					"keyOverrides": []map[string]any{{"key": "Port", "writable": false, "lockedBy": "RCONPort"}}}}})
		},
		"POST /v1/config/validate": func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			c.mu.Lock()
			c.sent = append(c.sent, string(b))
			reject := c.reject
			c.mu.Unlock()
			if reject {
				w.WriteHeader(http.StatusUnprocessableEntity)
				_, _ = io.WriteString(w, `{"ok":false,"errors":[{"section":"MatchState.Playing.KOTH","key":"ScorePeriod","code":"out_of_range","message":"18-30"}]}`)
				return
			}
			_, _ = io.WriteString(w, `{"ok":true,"changed":[{"section":"MatchState.Playing.KOTH","keys":["ScorePeriod"]}],"outcomes":[{"section":"MatchState.Playing.KOTH","state":"next-match"}]}`)
		},
		"PUT /v1/config": func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			c.mu.Lock()
			defer c.mu.Unlock()
			c.sent = append(c.sent, string(b))
			c.ifMatch = append(c.ifMatch, r.Header.Get("If-Match"))
			if c.moveOnce || r.Header.Get("If-Match") != `"`+c.revision+`"` {
				c.moveOnce = false
				c.revision += "x"
				w.WriteHeader(http.StatusPreconditionFailed)
				_, _ = io.WriteString(w, `{"ok":false,"error":{"code":"revision_mismatch","message":"moved"}}`)
				return
			}
			c.text, c.revision = string(b), c.revision+"1"
			_, _ = io.WriteString(w, `{"ok":true,"revision":"`+c.revision+`","outcomes":[{"section":"/Script/WDGame.WDGameSession","state":"applied"}]}`)
		},
	}
}

func newConfigDriver(t *testing.T) (drivers.ExternalReachable, *configServer) {
	t.Helper()
	cs := &configServer{revision: "r1", text: current}
	srv := wardogstest.New(t, fixture, wardogstest.Options{Handle: cs.handlers()})
	return newDriver(t, srv, wardogstest.Token), cs
}

func i64(v int64) *int64 { return &v }

var bands = []drivers.Band{{Section: "MatchState.Playing.KOTH", Key: "ScorePeriod", Min: i64(18), Max: i64(30)}}

func TestConfigIsRedacted(t *testing.T) {
	d, _ := newConfigDriver(t)
	doc, err := d.Config(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(doc.Text, secretPass) || strings.Contains(doc.Text, secretToken) || strings.Contains(doc.Text, "join-pass") {
		t.Errorf("a secret in the document:\n%s", doc.Text)
	}
	if doc.Revision != "r1" || len(doc.Bans) != 1 || doc.Bans[0].Subject != "76561190000000009" {
		t.Errorf("doc = %+v", doc)
	}
	if len(doc.Sections) != 1 || !slices.Equal(doc.Sections[0].Locked, []string{"Port: RCONPort"}) {
		t.Errorf("sections = %+v", doc.Sections)
	}
}

func TestPlanMergesWhatTheHubOwns(t *testing.T) {
	d, cs := newConfigDriver(t)
	bans := []drivers.Identity{{Provider: "steam", Subject: "76561190000000001"}, {Provider: "steam", Subject: "76561190000000002"}}
	plan, err := d.PlanConfig(context.Background(), drivers.ConfigDraft{Text: desired, Bans: bans, Bands: bands})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Revision != "r1" || !plan.Result.OK || len(plan.Result.Outcomes) != 1 {
		t.Errorf("plan = %+v", plan)
	}
	sent := cs.sent[0]
	for _, want := range []string{"Password=" + secretPass, "Port=7789", "Token=" + secretToken, "ServerPassword=join-pass", "ScorePeriod=27",
		"ServerName=HTG | NA", "!DefaultBannedPlayerIds=ClearArray\r\n.DefaultBannedPlayerIds=\"76561190000000001\"\r\n.DefaultBannedPlayerIds=\"76561190000000002\"\r\n"} {
		if !strings.Contains(sent, want) {
			t.Errorf("the validated document lacks %q:\n%s", want, sent)
		}
	}
	if strings.Contains(sent, "76561190000000009") || strings.Contains(sent, "\n\n") && !strings.Contains(sent, "\r\n") {
		t.Errorf("the server's old ban list or bare LFs survived:\n%q", sent)
	}
	for _, c := range plan.Changes {
		for _, v := range slices.Concat(c.Before, c.After) {
			if strings.Contains(v, secretPass) || strings.Contains(v, secretToken) || strings.Contains(v, "join-pass") {
				t.Errorf("a secret in the plan's changes: %+v", c)
			}
		}
	}
	keys := map[string]bool{}
	for _, c := range plan.Changes {
		keys[c.Key] = true
	}
	if !keys["ScorePeriod"] || !keys["ServerName"] || !keys["DefaultBannedPlayerIds"] || keys["Password"] || keys["ServerPassword"] {
		t.Errorf("changed keys = %v", keys)
	}
}

func TestPlanRefusesBeforeSending(t *testing.T) {
	d, cs := newConfigDriver(t)
	ctx := context.Background()
	for name, text := range map[string]string{
		"an owned section": desired + "\r\n[/Script/WDRCON.WDRCONSettings]\r\nAllowedHosts=1.2.3.4\r\n",
		"the ban list":     strings.Replace(desired, "MaxReservedSlots=0", "MaxReservedSlots=0\r\n.DefaultBannedPlayerIds=\"1\"", 1),
		"out of band":      strings.Replace(desired, "ScorePeriod=27", "ScorePeriod=60", 1),
		"not a number":     strings.Replace(desired, "ScorePeriod=27", "ScorePeriod=soon", 1),
	} {
		_, err := d.PlanConfig(ctx, drivers.ConfigDraft{Text: text, Bands: bands})
		var ie *drivers.InvalidConfigError
		if !errors.As(err, &ie) || len(ie.Problems) == 0 {
			t.Errorf("%s: %v", name, err)
		}
	}
	if len(cs.sent) != 0 {
		t.Errorf("sent %d documents", len(cs.sent))
	}
	// Without the hub's list (nil), the server's own list is kept.
	if _, err := d.PlanConfig(ctx, drivers.ConfigDraft{Text: desired}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cs.sent[0], `.DefaultBannedPlayerIds="76561190000000009"`) {
		t.Errorf("the server's list was dropped:\n%s", cs.sent[0])
	}
}

func TestPlanCarriesTheServersRefusal(t *testing.T) {
	d, cs := newConfigDriver(t)
	cs.reject = true
	plan, err := d.PlanConfig(context.Background(), drivers.ConfigDraft{Text: desired})
	if err != nil || plan.Result.OK || len(plan.Result.Problems) != 1 || plan.Result.Problems[0].Code != "out_of_range" {
		t.Errorf("plan = %+v, %v", plan, err)
	}
}

func TestApplyWritesAgainstThePlannedRevision(t *testing.T) {
	d, cs := newConfigDriver(t)
	ctx := context.Background()
	draft := drivers.ConfigDraft{Text: desired, Bands: bands}
	if _, err := d.ApplyConfig(ctx, draft, "r0"); !errors.Is(err, drivers.ErrConfigConflict) || len(cs.ifMatch) != 0 {
		t.Errorf("a stale revision: %v, %d writes", err, len(cs.ifMatch))
	}
	res, err := d.ApplyConfig(ctx, draft, "r1")
	if err != nil || !res.OK || res.Revision != "r11" || cs.ifMatch[0] != `"r1"` {
		t.Fatalf("apply = %+v, %v, if-match %v", res, err, cs.ifMatch)
	}
	cs.moveOnce = true
	if _, err := d.ApplyConfig(ctx, draft, "r11"); !errors.Is(err, drivers.ErrConfigConflict) {
		t.Errorf("a 412: %v", err)
	}
}

func TestSetConfigBans(t *testing.T) {
	d, cs := newConfigDriver(t)
	ctx := context.Background()
	cs.moveOnce = true // the first write races another; the driver reads again once
	res, err := d.SetConfigBans(ctx, []drivers.Identity{{Provider: "steam", Subject: "76561190000000003"}})
	if err != nil || !res.OK || len(cs.ifMatch) != 2 {
		t.Fatalf("set bans = %+v, %v, writes %v", res, err, cs.ifMatch)
	}
	if !strings.Contains(cs.text, `.DefaultBannedPlayerIds="76561190000000003"`) || strings.Contains(cs.text, "76561190000000009") ||
		!strings.Contains(cs.text, "Password="+secretPass) || !strings.Contains(cs.text, "ScorePeriod=24") {
		t.Errorf("document after:\n%s", cs.text)
	}
	if _, err := d.SetConfigBans(ctx, []drivers.Identity{{Provider: "discord", Subject: "1"}}); !errors.Is(err, drivers.ErrRejected) {
		t.Errorf("a non-steam ban: %v", err)
	}
}
