package wardogsim

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gravel-project/gravel/games/wardogs"
)

const testToken = "sim-token"

// clock is a settable clock shared by the simulator and the client.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }

func (c *clock) Add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

// busy is a profile with a full server all day and short matches and sessions, so a test sees
// matches end, players come and go, and rejoins within minutes of simulated time.
func busy() Profile {
	p := DefaultProfile()
	p.Timezone = "UTC"
	p.Population = slices.Repeat([]float64{30}, 24)
	p.Regulars = 60
	p.MatchMinutes = Range{Min: 10, Max: 10}
	p.SessionMinutes = Range{Min: 4, Max: 8}
	p.KillsPerMinute = 1.5
	p.RejoinRate = 0.5
	p.RestartHour = nil
	return p
}

type rig struct {
	sim *Server
	clk *clock
	srv *httptest.Server
	c   *wardogs.Client
}

func newRig(t *testing.T, opt Options) *rig {
	t.Helper()
	clk := &clock{t: time.Date(2026, 10, 11, 20, 0, 0, 0, time.UTC)}
	if opt.Token == "" {
		opt.Token = testToken
	}
	opt.Now = clk.Now
	opt.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	sim, err := New(opt)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(sim.Handler())
	t.Cleanup(srv.Close)
	c, err := wardogs.New(srv.URL, wardogs.Options{Token: opt.Token, Strict: true, Now: clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	return &rig{sim: sim, clk: clk, srv: srv, c: c}
}

func TestEveryAdvertisedRouteHasAHandler(t *testing.T) {
	for _, build := range wardogs.Recordings() {
		files, err := wardogs.Recording(build)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := fs.ReadFile(files, "v1/capabilities.json")
		if err != nil {
			t.Fatal(err)
		}
		var caps wardogs.Capabilities
		if err := json.Unmarshal(raw, &caps); err != nil {
			t.Fatal(err)
		}
		for _, r := range caps.Routes {
			route, err := wardogs.ParseRoute(r)
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := handlers[route.Shape()]; !ok {
				t.Errorf("%s advertises %s and the simulator does not answer it", build, r)
			}
		}
	}
}

// shapeOf reduces a JSON value to its shape: object keys with their values' shapes, an array's
// first element, a scalar's type. null is a wildcard (a recorded null is a string elsewhere).
func sameShape(t *testing.T, where string, want, got any) {
	t.Helper()
	if want == nil || got == nil {
		return
	}
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			t.Errorf("%s: want an object, got %T", where, got)
			return
		}
		for k, v := range w {
			gv, present := g[k]
			if !present {
				t.Errorf("%s: %q is missing", where, k)
				continue
			}
			sameShape(t, where+"."+k, v, gv)
		}
		for k := range g {
			if _, known := w[k]; !known {
				t.Errorf("%s: %q is not in the recording", where, k)
			}
		}
	case []any:
		g, ok := got.([]any)
		if !ok {
			t.Errorf("%s: want an array, got %T", where, got)
			return
		}
		if len(w) > 0 && len(g) > 0 {
			sameShape(t, where+"[0]", w[0], g[0])
		}
	default:
		if wt, gt := jsonType(want), jsonType(got); wt != gt {
			t.Errorf("%s: want a %s, got a %s", where, wt, gt)
		}
	}
}

func jsonType(v any) string {
	switch v.(type) {
	case string:
		return "string"
	case float64:
		return "number"
	case bool:
		return "bool"
	}
	return "other"
}

// TestRecordedShapes: every GET the newest build recorded answers in the recorded shape.
func TestRecordedShapes(t *testing.T) {
	r := newRig(t, Options{Profile: busy(), Seed: 1})
	files := r.sim.files
	var rec struct {
		Files []struct{ Route, File string } `json:"files"`
	}
	raw, err := fs.ReadFile(files, "recording.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}
	if len(rec.Files) == 0 {
		t.Fatal("the recording lists no files")
	}
	ctx := context.Background()
	for _, f := range rec.Files {
		method, target, _ := strings.Cut(f.Route, " ")
		want, err := fs.ReadFile(files, f.File)
		if err != nil {
			t.Fatal(err)
		}
		req, _ := http.NewRequestWithContext(ctx, method, r.srv.URL+target, nil)
		req.Header.Set("Authorization", "Bearer "+testToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s: %d %s", f.Route, resp.StatusCode, got)
			continue
		}
		var w, g any
		if err := json.Unmarshal(want, &w); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(got, &g); err != nil {
			t.Fatalf("%s: %v", f.Route, err)
		}
		sameShape(t, f.Route, w, g)
	}
}

// TestContract runs the client's read contract against the simulator, strictly.
func TestContract(t *testing.T) {
	r := newRig(t, Options{Profile: busy(), Seed: 2})
	ctx := context.Background()
	caps, err := r.c.Capabilities(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(caps.Build, r.sim.Build()) {
		t.Errorf("build %q is not the recording's %s", caps.Build, r.sim.Build())
	}
	if missing := caps.Missing(); len(missing) > 0 {
		t.Errorf("the simulator does not grant %v", missing)
	}
	st, err := r.c.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.ServerName != DefaultServerName || st.Map != "Ozeti" || st.Players.Max != 100 || st.Players.Current < 20 || len(st.Factions) != 3 || st.ScoreTick.Current != 24 {
		t.Errorf("status: %+v", st)
	}
	p, err := r.c.Players(ctx)
	if err != nil || p.Count != len(p.Players) || p.Count != st.Players.Current {
		t.Fatalf("players: %+v %v", p, err)
	}
	for _, pl := range p.Players {
		if !strings.HasPrefix(string(pl.SteamID), StandInSteamIDPrefix) || len(pl.SteamID) != 17 || pl.Name == "" || pl.Faction == "" {
			t.Errorf("player %+v", pl)
		}
	}
	rot, err := r.c.Rotation(ctx)
	if err != nil || rot.Count != 6 || rot.Entries[0].Status != "now" || rot.Entries[1].Status != "next" || rot.Entries[2].Status != "" {
		t.Errorf("rotation: %+v %v", rot, err)
	}
	for _, id := range []string{"Europe", "Kavkazi", "NorthAmerica"} {
		if a, err := r.c.MapAlternators(ctx, id); err != nil || a.Map != id || a.Count == 0 {
			t.Errorf("%s alternators: %+v %v", id, a, err)
		}
		if x, err := r.c.MapExperiences(ctx, id); err != nil || x.Map != id || x.Count == 0 {
			t.Errorf("%s experiences: %+v %v", id, x, err)
		}
	}
	if _, err := r.c.MapAlternators(ctx, "Atlantis"); !wardogs.IsCode(err, "map_not_found") {
		t.Errorf("an unknown map: %v", err)
	}
	h, err := r.c.Health(ctx)
	if err != nil || h.Status != "ok" {
		t.Errorf("health: %+v %v", h, err)
	}
	a, err := r.c.Audit(ctx, 5)
	if err != nil || a.Limit != 5 || a.Count != 5 || a.Entries[4].Event != "HTTP" {
		t.Errorf("audit: %+v %v", a, err)
	}
	if !strings.HasPrefix(a.Entries[4].Detail, "GET /v1/") {
		t.Errorf("audit detail %q", a.Entries[4].Detail)
	}
	if raw, err := r.c.Get(ctx, wardogs.Route{Method: "GET", Segments: []string{"v1", "audit"}}, nil); err != nil || !strings.Contains(string(raw), " -> 200") {
		t.Errorf("the admin log is not written as the server writes it: %v\n%s", err, raw)
	}
	sp, err := r.c.Sponsor(ctx)
	if err != nil || !strings.HasPrefix(sp.ImageURL, "https://") {
		t.Errorf("sponsor: %+v %v", sp, err)
	}
	if id, err := r.c.ServerID(ctx); err != nil || id.ServerID == "" {
		t.Errorf("server id: %+v %v", id, err)
	}
	doc, err := r.c.Config(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if doc.Revision == "" || !strings.Contains(doc.Text, "\r\n") || !strings.Contains(doc.Text, "Password="+testToken) || len(doc.Sections) != len(r.sim.sections) {
		t.Errorf("config: revision %q, sections %d", doc.Revision, len(doc.Sections))
	}
	if v, err := r.c.Version(ctx); err != nil || v.Build != caps.Build || v.Engine.Changelist == 0 {
		t.Errorf("version: %+v %v", v, err)
	}
	if f, err := r.c.Feed(ctx); err != nil || f.Enabled || f.EndpointCount != 0 {
		t.Errorf("feed with none set: %+v %v", f, err)
	}
}

// TestTheWorldMoves: kills and deaths accumulate, a match ends and the rotation moves, the
// counters start again, and players come and go, some rejoining.
func TestTheWorldMoves(t *testing.T) {
	r := newRig(t, Options{Profile: busy(), Seed: 3})
	ctx := context.Background()
	first, err := r.c.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	total := func() (kills, deaths int) {
		p, err := r.c.Players(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, pl := range p.Players {
			kills += pl.Kills
			deaths += pl.Deaths
		}
		return kills, deaths
	}
	r.clk.Add(3 * time.Minute)
	k1, d1 := total()
	if k1 == 0 || d1 == 0 {
		t.Fatalf("three minutes in, kills %d deaths %d", k1, d1)
	}
	r.clk.Add(2 * time.Minute)
	if k2, _ := total(); k2 <= k1/2 {
		t.Errorf("kills fell from %d to %d inside a match", k1, k2)
	}
	st, _ := r.c.Status(ctx)
	if st.Factions[0].Score+st.Factions[1].Score+st.Factions[2].Score == 0 {
		t.Error("no faction scored in five minutes")
	}

	// The match is ten minutes: past it, the rotation moved and the counters restarted.
	r.clk.Add(5*time.Minute + 10*time.Second)
	next, err := r.c.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if next.Rotation.NowIndex != first.Rotation.NextIndex || next.Map == first.Map {
		t.Errorf("after the match: %+v, before %+v", next.Rotation, first.Rotation)
	}
	if k, d := total(); k > k1 || d > d1 {
		t.Errorf("the counters did not restart: %d/%d after the match end", k, d)
	}

	// Players come and go; some come back within minutes with the game's counters restarted.
	seen := map[string]time.Time{}
	gone := map[string]time.Time{}
	rejoined, joined := 0, 0
	for range 360 { // 30 minutes of 5-second polls
		r.clk.Add(5 * time.Second)
		p, err := r.c.Players(ctx)
		if err != nil {
			t.Fatal(err)
		}
		on := map[string]bool{}
		for _, pl := range p.Players {
			id := string(pl.SteamID)
			on[id] = true
			if _, was := seen[id]; !was {
				joined++
			}
			if at, ok := gone[id]; ok {
				if r.clk.Now().Sub(at) <= 6*time.Minute {
					rejoined++
				}
				delete(gone, id)
			}
			seen[id] = r.clk.Now()
		}
		for id := range seen {
			if _, ok := gone[id]; !ok && !on[id] {
				gone[id] = r.clk.Now()
			}
		}
		if c := len(p.Players); c > 30+5 || c < 15 {
			t.Errorf("population %d drifted far from the profile's 30", c)
		}
	}
	if joined <= 30 || rejoined == 0 {
		t.Errorf("over 30 minutes: %d distinct players, %d rejoins", joined, rejoined)
	}
}

func TestDeterministic(t *testing.T) {
	read := func() string {
		// Forty minutes of joins, leaves, rejoins and match ends, read every five.
		r := newRig(t, Options{Profile: busy(), Seed: 42})
		var all strings.Builder
		for range 8 {
			r.clk.Add(5 * time.Minute)
			b, err := r.c.Get(context.Background(), wardogs.Route{Method: "GET", Segments: []string{"v1", "players"}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			all.Write(b)
		}
		return all.String()
	}
	if a, b := read(), read(); a != b {
		t.Error("the same seed and clock gave two worlds")
	}
}

func TestModeration(t *testing.T) {
	r := newRig(t, Options{Profile: busy(), Seed: 4})
	ctx := context.Background()
	p, err := r.c.Players(ctx)
	if err != nil || len(p.Players) < 3 {
		t.Fatalf("players: %v %v", p, err)
	}
	a, b, c := p.Players[0], p.Players[1], p.Players[2]
	on := func(id wardogs.SteamID) bool {
		p, err := r.c.Players(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return slices.ContainsFunc(p.Players, func(x wardogs.Player) bool { return x.SteamID == id })
	}

	if err := r.c.Message(ctx, a.SteamID, "hello"); err != nil {
		t.Errorf("message: %v", err)
	}
	if err := r.c.Message(ctx, a.SteamID, strings.Repeat("x", 257)); !wardogs.IsCode(err, "message_too_long") {
		t.Errorf("a long message: %v", err)
	}
	if res, err := r.c.Broadcast(ctx, "restart in 5"); err != nil || !res.OK {
		t.Errorf("broadcast: %+v %v", res, err)
	}
	other := "Valkyra"
	if a.Faction == other {
		other = "Manticore"
	}
	if err := r.c.MovePlayer(ctx, a.SteamID, other); err != nil {
		t.Errorf("move: %v", err)
	}
	if err := r.c.MovePlayer(ctx, a.SteamID, "Nobody"); !wardogs.IsCode(err, "invalid_faction") {
		t.Errorf("a move to no faction: %v", err)
	}
	p, _ = r.c.Players(ctx)
	if i := slices.IndexFunc(p.Players, func(x wardogs.Player) bool { return x.SteamID == a.SteamID }); i < 0 || p.Players[i].Faction != other {
		t.Errorf("after the move: %+v", p.Players)
	}
	if err := r.c.Kill(ctx, a.SteamID); err != nil {
		t.Errorf("kill: %v", err)
	}
	if err := r.c.Kick(ctx, a.SteamID, "spawn camping"); err != nil || on(a.SteamID) {
		t.Errorf("kick: %v, still on %v", err, on(a.SteamID))
	}
	if err := r.c.Kick(ctx, a.SteamID, "again"); !wardogs.IsCode(err, "player_not_found") {
		t.Errorf("kicking a player who left: %v", err)
	}
	if err := r.c.Ban(ctx, b.SteamID, "cheating"); err != nil || on(b.SteamID) {
		t.Errorf("ban: %v", err)
	}
	if err := r.c.Ban(ctx, a.SteamID, ""); !wardogs.IsCode(err, "player_not_found") {
		t.Errorf("banning a player who is not on: %v", err)
	}
	bans, err := r.c.Bans(ctx)
	if err != nil || bans.Count != 1 || bans.Bans[0].SteamID != b.SteamID || bans.Bans[0].BannedBy != "rcon" || bans.Bans[0].Reason != "cheating" {
		t.Errorf("bans: %+v %v", bans, err)
	}
	if at, ok := bans.Bans[0].BannedAt(); !ok || !at.Equal(r.clk.Now().Truncate(time.Millisecond)) {
		t.Errorf("banned at %v", bans.Bans[0].BannedAtUTC)
	}
	// A banned player never comes back while the ban stands.
	for range 120 {
		r.clk.Add(30 * time.Second)
		if on(b.SteamID) {
			t.Fatal("a banned player rejoined")
		}
	}
	if err := r.c.Unban(ctx, b.SteamID); err != nil {
		t.Errorf("unban: %v", err)
	}
	if err := r.c.Unban(ctx, c.SteamID); !wardogs.IsCode(err, "ban_not_found") {
		t.Errorf("unbanning a player with no ban: %v", err)
	}
}

// TestConfigRoundTrip runs on CL-509546, the build whose schema has the [WDServerFeed] section.
func TestConfigRoundTrip(t *testing.T) {
	r := newRig(t, Options{Profile: busy(), Seed: 5, Build: "CL-509546"})
	ctx := context.Background()
	doc, err := r.c.Config(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res, err := r.c.ValidateConfig(ctx, doc.Text); err != nil || !res.OK || len(res.Changed) != 0 {
		t.Errorf("validating the document as it is: %+v %v", res, err)
	}
	bad := strings.Replace(doc.Text, "ScorePeriod=24", "ScorePeriod=12", 1)
	if res, err := r.c.ValidateConfig(ctx, bad); !errors.Is(err, wardogs.ErrConfigRejected) || len(res.Errors) != 1 || res.Errors[0].Key != "ScorePeriod" {
		t.Errorf("a score period outside the band: %+v %v", res, err)
	}

	// An identity change applies at once; a score period at the next match; the feed at the
	// next restart. Bans go in the document's list; an unknown key is stripped.
	d := wardogs.ParseDoc(doc.Text)
	setValue(d.Section(secSession), "ServerName", "Demo | #1")
	setValue(d.Section(secKOTH), "ScorePeriod", "27")
	setValue(d.Section(secRCON), "Port", "9999")
	setValue(d.Section(secSession), "MadeUpKey", "1")
	d.Section(secSession).SetArray(keyBanned, []string{SteamID(998)})
	d.SetSection(wardogs.DocSection{Name: secFeed, Lines: []wardogs.DocLine{{Key: "Url", Value: "http://127.0.0.1:1"}, {Key: "Token", Value: "feed"}}})
	res, err := r.c.PutConfig(ctx, d.String(), doc.Revision, wardogs.ApplyOptions{})
	if err != nil {
		t.Fatalf("put: %+v %v", res, err)
	}
	if !res.OK || res.Revision == doc.Revision || len(res.Stripped) != 1 || res.Stripped[0].Key != "MadeUpKey" || len(res.Warnings) != 1 {
		t.Errorf("put: %+v", res)
	}
	when := map[string]string{}
	for _, o := range res.Outcomes {
		when[o.Section] = o.State
	}
	if when[secSession] != wardogs.AppliesNow || when[secKOTH] != wardogs.AppliesNextMatch || when[secFeed] != wardogs.AppliesNextRestart {
		t.Errorf("outcomes: %v", when)
	}
	if _, err := r.c.PutConfig(ctx, d.String(), doc.Revision, wardogs.ApplyOptions{}); !errors.Is(err, wardogs.ErrRevisionMismatch) {
		t.Errorf("a write on a stale revision: %v", err)
	}
	after, err := r.c.Config(ctx)
	if err != nil || after.Revision != res.Revision || strings.Contains(after.Text, "MadeUpKey") || !strings.Contains(after.Text, "Port=7789") {
		t.Errorf("after the write: %v\n%s", err, after.Text)
	}
	st, _ := r.c.Status(ctx)
	if st.ServerName != "Demo | #1" || st.ScoreTick.Current != 24 {
		t.Errorf("status right after: %q, score tick %d", st.ServerName, st.ScoreTick.Current)
	}
	bans, _ := r.c.Bans(ctx)
	if bans.Count != 1 || bans.Bans[0].BannedBy != "config" || string(bans.Bans[0].SteamID) != SteamID(998) {
		t.Errorf("a ban from the document: %+v", bans)
	}
	r.clk.Add(11 * time.Minute)
	if st, _ := r.c.Status(ctx); st.ScoreTick.Current != 27 {
		t.Errorf("the next match's score tick: %d", st.ScoreTick.Current)
	}
	r.sim.mu.Lock()
	feed := r.sim.live.feedURL
	r.sim.mu.Unlock()
	if feed != "" {
		t.Error("the feed was read before a restart")
	}

	// Clearing the list drops it from the document, as the server does with an empty array.
	d2 := wardogs.ParseDoc(after.Text)
	d2.Section(secSession).SetArray(keyBanned, nil)
	res2, err := r.c.PutConfig(ctx, d2.String(), after.Revision, wardogs.ApplyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	final, _ := r.c.Config(ctx)
	if final.Revision != res2.Revision || strings.Contains(final.Text, keyBanned) {
		t.Errorf("an emptied list stays in the document:\n%s", final.Text)
	}
}

func TestRestartReadsTheFeedSection(t *testing.T) {
	p := busy()
	hour := 21
	p.RestartHour = &hour
	r := newRig(t, Options{Profile: p, Seed: 6, Build: "CL-509546"})
	ctx := context.Background()
	doc, _ := r.c.Config(ctx)
	d := wardogs.ParseDoc(doc.Text)
	d.SetSection(wardogs.DocSection{Name: secFeed, Lines: []wardogs.DocLine{{Key: "Url", Value: "http://127.0.0.1:1"}, {Key: "Token", Value: "feed"}}})
	if _, err := r.c.PutConfig(ctx, d.String(), doc.Revision, wardogs.ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	h1, _ := r.c.Health(ctx)
	r.clk.Add(time.Hour + time.Minute) // past 21:00
	h2, _ := r.c.Health(ctx)
	if h2.UptimeSeconds >= h1.UptimeSeconds+3600 || h2.UptimeSeconds > 120 {
		t.Errorf("uptime %d then %d: no restart", h1.UptimeSeconds, h2.UptimeSeconds)
	}
	r.sim.mu.Lock()
	feed := r.sim.live.feedURL
	r.sim.mu.Unlock()
	if feed != "http://127.0.0.1:1" {
		t.Errorf("after the restart the feed is %q", feed)
	}
}

func TestTokenThrottle(t *testing.T) {
	r := newRig(t, Options{Profile: busy(), Seed: 7})
	ctx := context.Background()
	// The client learns the routes first: from Season 02 that read needs the token too, and the
	// lock below would refuse it.
	if _, err := r.c.Capabilities(ctx); err != nil {
		t.Fatal(err)
	}
	wrong, err := wardogs.New(r.srv.URL, wardogs.Options{Token: "wrong", Now: r.clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wrong.Status(ctx); !errors.Is(err, wardogs.ErrTokenRefused) {
		t.Fatalf("a wrong token: %v", err)
	}
	// do answers a status read's code and Retry-After.
	do := func(token string) (int, string) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, r.srv.URL+"/v1/status", nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode, resp.Header.Get("Retry-After")
	}
	if code, _ := do(""); code != http.StatusUnauthorized {
		t.Errorf("the second strike: %d", code)
	}
	if code, retry := do("also-wrong"); code != http.StatusTooManyRequests || retry != "60" {
		t.Errorf("the third strike: %d, Retry-After %q", code, retry)
	}
	if code, _ := do(testToken); code != http.StatusTooManyRequests {
		t.Errorf("the right token while locked: %d", code)
	}
	if h, err := r.c.Health(ctx); err != nil || h.Status != "ok" {
		t.Errorf("health is public, locked or not: %v", err)
	}
	r.clk.Add(30 * time.Second)
	if code, retry := do(testToken); code != http.StatusTooManyRequests || retry != "30" {
		t.Errorf("half way: %d, Retry-After %q", code, retry)
	}
	// The client keeps to the Retry-After it was given.
	if _, err := r.c.Status(ctx); err == nil {
		t.Error("the client got through a lock")
	} else if _, err := r.c.Status(ctx); !errors.As(err, new(*wardogs.RateLimitedError)) {
		t.Errorf("the client called again inside its Retry-After: %v", err)
	}
	r.clk.Add(31 * time.Second)
	if _, err := r.c.Status(ctx); err != nil {
		t.Errorf("after the lock: %v", err)
	}
	a, _ := r.c.Audit(ctx, 500)
	if !slices.ContainsFunc(a.Entries, func(e wardogs.AuditEntry) bool { return e.Event == "AUTH_FAIL" }) {
		t.Error("the admin log has no failed token")
	}
}

// TestFeed: the batches read as the feed's wire format (games/wardogs/feed.go), the server's
// start first; Season 02 posts to the Url as given, CL-509546 appends the path.
func TestFeed(t *testing.T) {
	for _, tc := range []struct {
		build, url, path string
	}{
		{"CL-516586", "/api/ingest/events", wardogs.FeedPath},
		{"CL-516586", "/hooks/feed", "/hooks/feed"},
		{"CL-509546", "/", wardogs.FeedPath},
	} {
		t.Run(tc.build+tc.url, func(t *testing.T) {
			type batch struct {
				auth, path string
				body       wardogs.FeedBatch
			}
			got := make(chan batch, 100)
			sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				b, err := wardogs.ParseFeedBatch(body)
				if err != nil {
					t.Errorf("a batch the feed's reader refuses: %v: %s", err, body)
				}
				got <- batch{auth: r.Header.Get("Authorization"), path: r.URL.Path, body: b}
			}))
			t.Cleanup(sink.Close)
			p := busy()
			p.KillsPerMinute = 6
			sim, err := New(Options{Token: testToken, Profile: p, Seed: 8, Build: tc.build, FeedURL: sink.URL + tc.url, FeedToken: "feed-token"})
			if err != nil {
				t.Fatal(err)
			}
			if target := sim.FeedTarget(); target != sink.URL+tc.path {
				t.Errorf("target = %q", target)
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- sim.Run(ctx) }()
			t.Cleanup(func() { cancel(); <-done })
			first, kills, joins := true, 0, 0
			for kills == 0 || joins == 0 {
				select {
				case b := <-got:
					if b.auth != "Bearer feed-token" || b.path != tc.path || len(b.body.Events) < 1 || len(b.body.Events) > feedBatch ||
						!strings.HasPrefix(b.body.ServerID, "00000000-0000-4000-8000-") || b.body.ServerName != DefaultServerName {
						t.Fatalf("batch: %+v", b)
					}
					for i, e := range b.body.Events {
						if e.EventID == "" || !e.HasTime {
							t.Errorf("event %+v", e)
						}
						if first && i < 2 {
							m, err := e.MatchState()
							if err != nil || m.MapName == "" || !strings.HasPrefix(m.State, "MatchState.PreMatch") {
								t.Errorf("the start's event %d: %+v %v", i, m, err)
							}
						}
						if e.Type == wardogs.FeedPlayerJoined || e.Type == wardogs.FeedPlayerLeft {
							pc, err := e.PlayerChange()
							if err != nil || !strings.HasPrefix(string(pc.PlayerSteamID), StandInSteamIDPrefix) || pc.PlayerName == "" ||
								pc.Players == nil || pc.MatchID == "" || pc.MapName == "" || !strings.HasPrefix(pc.PlayerID, "00000000-0000-4000-8001-") {
								t.Errorf("%s %s: %v", e.Type, e.Raw, err)
							}
							if e.Type == wardogs.FeedPlayerJoined {
								joins++
							}
						}
						if e.Type == wardogs.FeedKilled {
							k, err := e.Kill()
							if err != nil || !strings.HasPrefix(string(k.VictimSteamID), StandInSteamIDPrefix) || k.MatchID == "" || k.MapName == "" ||
								(k.KillerSteamID != "" && (k.Distance == nil || k.Cause == "" || k.KillerName == "")) {
								t.Errorf("killed %s: %v", e.Raw, err)
							}
							kills++
						}
					}
					first = false
				case <-time.After(30 * time.Second):
					t.Fatalf("no kill or no join reached the sink (%d kills, %d joins)", kills, joins)
				}
			}
		})
	}
}

// TestSeason02FeedIsFileOnly: from CL-516586 the feed is set in the server's own file. A
// [WDServerFeed] written over RCON is stripped as an unknown section and no restart turns it on;
// the host's file (FeedURL here) does, and GET /v1/feed says so. The recording's own redacted
// feed section is no feed.
func TestSeason02FeedIsFileOnly(t *testing.T) {
	p := busy()
	hour := 21
	p.RestartHour = &hour
	r := newRig(t, Options{Profile: p, Seed: 9, Build: "CL-516586"})
	ctx := context.Background()
	doc, err := r.c.Config(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := doc.Schema(secFeed); ok {
		t.Fatalf("CL-516586's schema has %s", secFeed)
	}
	d := wardogs.ParseDoc(doc.Text)
	d.SetSection(wardogs.DocSection{Name: secFeed, Lines: []wardogs.DocLine{{Key: "Url", Value: "http://127.0.0.1:1"}, {Key: "Token", Value: "feed"}}})
	res, err := r.c.PutConfig(ctx, d.String(), doc.Revision, wardogs.ApplyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(res.Stripped, func(s wardogs.Stripped) bool { return s.Section == secFeed && s.Reason == "unknown_section" }) {
		t.Errorf("the feed section was not stripped: %+v", res.Stripped)
	}
	r.clk.Add(time.Hour + time.Minute) // past 21:00
	if f, err := r.c.Feed(ctx); err != nil || f.Enabled {
		t.Errorf("the feed after a restart: %+v %v", f, err)
	}

	host := newRig(t, Options{Profile: busy(), Seed: 9, Build: "CL-516586", FeedURL: "http://127.0.0.1:1", FeedToken: "feed"})
	if f, err := host.c.Feed(ctx); err != nil || !f.Enabled || f.EndpointCount != 1 {
		t.Errorf("the feed from the host's file: %+v %v", f, err)
	}
}

func TestNewNeedsAToken(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Error("a server with no token")
	}
	if _, err := New(Options{Token: "x", Build: "CL-1"}); err == nil {
		t.Error("a build that is not recorded")
	}
}
