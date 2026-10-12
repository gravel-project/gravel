// Package matchresults posts each finished match result in Discord (ADR-0024): the series a
// server's match plugin reported (MatchZy on Counter-Strike 2), read from the hub's
// ListMatchResults and GetMatchResult. The Organization settings' discord.match_results name the
// channel and, optionally, the servers whose results are posted; bot.yaml turns the module on and
// sets how often it runs.
//
// A pass reads the settings and, when a channel is set, the series that ended within CatchUp. Each
// one the channel lacks is read in full and posted as one Components V2 message: the teams and the
// maps won, the winner, a line per map with its score, the top three by kills across the series by
// the names the hub shows (a member's chosen name, else a pseudonym), a link to the hub's result
// page and a marker ("-# result `<id>`"). Oldest first, at most PostsPerPass a pass; a post pings
// no one.
//
// The bot keeps nothing (ADR-0008): before it posts, it reads the channel's newest 100 messages
// for its own markers, so after a restart it posts only what is missing. The first time it looks
// at a channel in a process it posts nothing that ended before the newest result it finds there,
// or, when it finds none, before the module started, so turning it on never floods the channel
// with history.
package matchresults

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/gravel-project/gravel/discord/bot"
	"github.com/gravel-project/gravel/discord/hubclient"
	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
)

const (
	// CatchUp is how long after a series ends it is still posted, after the bot was away.
	CatchUp = 24 * time.Hour
	// PostsPerPass bounds a pass's posts; a burst beyond it waits for the next pass.
	PostsPerPass = 5
	// TopPlayers is how many players a post names.
	TopPlayers = 3
	// searchDepth is how many of the channel's newest messages are read for what was posted.
	searchDepth = 100
	// pageSize is how many results one ListMatchResults asks for: the hub's most.
	pageSize = 200
)

// Config is how the module runs; the channel and the servers are in the Organization settings.
type Config struct {
	// Interval is the time between passes.
	Interval time.Duration
	// PublicURL is the hub's public origin, for the link to the result page; empty posts none.
	PublicURL string
}

// Discord is the part of Discord's REST API the module uses; the runtime's client is one.
type Discord interface {
	GetMessages(channelID snowflake.ID, around snowflake.ID, before snowflake.ID, after snowflake.ID, limit int, opts ...rest.RequestOpt) ([]discord.Message, error)
	CreateMessage(channelID snowflake.ID, messageCreate discord.MessageCreate, opts ...rest.RequestOpt) (*discord.Message, error)
}

// markerPrefix starts a post's last line; the result's id after it is how the module finds it.
const markerPrefix = "-# result `"

// marker reads a result's id from a post's last line.
var marker = regexp.MustCompile("^-# result `([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})`$")

// noMentions keeps a post from pinging anyone, whatever a name says.
var noMentions = &discord.AllowedMentions{Parse: []discord.AllowedMentionType{}, Roles: []snowflake.ID{}, Users: []snowflake.ID{}}

// Module is the match results module.
type Module struct {
	hub *hubclient.Client
	cfg Config

	discord Discord
	self    snowflake.ID
	logger  *slog.Logger
	now     func() time.Time

	mu      sync.Mutex
	started time.Time                  // the first pass: a channel with no result of ours starts here
	since   map[snowflake.ID]time.Time // where each channel starts, set by its first look in this process
	done    map[string]bool            // results posted, found or passed over in this process
	warned  map[string]string          // the problem being logged once, by subject
	idle    bool

	results *prometheus.CounterVec
	lastOK  prometheus.Gauge
}

// New builds the module.
func New(hub *hubclient.Client, cfg Config) *Module {
	return &Module{hub: hub, cfg: cfg, logger: slog.Default(), now: time.Now, since: map[snowflake.ID]time.Time{},
		done: map[string]bool{}, warned: map[string]string{}}
}

// Name is the module's name.
func (m *Module) Name() string { return "matchresults" }

// Register adds the job.
func (m *Module) Register(r *bot.Registry) error {
	m.discord = r.Rest()
	m.self = r.ApplicationID()
	m.logger = r.Logger()
	m.results = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "gravel_bot_match_results_total", Help: "Match results, by result (posted, error)."}, []string{"result"})
	m.lastOK = prometheus.NewGauge(prometheus.GaugeOpts{Name: "gravel_bot_match_results_last_success_timestamp_seconds", Help: "When the last pass in which the hub answered, the channel was read and every post went through completed, as a Unix time."})
	for _, c := range []prometheus.Collector{m.results, m.lastOK} {
		if err := r.Metrics().Register(c); err != nil {
			return fmt.Errorf("matchresults: metrics: %w", err)
		}
	}
	m.results.WithLabelValues("posted")
	m.results.WithLabelValues("error")
	r.Job("post", m.Run)
	return nil
}

// Run posts until ctx is done; a failed pass is logged and the next one tries again.
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
	if _, err := m.Pass(ctx); err != nil && ctx.Err() == nil {
		m.logger.WarnContext(ctx, "match results: the hub is unavailable, will retry", "error", err.Error())
	}
}

// Result is what a pass did.
type Result struct {
	Idle   bool     // no channel set
	Posted []string // the results posted, in order
	Found  []string // posted before, found in the channel
	// Passed are results that ended before the channel's start: history, never posted.
	Passed []string
	// Failed is set when the hub, the channel or a post failed; what is left waits for the next pass.
	Failed bool
}

// Pass checks for finished results once. An error means the settings could not be read.
func (m *Module) Pass(ctx context.Context) (Result, error) {
	set, err := m.hub.Organization().GetOrganizationSettings(ctx, connect.NewRequest(&hubv1.GetOrganizationSettingsRequest{}))
	if err != nil {
		return Result{}, fmt.Errorf("read the settings: %w", err)
	}
	mr := set.Msg.GetSettings().GetDiscord().GetMatchResults()
	channel, cerr := snowflake.Parse(mr.GetChannel())
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	if m.started.IsZero() {
		m.started = now
	}
	wasIdle := m.idle
	m.idle = mr.GetChannel() == "" || cerr != nil // the hub validated the channel; one that doesn't parse is not guessed at
	if m.idle {
		if !wasIdle {
			m.logger.InfoContext(ctx, "match results idle: the organization settings set no channel")
		}
		m.lastOK.SetToCurrentTime()
		return Result{Idle: true}, nil
	}
	var res Result
	if m.pass(ctx, channel, mr.GetServers(), now, &res) {
		m.lastOK.SetToCurrentTime()
	}
	return res, nil
}

// pass posts what the channel lacks; false when anything failed. The caller holds the lock.
func (m *Module) pass(ctx context.Context, channel snowflake.ID, servers []string, now time.Time, res *Result) bool {
	listed, err := m.list(ctx, servers, now.Add(-CatchUp))
	if err != nil {
		m.results.WithLabelValues("error").Inc()
		msg := "match results: can't read the results, will retry"
		kind := "error"
		if c := connect.CodeOf(err); c == connect.CodePermissionDenied || c == connect.CodeUnauthenticated {
			kind = "denied"
			msg = "match results: the hub won't show the bot its results; give its app stats:read or make stats public"
		}
		m.warn(ctx, "hub", kind, msg, err)
		res.Failed = true
		return false
	}
	m.clear("hub")
	var fresh []*hubv1.MatchResult
	for _, r := range listed {
		if !m.done[r.GetId()] {
			fresh = append(fresh, r)
		}
	}
	if len(fresh) == 0 {
		return true // nothing new costs no Discord call
	}

	opt := rest.WithCtx(ctx)
	msgs, err := m.discord.GetMessages(channel, 0, 0, 0, searchDepth, opt)
	if err != nil {
		m.results.WithLabelValues("error").Inc()
		m.channelFailed(ctx, channel, err)
		res.Failed = true
		return false
	}
	m.clear("channel")
	ours := m.posted(msgs)

	// The first look sets where the channel starts, for the life of the process: after the newest
	// result of ours it holds, or at the module's start when it holds none. Nothing older is
	// posted, so the channel never fills with history.
	since, seen := m.since[channel]
	if !seen {
		since = m.started
		var newest time.Time
		for _, r := range listed {
			if at := r.GetEndedAt().AsTime(); ours[r.GetId()] && at.After(newest) {
				newest = at
			}
		}
		if !newest.IsZero() {
			since = newest
		}
		m.since[channel] = since
	}
	var todo []*hubv1.MatchResult
	for _, r := range fresh {
		switch id := r.GetId(); {
		case ours[id]:
			m.done[id] = true
			res.Found = append(res.Found, id)
		case !r.GetEndedAt().AsTime().After(since):
			m.done[id] = true
			res.Passed = append(res.Passed, id)
		default:
			todo = append(todo, r)
		}
	}
	slices.SortStableFunc(todo, func(a, b *hubv1.MatchResult) int {
		return cmp.Or(a.GetEndedAt().AsTime().Compare(b.GetEndedAt().AsTime()), strings.Compare(a.GetId(), b.GetId()))
	})
	if len(todo) > PostsPerPass {
		todo = todo[:PostsPerPass]
	}
	for _, r := range todo {
		full, err := m.hub.Stats().GetMatchResult(ctx, connect.NewRequest(&hubv1.GetMatchResultRequest{Id: r.GetId()}))
		if err != nil {
			// The rest wait, so the channel stays in order.
			m.results.WithLabelValues("error").Inc()
			m.warn(ctx, "result", "error", "match results: can't read a result, will retry", err, "result", r.GetId())
			res.Failed = true
			return false
		}
		m.clear("result")
		msg, err := m.discord.CreateMessage(channel, discord.NewMessageCreateV2(Render(full.Msg.GetResult(), m.link(r.GetId()))...).WithAllowedMentions(noMentions), opt)
		if err != nil {
			m.results.WithLabelValues("error").Inc()
			m.channelFailed(ctx, channel, err)
			res.Failed = true
			return false
		}
		m.done[r.GetId()] = true
		res.Posted = append(res.Posted, r.GetId())
		m.results.WithLabelValues("posted").Inc()
		m.logger.InfoContext(ctx, "match results: posted", "result", r.GetId(), "server", r.GetServerId(), "message", msg.ID.String())
	}
	m.clear("channel")
	return true
}

// list is the series that ended after since: every server's, or the listed servers', by id.
func (m *Module) list(ctx context.Context, servers []string, since time.Time) ([]*hubv1.MatchResult, error) {
	asks := servers
	if len(asks) == 0 {
		asks = []string{""} // every server
	}
	seen := map[string]bool{}
	var out []*hubv1.MatchResult
	for _, s := range asks {
		resp, err := m.hub.Stats().ListMatchResults(ctx, connect.NewRequest(&hubv1.ListMatchResultsRequest{ServerId: s, EndedOnly: true,
			EndedAfter: timestamppb.New(since), PageSize: pageSize}))
		if err != nil {
			return nil, err
		}
		for _, r := range resp.Msg.GetResults() {
			if r.GetId() == "" || r.GetEndedAt() == nil || seen[r.GetId()] {
				continue
			}
			if len(servers) > 0 && !slices.Contains(servers, r.GetServerId()) {
				continue
			}
			seen[r.GetId()] = true
			out = append(out, r)
		}
	}
	return out, nil
}

// posted is the results the bot's own posts in msgs name.
func (m *Module) posted(msgs []discord.Message) map[string]bool {
	out := map[string]bool{}
	for _, msg := range msgs {
		if msg.Author.ID != m.self || !msg.Flags.Has(discord.MessageFlagIsComponentsV2) {
			continue
		}
		if id, ok := Marker(msg.Components); ok {
			out[id] = true
		}
	}
	return out
}

// link is the hub's page for the result, "" without a public http(s) origin.
func (m *Module) link(id string) string {
	pub := strings.TrimSuffix(m.cfg.PublicURL, "/")
	if !strings.HasPrefix(pub, "https://") && !strings.HasPrefix(pub, "http://") {
		return ""
	}
	return pub + "/results/" + url.PathEscape(id)
}

func (m *Module) channelFailed(ctx context.Context, channel snowflake.ID, err error) {
	kind, msg := classify(err), "match results: the channel failed, will retry"
	if kind == "forbidden" {
		msg = "match results: the bot may not use the channel; it needs View Channel, Send Messages and Read Message History there"
	}
	m.warn(ctx, "channel", kind, msg, err, "channel", channel.String())
}

// warn logs a problem once until it changes or clears.
func (m *Module) warn(ctx context.Context, subject, kind, msg string, err error, attrs ...any) {
	if m.warned[subject] == kind {
		return
	}
	m.warned[subject] = kind
	if err != nil {
		attrs = append(attrs, "error", err.Error())
	}
	m.logger.WarnContext(ctx, msg, attrs...)
}

func (m *Module) clear(subject string) { delete(m.warned, subject) }

// classify sorts a REST error: "forbidden" when the bot may not use the channel, else "error".
func classify(err error) string {
	var re *rest.Error
	if errors.As(err, &re) && (re.Code == rest.JSONErrorCodeMissingAccess || re.Code == rest.JSONErrorCodeLackPermissionsToPerformAction ||
		re.Response != nil && re.Response.StatusCode == http.StatusForbidden) {
		return "forbidden"
	}
	return "error"
}

// Marker is the result a post's marker names; false when the components are not a result post.
func Marker(components []discord.LayoutComponent) (id string, ok bool) {
	check := func(c discord.Component) (string, bool) {
		t, isText := c.(discord.TextDisplayComponent)
		if !isText || !strings.HasPrefix(t.Content, markerPrefix) {
			return "", false
		}
		g := marker.FindStringSubmatch(t.Content)
		if g == nil {
			return "", false
		}
		return g[1], true
	}
	for _, lc := range components {
		if id, ok := check(lc); ok {
			return id, true
		}
		if box, isBox := lc.(discord.ContainerComponent); isBox {
			for _, sc := range box.Components {
				if id, ok := check(sc); ok {
					return id, true
				}
			}
		}
	}
	return "", false
}

// Render is a result's post: one container with the teams and the maps won, the winner, a line
// per map, the top players by kills across the series, the link to the result page when there is
// one, and the marker. Every name a team, a map or a player chose is escaped.
func Render(r *hubv1.MatchResult, link string) []discord.LayoutComponent {
	t1, t2 := teamName(r.GetTeam1Name(), "Team 1"), teamName(r.GetTeam2Name(), "Team 2")
	parts := []discord.ContainerSubComponent{
		discord.NewTextDisplay(fmt.Sprintf("## %s %d–%d %s", t1, r.GetTeam1Score(), r.GetTeam2Score(), t2)),
	}
	switch r.GetWinner() {
	case "team1":
		parts = append(parts, discord.NewTextDisplay("**"+t1+"** wins"))
	case "team2":
		parts = append(parts, discord.NewTextDisplay("**"+t2+"** wins"))
	case "none":
		parts = append(parts, discord.NewTextDisplay("A draw"))
	}
	maps := slices.Clone(r.GetMaps())
	slices.SortStableFunc(maps, func(a, b *hubv1.MatchResultMap) int { return cmp.Compare(a.GetMapNumber(), b.GetMapNumber()) })
	var lines []string
	for _, mp := range maps {
		lines = append(lines, fmt.Sprintf("%s %d–%d", cmp.Or(plain(mp.GetMapName()), "Unknown map"), mp.GetTeam1Score(), mp.GetTeam2Score()))
	}
	if len(lines) > 0 {
		parts = append(parts, discord.NewTextDisplay(strings.Join(lines, "\n")))
	}
	if top := Top(r.GetPlayers(), TopPlayers); len(top) > 0 {
		lines = []string{"**Top by kills**"}
		for i, p := range top {
			lines = append(lines, fmt.Sprintf("%d. %s: **%d** %s", i+1, cmp.Or(plain(p.Name), "Unknown player"), p.Kills, plural(p.Kills, "kill", "kills")))
		}
		parts = append(parts, discord.NewTextDisplay(strings.Join(lines, "\n")))
	}
	if link != "" {
		parts = append(parts, discord.NewTextDisplay("[Full result]("+link+")"))
	}
	parts = append(parts, discord.NewSmallSeparator(), discord.NewTextDisplay(markerPrefix+r.GetId()+"`"))
	return []discord.LayoutComponent{discord.NewContainer(parts...)}
}

// Player is one player's kills across a series.
type Player struct {
	Name  string
	Kills int
}

// Top is the n players with the most kills across every map of a series, by the names the hub
// shows; a member shown by name is one player across maps, as is a pseudonym. Ties go to fewer
// deaths, then the name.
func Top(players []*hubv1.MatchResultPlayer, n int) []Player {
	type total struct {
		name          string
		kills, deaths int
	}
	sums := map[string]*total{}
	var order []string
	for _, p := range players {
		k := "name:" + p.GetName()
		if p.GetUserId() != "" {
			k = "user:" + p.GetUserId()
		}
		t := sums[k]
		if t == nil {
			t = &total{name: p.GetName()}
			sums[k] = t
			order = append(order, k)
		}
		t.kills += int(p.GetKills())
		t.deaths += int(p.GetDeaths())
	}
	all := make([]*total, 0, len(order))
	for _, k := range order {
		all = append(all, sums[k])
	}
	slices.SortStableFunc(all, func(a, b *total) int {
		return cmp.Or(cmp.Compare(b.kills, a.kills), cmp.Compare(a.deaths, b.deaths), strings.Compare(a.name, b.name))
	})
	out := make([]Player, 0, min(n, len(all)))
	for _, t := range all[:min(n, len(all))] {
		out = append(out, Player{Name: t.name, Kills: t.kills})
	}
	return out
}

func teamName(s, fallback string) string { return cmp.Or(plain(s), fallback) }

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// plain keeps a value a team, a map or a player chose from being read as Discord markdown, a
// mention or a link, on one line (as the server cards do).
func plain(s string) string { return strings.TrimSpace(markdown.Replace(s)) }

var markdown = strings.NewReplacer("\\", "\\\\", "*", "\\*", "_", "\\_", "~", "\\~", "`", "\\`", "|", "\\|", ">", "\\>", "#", "\\#",
	"@", "@\u200b", "\n", " ", "\r", " ", "[", "\\[", "]", "\\]", "<", "\\<")
