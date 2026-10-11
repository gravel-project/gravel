// Package demo is a demo hub's fake history (gravel#112): members with linked Discord and Steam
// identities, past matches on every War Dogs server the hub has, and moderation with bans, made
// up from a calibration profile so the boards, match lists, profiles and moderation pages look as
// they will with a community on them. The live half is the simulated server
// (games/wardogs/wardogsim), whose made-up players are the same stand-in SteamIDs.
//
// Generate is pure: the same options give the same history. Seed writes it through the store,
// which refuses any database that holds a user who signed in or any history at all
// (store.SeedDemo); Seed itself refuses a hub that is not a demo. Production gets no fake rows:
// audit_log is append-only, and pseudonyms and matches are permanent.
package demo

import (
	"cmp"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/gravel-project/gravel/games/wardogs/wardogsim"
	"github.com/gravel-project/gravel/internal/stats"
	"github.com/gravel-project/gravel/internal/store"
)

// Defaults and limits of a seed.
const (
	DefaultDays    = 90
	DefaultMembers = 40
	MaxDays        = 366
	// MinMatchPlayers and MaxMatchPlayers bound a seeded match's lobby.
	MinMatchPlayers = 10
	MaxMatchPlayers = 40
	// game is the game the seed writes matches for: the one the simulator plays.
	game = "wardogs"
)

// Errors a seed refuses with, before anything is written.
var (
	// ErrNotDemo is a hub whose hub.yaml does not say demo: true.
	ErrNotDemo = errors.New("demo: hub.yaml does not say demo: true; the seed writes made-up members and matches and runs only on a demo hub")
	// ErrNoServers is a hub with no War Dogs server to give matches to.
	ErrNoServers = errors.New("demo: the hub has no War Dogs server; apply servers.yaml first (gravel-hub servers apply), then seed: a seed runs once")
)

// Options shape a seeded history.
type Options struct {
	// Days is how far back the history goes.
	Days int
	// Members is how many made-up members there are; they are the profile's first regulars.
	Members int
	// Seed makes the history repeatable.
	Seed uint64
	// Profile is the calibration the history takes its shapes from; the zero value is
	// wardogsim.DefaultProfile.
	Profile wardogsim.Profile
	// Now is where the history ends; matches end before it.
	Now time.Time
}

// Summary is what a seed wrote.
type Summary struct {
	Members, Moderators, ShowingNames int
	// Matches and Rows are per server.
	Matches, Rows map[string]int
	AuditEntries  int
	Bans, Lifted  int
	// Skipped are servers of another game, which get no matches.
	Skipped []string
}

// Store is what a seed needs from the database.
type Store interface {
	ListManagedServers(ctx context.Context, orgID uuid.UUID, includeRemoved bool) ([]store.ManagedServer, error)
	SeedDemo(ctx context.Context, orgID uuid.UUID, h store.DemoHistory) error
}

// Seed generates a history for the organization's servers and writes it. isDemo is hub.yaml's
// demo key: without it nothing is read or written. The store refuses a database that is not
// empty (store.ErrNotADemoDatabase).
func Seed(ctx context.Context, st Store, orgID uuid.UUID, isDemo bool, opt Options) (Summary, []store.PlayerKey, error) {
	if !isDemo {
		return Summary{}, nil, ErrNotDemo
	}
	servers, err := st.ListManagedServers(ctx, orgID, false)
	if err != nil {
		return Summary{}, nil, err
	}
	// A seed runs once, so one before the servers are applied would leave the hub without matches.
	if !slices.ContainsFunc(servers, func(s store.ManagedServer) bool { return s.GameID == game }) {
		return Summary{}, nil, ErrNoServers
	}
	h, sum, keys, err := Generate(servers, opt)
	if err != nil {
		return Summary{}, nil, err
	}
	if err := st.SeedDemo(ctx, orgID, h); err != nil {
		return Summary{}, nil, err
	}
	return sum, keys, nil
}

// Generate makes the history: members, then for each War Dogs server its bans and other
// moderation, then its matches, which leave out a player while a ban stands. It returns the
// identities that played, for their pseudonyms.
func Generate(servers []store.ManagedServer, opt Options) (store.DemoHistory, Summary, []store.PlayerKey, error) {
	if opt.Days == 0 {
		opt.Days = DefaultDays
	}
	if opt.Profile.Version == 0 {
		opt.Profile = wardogsim.DefaultProfile()
	}
	p := opt.Profile
	if err := p.Validate(); err != nil {
		return store.DemoHistory{}, Summary{}, nil, err
	}
	switch {
	case opt.Days < 1 || opt.Days > MaxDays:
		return store.DemoHistory{}, Summary{}, nil, fmt.Errorf("demo: %d days is not 1–%d", opt.Days, MaxDays)
	case opt.Members < 0 || opt.Members > p.Regulars:
		return store.DemoHistory{}, Summary{}, nil, fmt.Errorf("demo: %d members is not 0–%d (the profile's regulars)", opt.Members, p.Regulars)
	case opt.Now.IsZero():
		return store.DemoHistory{}, Summary{}, nil, errors.New("demo: the history needs an end (Options.Now)")
	}
	lobby, err := wardogsim.RecordedLobby("")
	if err != nil {
		return store.DemoHistory{}, Summary{}, nil, err
	}
	g := &gen{opt: opt, p: p, lobby: lobby, rng: rand.New(rand.NewPCG(opt.Seed, opt.Seed^0x5eed5eed5eed5eed)),
		end: opt.Now.UTC().Truncate(time.Minute), played: map[store.PlayerKey]bool{}}
	g.start = g.end.Add(-time.Duration(opt.Days) * 24 * time.Hour)
	g.pool = wardogsim.Regulars(p, g.rng)
	g.members()

	sum := Summary{Members: len(g.h.Members), Matches: map[string]int{}, Rows: map[string]int{}}
	for _, m := range g.h.Members {
		if m.ShowName {
			sum.ShowingNames++
		}
		if len(m.User.Roles) > 0 {
			sum.Moderators++
		}
	}
	ordered := slices.SortedFunc(slices.Values(servers), func(a, b store.ManagedServer) int { return cmp.Compare(a.ID, b.ID) })
	g.h.BanLists = map[string]time.Time{}
	var events []event
	busy := 1.0
	for _, srv := range ordered {
		if srv.GameID != game {
			sum.Skipped = append(sum.Skipped, srv.ID)
			continue
		}
		g.h.BanLists[srv.ID] = g.start
		evs := g.moderation(srv.ID)
		events = append(events, evs...)
		before := len(g.h.Matches)
		rows := g.matches(srv, busy, evs)
		sum.Matches[srv.ID], sum.Rows[srv.ID] = len(g.h.Matches)-before, rows
		busy *= 0.65 // a second server fills less than the first
	}
	g.writeModeration(events)
	sum.AuditEntries = len(g.h.Moderation)
	for _, b := range g.h.Bans {
		sum.Bans++
		if b.LiftEntry >= 0 {
			sum.Lifted++
		}
	}
	keys := make([]store.PlayerKey, 0, len(g.played))
	for k := range g.played {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b store.PlayerKey) int { return cmp.Compare(a.Subject, b.Subject) })
	return g.h, sum, keys, nil
}

type gen struct {
	opt        Options
	p          wardogsim.Profile
	lobby      wardogsim.Lobby
	rng        *rand.Rand
	start, end time.Time
	pool       []wardogsim.Player
	moderators []uuid.UUID
	h          store.DemoHistory
	played     map[store.PlayerKey]bool
}

// DiscordSubject is the n-th demo member's Discord id: a number below the first id Discord's
// clock could have issued to an account (2^22 is one millisecond into its epoch), so it is
// nobody's.
func DiscordSubject(n int) string { return fmt.Sprint(1_000_000 + n) }

// callsigns are the members' Discord display names; past the list they are numbered.
var callsigns = []string{
	"Atlas", "Banshee", "Briar", "Cipher", "Cobalt", "Corvus", "Dakota", "Dune", "Ember", "Fable",
	"Gambit", "Halcyon", "Indigo", "Juno", "Kestrel", "Lumen", "Marlowe", "Nimbus", "Orion", "Pax",
	"Quasar", "Raven", "Sierra", "Tarot", "Umber", "Vesper", "Willow", "Xeno", "Yarrow", "Zephyr",
	"Aster", "Bramble", "Cinder", "Drover", "Echo", "Ferro", "Gale", "Harbor", "Ivo", "Jasper",
}

// uuid is a random version-4 UUID from the generator, so a seed's ids repeat with it.
func (g *gen) uuid() uuid.UUID {
	var u uuid.UUID
	binary.BigEndian.PutUint64(u[:8], g.rng.Uint64())
	binary.BigEndian.PutUint64(u[8:], g.rng.Uint64())
	u[6] = (u[6] & 0x0f) | 0x40
	u[8] = (u[8] & 0x3f) | 0x80
	return u
}

func (g *gen) between(a, b time.Time) time.Time {
	if !b.After(a) {
		return a
	}
	return a.Add(time.Duration(g.rng.Int64N(int64(b.Sub(a))))).Truncate(time.Second)
}

// members makes the profile's first regulars members: they joined across the first half of the
// window, linked Steam soon after, the first three moderate, and most show their names.
func (g *gen) members() {
	for i := range g.opt.Members {
		n := i + 1
		reg := &g.pool[i]
		reg.Activity *= 2 // members play more than the passers-by
		name := callsigns[i%len(callsigns)]
		if i >= len(callsigns) {
			name = fmt.Sprintf("%s %d", name, i/len(callsigns)+1)
		}
		joined := g.between(g.start, g.start.Add(g.end.Sub(g.start)/2))
		linked := joined.Add(time.Duration(g.rng.IntN(72*60)) * time.Minute)
		last := g.between(g.end.Add(-7*24*time.Hour), g.end)
		u := store.User{ID: g.uuid(), DisplayName: name, CreatedAt: joined, LastLoginAt: &last}
		if n <= 3 {
			u.Roles = []string{store.RoleModerator}
			g.moderators = append(g.moderators, u.ID)
		}
		g.h.Members = append(g.h.Members, store.DemoMember{
			User:     u,
			ShowName: g.rng.Float64() < 0.6,
			Identities: []store.Identity{
				{Provider: "discord", Subject: DiscordSubject(n), DisplayName: name, VerifiedAt: joined, LinkedAt: joined, LastLoginAt: &last},
				{Provider: "steam", Subject: reg.SteamID, DisplayName: reg.Name, VerifiedAt: linked, LinkedAt: linked},
			},
		})
	}
}

// event is a moderation action before it is written: entries are numbered once every server's are
// in time order.
type event struct {
	at      time.Time
	server  string
	action  string
	target  string // a SteamID; "" for a broadcast
	reason  string
	detail  map[string]any
	outcome string
	// ban numbers a ban on its server; lifts is the ban an unban lifts. 0 is neither.
	ban, lifts int
}

var (
	reasons    = []string{"Spawn camping after a warning", "Team killing", "Toxic chat", "AFK in a full server", "Name breaks the rules", "Exploiting a map bug"}
	banReasons = []string{"Cheating, reviewed on video", "Repeated team killing", "Slurs in chat", "Ban evasion", "Harassing players"}
	whispers   = []string{"Please keep the chat friendly.", "Heads up: the next team kill is a kick.", "Your name breaks the rules; change it before the next match, please.", "Thanks for seeding!"}
	broadcasts = []string{"Server restart in 10 minutes.", "Seeding night tonight: bring a friend!", "New rotation from the next map.", "Be kind in chat. Rules are on the Discord."}
)

// moderation makes a server's actions, in time order: about one and a half a day, by a
// moderator, mostly kicks and whispers to passers-by (never a member), some bans, a third of them
// lifted later on appeal.
func (g *gen) moderation(server string) []event {
	members := len(g.h.Members)
	if len(g.moderators) == 0 || members >= len(g.pool) {
		return nil
	}
	target := func() string { return g.pool[members+g.rng.IntN(len(g.pool)-members)].SteamID }
	var evs []event
	bans := 0
	for t := g.start; ; {
		t = t.Add(time.Duration(g.rng.ExpFloat64() * 16 * float64(time.Hour)))
		if !t.Before(g.end) {
			break
		}
		ev := event{at: t.Truncate(time.Second), server: server, outcome: "ok", detail: map[string]any{}}
		switch x := g.rng.Float64(); {
		case x < 0.35:
			ev.action, ev.target, ev.reason = "kick", target(), reasons[g.rng.IntN(len(reasons))]
			if g.rng.Float64() < 0.08 {
				ev.outcome = "player_not_found"
			}
		case x < 0.55:
			ev.action, ev.target = "message", target()
			ev.detail["message"] = whispers[g.rng.IntN(len(whispers))]
		case x < 0.70:
			ev.action = "broadcast"
			ev.detail["message"] = broadcasts[g.rng.IntN(len(broadcasts))]
		case x < 0.82:
			ev.action, ev.target = "move_player", target()
			ev.detail["team"] = g.lobby.Factions[g.rng.IntN(len(g.lobby.Factions))].Name
			ev.detail["respawn"] = true
		default:
			who := target()
			if bannedAt(evs, who, ev.at) {
				continue
			}
			bans++
			ev.action, ev.target, ev.reason, ev.ban = "ban", who, banReasons[g.rng.IntN(len(banReasons))], bans
			if g.rng.Float64() < 0.33 {
				lift := ev.at.Add(time.Duration(1+g.rng.IntN(20*24)) * time.Hour)
				if lift.Before(g.end) {
					evs = append(evs, event{at: lift, server: server, action: "unban", target: ev.target, reason: "Appeal accepted",
						outcome: "ok", lifts: bans, detail: map[string]any{}})
				}
			}
		}
		evs = append(evs, ev)
	}
	slices.SortStableFunc(evs, func(a, b event) int { return a.at.Compare(b.at) })
	return evs
}

// bannedAt reports whether a player was under a ban on the server of evs at t.
func bannedAt(evs []event, steamID string, t time.Time) bool {
	for _, e := range evs {
		if e.action != "ban" || e.target != steamID || e.at.After(t) {
			continue
		}
		lifted := slices.ContainsFunc(evs, func(u event) bool { return u.action == "unban" && u.lifts == e.ban && !u.at.After(t) })
		if !lifted {
			return true
		}
	}
	return false
}

// writeModeration numbers every server's actions in time order as audit entries and records the
// bans they made and lifted.
func (g *gen) writeModeration(events []event) {
	slices.SortStableFunc(events, func(a, b event) int { return a.at.Compare(b.at) })
	type banKey struct {
		server string
		ban    int
	}
	bans := map[banKey]int{} // a ban → its position in h.Bans
	for at, e := range events {
		actor := g.moderators[g.rng.IntN(len(g.moderators))]
		detail, _ := json.Marshal(e.detail)
		finished := e.at.Add(time.Duration(200+g.rng.IntN(1500)) * time.Millisecond)
		entry := store.AuditEntry{At: e.at, ActorUserID: &actor, ServerID: e.server, Action: e.action, Reason: e.reason,
			Detail: detail, RequestID: fmt.Sprintf("demo-%d", at+1), Outcome: e.outcome, FinishedAt: &finished}
		if e.target != "" {
			entry.TargetProvider, entry.TargetSubject = "steam", e.target
		}
		g.h.Moderation = append(g.h.Moderation, entry)
		switch e.action {
		case "ban":
			bans[banKey{e.server, e.ban}] = len(g.h.Bans)
			g.h.Bans = append(g.h.Bans, store.DemoBan{ServerID: e.server, Provider: "steam", Subject: e.target, Reason: e.reason, BanEntry: at, LiftEntry: -1})
		case "unban":
			if b, ok := bans[banKey{e.server, e.lifts}]; ok {
				g.h.Bans[b].LiftEntry = at
			}
		}
	}
}

// matches makes a server's history, start to end: a match whenever enough players are on for
// the hour, quiet stretches otherwise, and the occupancy of every minute anyone was on. busy
// scales the profile's population for this server. It returns the player rows written.
func (g *gen) matches(srv store.ManagedServer, busy float64, mod []event) int {
	loc := g.p.Location()
	dayFactor := map[string]float64{}
	rows, rot := 0, 0
	for t := g.start; t.Before(g.end); {
		day := t.In(loc).Format(time.DateOnly)
		f, ok := dayFactor[day]
		if !ok {
			f = 0.75 + g.rng.Float64()*0.45
			if wd := t.In(loc).Weekday(); wd == time.Friday || wd == time.Saturday {
				f *= 1.25
			}
			dayFactor[day] = f
		}
		n := int(math.Round(g.p.PopulationAt(t)*busy*f + g.rng.NormFloat64()*2))
		if n < MinMatchPlayers {
			// Too few for a match: a handful wait in the pre-match for ten minutes.
			for m := range 10 {
				if n > 0 && t.Add(time.Duration(m)*time.Minute).Before(g.end) {
					g.h.Occupancy = append(g.h.Occupancy, store.DemoOccupancy{ServerID: srv.ID, Minute: t.Add(time.Duration(m) * time.Minute), Fewest: max(0, n-1), Most: n})
				}
			}
			t = t.Add(10 * time.Minute)
			continue
		}
		n = min(n, MaxMatchPlayers)
		length := time.Duration((g.p.MatchMinutes.Min + g.rng.Float64()*(g.p.MatchMinutes.Max-g.p.MatchMinutes.Min)) * float64(time.Minute))
		end := t.Add(length).Truncate(time.Second)
		if !end.Before(g.end) {
			break
		}
		rows += g.match(srv, t, end, n, rot, mod)
		rot = (rot + 1) % len(g.lobby.Maps)
		t = end.Add(time.Duration(60+g.rng.IntN(120)) * time.Second).Truncate(time.Minute)
	}
	return rows
}

// match is one match of n players from the pool, by activity, none of them banned at the time.
func (g *gen) match(srv store.ManagedServer, start, end time.Time, n, rot int, mod []event) int {
	length := end.Sub(start)
	chosen := map[int]bool{}
	var players []wardogsim.Player
	var total float64
	for _, p := range g.pool {
		total += p.Activity
	}
	for tries := 0; len(players) < n && tries < n*20 && total > 0; tries++ {
		x := g.rng.Float64() * total
		for i, p := range g.pool {
			if chosen[i] {
				continue
			}
			if x -= p.Activity; x <= 0 {
				chosen[i] = true
				total -= p.Activity
				if !bannedAt(mod, p.SteamID, start) {
					players = append(players, p)
				}
				break
			}
		}
	}
	m := store.DemoMatch{Match: store.Match{ServerID: srv.ID, GameID: srv.GameID, StartedAt: start, EndedAt: &end,
		Map: g.lobby.Maps[rot%len(g.lobby.Maps)], RotationIndex: rot}}
	type span struct{ from, to time.Time }
	spans := make([]span, len(players))
	minutes := make([]float64, len(players))
	var weight float64
	weights := make([]float64, len(players))
	for i, p := range players {
		from, to := start, end
		if g.rng.Float64() < 0.3 {
			from = start.Add(time.Duration(g.rng.Float64() * 0.6 * float64(length)))
		}
		if g.rng.Float64() < 0.25 {
			to = from.Add(3*time.Minute + time.Duration(g.rng.Float64()*float64(end.Sub(from)-3*time.Minute)))
			if to.After(end) {
				to = end
			}
		}
		spans[i] = span{from.Truncate(time.Second), to.Truncate(time.Second)}
		minutes[i] = to.Sub(from).Minutes()
		weights[i] = minutes[i] / math.Sqrt(p.Skill)
		weight += weights[i]
	}
	kills := make([]int, len(players))
	allKills := 0
	for i, p := range players {
		kills[i] = wardogsim.Poisson(g.rng, g.p.KillsPerMinute*p.Skill*minutes[i])
		allKills += kills[i]
	}
	factions := g.lobby.Factions
	scores := make([]int64, len(factions))
	strength := make([]float64, len(factions))
	for i, p := range players {
		team := i % len(factions)
		deaths := 0
		if weight > 0 {
			deaths = wardogsim.Poisson(g.rng, float64(allKills)*weights[i]/weight)
		}
		scores[team] += int64(kills[i]) * 5
		strength[team] += p.Skill * minutes[i]
		m.Players = append(m.Players, store.MatchPlayer{Provider: "steam", Subject: p.SteamID, Team: factions[team].Name,
			Kills: kills[i], Deaths: deaths, SecondsOn: int(spans[i].to.Sub(spans[i].from).Seconds()), FirstSeen: spans[i].from, LastSeen: spans[i].to,
			Source: stats.SourceServerAdapter, Trust: srv.Trust})
		g.played[store.PlayerKey{Provider: "steam", Subject: p.SteamID}] = true
	}
	// The hill: each score period goes to a faction by its strength on the field.
	var all float64
	for _, s := range strength {
		all += s
	}
	for range int(length.Seconds() / 24) {
		x := g.rng.Float64() * all
		for i, s := range strength {
			if x -= s; x <= 0 {
				scores[i] += 100
				break
			}
		}
	}
	for i, f := range factions {
		m.Teams = append(m.Teams, store.TeamScore{Name: f.Name, Color: f.ColorHex, Score: scores[i]})
	}
	g.h.Matches = append(g.h.Matches, m)
	// Every minute of the match: the fewest and the most players on in it.
	for minute := start.Truncate(time.Minute); minute.Before(end); minute = minute.Add(time.Minute) {
		fewest, most := 0, 0
		for _, s := range spans {
			if !s.from.After(minute.Add(59*time.Second)) && !s.to.Before(minute) {
				most++
				if !s.from.After(minute) && !s.to.Before(minute.Add(59*time.Second)) {
					fewest++
				}
			}
		}
		if most > 0 {
			g.h.Occupancy = append(g.h.Occupancy, store.DemoOccupancy{ServerID: srv.ID, Minute: minute, Fewest: fewest, Most: most})
		}
	}
	return len(m.Players)
}
