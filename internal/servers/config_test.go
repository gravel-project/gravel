package servers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/gravel-project/gravel/drivers"
	"github.com/gravel-project/gravel/games/wardogs/wardogstest"
)

const (
	steamB    = "76561190000000002"
	hostBan   = "76561190000000009" // banned by the host before the hub adopted the list
	liveDoc   = "[/Script/WDRCON.WDRCONSettings]\r\nPassword=" + wardogstest.Token + "\r\n\r\n[/Script/WDGame.WDGameSession]\r\nServerName=HTG\r\n!DefaultBannedPlayerIds=ClearArray\r\n.DefaultBannedPlayerIds=\"" + hostBan + "\"\r\n\r\n[MatchState.Playing.KOTH]\r\nScorePeriod=24\r\n"
	wantedDoc = "[/Script/WDGame.WDGameSession]\r\nServerName=HTG | NA\r\n\r\n[MatchState.Playing.KOTH]\r\nScorePeriod=27\r\n"
)

// liveConfig is a War Dogs configuration endpoint with a document and a revision.
type liveConfig struct {
	mu       sync.Mutex
	text     string
	revision int
	puts     int
	refuse   bool // PUT answers 422
}

func (c *liveConfig) rev() string { return "r" + string(rune('0'+c.revision)) }

func (c *liveConfig) handle(h map[string]http.HandlerFunc) {
	h["GET /v1/config"] = func(w http.ResponseWriter, _ *http.Request) {
		c.mu.Lock()
		defer c.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"revision": c.rev(), "writable": true, "text": c.text})
	}
	h["POST /v1/config/validate"] = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"ok":true,"outcomes":[{"section":"MatchState.Playing.KOTH","state":"next-match"}]}`)
	}
	h["PUT /v1/config"] = func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		defer c.mu.Unlock()
		c.puts++
		if c.refuse {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = io.WriteString(w, `{"ok":false,"errors":[{"section":"/Script/WDGame.WDGameSession","key":"DefaultBannedPlayerIds","code":"invalid","message":"no"}]}`)
			return
		}
		if r.Header.Get("If-Match") != `"`+c.rev()+`"` {
			w.WriteHeader(http.StatusPreconditionFailed)
			_, _ = io.WriteString(w, `{"ok":false}`)
			return
		}
		c.text = string(b)
		c.revision++
		_, _ = io.WriteString(w, `{"ok":true,"revision":"`+c.rev()+`"}`)
	}
}

func (c *liveConfig) bans() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var ids []string
	for _, l := range strings.Split(c.text, "\r\n") {
		if strings.HasPrefix(l, ".DefaultBannedPlayerIds=") {
			ids = append(ids, strings.Trim(strings.TrimPrefix(l, ".DefaultBannedPlayerIds="), `"`))
		}
	}
	return strings.Join(ids, ",")
}

func newConfigRig(t *testing.T, opt wardogstest.Options, handlers map[string][2]string) (*moderationRig, *liveConfig) {
	t.Helper()
	lc := &liveConfig{text: liveDoc, revision: 1}
	if opt.Handle == nil {
		opt.Handle = map[string]http.HandlerFunc{}
	}
	lc.handle(opt.Handle)
	r := newModerationRig(t, opt, handlers)
	lc.handle(opt.Handle) // the rig set its PUT; the live endpoint's wins
	if _, err := r.poll(t); err != nil {
		t.Fatal(err)
	}
	return r, lc
}

// The first ban adopts the host's list; a ban of a player who is not on still holds, in the
// configuration; an unban of a host ban lifts it.
func TestHubOwnsTheBanList(t *testing.T) {
	r, lc := newConfigRig(t, wardogstest.Options{}, map[string][2]string{
		"POST /v1/bans":      {"404", `{"error":{"code":"player_not_found","message":"not on"}}`},
		"DELETE /v1/bans/{}": {"404", `{"error":{"code":"ban_not_found","message":"no rcon ban"}}`},
	})
	ctx := context.Background()
	id, err := r.mod.Do(ctx, r.asOwner(), Action{Server: "wd-1", Kind: ActionBan, Player: drivers.Identity{Subject: steamB}, Reason: "cheating"})
	if err != nil {
		t.Fatalf("an offline ban: %v", err)
	}
	if got := lc.bans(); got != hostBan+","+steamB {
		t.Errorf("configuration bans = %s", got)
	}
	if e := r.audit.Entries()[id-1]; e.Outcome != OutcomeOK {
		t.Errorf("outcome = %q", e.Outcome)
	}
	all := r.bans.All()
	if len(all) != 2 || all[0].Subject != hostBan || all[0].BanAuditID != nil || all[1].Subject != steamB || *all[1].BanAuditID != id {
		t.Errorf("hub bans = %+v", all)
	}
	if n := len(r.b.of("POST /v1/bans")); n != 1 {
		t.Errorf("the immediate RCON ban was tried %d times", n)
	}

	// Banning again re-sends the list and adds no row.
	if _, err := r.mod.Do(ctx, r.asOwner(), Action{Server: "wd-1", Kind: ActionBan, Player: drivers.Identity{Subject: steamB}, Reason: "again"}); err != nil {
		t.Fatal(err)
	}
	if n := len(r.bans.All()); n != 2 {
		t.Errorf("bans after a repeat = %d", n)
	}

	// The host's ban, lifted through the hub.
	uid, err := r.mod.Do(ctx, r.asOwner(), Action{Server: "wd-1", Kind: ActionUnban, Player: drivers.Identity{Subject: hostBan}})
	if err != nil {
		t.Fatal(err)
	}
	if got := lc.bans(); got != steamB {
		t.Errorf("configuration bans after the unban = %s", got)
	}
	if b := r.bans.All()[0]; b.LiftedAt == nil || b.LiftAuditID == nil || *b.LiftAuditID != uid {
		t.Errorf("lifted = %+v", b)
	}
	// Nobody to unban: neither the hub nor the server holds a ban.
	if _, err := r.mod.Do(ctx, r.asOwner(), Action{Server: "wd-1", Kind: ActionUnban, Player: drivers.Identity{Subject: "76561190000000077"}}); !errors.Is(err, drivers.ErrBanNotFound) {
		t.Errorf("an unban of nobody: %v", err)
	}

	bans, err := r.mod.Bans(ctx, "wd-1")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, b := range bans {
		if b.Identity.Subject == steamB {
			found = b.Hub != nil && b.Hub.Reason == "cheating"
		}
	}
	if !found {
		t.Errorf("bans = %+v", bans)
	}
}

// A server that refuses the list rolls the hub's record back: both directions fail toward banned.
func TestABanTheServerRefusesIsRolledBack(t *testing.T) {
	r, lc := newConfigRig(t, wardogstest.Options{}, nil)
	lc.refuse = true
	id, err := r.mod.Do(context.Background(), r.asOwner(), Action{Server: "wd-1", Kind: ActionBan, Player: drivers.Identity{Subject: steamB}, Reason: "x"})
	if !errors.Is(err, drivers.ErrConfigRejected) {
		t.Fatalf("err = %v", err)
	}
	if e := r.audit.Entries()[id-1]; e.Outcome != OutcomeRejected {
		t.Errorf("outcome = %q", e.Outcome)
	}
	for _, b := range r.bans.All() {
		if b.Subject == steamB && b.LiftedAt == nil {
			t.Errorf("the refused ban stayed active: %+v", b)
		}
	}
	if n := len(r.b.of("POST /v1/bans")); n != 0 {
		t.Errorf("the RCON ban ran after the refusal")
	}
}

// Without a writable configuration the hub falls back to the server's own ban route.
func TestBansWithoutConfigurationUseTheServersRoute(t *testing.T) {
	r, _ := newConfigRig(t, wardogstest.Options{Remove: []string{"PUT /v1/config"}}, nil)
	if _, err := r.mod.Do(context.Background(), r.asOwner(), Action{Server: "wd-1", Kind: ActionBan, Player: drivers.Identity{Subject: steamB}, Reason: "x"}); err != nil {
		t.Fatal(err)
	}
	if len(r.bans.All()) != 0 || len(r.b.of("POST /v1/bans")) != 1 {
		t.Errorf("hub bans %d, rcon bans %d", len(r.bans.All()), len(r.b.of("POST /v1/bans")))
	}
}

func TestConfigPlanAndApply(t *testing.T) {
	r, lc := newConfigRig(t, wardogstest.Options{}, nil)
	ctx := context.Background()

	doc, err := r.mod.Config(ctx, "wd-1")
	if err != nil || strings.Contains(doc.Text, wardogstest.Token) || doc.Revision != "r1" {
		t.Fatalf("config = %+v, %v", doc, err)
	}
	plan, err := r.mod.PlanConfig(ctx, "wd-1", wantedDoc)
	if err != nil || plan.Revision != "r1" || !plan.Result.OK {
		t.Fatalf("plan = %+v, %v", plan, err)
	}
	keys := map[string]bool{}
	for _, c := range plan.Changes {
		keys[c.Key] = true
	}
	if !keys["ScorePeriod"] || !keys["ServerName"] || keys["DefaultBannedPlayerIds"] || keys["Password"] {
		t.Errorf("plan changes %v (the adopted list is unchanged; the RCON block kept)", keys)
	}
	if lc.puts != 0 || len(r.audit.Entries()) != 0 {
		t.Errorf("a plan wrote or audited: %d puts, %d entries", lc.puts, len(r.audit.Entries()))
	}

	// The game's band: War Dogs pays ScorePeriod 18-30.
	if _, err := r.mod.PlanConfig(ctx, "wd-1", strings.Replace(wantedDoc, "27", "60", 1)); !errors.Is(err, drivers.ErrInvalidConfig) {
		t.Errorf("out of band: %v", err)
	}

	id, res, err := r.mod.ApplyConfig(ctx, r.asOwner(), "wd-1", wantedDoc, plan.Revision, "wardogs-server#12 abc123")
	if err != nil || !res.OK {
		t.Fatalf("apply = %+v, %v", res, err)
	}
	e := r.audit.Entries()[id-1]
	if e.Action != ActionApplyConfig || e.Outcome != OutcomeOK || e.Reason != "wardogs-server#12 abc123" || string(e.Detail) != `{"revision":"r1"}` {
		t.Errorf("entry = %+v", e)
	}
	if !strings.Contains(lc.text, "ScorePeriod=27") || !strings.Contains(lc.text, "Password="+wardogstest.Token) || lc.bans() != hostBan {
		t.Errorf("document after:\n%s", lc.text)
	}

	// The same plan again: the server has moved on.
	id, _, err = r.mod.ApplyConfig(ctx, r.asOwner(), "wd-1", wantedDoc, plan.Revision, "")
	if !errors.Is(err, drivers.ErrConfigConflict) || r.audit.Entries()[id-1].Outcome != OutcomeConflict {
		t.Errorf("a stale plan: %v", err)
	}
	if _, _, err := r.mod.ApplyConfig(ctx, r.asOwner(), "wd-1", "", "r2", ""); !errors.Is(err, ErrInvalidModeration) {
		t.Errorf("an empty document: %v", err)
	}
	if _, _, err := r.mod.ApplyConfig(ctx, r.asOwner(), "wd-1", wantedDoc, "", ""); !errors.Is(err, ErrInvalidModeration) {
		t.Errorf("no revision: %v", err)
	}
}

// A server with a feed gets [WDServerFeed] written from servers.yaml and the token file's first
// line; the plan shows the keys and never the values.
func TestConfigWritesTheFeed(t *testing.T) {
	r, lc := newConfigRig(t, wardogstest.Options{}, nil)
	ctx := context.Background()
	tokenFile := filepath.Join(t.TempDir(), "feed")
	if err := os.WriteFile(tokenFile, []byte("feed-new-token\nfeed-old-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.m.Servers[0].Feed = &Feed{URL: "https://ingest.example.com/", TokenFile: tokenFile}
	r.apply(t)

	plan, err := r.mod.PlanConfig(ctx, "wd-1", wantedDoc)
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]bool{}
	for _, c := range plan.Changes {
		if c.Section == "WDServerFeed" {
			keys[c.Key] = true
		}
		for _, v := range slices.Concat(c.Before, c.After) {
			if strings.Contains(v, "feed-new-token") {
				t.Errorf("the token in the plan: %+v", c)
			}
		}
	}
	if !keys["Url"] || !keys["Token"] {
		t.Errorf("the feed's keys are not in the plan: %v", keys)
	}
	if _, res, err := r.mod.ApplyConfig(ctx, r.asOwner(), "wd-1", wantedDoc, plan.Revision, ""); err != nil || !res.OK {
		t.Fatalf("apply = %+v, %v", res, err)
	}
	if !strings.Contains(lc.text, "[WDServerFeed]\r\nUrl=https://ingest.example.com\r\nToken=feed-new-token\r\n") || strings.Contains(lc.text, "feed-old-token") {
		t.Errorf("document after:\n%s", lc.text)
	}

	// The token file gone: refused before anything is sent.
	if err := os.Remove(tokenFile); err != nil {
		t.Fatal(err)
	}
	if _, err := r.mod.PlanConfig(ctx, "wd-1", wantedDoc); !errors.Is(err, ErrCredentialFile) {
		t.Errorf("no token file: %v", err)
	}
}
