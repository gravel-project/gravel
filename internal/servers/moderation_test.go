package servers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/gravel-project/gravel/drivers"
	"github.com/gravel-project/gravel/games/wardogs/wardogstest"
	"github.com/gravel-project/gravel/internal/servers/serverstest"
)

const steamA = "76561190000000001"

// bodies records the JSON bodies the fake server received, by route shape.
type bodies struct {
	mu  sync.Mutex
	got map[string][]map[string]any
}

func (b *bodies) handler(shape, status, answer string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		if raw, _ := io.ReadAll(r.Body); len(raw) > 0 {
			_ = json.Unmarshal(raw, &m)
		}
		b.mu.Lock()
		b.got[shape] = append(b.got[shape], m)
		b.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if status != "" {
			switch status {
			case "404":
				w.WriteHeader(http.StatusNotFound)
			case "400":
				w.WriteHeader(http.StatusBadRequest)
			case "500":
				w.WriteHeader(http.StatusInternalServerError)
			}
		}
		_, _ = io.WriteString(w, answer)
	}
}

func (b *bodies) of(shape string) []map[string]any {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.got[shape]
}

type moderationRig struct {
	*monitorRig
	mod   *Moderation
	audit *serverstest.AuditStore
	bans  *serverstest.BanStore
	b     *bodies
	owner uuid.UUID
}

func newModerationRig(t *testing.T, opt wardogstest.Options, handlers map[string][2]string) *moderationRig {
	t.Helper()
	b := &bodies{got: map[string][]map[string]any{}}
	if opt.Handle == nil {
		opt.Handle = map[string]http.HandlerFunc{}
	}
	ok := `{"ok":true}`
	for shape, h := range map[string][2]string{
		"POST /v1/players/{}/kick":    {"", ok},
		"POST /v1/players/{}/kill":    {"", ok},
		"POST /v1/players/{}/message": {"", ok},
		"PATCH /v1/players/{}":        {"", ok},
		"POST /v1/broadcast":          {"", `{"ok":true,"pending":false,"message":"sent"}`},
		"POST /v1/bans":               {"", ok},
		"DELETE /v1/bans/{}":          {"", ok},
		"PUT /v1/config":              {"", `{"ok":true,"revision":"r2","outcomes":[{"section":"/Script/WDGame.WDGameSession","state":"applied"}]}`},
	} {
		if o, set := handlers[shape]; set {
			h = o
		}
		opt.Handle[shape] = b.handler(shape, h[0], h[1])
	}
	r := &moderationRig{monitorRig: newMonitorRig(t, opt), audit: serverstest.NewAuditStore(), bans: serverstest.NewBanStore(), b: b, owner: uuid.New()}
	r.mod = NewModeration(r.svc, r.mon, r.audit, r.bans, r.svc.orgID, prometheus.NewRegistry(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	return r
}

func (r *moderationRig) asOwner() Actor { return Actor{UserID: &r.owner, RequestID: "req-1"} }

func TestModerationNeedsAnObservedServer(t *testing.T) {
	r := newModerationRig(t, wardogstest.Options{}, nil)
	ctx := context.Background()
	if _, err := r.mod.Do(ctx, r.asOwner(), Action{Server: "wd-1", Kind: ActionKick, Player: drivers.Identity{Subject: steamA}, Reason: "x"}); !errors.Is(err, ErrNotObserved) {
		t.Errorf("before the first poll: %v", err)
	}
	if _, err := r.mod.Do(ctx, r.asOwner(), Action{Server: "nope", Kind: ActionKick, Player: drivers.Identity{Subject: steamA}, Reason: "x"}); err == nil {
		t.Error("an unknown server was acted on")
	}
	if n := len(r.audit.Entries()); n != 0 {
		t.Errorf("%d entries for calls never made", n)
	}
}

func TestModerationCallsTheDriverAndAuditsEachCall(t *testing.T) {
	r := newModerationRig(t, wardogstest.Options{}, nil)
	if _, err := r.poll(t); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	app := uuid.New()
	bot := Actor{AppID: &app, OnBehalfOf: &drivers.Identity{Provider: "discord", Subject: "42"}, RequestID: "req-2"}
	p := drivers.Identity{Subject: steamA}
	for _, tc := range []struct {
		actor Actor
		a     Action
		shape string
		body  map[string]any
	}{
		{r.asOwner(), Action{Kind: ActionKick, Player: p, Reason: "spawn camping"}, "POST /v1/players/{}/kick", map[string]any{"reason": "spawn camping"}},
		{bot, Action{Kind: ActionBan, Player: p, Reason: "cheating"}, "POST /v1/bans", map[string]any{"steamId": steamA, "reason": "cheating"}},
		{bot, Action{Kind: ActionUnban, Player: p}, "DELETE /v1/bans/{}", nil},
		{r.asOwner(), Action{Kind: ActionMessage, Player: p, Message: "please stop"}, "POST /v1/players/{}/message", map[string]any{"message": "please stop"}},
		{r.asOwner(), Action{Kind: ActionBroadcast, Message: "restart in 5"}, "POST /v1/broadcast", map[string]any{"message": "restart in 5"}},
		{r.asOwner(), Action{Kind: ActionMovePlayer, Player: p, Team: "Valkyra", Respawn: true}, "PATCH /v1/players/{}", map[string]any{"faction": "Valkyra"}},
	} {
		tc.a.Server = "wd-1"
		id, err := r.mod.Do(ctx, tc.actor, tc.a)
		if err != nil {
			t.Fatalf("%s: %v", tc.a.Kind, err)
		}
		got := r.b.of(tc.shape)
		if len(got) != 1 || (tc.body != nil && !equalJSON(got[0], tc.body)) {
			t.Errorf("%s: sent %v, want %v", tc.a.Kind, got, tc.body)
		}
		e := r.audit.Entries()[id-1]
		if e.Outcome != OutcomeOK || e.FinishedAt == nil || e.ServerID != "wd-1" || e.Action != tc.a.Kind || e.RequestID != tc.actor.RequestID {
			t.Errorf("%s: entry %+v", tc.a.Kind, e)
		}
		if tc.a.Kind != ActionBroadcast && (e.TargetProvider != "steam" || e.TargetSubject != steamA) {
			t.Errorf("%s: target %s:%s (the game's provider fills an empty one)", tc.a.Kind, e.TargetProvider, e.TargetSubject)
		}
	}
	if n := len(r.b.of("POST /v1/players/{}/kill")); n != 1 {
		t.Errorf("respawn kills = %d, want 1", n)
	}
	es := r.audit.Entries()
	if b := es[1]; b.ActorAppID == nil || *b.ActorAppID != app || b.ActorUserID != nil || b.OnBehalfProvider != "discord" || b.OnBehalfSubject != "42" || b.Reason != "cheating" {
		t.Errorf("bot's ban = %+v", b)
	}
	if string(es[4].Detail) != `{"message":"restart in 5"}` || string(es[5].Detail) != `{"respawn":true,"team":"Valkyra"}` {
		t.Errorf("details = %s, %s", es[4].Detail, es[5].Detail)
	}
	if v := testutil.ToFloat64(r.mod.total.WithLabelValues("wd-1", ActionKick, OutcomeOK)); v != 1 {
		t.Errorf("kick ok counter = %v", v)
	}
	page, more, err := r.mod.Log(ctx, "wd-1", 0, 4)
	if err != nil || len(page) != 4 || !more || page[0].ID != 6 {
		t.Errorf("log page = %d entries, more %v, %v", len(page), more, err)
	}
	rest, more, err := r.mod.Log(ctx, "wd-1", page[3].ID, 4)
	if err != nil || len(rest) != 2 || more {
		t.Errorf("log rest = %d entries, more %v, %v", len(rest), more, err)
	}
}

// A refusal by the server is recorded as a word, and the error the caller gets can be told apart.
func TestModerationOutcomes(t *testing.T) {
	r := newModerationRig(t, wardogstest.Options{}, map[string][2]string{
		"POST /v1/players/{}/kick":    {"404", `{"error":{"code":"player_not_found","message":"no such player"}}`},
		"DELETE /v1/bans/{}":          {"404", `{"error":{"code":"ban_not_found","message":"no ban"}}`},
		"POST /v1/players/{}/message": {"400", `{"error":{"code":"message_too_long","message":"257 > 256"}}`},
		"POST /v1/players/{}/kill":    {"500", `{}`},
	})
	if _, err := r.poll(t); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	p := drivers.Identity{Subject: steamA}
	for _, tc := range []struct {
		a       Action
		want    error
		outcome string
	}{
		{Action{Kind: ActionKick, Player: p, Reason: "x"}, drivers.ErrPlayerNotFound, OutcomePlayerNotFound},
		{Action{Kind: ActionUnban, Player: p}, drivers.ErrBanNotFound, OutcomeBanNotFound},
		{Action{Kind: ActionMessage, Player: p, Message: strings.Repeat("a", 257)}, drivers.ErrRejected, OutcomeRejected},
		{Action{Kind: ActionMovePlayer, Player: p, Team: "Valkyra", Respawn: true}, ErrRespawnFailed, OutcomeMovedNotRespawned},
		{Action{Kind: ActionKick, Player: drivers.Identity{Provider: "discord", Subject: "42"}, Reason: "x"}, drivers.ErrRejected, OutcomeRejected},
	} {
		tc.a.Server = "wd-1"
		id, err := r.mod.Do(ctx, r.asOwner(), tc.a)
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", tc.a.Kind, err, tc.want)
		}
		if e := r.audit.Entries()[id-1]; e.Outcome != tc.outcome {
			t.Errorf("%s: outcome %q, want %q", tc.a.Kind, e.Outcome, tc.outcome)
		}
	}
	var rej *drivers.RejectedError
	_, err := r.mod.Do(ctx, r.asOwner(), Action{Server: "wd-1", Kind: ActionMessage, Player: p, Message: strings.Repeat("a", 257)})
	if !errors.As(err, &rej) || rej.Code != "message_too_long" {
		t.Errorf("the game's code: %v", err)
	}
	for _, e := range r.audit.Entries() {
		if strings.Contains(e.Outcome, "127.0.0.1") {
			t.Errorf("an outcome carries the address: %q", e.Outcome)
		}
	}
}

// Nothing is sent or recorded for an invalid request, an action the server does not offer, or
// when the audit row cannot be written.
func TestModerationRefusesBeforeSending(t *testing.T) {
	r := newModerationRig(t, wardogstest.Options{Remove: []string{"POST /v1/players/{}/kick"}}, nil)
	if _, err := r.poll(t); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	p := drivers.Identity{Subject: steamA}
	app := uuid.New()
	for name, tc := range map[string]struct {
		actor Actor
		a     Action
		want  error
	}{
		"no reason for a ban":       {r.asOwner(), Action{Kind: ActionBan, Player: p, Reason: "  "}, ErrInvalidModeration},
		"no player":                 {r.asOwner(), Action{Kind: ActionBan, Reason: "x"}, ErrInvalidModeration},
		"a long reason":             {r.asOwner(), Action{Kind: ActionBan, Player: p, Reason: strings.Repeat("r", MaxReasonLength+1)}, ErrInvalidModeration},
		"no message":                {r.asOwner(), Action{Kind: ActionBroadcast}, ErrInvalidModeration},
		"a long message":            {r.asOwner(), Action{Kind: ActionBroadcast, Message: strings.Repeat("m", MaxMessageLength+1)}, ErrInvalidModeration},
		"no team":                   {r.asOwner(), Action{Kind: ActionMovePlayer, Player: p}, ErrInvalidModeration},
		"an unknown action":         {r.asOwner(), Action{Kind: "slap", Player: p}, ErrInvalidModeration},
		"no actor":                  {Actor{}, Action{Kind: ActionBroadcast, Message: "m"}, ErrInvalidModeration},
		"a user acting for someone": {Actor{UserID: &r.owner, OnBehalfOf: &drivers.Identity{Provider: "discord", Subject: "1"}}, Action{Kind: ActionBroadcast, Message: "m"}, ErrInvalidModeration},
		"a half on_behalf_of":       {Actor{AppID: &app, OnBehalfOf: &drivers.Identity{Provider: "discord"}}, Action{Kind: ActionBroadcast, Message: "m"}, ErrInvalidModeration},
		"a route the build lacks":   {r.asOwner(), Action{Kind: ActionKick, Player: p, Reason: "x"}, drivers.ErrNotSupported},
	} {
		tc.a.Server = "wd-1"
		if _, err := r.mod.Do(ctx, tc.actor, tc.a); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", name, err, tc.want)
		}
	}
	r.audit.FailInsert = true
	if _, err := r.mod.Do(ctx, r.asOwner(), Action{Server: "wd-1", Kind: ActionBroadcast, Message: "m"}); err == nil {
		t.Error("acted although the audit row was not written")
	}
	if n := len(r.audit.Entries()); n != 0 {
		t.Errorf("%d entries", n)
	}
	for _, req := range r.fake.Requests() {
		if req.Method != http.MethodGet {
			t.Errorf("sent %s %s", req.Method, req.Path)
		}
	}
}

func TestModerationBans(t *testing.T) {
	r := newModerationRig(t, wardogstest.Options{Handle: map[string]http.HandlerFunc{
		"GET /v1/bans": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"bans":[{"steamId":"76561190000000001","bannedAtUtc":"2026-10-09T20:00:00Z","bannedBy":"rcon","reason":"cheating"},
				{"steamId":76561190000000002,"bannedAtUtc":"0001-01-01T00:00:00","bannedBy":"config","reason":null}],"count":2}`)
		},
	}}, nil)
	if _, err := r.mod.Bans(context.Background(), "wd-1"); !errors.Is(err, ErrNotObserved) {
		t.Errorf("before the first poll: %v", err)
	}
	if _, err := r.poll(t); err != nil {
		t.Fatal(err)
	}
	bans, err := r.mod.Bans(context.Background(), "wd-1")
	if err != nil || len(bans) != 2 {
		t.Fatalf("bans = %+v, %v", bans, err)
	}
	if b := bans[0]; b.Identity != (drivers.Identity{Provider: "steam", Subject: steamA}) || b.At.IsZero() || b.By != "rcon" || b.Reason != "cheating" {
		t.Errorf("first = %+v", b)
	}
	if b := bans[1]; b.Identity.Subject != "76561190000000002" || !b.At.IsZero() || b.By != "config" {
		t.Errorf("config ban = %+v", b)
	}
	if n := len(r.audit.Entries()); n != 0 {
		t.Errorf("a read was audited: %d", n)
	}
}

func equalJSON(a, b map[string]any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}
