// Package recaps posts a session recap in Discord after each of a server's weekly windows
// (ADR-0018): the Organization settings' discord.recaps name the server, the channel, the session's
// title, its hours (seeding's windows, in stats.timezone) and the floor its minutes count against.
//
// A pass reads the settings and, for each rule, the most recent window that ended at least Grace
// ago. When that window has no recap yet, the module reads the hub's GetSessionRecap for it and
// posts one message: peak players, minutes at or above the floor, the matches with their scores,
// the top five by kills (as the boards show them) and a link to the hub's recap page. Nobody on
// means no post. The post carries a marker ("-# recap `<server>` · <window start>"), so after a
// restart the module finds what it posted among its own messages and posts only what is missing,
// for a window that ended within CatchUp. A post pings no one.
package recaps

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
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
	"github.com/gravel-project/gravel/discord/seedhours"
	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
)

// Defaults for Config.
const (
	// DefaultGrace is how long after a window's end the recap is read: the last polls are in.
	DefaultGrace = 3 * time.Minute
	// CatchUp is how long after a window's end a recap is still posted, after the bot was away.
	CatchUp = 24 * time.Hour
	// maxMatchLines bounds the matches listed in a post; the page lists them all.
	maxMatchLines = 8
	// searchDepth is how many of the channel's newest messages are read to find a recap.
	searchDepth = 100
)

// Config is how the module runs; the rules are in the Organization settings.
type Config struct {
	// Interval is the time between passes.
	Interval time.Duration
	// Grace is how long after a window's end its recap is read; 0 is DefaultGrace.
	Grace time.Duration
	// PublicURL is the hub's public origin, for the link to the recap page; empty posts none.
	PublicURL string
}

// Discord is the part of Discord's REST API the module uses; the runtime's client is one.
type Discord interface {
	GetMessages(channelID snowflake.ID, around snowflake.ID, before snowflake.ID, after snowflake.ID, limit int, opts ...rest.RequestOpt) ([]discord.Message, error)
	CreateMessage(channelID snowflake.ID, messageCreate discord.MessageCreate, opts ...rest.RequestOpt) (*discord.Message, error)
}

// marker finds a recap's server and window start in a message the module posted.
var marker = regexp.MustCompile("(?m)^-# recap `([a-z0-9][a-z0-9-]{0,62})` · ([0-9]{1,12})$")

// noMentions keeps a post from pinging anyone, whatever a name says.
var noMentions = &discord.AllowedMentions{Parse: []discord.AllowedMentionType{}, Roles: []snowflake.ID{}, Users: []snowflake.ID{}}

// Module is the recaps module.
type Module struct {
	hub *hubclient.Client
	cfg Config

	discord Discord
	self    snowflake.ID
	logger  *slog.Logger
	now     func() time.Time

	mu      sync.Mutex
	done    map[key]bool   // windows recapped (posted, or nobody came) this process
	failing map[key]string // the failure being logged once per window
	idle    bool

	results    *prometheus.CounterVec
	lastPassOK prometheus.Gauge
}

// key is one window of one server.
type key struct {
	server string
	start  int64
}

// New builds the module.
func New(hub *hubclient.Client, cfg Config) *Module {
	if cfg.Grace <= 0 {
		cfg.Grace = DefaultGrace
	}
	return &Module{hub: hub, cfg: cfg, logger: slog.Default(), now: time.Now, done: map[key]bool{}, failing: map[key]string{}}
}

// Name is the module's name.
func (m *Module) Name() string { return "recaps" }

// Register adds the job.
func (m *Module) Register(r *bot.Registry) error {
	m.discord = r.Rest()
	m.self = r.ApplicationID()
	m.logger = r.Logger()
	m.results = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "gravel_bot_recaps_total", Help: "Session recaps, by server and result (posted, nobody, forbidden, error)."}, []string{"server", "result"})
	m.lastPassOK = prometheus.NewGauge(prometheus.GaugeOpts{Name: "gravel_bot_recaps_last_success_timestamp_seconds", Help: "When the last pass that left no recap failing completed, as a Unix time."})
	for _, c := range []prometheus.Collector{m.results, m.lastPassOK} {
		if err := r.Metrics().Register(c); err != nil {
			return fmt.Errorf("recaps: metrics: %w", err)
		}
	}
	r.Job("post", m.Run)
	return nil
}

// Run posts recaps until ctx is done; a failed pass is logged and the next one tries again.
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
		m.logger.WarnContext(ctx, "recaps: the hub is unavailable, will retry", "error", err.Error())
	}
}

// Result is what a pass did.
type Result struct {
	Idle    bool // no rule
	Posted  []string
	Nobody  []string
	Found   []string // posted before, found in the channel
	Failed  []string
	Pending []string // a window not ended (plus the grace) yet, or none ended within CatchUp
}

// Pass checks every rule once. An error means the settings could not be read.
func (m *Module) Pass(ctx context.Context) (Result, error) {
	set, err := m.hub.Organization().GetOrganizationSettings(ctx, connect.NewRequest(&hubv1.GetOrganizationSettingsRequest{}))
	if err != nil {
		return Result{}, fmt.Errorf("read the settings: %w", err)
	}
	rules := set.Msg.GetSettings().GetDiscord().GetRecaps()
	m.mu.Lock()
	wasIdle := m.idle
	m.idle = len(rules) == 0
	m.mu.Unlock()
	if len(rules) == 0 {
		if !wasIdle {
			m.logger.InfoContext(ctx, "recaps idle: the organization settings set no recap")
		}
		m.lastPassOK.SetToCurrentTime()
		return Result{Idle: true}, nil
	}
	loc := time.UTC
	if tz := set.Msg.GetSettings().GetStats().GetTimezone(); tz != "" {
		if l, err := time.LoadLocation(tz); err == nil {
			loc = l
		}
	}
	now := m.now()
	var res Result
	var names map[string]string
	for _, r := range rules {
		hours, probs := seedhours.Parse(windows(r.GetHours()))
		channel, cerr := strconv.ParseUint(r.GetChannel(), 10, 64)
		if len(probs) > 0 || cerr != nil {
			res.Failed = append(res.Failed, r.GetServer())
			continue // the hub validated the rule; one that doesn't parse here is skipped, not guessed at
		}
		start, end, ok := hours.Last(now.Add(-m.cfg.Grace), loc)
		if !ok || now.Sub(end) > CatchUp {
			res.Pending = append(res.Pending, r.GetServer())
			continue
		}
		k := key{server: r.GetServer(), start: start.Unix()}
		m.mu.Lock()
		done := m.done[k]
		m.mu.Unlock()
		if done {
			continue
		}
		if names == nil {
			names = m.serverNames(ctx)
		}
		switch outcome, err := m.recap(ctx, r, snowflake.ID(channel), start, end, names[r.GetServer()]); {
		case err != nil:
			res.Failed = append(res.Failed, r.GetServer())
			kind := "error"
			if classify(err) == "forbidden" {
				kind = "forbidden"
			}
			m.results.WithLabelValues(r.GetServer(), kind).Inc()
			m.fail(ctx, k, kind, err)
		default:
			m.mu.Lock()
			m.done[k] = true
			delete(m.failing, k)
			m.mu.Unlock()
			switch outcome {
			case "posted":
				res.Posted = append(res.Posted, r.GetServer())
				m.results.WithLabelValues(r.GetServer(), "posted").Inc()
			case "nobody":
				res.Nobody = append(res.Nobody, r.GetServer())
				m.results.WithLabelValues(r.GetServer(), "nobody").Inc()
			case "found":
				res.Found = append(res.Found, r.GetServer())
			}
		}
	}
	if len(res.Failed) == 0 {
		m.lastPassOK.SetToCurrentTime()
	}
	return res, nil
}

// recap posts one window's recap unless it was posted already or nobody came: "posted", "found"
// or "nobody".
func (m *Module) recap(ctx context.Context, r *hubv1.DiscordRecap, channel snowflake.ID, start, end time.Time, name string) (string, error) {
	opt := rest.WithCtx(ctx)
	found, err := m.find(channel, r.GetServer(), start, opt)
	if err != nil {
		return "", err
	}
	if found {
		return "found", nil
	}
	resp, err := m.hub.Stats().GetSessionRecap(ctx, connect.NewRequest(&hubv1.GetSessionRecapRequest{ServerId: r.GetServer(),
		From: timestamppb.New(start), To: timestamppb.New(end), Floor: r.GetFloor()}))
	if err != nil {
		return "", fmt.Errorf("read the recap: %w", err)
	}
	rc := resp.Msg
	if rc.GetPeakPlayers() == 0 && rc.GetPlayers() == 0 {
		m.logger.InfoContext(ctx, "recaps: nobody was on, nothing posted", "server", r.GetServer(), "start", start.Format(time.RFC3339))
		return "nobody", nil
	}
	content := Render(Post{Title: r.GetTitle(), Server: r.GetServer(), ServerName: name, Start: start, End: end, Floor: int(r.GetFloor()),
		Note: r.GetNote(), Link: m.link(r.GetServer(), start, end, int(r.GetFloor())), Recap: rc})
	msg, err := m.discord.CreateMessage(channel, discord.MessageCreate{Content: content, AllowedMentions: noMentions, Flags: discord.MessageFlagSuppressEmbeds}, opt)
	if err != nil {
		return "", err
	}
	m.logger.InfoContext(ctx, "recaps: posted", "server", r.GetServer(), "start", start.Format(time.RFC3339), "message", msg.ID.String())
	return "posted", nil
}

// find reports whether the bot already posted the window's recap in the channel.
func (m *Module) find(channel snowflake.ID, server string, start time.Time, opts ...rest.RequestOpt) (bool, error) {
	msgs, err := m.discord.GetMessages(channel, 0, 0, 0, searchDepth, opts...)
	if err != nil {
		return false, err
	}
	for _, msg := range msgs {
		if msg.Author.ID != m.self {
			continue
		}
		if s, at, ok := Marker(msg.Content); ok && s == server && at == start.Unix() {
			return true, nil
		}
	}
	return false, nil
}

// serverNames are the hub's servers' names by id; a failure leaves the ids shown.
func (m *Module) serverNames(ctx context.Context) map[string]string {
	out := map[string]string{}
	list, err := m.hub.Servers().ListServers(ctx, connect.NewRequest(&hubv1.ListServersRequest{}))
	if err != nil {
		return out
	}
	for _, s := range list.Msg.GetServers() {
		out[s.GetId()] = s.GetName()
	}
	return out
}

// link is the hub's recap page for the window, "" without a public https origin.
func (m *Module) link(server string, start, end time.Time, floor int) string {
	pub := strings.TrimSuffix(m.cfg.PublicURL, "/")
	if !strings.HasPrefix(pub, "https://") && !strings.HasPrefix(pub, "http://") {
		return ""
	}
	q := url.Values{"from": {start.UTC().Format(time.RFC3339)}, "to": {end.UTC().Format(time.RFC3339)}}
	if floor > 0 {
		q.Set("floor", strconv.Itoa(floor))
	}
	return pub + "/servers/" + url.PathEscape(server) + "/recap?" + q.Encode()
}

func (m *Module) fail(ctx context.Context, k key, kind string, err error) {
	m.mu.Lock()
	same := m.failing[k] == kind
	m.failing[k] = kind
	m.mu.Unlock()
	if same {
		return
	}
	msg := "recaps: a recap failed, will retry"
	if kind == "forbidden" {
		msg = "recaps: the bot may not use the channel; it needs View Channel, Send Messages and Read Message History there"
	}
	m.logger.WarnContext(ctx, msg, "server", k.server, "start", time.Unix(k.start, 0).UTC().Format(time.RFC3339), "error", err.Error())
}

func windows(hs []*hubv1.DiscordSeedingHours) []seedhours.Window {
	out := make([]seedhours.Window, len(hs))
	for i, h := range hs {
		out[i] = seedhours.Window{Days: h.GetDays(), From: h.GetFrom(), To: h.GetTo()}
	}
	return out
}

// classify sorts a REST error: "forbidden" when the bot may not use the channel, else "error".
func classify(err error) string {
	var re *rest.Error
	if errors.As(err, &re) && (re.Code == rest.JSONErrorCodeMissingAccess || re.Code == rest.JSONErrorCodeLackPermissionsToPerformAction ||
		re.Response != nil && re.Response.StatusCode == http.StatusForbidden) {
		return "forbidden"
	}
	return "error"
}

// Marker reads a recap's marker: its server and its window's start, as a Unix time.
func Marker(content string) (server string, start int64, ok bool) {
	g := marker.FindStringSubmatch(content)
	if g == nil {
		return "", 0, false
	}
	at, err := strconv.ParseInt(g[2], 10, 64)
	if err != nil {
		return "", 0, false
	}
	return g[1], at, true
}

// Post is what a recap message shows.
type Post struct {
	Title, Server, ServerName string
	Start, End                time.Time
	Floor                     int
	Note, Link                string
	Recap                     *hubv1.GetSessionRecapResponse
}

// Render is the recap message: the title and window (Discord shows the times in the reader's
// zone), the numbers, the matches with their scores, the top five, the note, the link and the
// marker. Names a game or a player chose are escaped.
func Render(p Post) string {
	rc := p.Recap
	name := p.ServerName
	if name == "" {
		name = p.Server
	}
	var b strings.Builder
	fmt.Fprintf(&b, "## %s recap\n", plain(p.Title))
	fmt.Fprintf(&b, "**%s** · <t:%d:f> to <t:%d:t>\n", plain(name), p.Start.Unix(), p.End.Unix())
	numbers := []string{fmt.Sprintf("Peak **%d** players", rc.GetPeakPlayers())}
	if p.Floor > 0 {
		numbers = append(numbers, fmt.Sprintf("**%s** at %d or more", minutes(int(rc.GetMinutesAtFloor())), p.Floor))
	}
	numbers = append(numbers, fmt.Sprintf("**%d** %s", rc.GetPlayers(), plural(int(rc.GetPlayers()), "player", "players")))
	b.WriteString(strings.Join(numbers, " · ") + "\n")
	if ms := rc.GetMatches(); len(ms) > 0 {
		b.WriteString("\n**Matches**\n")
		for i, x := range ms {
			if i == maxMatchLines {
				fmt.Fprintf(&b, "…and %d more\n", len(ms)-maxMatchLines)
				break
			}
			line := "- " + cmpOr(plain(x.GetMap()), "Unknown map")
			var teams []string
			for _, t := range x.GetTeams() {
				teams = append(teams, fmt.Sprintf("%s **%d**", plain(t.GetName()), t.GetScore()))
			}
			if len(teams) > 0 {
				line += ": " + strings.Join(teams, " · ")
			}
			b.WriteString(line + "\n")
		}
	} else {
		b.WriteString("\nNo match started.\n")
	}
	if top := rc.GetTop(); len(top) > 0 {
		b.WriteString("\n**Top by kills**\n")
		for _, e := range top {
			fmt.Fprintf(&b, "%d. %s: **%d** %s\n", e.GetRank(), plain(e.GetName()), e.GetKills(), plural(int(e.GetKills()), "kill", "kills"))
		}
	}
	if note := plain(p.Note); note != "" {
		b.WriteString("\n" + note + "\n")
	}
	if p.Link != "" {
		fmt.Fprintf(&b, "\n[The full recap](%s)\n", p.Link)
	}
	fmt.Fprintf(&b, "-# recap `%s` · %d", p.Server, p.Start.Unix())
	return b.String()
}

func minutes(n int) string {
	if n >= 60 {
		return fmt.Sprintf("%dh %02dm", n/60, n%60)
	}
	return fmt.Sprintf("%d %s", n, plural(n, "minute", "minutes"))
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func cmpOr(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// plain keeps a value a game, a host or a player chose from being read as Discord markdown or a
// mention, on one line (as the server cards do).
func plain(s string) string { return strings.TrimSpace(markdown.Replace(s)) }

var markdown = strings.NewReplacer("\\", "\\\\", "*", "\\*", "_", "\\_", "~", "\\~", "`", "\\`", "|", "\\|", ">", "\\>", "#", "\\#",
	"@", "@\u200b", "\n", " ", "\r", " ", "[", "\\[", "]", "\\]", "<", "\\<")
