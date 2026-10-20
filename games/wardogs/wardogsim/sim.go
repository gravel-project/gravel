package wardogsim

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"math"
	"math/rand/v2"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gravel-project/gravel/games/wardogs"
)

// tick is the simulation's step: the world moves on in five-second steps, however often it is
// read.
const tick = 5 * time.Second

// DefaultServerName is what a simulated server calls itself unless told otherwise.
const DefaultServerName = "Demo War Dogs server (made-up players)"

// maxCatchUp is the longest stretch the world catches up on in one go; after a longer pause (a
// suspended host) it jumps, as a server that was down would.
const maxCatchUp = 6 * time.Hour

// Options configure a simulated server.
type Options struct {
	// Token is the RCON password the protected routes accept as a bearer token. Required.
	Token string
	// Profile is the calibration; the zero value means DefaultProfile.
	Profile Profile
	// Seed makes the simulation repeatable: the same seed and the same clock give the same world.
	Seed uint64
	// Build is the recorded build to answer as (games/wardogs/testdata/<build>); "" is the newest.
	Build string
	// Config is the configuration document the server starts with; "" is the recording's. Its
	// RCON password is replaced by Token, as the real server's document holds its own.
	Config string
	// ServerName replaces the document's ServerName; "" keeps a Config's and gives the
	// recording's document DefaultServerName, since the recorded name is a real community's.
	ServerName string
	// FeedURL and FeedToken start the server with [WDServerFeed] set, as a host's own document
	// would; the hub writes the same section through PUT /v1/config, read at the next restart.
	FeedURL, FeedToken string
	// Now is the clock (tests); nil means time.Now.
	Now func() time.Time
	// HTTPClient posts the feed's batches; nil means one with a 5-second timeout.
	HTTPClient *http.Client
	// Logger hears about restarts, matches and feed failures; nil means slog.Default.
	Logger *slog.Logger
}

// Server is one simulated War Dogs server: the RCON API as the recorded build answers it, over a
// world of made-up players that changes with time (gravel#112). It is safe for concurrent use.
type Server struct {
	opt     Options
	profile Profile
	logger  *slog.Logger
	now     func() time.Time
	feed    *http.Client

	// From the recording, read once.
	files     fs.FS
	build     string
	caps      []byte
	routes    map[string]bool // advertised shapes
	tokenless map[string]bool // shapes the build answers without a token
	sections  []wardogs.Section
	rawSchema json.RawMessage
	factions  []wardogs.FactionScore
	maps      map[string]bool
	lightings map[string]bool
	serverID  string // the recording's stand-in, which the feed's batches name

	mu       sync.Mutex
	rng      *rand.Rand
	clock    time.Time // how far the world has moved
	booted   time.Time // the last restart
	restart  time.Time // the next restart; zero for never
	doc      wardogs.Doc
	revision string
	written  settings // the document as written
	live     settings // the document as read at the last restart (next-restart sections)
	regulars []Player
	online   []*player // in join order
	back     map[int]time.Time
	away     map[int]time.Time // a player who left stays away until then
	match    match
	rconBans []ban
	lighting string
	audit    []wardogs.AuditEntry
	auth     map[string]*peer
	events   []FeedEvent
	eventSeq uint64
	dropped  int
	inFlight int
	// delivery is the feed's own counts, as GET /v1/feed reports them.
	delivery struct {
		sent, failures, lastStatus int
		lastSuccess                time.Time
	}
}

// player is a regular who is on the server.
type player struct {
	*Player
	faction int
	kills   int
	deaths  int
	cash    int64
	leaveAt time.Time
}

// match is the match in progress, or the pre-match wait before one.
type match struct {
	id          string // the match's id in the feed's player events
	playing     bool
	index, next int // rotation positions
	entry       rotationEntry
	startedAt   time.Time
	endsAt      time.Time
	scores      []int64
	nextScore   time.Time
	scorePeriod int
	minPlayers  int
}

// ban is one ban made over RCON.
type ban struct {
	steamID, reason string
	at              time.Time
}

// New builds a simulated server with its world already populated for the profile's current
// hour. Nothing runs until Handler serves a request or Run is called.
func New(opt Options) (*Server, error) {
	if strings.TrimSpace(opt.Token) == "" {
		return nil, errors.New("wardogsim: a token is required: the protected routes would accept nothing")
	}
	profile := opt.Profile
	if profile.Version == 0 {
		profile = DefaultProfile()
	}
	if err := profile.Validate(); err != nil {
		return nil, err
	}
	s := &Server{opt: opt, profile: profile, logger: opt.Logger, now: opt.Now, feed: opt.HTTPClient,
		rng:  rand.New(rand.NewPCG(opt.Seed, opt.Seed^0x9e3779b97f4a7c15)),
		back: map[int]time.Time{}, away: map[int]time.Time{}, auth: map[string]*peer{}}
	if s.logger == nil {
		s.logger = slog.Default()
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.feed == nil {
		s.feed = &http.Client{Timeout: 5 * time.Second}
	}
	if err := s.load(opt.Build); err != nil {
		return nil, err
	}
	text := opt.Config
	if text == "" {
		var cfg wardogs.ConfigDocument
		if err := s.readJSON("v1/config.json", &cfg); err != nil {
			return nil, err
		}
		text = cfg.Text
	}
	doc := wardogs.ParseDoc(text)
	if sec := doc.Section(secRCON); sec != nil {
		setValue(sec, "Password", opt.Token)
	}
	name := opt.ServerName
	if name == "" && opt.Config == "" {
		name = DefaultServerName
	}
	if sec := doc.Section(secSession); sec != nil && name != "" {
		setValue(sec, "ServerName", name)
	}
	if opt.FeedURL != "" {
		doc.SetSection(wardogs.DocSection{Name: secFeed, Lines: []wardogs.DocLine{
			{Key: "Url", Value: strings.TrimRight(opt.FeedURL, "/")}, {Key: "Token", Value: opt.FeedToken}}})
	}
	s.setDoc(doc)
	s.live = s.written

	now := s.now()
	s.clock, s.booted, s.restart = now, now, profile.NextRestart(now)
	s.regulars = Regulars(profile, s.rng)
	s.enterRotation(0)
	s.started(now)
	// A server that has been up a while: the hour's population is on, and a match has begun.
	for range int(math.Round(math.Min(profile.PopulationAt(now), float64(s.capacity())))) {
		s.join(now, s.pick())
	}
	s.startIfReady(now)
	return s, nil
}

// load reads the recorded build's static answers.
func (s *Server) load(build string) error {
	var err error
	if build == "" {
		s.build, s.files, err = wardogs.NewestRecording()
	} else {
		s.build = build
		s.files, err = wardogs.Recording(build)
	}
	if err != nil {
		return fmt.Errorf("wardogsim: %w", err)
	}
	if s.caps, err = fs.ReadFile(s.files, "v1/capabilities.json"); err != nil {
		return fmt.Errorf("wardogsim: %w", err)
	}
	var id struct {
		ServerID string `json:"serverId"`
	}
	if b, err := fs.ReadFile(s.files, "v1/server-id.json"); err == nil && json.Unmarshal(b, &id) == nil {
		s.serverID = id.ServerID
	}
	if s.tokenless, err = wardogs.TokenlessShapes(s.files); err != nil {
		return fmt.Errorf("wardogsim: %w", err)
	}
	var caps wardogs.Capabilities
	if err := json.Unmarshal(s.caps, &caps); err != nil {
		return fmt.Errorf("wardogsim: capabilities: %w", err)
	}
	s.routes = map[string]bool{}
	for _, r := range caps.Routes {
		route, err := wardogs.ParseRoute(r)
		if err != nil {
			return fmt.Errorf("wardogsim: capabilities: %w", err)
		}
		s.routes[route.Shape()] = true
	}
	var cfg struct {
		Sections json.RawMessage `json:"sections"`
	}
	if err := s.readJSON("v1/config.json", &cfg); err != nil {
		return err
	}
	s.rawSchema = cfg.Sections
	if err := json.Unmarshal(cfg.Sections, &s.sections); err != nil {
		return fmt.Errorf("wardogsim: config sections: %w", err)
	}
	var status wardogs.Status
	if err := s.readJSON("v1/status.json", &status); err != nil {
		return err
	}
	s.factions = status.Factions
	if len(s.factions) == 0 {
		s.factions = []wardogs.FactionScore{{Name: "Lonestar", ColorHex: "#4CB1EF"}, {Name: "Valkyra", ColorHex: "#FA503E"}}
	}
	var maps wardogs.MapCatalog
	if err := s.readJSON("v1/catalog/maps.json", &maps); err != nil {
		return err
	}
	s.maps = map[string]bool{}
	for _, m := range maps.Maps {
		s.maps[m.ID] = true
	}
	var lightings wardogs.LightingCatalog
	if err := s.readJSON("v1/catalog/lightings.json", &lightings); err != nil {
		return err
	}
	s.lightings = map[string]bool{}
	for _, l := range lightings.Lightings {
		s.lightings[l.ID] = true
	}
	return nil
}

func (s *Server) readJSON(name string, out any) error {
	b, err := fs.ReadFile(s.files, name)
	if err != nil {
		return fmt.Errorf("wardogsim: %s %w", s.build, err)
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("wardogsim: %s %s: %w", s.build, name, err)
	}
	return nil
}

// Build is the recorded build the server answers as.
func (s *Server) Build() string { return s.build }

// setDoc replaces the document as written.
func (s *Server) setDoc(d wardogs.Doc) {
	s.doc = d
	text := d.String()
	s.revision = revisionOf(text)
	s.written = read(d)
}

// capacity is the public slots: MaxPlayers (read at the last restart) less the reserved ones.
func (s *Server) capacity() int { return max(0, s.live.maxPlayers-s.written.reservedSlots) }

// advance moves the world on to now, a tick at a time. The caller holds mu.
func (s *Server) advance(now time.Time) {
	if now.Sub(s.clock) > maxCatchUp {
		s.clock = now.Add(-maxCatchUp)
	}
	for !s.clock.Add(tick).After(now) {
		s.clock = s.clock.Add(tick)
		s.step(s.clock)
	}
}

// step is one tick of the world at t.
func (s *Server) step(t time.Time) {
	if !s.restart.IsZero() && !t.Before(s.restart) {
		s.reboot(t)
	}
	// Leaving: a session that ran out; some of them come back to the same match shortly.
	for _, p := range slices.Clone(s.online) {
		if t.Before(p.leaveAt) {
			continue
		}
		s.leave(t, p)
		if s.rng.Float64() < s.profile.RejoinRate {
			s.back[p.N] = t.Add(time.Duration(60+s.rng.IntN(240)) * time.Second)
		} else {
			s.away[p.N] = t.Add(time.Duration(10+s.rng.IntN(240)) * time.Minute)
		}
	}
	for _, n := range slices.Sorted(maps.Keys(s.back)) { // in order, so a seed repeats
		if at := s.back[n]; !t.Before(at) {
			delete(s.back, n)
			if len(s.online) < s.capacity() && !s.isOnline(n) && !s.banned(SteamID(n)) {
				s.join(t, &s.regulars[n-1])
			}
		}
	}
	// Joining: the gap to the hour's population closes over a few minutes.
	target := math.Min(s.profile.PopulationAt(t), float64(s.capacity()))
	if gap := target - float64(len(s.online)); gap > 0 {
		for range Poisson(s.rng, gap*tick.Minutes()/3) {
			if len(s.online) >= s.capacity() {
				break
			}
			if p := s.pick(); p != nil {
				s.join(t, p)
			}
		}
	} else if gap < -2 && len(s.online) > 0 && s.rng.Float64() < -gap*tick.Minutes()/10 {
		p := s.online[s.rng.IntN(len(s.online))]
		s.leave(t, p)
		s.away[p.N] = t.Add(time.Duration(30+s.rng.IntN(180)) * time.Minute)
	}

	if !s.match.playing {
		s.startIfReady(t)
		return
	}
	if len(s.online) == 0 {
		// Everyone left: the match is over and the server waits again.
		s.ended(t)
		s.match.playing = false
		return
	}
	s.fight(t)
	if !t.Before(s.match.nextScore) {
		s.score()
		s.match.nextScore = t.Add(time.Duration(s.match.scorePeriod) * time.Second)
	}
	if !t.Before(s.match.endsAt) {
		s.endMatch(t)
	}
}

// pick is a regular who is not on, not banned and not away, by activity. When everyone free is
// away, it is the one whose time away ends first; nil when nobody can join.
func (s *Server) pick() *Player {
	var total float64
	free := make([]*Player, 0, len(s.regulars))
	var soonest *Player
	for i := range s.regulars {
		p := &s.regulars[i]
		if s.isOnline(p.N) || s.banned(p.SteamID) {
			continue
		}
		if _, returning := s.back[p.N]; returning {
			continue
		}
		if s.clock.Before(s.away[p.N]) {
			if soonest == nil || s.away[p.N].Before(s.away[soonest.N]) {
				soonest = p
			}
			continue
		}
		free = append(free, p)
		total += p.Activity
	}
	if len(free) == 0 {
		return soonest
	}
	x := s.rng.Float64() * total
	for _, p := range free {
		if x -= p.Activity; x <= 0 {
			return p
		}
	}
	return free[len(free)-1]
}

func (s *Server) isOnline(n int) bool {
	return slices.ContainsFunc(s.online, func(p *player) bool { return p.N == n })
}

func (s *Server) find(steamID string) *player {
	for _, p := range s.online {
		if p.SteamID == steamID {
			return p
		}
	}
	return nil
}

// banned reports a ban from the document or over RCON.
func (s *Server) banned(steamID string) bool {
	return slices.Contains(s.written.banned, steamID) || slices.ContainsFunc(s.rconBans, func(b ban) bool { return b.steamID == steamID })
}

// join puts a regular on the smallest faction with the game's counters at zero.
func (s *Server) join(t time.Time, p *Player) {
	if p == nil {
		return
	}
	counts := make([]int, len(s.factions))
	for _, o := range s.online {
		counts[o.faction]++
	}
	faction := 0
	for i, c := range counts {
		if c < counts[faction] {
			faction = i
		}
	}
	session := time.Duration(s.profile.SessionMinutes.draw(s.rng.Float64()) * float64(time.Minute))
	s.online = append(s.online, &player{Player: p, faction: faction, leaveAt: t.Add(session)})
	s.playerEvent(t, wardogs.FeedPlayerJoined, p)
}

func (s *Server) leave(t time.Time, p *player) {
	s.online = slices.DeleteFunc(s.online, func(o *player) bool { return o == p })
	s.playerEvent(t, wardogs.FeedPlayerLeft, p.Player)
}

// startIfReady starts a match once the document's minimum is on.
func (s *Server) startIfReady(t time.Time) {
	if s.match.playing || len(s.online) == 0 || len(s.online) < s.match.minPlayers {
		return
	}
	length := time.Duration(s.profile.MatchMinutes.draw(s.rng.Float64()) * float64(time.Minute))
	s.match.playing, s.match.startedAt, s.match.endsAt = true, t, t.Add(length)
	s.match.scores = make([]int64, len(s.factions))
	s.match.nextScore = t.Add(time.Duration(s.match.scorePeriod) * time.Second)
}

// fight makes this tick's kills: killers by skill, victims on another faction, weaker ones more often.
func (s *Server) fight(t time.Time) {
	if len(s.online) < 2 {
		return
	}
	var rate float64
	for _, p := range s.online {
		rate += p.Skill
	}
	kills := Poisson(s.rng, rate*s.profile.KillsPerMinute*tick.Minutes())
	for range kills {
		killer := s.weighted(func(p *player) float64 { return p.Skill }, nil)
		victim := s.weighted(func(p *player) float64 { return 1 / math.Sqrt(p.Skill) }, func(p *player) bool { return p != killer && p.faction != killer.faction })
		if victim == nil {
			victim = s.weighted(func(p *player) float64 { return 1 / math.Sqrt(p.Skill) }, func(p *player) bool { return p != killer })
		}
		killer.kills++
		killer.cash += 150
		victim.deaths++
		s.match.scores[killer.faction] += 5
		s.queue(t, killer, victim)
	}
}

// weighted picks an online player with probability by weight, among those keep allows.
func (s *Server) weighted(weight func(*player) float64, keep func(*player) bool) *player {
	var total float64
	for _, p := range s.online {
		if keep == nil || keep(p) {
			total += weight(p)
		}
	}
	if total <= 0 {
		return nil
	}
	x := s.rng.Float64() * total
	var last *player
	for _, p := range s.online {
		if keep != nil && !keep(p) {
			continue
		}
		last = p
		if x -= weight(p); x <= 0 {
			return p
		}
	}
	return last
}

// score is a score period's points: the faction that holds the hill, by numbers and skill.
func (s *Server) score() {
	strength := make([]float64, len(s.factions))
	for _, p := range s.online {
		strength[p.faction] += p.Skill
	}
	var total float64
	for _, v := range strength {
		total += v
	}
	if total <= 0 {
		return
	}
	x := s.rng.Float64() * total
	for i, v := range strength {
		if x -= v; x <= 0 {
			s.match.scores[i] += 100
			return
		}
	}
}

// endMatch ends the match: the rotation moves to its next entry, the game's counters and the
// scores start again, and the next match waits for its minimum.
func (s *Server) endMatch(t time.Time) {
	s.logger.Debug("wardogsim: match ended", "map", s.match.entry.Map, "players", len(s.online), "minutes", int(t.Sub(s.match.startedAt).Minutes()))
	if s.match.playing {
		s.ended(t)
	}
	s.match.playing = false
	s.enterRotation(s.match.next)
	for _, p := range s.online {
		p.kills, p.deaths = 0, 0
	}
	s.startIfReady(t)
}

// enterRotation makes rotation position i the map in play and reads the next-match sections.
func (s *Server) enterRotation(i int) {
	rot := s.written.rotation
	if len(rot) == 0 {
		s.match.index, s.match.next, s.match.entry = 0, 0, rotationEntry{Map: "Europe", Alternator: "ZoneAlternator.Ozeti.Default.Circle"}
	} else {
		if i < 0 || i >= len(rot) {
			i = 0
		}
		for range len(rot) { // a denied entry is skipped
			if s.maps[rot[i].Map] {
				break
			}
			i = (i + 1) % len(rot)
		}
		s.match.index, s.match.entry = i, rot[i]
		s.match.next = s.nextIndex(i)
	}
	s.lighting = s.match.entry.Lighting
	s.match.scorePeriod = min(max(s.written.scorePeriod, scorePeriodMin), scorePeriodMax)
	s.match.minPlayers = max(s.written.minPlayers, 1)
	s.match.scores = make([]int64, len(s.factions))
}

// nextIndex is the rotation position after i: the next one in order, or another at random.
func (s *Server) nextIndex(i int) int {
	n := len(s.written.rotation)
	switch {
	case n == 0 || !s.written.rotationOn:
		return max(0, min(i, n-1))
	case s.written.rotationRandom && n > 1:
		j := s.rng.IntN(n - 1)
		if j >= i {
			j++
		}
		return j
	}
	return (i + 1) % n
}

// reboot is the host's daily restart: everyone drops, the rotation starts over and the sections
// that apply at the next restart are read.
func (s *Server) reboot(t time.Time) {
	s.logger.Info("wardogsim: daily restart", "players_dropped", len(s.online))
	for _, p := range s.online {
		s.away[p.N] = t.Add(time.Duration(1+s.rng.IntN(20)) * time.Minute)
	}
	s.online, s.back = nil, map[int]time.Time{}
	s.live = s.written
	s.booted, s.restart = t, s.profile.NextRestart(t)
	// The feed's counts are since the server started (GET /v1/feed). Whether the real server keeps
	// its queue across a restart is not known; the simulation keeps it.
	s.dropped, s.delivery.sent, s.delivery.failures = 0, 0, 0
	s.match.playing = false
	s.enterRotation(0)
	s.started(t)
}
