// Package seeding calls for seeders and counts seeding minutes (ADR-0016). Some games start a
// match only once enough players are on (War Dogs needs 20), so an empty server stays empty unless
// people join together. The Organization settings' discord.seeding rules say, per server, where to
// call (a channel), whom (an opt-in seeder role), under what floor (below), when (weekly windows in
// stats.timezone) and how often (a cooldown).
//
// A pass reads the settings and the hub's public server list. Inside a window, while a server is
// live and under its floor, and once its cooldown has run out since the last call, the module
// posts a call that pings that role and nothing else. A call opens a seeding session: each pass
// reads who is on (ListServerPlayers) and adds the time since the last pass to every player there.
// The session ends when the server reaches its floor (the call is edited to say it started) or the
// window closes (the call is edited to say so). Each player who seeded a minute or more is then
// recorded as one participation event of kind "seeding" (RecordParticipation, ADR-0014), keyed by
// the server, the session's start and the player's identity, so a retry or a restart never counts
// twice.
//
// The bot keeps no state in the hub (ADR-0008). The last call is found again after a restart from
// the marker in its own posts ("-# seeding <server> · <start> · open"): the cooldown counts from
// it, and a call still open inside its window resumes its session, counting from the restart. A
// failure is logged once while it lasts, counted, and retried at the next pass.
package seeding

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	_ "time/tzdata" // stats.timezone resolves in a container with no zoneinfo

	"connectrpc.com/connect"
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/gravel-project/gravel/discord/bot"
	"github.com/gravel-project/gravel/discord/hubclient"
	"github.com/gravel-project/gravel/discord/seedhours"
	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
)

// Kind is the participation kind a seeding session records.
const Kind = "seeding"

// DefaultCooldown is a rule's cooldown when it sets none (the settings' default).
const DefaultCooldown = 2 * time.Hour

// MinSeconds is the least seeding time a player needs for the session to record them.
const MinSeconds = 60

// maxDuration is the longest duration_seconds the hub takes for one event.
const maxDuration = 86400

// searchDepth is how many of a channel's newest messages are read for the last call.
const searchDepth = 100

// batchSize is the most events RecordParticipation takes at once.
const batchSize = 100

// maxPending bounds the events kept for one server while the hub refuses or is away.
const maxPending = 5000

// Session outcomes, as the metric labels them.
const (
	outcomeStarted = "started"
	outcomeReached = "reached"
	outcomeClosed  = "window_closed"
)

// marker finds a call's server, its start and its state in a message the module posted.
var marker = regexp.MustCompile(`(?m)^-# seeding ([a-z0-9][a-z0-9-]{0,62}) · ([0-9]{1,12}) · (open|reached|closed)$`)

// noMentions keeps an edit from pinging anyone.
var noMentions = &discord.AllowedMentions{Parse: []discord.AllowedMentionType{}, Roles: []snowflake.ID{}, Users: []snowflake.ID{}}

// Config is how the module runs; the rules are in the Organization settings.
type Config struct {
	// Interval is the time between passes.
	Interval time.Duration
}

// Discord is the part of Discord's REST API the module uses; the runtime's client is one.
type Discord interface {
	GetMessages(channelID snowflake.ID, around snowflake.ID, before snowflake.ID, after snowflake.ID, limit int, opts ...rest.RequestOpt) ([]discord.Message, error)
	CreateMessage(channelID snowflake.ID, messageCreate discord.MessageCreate, opts ...rest.RequestOpt) (*discord.Message, error)
	UpdateMessage(channelID snowflake.ID, messageID snowflake.ID, messageUpdate discord.MessageUpdate, opts ...rest.RequestOpt) (*discord.Message, error)
}

// Module is the seeding module.
type Module struct {
	hub *hubclient.Client
	cfg Config

	discord Discord
	self    snowflake.ID
	logger  *slog.Logger
	now     func() time.Time

	mu      sync.Mutex
	servers map[string]*state
	idle    bool
	hubDown bool

	calls        *prometheus.CounterVec
	sessions     *prometheus.CounterVec
	seconds      *prometheus.CounterVec
	recordErrors *prometheus.CounterVec
}

// state is what the module knows about one server's rule.
type state struct {
	channel  snowflake.ID
	found    bool      // the channel was read for the last call since start, or since the channel changed
	lastCall time.Time // the last call's start, for the cooldown
	session  *session
	edit     *edit // an ended call whose edit Discord has not taken yet
	pending  []*hubv1.ParticipationEvent
	failing  map[string]bool // the failures being logged once
}

// session is an open call: from the call until the floor or the window's end.
type session struct {
	message   snowflake.ID
	start     time.Time // the call's time, whole seconds: the marker's and the keys'
	windowEnd time.Time
	name      string
	below     int32
	last      time.Time // the time counted up to
	seconds   map[player]time.Duration
}

type player struct{ provider, subject string }

type edit struct {
	channel, message snowflake.ID
	content          string
}

// DefaultInterval is the time between passes when Config sets none (bot.yaml's default).
const DefaultInterval = 30 * time.Second

// New builds the module; the bot's app needs servers:read and participation:write.
func New(hub *hubclient.Client, cfg Config) *Module {
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultInterval
	}
	return &Module{hub: hub, cfg: cfg, servers: map[string]*state{}, logger: slog.Default(), now: time.Now}
}

// Name is the module's name.
func (*Module) Name() string { return "seeding" }

// Register adds the job and the metrics.
func (m *Module) Register(r *bot.Registry) error {
	m.discord = r.Rest()
	m.self = r.ApplicationID()
	m.logger = r.Logger()
	m.calls = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "gravel_bot_seeding_calls_total", Help: "Seeding calls, by server and result (posted, forbidden, error)."}, []string{"server", "result"})
	m.sessions = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "gravel_bot_seeding_sessions_total", Help: "Seeding sessions, by server and outcome (started, reached, window_closed)."}, []string{"server", "outcome"})
	m.seconds = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "gravel_bot_seeding_recorded_seconds_total", Help: "Seeding time recorded as participation, by server, in seconds (duplicates not counted)."}, []string{"server"})
	m.recordErrors = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "gravel_bot_seeding_record_errors_total", Help: "Failed attempts to record seeding minutes, by server; each is retried, an invalid event dropped."}, []string{"server"})
	for _, c := range []prometheus.Collector{m.calls, m.sessions, m.seconds, m.recordErrors} {
		if err := r.Metrics().Register(c); err != nil {
			return fmt.Errorf("seeding: metrics: %w", err)
		}
	}
	r.Job("seed", m.Run)
	return nil
}

// Run passes until ctx is done; a failed pass is logged and the next one tries again.
func (m *Module) Run(ctx context.Context) error {
	m.runPass(ctx)
	t := time.NewTicker(m.cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			m.runPass(ctx)
		}
	}
}

func (m *Module) runPass(ctx context.Context) {
	_, err := m.Pass(ctx)
	m.mu.Lock()
	was := m.hubDown
	m.hubDown = err != nil && ctx.Err() == nil
	now := m.hubDown
	m.mu.Unlock()
	switch {
	case now && !was:
		m.logger.WarnContext(ctx, "seeding: the hub is unavailable, will retry", "error", err.Error())
	case !now && was:
		m.logger.InfoContext(ctx, "seeding: the hub is back")
	}
}

// Result is what a pass did.
type Result struct {
	Idle      bool // no rule, no open session, nothing to record
	Rules     int
	Called    int // calls posted
	Resumed   int // open calls found after a restart and carried on
	Reached   int // sessions ended at the floor
	Closed    int // sessions ended by their window
	Unknown   int // rules whose server the hub does not list
	Forbidden int // Discord refused: the bot may not use the channel
	Failed    int // Discord, or the hub's player list, failed; retried next pass
	// Recorded and Duplicates are the participation events the hub answered for; Dropped those it
	// refused as invalid; RecordFailed the servers whose events wait for the next pass.
	Recorded, Duplicates, Dropped, RecordFailed int
}

// rule is one seeding rule from the settings.
type rule struct {
	server        string
	channel, role snowflake.ID
	below         int32
	hours         seedhours.Hours
	cooldown      time.Duration
	atStart       bool
	note          string
}

// Pass checks every rule once. An error means the hub could not be read; a server's own failure
// is counted in the result and logged, and the other rules still run.
func (m *Module) Pass(ctx context.Context) (Result, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	set, err := m.hub.Organization().GetOrganizationSettings(ctx, connect.NewRequest(&hubv1.GetOrganizationSettingsRequest{}))
	if err != nil {
		return Result{}, fmt.Errorf("read the settings: %w", err)
	}
	rules, err := rulesFrom(set.Msg.GetSettings().GetDiscord())
	if err != nil {
		return Result{}, err
	}
	busy := false
	for _, st := range m.servers {
		busy = busy || st.session != nil || st.edit != nil || len(st.pending) > 0
	}
	wasIdle := m.idle
	m.idle = len(rules) == 0 && !busy
	if m.idle {
		if !wasIdle {
			m.logger.InfoContext(ctx, "seeding idle: the organization settings set no rule (discord.seeding)")
		}
		return Result{Idle: true}, nil
	}
	loc := time.UTC
	if tz := set.Msg.GetSettings().GetStats().GetTimezone(); tz != "" {
		if l, err := time.LoadLocation(tz); err == nil {
			loc = l
		}
	}
	list, err := m.hub.Servers().ListServers(ctx, connect.NewRequest(&hubv1.ListServersRequest{}))
	if err != nil {
		return Result{}, fmt.Errorf("list the servers: %w", err)
	}
	servers := map[string]*hubv1.Server{}
	for _, s := range list.Msg.GetServers() {
		servers[s.GetId()] = s
	}
	res := Result{Rules: len(rules)}
	ruled := map[string]bool{}
	for _, r := range rules {
		ruled[r.server] = true
		m.apply(ctx, r, servers[r.server], loc, now, &res)
	}
	for _, id := range slices.Sorted(maps.Keys(m.servers)) {
		st := m.servers[id]
		if !ruled[id] && st.session != nil { // the rule was removed: its call closes
			m.end(ctx, id, st, outcomeClosed, servers[id], now, &res)
		}
		// The minutes before the edit: a crash between the two leaves the call open, and the
		// session resumed from it records under the same keys, which the hub keeps once.
		m.flush(ctx, id, st, &res)
		m.retryEdit(ctx, id, st, &res)
	}
	return res, nil
}

// rulesFrom reads the settings' rules. The hub validated them; one that does not parse here is
// still refused rather than guessed at.
func rulesFrom(d *hubv1.DiscordSettings) ([]rule, error) {
	var out []rule
	for i, r := range d.GetSeeding() {
		channel, err1 := snowflake.Parse(r.GetChannel())
		role, err2 := snowflake.Parse(r.GetRole())
		ws := make([]seedhours.Window, 0, len(r.GetHours()))
		for _, h := range r.GetHours() {
			ws = append(ws, seedhours.Window{Days: h.GetDays(), From: h.GetFrom(), To: h.GetTo()})
		}
		hours, probs := seedhours.Parse(ws)
		cooldown := DefaultCooldown
		var err3 error
		if c := r.GetCooldown(); c != "" {
			cooldown, err3 = time.ParseDuration(c)
		}
		if err1 != nil || err2 != nil || err3 != nil || len(probs) > 0 || r.GetServer() == "" || r.GetBelow() < 2 || cooldown <= 0 {
			return nil, fmt.Errorf("seeding[%d]: the rule for %q does not parse; check discord.seeding", i, r.GetServer())
		}
		out = append(out, rule{server: r.GetServer(), channel: channel, role: role, below: r.GetBelow(), hours: hours,
			cooldown: cooldown, atStart: r.GetCallAtStart(), note: strings.TrimSpace(r.GetNote())})
	}
	return out, nil
}

// apply runs one rule: find the last call after a restart, count an open session's time, end it
// at the floor or the window's end, and call when one is due.
func (m *Module) apply(ctx context.Context, r rule, s *hubv1.Server, loc *time.Location, now time.Time, res *Result) {
	st := m.servers[r.server]
	if st == nil {
		st = &state{failing: map[string]bool{}}
		m.servers[r.server] = st
	}
	if st.channel != r.channel {
		if st.session != nil { // the rule moved: the old call closes
			m.end(ctx, r.server, st, outcomeClosed, s, now, res)
		}
		st.channel, st.found, st.lastCall = r.channel, false, time.Time{}
	}
	if s == nil {
		res.Unknown++
		m.fail(ctx, r.server, st, "unknown", "seeding: the hub lists no such server; check discord.seeding", nil)
	} else {
		m.recover(ctx, r.server, st, "unknown")
	}
	wStart, wEnd, open := r.hours.At(now, loc)
	if !st.found {
		if err := m.resume(ctx, r, st, s, wStart, wEnd, open, now, res); err != nil {
			m.discordFailed(ctx, r.server, st, "find", err, res)
			return
		}
		st.found = true
	}
	if st.session != nil {
		m.credit(ctx, r.server, st, s, now, res)
		switch {
		case live(s) && s.GetStatus().GetPlayers() >= r.below:
			m.end(ctx, r.server, st, outcomeReached, s, now, res)
		case !now.Before(st.session.windowEnd):
			m.end(ctx, r.server, st, outcomeClosed, s, now, res)
		}
	}
	if st.session == nil && open && live(s) && s.GetStatus().GetPlayers() < r.below && due(r, st, wStart, now) {
		m.call(ctx, r, st, s, wEnd, now, res)
	}
}

// live is a server the hub reads well right now.
func live(s *hubv1.Server) bool {
	return s != nil && s.GetStatus().GetState() == "ok" && s.GetStatus().GetReachable()
}

// due reports whether a call may go out now: the first ever, the cooldown run out since the last,
// or, with call_at_start, the window's first call whatever the cooldown left from before it.
func due(r rule, st *state, windowStart, now time.Time) bool {
	switch {
	case st.lastCall.IsZero():
		return true
	case r.atStart && st.lastCall.Before(windowStart):
		return true
	}
	return now.Sub(st.lastCall) >= r.cooldown
}

// call posts a call and opens its session.
func (m *Module) call(ctx context.Context, r rule, st *state, s *hubv1.Server, windowEnd, now time.Time, res *Result) {
	start := time.Unix(now.Unix(), 0)
	content := CallText(r.server, s.GetName(), r.role, s.GetStatus().GetPlayers(), r.below, r.note, start)
	mentions := &discord.AllowedMentions{Parse: []discord.AllowedMentionType{}, Roles: []snowflake.ID{r.role}, Users: []snowflake.ID{}}
	msg, err := m.discord.CreateMessage(r.channel, discord.MessageCreate{Content: content, AllowedMentions: mentions}, rest.WithCtx(ctx))
	if err != nil {
		m.discordFailed(ctx, r.server, st, "call", err, res)
		return
	}
	m.recover(ctx, r.server, st, "call")
	st.lastCall = start
	st.session = &session{message: msg.ID, start: start, windowEnd: windowEnd, name: s.GetName(), below: r.below, last: now, seconds: map[player]time.Duration{}}
	res.Called++
	m.calls.WithLabelValues(r.server, "posted").Inc()
	m.sessions.WithLabelValues(r.server, outcomeStarted).Inc()
	m.logger.InfoContext(ctx, "seeding: call posted", "server", r.server, "players", s.GetStatus().GetPlayers(), "below", r.below, "message", msg.ID.String())
}

// credit adds the time since the last pass to every player on the server: at most twice the
// interval, so a pass that came late (the hub or the bot away) never credits the gap, and never
// past the window's end.
func (m *Module) credit(ctx context.Context, server string, st *state, s *hubv1.Server, now time.Time, res *Result) {
	sess := st.session
	until := now
	if until.After(sess.windowEnd) {
		until = sess.windowEnd
	}
	elapsed := min(until.Sub(sess.last), 2*m.cfg.Interval)
	if elapsed <= 0 {
		return
	}
	if !live(s) { // nothing to read: the time passes uncounted
		sess.last = now
		return
	}
	resp, err := m.hub.Servers().ListServerPlayers(ctx, connect.NewRequest(&hubv1.ListServerPlayersRequest{ServerId: server}))
	if err != nil {
		res.Failed++
		m.fail(ctx, server, st, "players", "seeding: cannot read who is on (the bot's app needs servers:read), will retry", err)
		return
	}
	m.recover(ctx, server, st, "players")
	for _, p := range resp.Msg.GetPlayers() {
		if p.GetProvider() == "" || p.GetSubject() == "" {
			continue
		}
		sess.seconds[player{p.GetProvider(), p.GetSubject()}] += elapsed
	}
	sess.last = now
}

// end closes the open session: its players' time is queued, and so is the call's edit saying how
// it ended; the pass sends both, the minutes first. s is nil when the hub does not list the server.
func (m *Module) end(ctx context.Context, server string, st *state, outcome string, s *hubv1.Server, now time.Time, res *Result) {
	sess := st.session
	st.session = nil
	st.pending = append(st.pending, events(server, sess.start, sess.seconds)...)
	if over := len(st.pending) - maxPending; over > 0 {
		st.pending = st.pending[over:]
		m.logger.WarnContext(ctx, "seeding: too many minutes waiting for the hub; the oldest are dropped", "server", server, "dropped", over)
	}
	m.sessions.WithLabelValues(server, outcome).Inc()
	name := sess.name
	if s != nil {
		name = s.GetName()
	}
	var content string
	if outcome == outcomeReached {
		res.Reached++
		content = ReachedText(server, name, s.GetStatus().GetPlayers(), sess.below, sess.start, now)
	} else {
		res.Closed++
		var players *int32
		if live(s) {
			n := s.GetStatus().GetPlayers()
			players = &n
		}
		content = ClosedText(server, name, players, sess.below, sess.start)
	}
	m.logger.InfoContext(ctx, "seeding: session ended", "server", server, "outcome", outcome, "players", len(sess.seconds), "started", sess.start)
	st.edit = &edit{channel: st.channel, message: sess.message, content: content} // made after the minutes are sent (Pass)
}

// retryEdit edits an ended call; a call deleted since is left alone.
func (m *Module) retryEdit(ctx context.Context, server string, st *state, res *Result) {
	e := st.edit
	if e == nil {
		return
	}
	content := e.content
	_, err := m.discord.UpdateMessage(e.channel, e.message, discord.MessageUpdate{Content: &content, AllowedMentions: noMentions}, rest.WithCtx(ctx))
	if err != nil && classify(err) != "gone" {
		m.discordFailed(ctx, server, st, "edit", err, res)
		return
	}
	m.recover(ctx, server, st, "edit")
	st.edit = nil
}

// resume reads the channel for the module's last call for the server: its time starts the
// cooldown, and a call still open is carried on inside its window or closed outside it.
func (m *Module) resume(ctx context.Context, r rule, st *state, s *hubv1.Server, wStart, wEnd time.Time, open bool, now time.Time, res *Result) error {
	msgs, err := m.discord.GetMessages(r.channel, 0, 0, 0, searchDepth, rest.WithCtx(ctx))
	if err != nil {
		return err
	}
	for _, msg := range msgs { // newest first
		if msg.Author.ID != m.self {
			continue
		}
		server, start, state, ok := Marker(msg.Content)
		if !ok || server != r.server {
			continue
		}
		st.lastCall = start
		if state != "open" {
			return nil
		}
		name := r.server
		if s != nil {
			name = s.GetName()
		}
		st.session = &session{message: msg.ID, start: start, windowEnd: wEnd, name: name, below: r.below, last: now, seconds: map[player]time.Duration{}}
		if open && !start.Before(wStart) && start.Before(wEnd) {
			res.Resumed++
			m.logger.InfoContext(ctx, "seeding: open call found, session resumed", "server", r.server, "started", start, "message", msg.ID.String())
			return nil
		}
		m.end(ctx, r.server, st, outcomeClosed, s, now, res) // its window closed while the bot was away
		return nil
	}
	return nil
}

// flush sends the server's waiting events, a batch at a time. A refused batch is sent again one
// event at a time, so one event the hub calls invalid (a server since removed, an identity it does
// not take) is dropped alone; any other failure keeps them for the next pass.
func (m *Module) flush(ctx context.Context, server string, st *state, res *Result) {
	for len(st.pending) > 0 {
		n := min(batchSize, len(st.pending))
		batch := st.pending[:n]
		resp, err := m.hub.Stats().RecordParticipation(ctx, connect.NewRequest(&hubv1.RecordParticipationRequest{Events: batch}))
		if err != nil {
			m.recordErrors.WithLabelValues(server).Inc()
			if connect.CodeOf(err) == connect.CodeInvalidArgument {
				if err = m.flushOneByOne(ctx, server, st, n, res); err == nil {
					continue
				}
			}
			res.RecordFailed++
			m.fail(ctx, server, st, "record", "seeding: cannot record the minutes (the bot's app needs participation:write), will retry", err)
			return
		}
		m.recover(ctx, server, st, "record")
		m.count(server, batch, resp.Msg.GetResults(), res)
		st.pending = st.pending[n:]
	}
}

// flushOneByOne sends the first n waiting events singly, dropping each the hub refuses as invalid.
// Any other failure stops it with the rest still waiting.
func (m *Module) flushOneByOne(ctx context.Context, server string, st *state, n int, res *Result) error {
	for range n {
		e := st.pending[0]
		resp, err := m.hub.Stats().RecordParticipation(ctx, connect.NewRequest(&hubv1.RecordParticipationRequest{Events: []*hubv1.ParticipationEvent{e}}))
		switch {
		case err == nil:
			m.count(server, []*hubv1.ParticipationEvent{e}, resp.Msg.GetResults(), res)
		case connect.CodeOf(err) == connect.CodeInvalidArgument:
			m.recordErrors.WithLabelValues(server).Inc()
			res.Dropped++
			m.logger.ErrorContext(ctx, "seeding: the hub refused a seeding event; it is dropped", "server", server, "key", e.GetKey(), "error", err.Error())
		default:
			m.recordErrors.WithLabelValues(server).Inc()
			return err
		}
		st.pending = st.pending[1:]
	}
	return nil
}

func (m *Module) count(server string, batch []*hubv1.ParticipationEvent, results []*hubv1.ParticipationResult, res *Result) {
	for i, r := range results {
		if i >= len(batch) {
			break
		}
		if r.GetOutcome() == "recorded" {
			res.Recorded++
			m.seconds.WithLabelValues(server).Add(float64(batch[i].GetDurationSeconds()))
		} else {
			res.Duplicates++
		}
	}
}

// events are a session's participation events: one per player who seeded MinSeconds or more, in
// the order of their keys. The key is the server, the session's start and the identity, so the
// same session sent again, by a retry or after a restart, is a duplicate the hub does not store.
func events(server string, start time.Time, seconds map[player]time.Duration) []*hubv1.ParticipationEvent {
	var out []*hubv1.ParticipationEvent
	ref := "seeding:" + server + ":" + strconv.FormatInt(start.Unix(), 10)
	for p, d := range seconds {
		secs := int32(min(int64(d/time.Second), maxDuration)) //nolint:gosec // bounded by maxDuration
		if secs < MinSeconds {
			continue
		}
		out = append(out, &hubv1.ParticipationEvent{
			Key:      ref + ":" + p.provider + ":" + p.subject,
			Kind:     Kind,
			Identity: &hubv1.PlayerIdentity{Provider: p.provider, Subject: p.subject},
			ServerId: server, Session: ref, OccurredAt: timestamppb.New(start), DurationSeconds: secs,
		})
	}
	slices.SortFunc(out, func(a, b *hubv1.ParticipationEvent) int { return strings.Compare(a.GetKey(), b.GetKey()) })
	return out
}

// discordFailed counts and logs a Discord refusal or failure for one server.
func (m *Module) discordFailed(ctx context.Context, server string, st *state, what string, err error, res *Result) {
	if classify(err) == "forbidden" {
		res.Forbidden++
		if what == "call" {
			m.calls.WithLabelValues(server, "forbidden").Inc()
		}
		m.fail(ctx, server, st, what+"-forbidden", "seeding: the bot may not use the channel; it needs View Channel, Send Messages and Read Message History there", err)
		return
	}
	res.Failed++
	if what == "call" {
		m.calls.WithLabelValues(server, "error").Inc()
	}
	m.fail(ctx, server, st, what, "seeding: Discord failed, will retry", err)
}

// fail logs a server's failure once while it lasts.
func (m *Module) fail(ctx context.Context, server string, st *state, kind, msg string, err error) {
	if st.failing[kind] {
		return
	}
	st.failing[kind] = true
	args := []any{"server", server, "channel", st.channel.String()}
	if err != nil {
		args = append(args, "error", err.Error())
	}
	m.logger.WarnContext(ctx, msg, args...)
}

func (m *Module) recover(ctx context.Context, server string, st *state, kind string) {
	if !st.failing[kind] && !st.failing[kind+"-forbidden"] {
		return
	}
	delete(st.failing, kind)
	delete(st.failing, kind+"-forbidden")
	m.logger.InfoContext(ctx, "seeding: working again", "server", server, "what", kind)
}

// classify sorts a REST error: gone (the message was deleted), forbidden, or error.
func classify(err error) string {
	var re *rest.Error
	if !errors.As(err, &re) {
		return "error"
	}
	switch {
	case re.Code == rest.JSONErrorCodeUnknownMessage:
		return "gone"
	case re.Code == rest.JSONErrorCodeMissingAccess, re.Code == rest.JSONErrorCodeLackPermissionsToPerformAction,
		re.Response != nil && re.Response.StatusCode == http.StatusForbidden:
		return "forbidden"
	}
	return "error"
}

// footer is the marker line the module finds its calls by.
func footer(server string, start time.Time, state string) string {
	return "-# seeding " + server + " · " + strconv.FormatInt(start.Unix(), 10) + " · " + state
}

// Marker reads a call's marker: its server, its start and its state (open, reached, closed).
func Marker(content string) (server string, start time.Time, state string, ok bool) {
	g := marker.FindStringSubmatch(content)
	if g == nil {
		return "", time.Time{}, "", false
	}
	unix, err := strconv.ParseInt(g[2], 10, 64)
	if err != nil {
		return "", time.Time{}, "", false
	}
	return g[1], time.Unix(unix, 0), g[3], true
}

// CallText is a call: the role, the server and its count against the floor, the host's note, and
// the marker.
func CallText(server, name string, role snowflake.ID, players, below int32, note string, start time.Time) string {
	lines := []string{fmt.Sprintf("<@&%s> **%s** is at %d/%d: join to get the match going.", role, plain(name, server), players, below)}
	if note != "" {
		lines = append(lines, note)
	}
	return strings.Join(append(lines, footer(server, start, "open")), "\n")
}

// ReachedText is a call once the server reached its floor: "Started: 20/20 at 19:42", the time
// in each reader's own zone.
func ReachedText(server, name string, players, below int32, start, at time.Time) string {
	return fmt.Sprintf("**%s** started: %d/%d at <t:%d:t>. Thanks for seeding!\n%s", plain(name, server), players, below, at.Unix(), footer(server, start, "reached"))
}

// ClosedText is a call whose window closed first; players is nil when the count is not known.
func ClosedText(server, name string, players *int32, below int32, start time.Time) string {
	line := fmt.Sprintf("**%s**: the seeding window closed.", plain(name, server))
	if players != nil {
		line = fmt.Sprintf("**%s**: the seeding window closed at %d/%d.", plain(name, server), *players, below)
	}
	return line + "\n" + footer(server, start, "closed")
}

// plain is a server's name as Discord shows it literally (its id when it has none): no markdown,
// no mention, one line.
func plain(name, server string) string {
	if strings.TrimSpace(name) == "" {
		name = server
	}
	return strings.TrimSpace(markdown.Replace(name))
}

var markdown = strings.NewReplacer("\\", "\\\\", "*", "\\*", "_", "\\_", "~", "\\~", "`", "\\`", "|", "\\|", ">", "\\>", "#", "\\#",
	"@", "@\u200b", "\n", " ", "\r", " ")
