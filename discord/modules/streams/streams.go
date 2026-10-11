// Package streams posts in Discord when the community's streamers go live (ADR-0019): the
// Organization settings' discord.streams name the channel, an opt-in role to ping, a note and the
// streamers, each a Twitch or Kick channel. The platforms' developer applications are bot.yaml's.
//
// A pass asks each platform which of its streamers are live: one call per platform, with an app
// access token, which reads stream metadata and nothing about viewers. A streamer who goes live
// gets one post (the title, the game, the link, the note and the role's ping); when the stream
// ends the post is edited to say so, and edits ping no one. A stream that drops and comes back
// within Rejoin edits its post back to live rather than posting (and pinging) again. Each post
// carries a marker ("-# stream twitch `login` · <stream id>"), so after a restart the module finds
// its posts among its own messages: a stream it already announced is not announced twice, and one
// that ended while the bot was away is marked ended. A platform that fails or rate-limits the
// bot leaves its streamers' posts as they are until it answers again.
package streams

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
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

	"github.com/gravel-project/gravel/discord/bot"
	"github.com/gravel-project/gravel/discord/hubclient"
	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
)

const (
	// Rejoin is how soon after a stream ends a new one from the same streamer resumes the post
	// instead of posting again: a dropped connection, a restarted encoder.
	Rejoin = 10 * time.Minute
	// searchDepth is how many of the channel's newest messages are read to find the posts.
	searchDepth = 100
	// httpTimeout bounds one call to a platform.
	httpTimeout = 15 * time.Second
)

// Config is how the module runs; the streamers are in the Organization settings.
type Config struct {
	// Interval is the time between passes.
	Interval time.Duration
	// Platforms are the platforms with an application; a streamer on another is skipped.
	Platforms []Platform
}

// Discord is the part of Discord's REST API the module uses; the runtime's client is one.
type Discord interface {
	GetMessages(channelID snowflake.ID, around snowflake.ID, before snowflake.ID, after snowflake.ID, limit int, opts ...rest.RequestOpt) ([]discord.Message, error)
	CreateMessage(channelID snowflake.ID, messageCreate discord.MessageCreate, opts ...rest.RequestOpt) (*discord.Message, error)
	UpdateMessage(channelID snowflake.ID, messageID snowflake.ID, messageUpdate discord.MessageUpdate, opts ...rest.RequestOpt) (*discord.Message, error)
}

// marker finds a post's platform, login and stream id, and whether it is marked ended, in a
// message the module posted.
var marker = regexp.MustCompile("(?m)^-# stream (twitch|kick) `([a-z0-9][a-z0-9_-]{0,24})` · ([0-9]{1,24})( · ended)?$")

// noMentions keeps an edit or an unpinged post from pinging anyone, whatever a title says.
var noMentions = &discord.AllowedMentions{Parse: []discord.AllowedMentionType{}, Roles: []snowflake.ID{}, Users: []snowflake.ID{}}

// Module is the streams module.
type Module struct {
	hub *hubclient.Client
	cfg Config

	discord Discord
	self    snowflake.ID
	logger  *slog.Logger
	now     func() time.Time

	mu        sync.Mutex
	channel   snowflake.ID         // the channel the posts below are in
	loaded    bool                 // the channel was searched for earlier posts
	posts     map[streamer]*post   // each streamer's newest post
	throttled map[string]time.Time // a platform's rate limit, until
	warned    map[string]string    // the problem being logged once, by subject
	platforms map[string]Platform  // by name
	idle      bool

	posted     *prometheus.CounterVec
	polls      *prometheus.CounterVec
	live       *prometheus.GaugeVec
	lastPassOK prometheus.Gauge
}

// streamer is one channel on one platform.
type streamer struct{ platform, login string }

// post is a message the module posted for a streamer.
type post struct {
	msg     snowflake.ID
	id      string // the stream's
	content string // as posted, last edited or found in the channel
	live    bool
	ended   time.Time
}

// New builds the module.
func New(hub *hubclient.Client, cfg Config) *Module {
	m := &Module{hub: hub, cfg: cfg, logger: slog.Default(), now: time.Now, posts: map[streamer]*post{}, throttled: map[string]time.Time{},
		warned: map[string]string{}, platforms: map[string]Platform{}}
	for _, p := range cfg.Platforms {
		m.platforms[p.Name()] = p
	}
	return m
}

// HTTPClient is the client the platforms are called with.
func HTTPClient() *http.Client { return &http.Client{Timeout: httpTimeout} }

// Name is the module's name.
func (m *Module) Name() string { return "streams" }

// Register adds the job.
func (m *Module) Register(r *bot.Registry) error {
	m.discord = r.Rest()
	m.self = r.ApplicationID()
	m.logger = r.Logger()
	m.posted = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "gravel_bot_streams_posts_total", Help: "Go-live posts, by platform and result (posted, resumed, ended, forbidden, error)."}, []string{"platform", "result"})
	m.polls = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "gravel_bot_streams_polls_total", Help: "Calls asking a platform who is live, by platform and result (ok, throttled, unauthorized, error)."}, []string{"platform", "result"})
	m.live = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "gravel_bot_streams_live", Help: "The listed streamers live at the last answer, by platform."}, []string{"platform"})
	m.lastPassOK = prometheus.NewGauge(prometheus.GaugeOpts{Name: "gravel_bot_streams_last_success_timestamp_seconds", Help: "When the last pass in which every platform answered and every post went through completed, as a Unix time."})
	for _, c := range []prometheus.Collector{m.posted, m.polls, m.live, m.lastPassOK} {
		if err := r.Metrics().Register(c); err != nil {
			return fmt.Errorf("streams: metrics: %w", err)
		}
	}
	r.Job("watch", m.Run)
	return nil
}

// Run watches until ctx is done; a failed pass is logged and the next one tries again.
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
		m.logger.WarnContext(ctx, "streams: the hub is unavailable, will retry", "error", err.Error())
	}
}

// Result is what a pass did, as "platform/login".
type Result struct {
	Idle    bool // no streamer
	Posted  []string
	Resumed []string // a post edited back to live
	Ended   []string
	Failed  []string // a post or an edit that failed
	// Unknown are the streamers whose platform has no application, failed or is rate-limited.
	Unknown []string
}

// Pass checks every streamer once. An error means the settings could not be read.
func (m *Module) Pass(ctx context.Context) (Result, error) {
	set, err := m.hub.Organization().GetOrganizationSettings(ctx, connect.NewRequest(&hubv1.GetOrganizationSettingsRequest{}))
	if err != nil {
		return Result{}, fmt.Errorf("read the settings: %w", err)
	}
	cfg := set.Msg.GetSettings().GetDiscord().GetStreams()
	m.mu.Lock()
	defer m.mu.Unlock()
	wasIdle := m.idle
	m.idle = len(cfg.GetStreamers()) == 0
	if m.idle {
		if !wasIdle {
			m.logger.InfoContext(ctx, "streams idle: the organization settings list no streamer")
		}
		m.lastPassOK.SetToCurrentTime()
		return Result{Idle: true}, nil
	}
	channel, err := snowflake.Parse(cfg.GetChannel())
	if err != nil {
		return Result{}, fmt.Errorf("discord.streams.channel %q: %w", cfg.GetChannel(), err) // the hub validated it
	}
	var role snowflake.ID
	if cfg.GetRole() != "" {
		role, _ = snowflake.Parse(cfg.GetRole())
	}
	if channel != m.channel {
		m.channel, m.loaded, m.posts = channel, false, map[streamer]*post{}
	}
	opt := rest.WithCtx(ctx)
	var res Result
	if !m.loaded {
		if err := m.load(opt); err != nil {
			kind := classify(err)
			m.warn(ctx, "channel", kind, "streams: can't read the channel; it needs View Channel, Send Messages and Read Message History there", err)
			for _, s := range cfg.GetStreamers() {
				res.Unknown = append(res.Unknown, s.GetPlatform()+"/"+s.GetLogin())
			}
			return res, nil
		}
		m.loaded = true
		m.clear("channel")
	}

	byPlatform := map[string][]*hubv1.DiscordStreamer{}
	var order []string
	for _, s := range cfg.GetStreamers() {
		if _, seen := byPlatform[s.GetPlatform()]; !seen {
			order = append(order, s.GetPlatform())
		}
		byPlatform[s.GetPlatform()] = append(byPlatform[s.GetPlatform()], s)
	}
	now := m.now()
	ok := true
	for _, name := range order {
		list := byPlatform[name]
		live, known := m.ask(ctx, name, list, now)
		if !known {
			ok = false
			for _, s := range list {
				res.Unknown = append(res.Unknown, name+"/"+s.GetLogin())
			}
			continue
		}
		m.live.WithLabelValues(name).Set(float64(len(live)))
		for _, s := range list {
			key := streamer{platform: name, login: s.GetLogin()}
			label := s.GetLabel()
			if label == "" {
				label = s.GetLogin()
			}
			if !m.update(ctx, key, label, live[s.GetLogin()], channel, role, cfg.GetNote(), now, opt, &res) {
				ok = false
			}
		}
	}
	if ok && len(res.Failed) == 0 {
		m.lastPassOK.SetToCurrentTime()
	}
	return res, nil
}

// ask is the platform's live streams among list by login; known is false when the platform has no
// application, is rate-limited or failed.
func (m *Module) ask(ctx context.Context, name string, list []*hubv1.DiscordStreamer, now time.Time) (map[string]Stream, bool) {
	p, ok := m.platforms[name]
	if !ok {
		m.warn(ctx, "platform:"+name, "unconfigured", "streams: a streamer is on a platform with no application in bot.yaml (streams."+name+"); skipped", nil)
		return nil, false
	}
	if until, ok := m.throttled[name]; ok && now.Before(until) {
		return nil, false
	}
	logins := make([]string, len(list))
	for i, s := range list {
		logins[i] = s.GetLogin()
	}
	streams, err := p.Live(ctx, logins)
	var te *ThrottledError
	switch {
	case err == nil:
	case errors.As(err, &te):
		m.throttled[name] = now.Add(te.RetryAfter)
		m.polls.WithLabelValues(name, "throttled").Inc()
		m.warn(ctx, "platform:"+name, "throttled", "streams: the platform is rate-limiting the bot; waiting", err)
		return nil, false
	case errors.Is(err, ErrUnauthorized):
		m.polls.WithLabelValues(name, "unauthorized").Inc()
		m.warn(ctx, "platform:"+name, "unauthorized", "streams: the platform refuses the application; check streams."+name+" in bot.yaml", err)
		return nil, false
	default:
		m.polls.WithLabelValues(name, "error").Inc()
		m.warn(ctx, "platform:"+name, "error", "streams: the platform failed, will retry", err)
		return nil, false
	}
	delete(m.throttled, name)
	m.polls.WithLabelValues(name, "ok").Inc()
	m.clear("platform:" + name)
	out := map[string]Stream{}
	for _, s := range streams {
		if slices.Contains(logins, s.Login) {
			out[s.Login] = s
		}
	}
	return out, true
}

// update brings one streamer's post in line with whether they are live; false when Discord failed.
func (m *Module) update(ctx context.Context, key streamer, label string, s Stream, channel, role snowflake.ID, note string, now time.Time,
	opt rest.RequestOpt, res *Result) bool {
	name := key.platform + "/" + key.login
	p := m.posts[key]
	isLive := s.ID != ""
	switch {
	case isLive && p != nil && p.id == s.ID && p.live:
		return true // announced, still live
	case isLive && p != nil && p.id == s.ID && s.ID != UnknownID && !p.live,
		isLive && p != nil && (p.live || now.Sub(p.ended) < Rejoin):
		// The same stream after the bot marked it ended, or a new one right after: resume the post.
		content := Render(Post{Label: label, Stream: s, Note: note})
		if _, err := m.discord.UpdateMessage(channel, p.msg, discord.MessageUpdate{Content: &content, AllowedMentions: noMentions}, opt); err != nil {
			if isUnknownMessage(err) {
				delete(m.posts, key) // deleted by someone: post anew next pass
				return true
			}
			m.failed(ctx, key, err, res)
			return false
		}
		*p = post{msg: p.msg, id: s.ID, content: content, live: true}
		res.Resumed = append(res.Resumed, name)
		m.posted.WithLabelValues(key.platform, "resumed").Inc()
		m.logger.InfoContext(ctx, "streams: resumed", "streamer", name, "stream", s.ID, "message", p.msg.String())
		return true
	case isLive:
		content := Render(Post{Label: label, Stream: s, Note: note, Role: role})
		mentions := noMentions
		if role != 0 {
			mentions = &discord.AllowedMentions{Parse: []discord.AllowedMentionType{}, Roles: []snowflake.ID{role}, Users: []snowflake.ID{}}
		}
		msg, err := m.discord.CreateMessage(channel, discord.MessageCreate{Content: content, AllowedMentions: mentions}, opt)
		if err != nil {
			m.failed(ctx, key, err, res)
			return false
		}
		m.posts[key] = &post{msg: msg.ID, id: s.ID, content: content, live: true}
		res.Posted = append(res.Posted, name)
		m.posted.WithLabelValues(key.platform, "posted").Inc()
		m.logger.InfoContext(ctx, "streams: posted", "streamer", name, "stream", s.ID, "message", msg.ID.String())
		return true
	case p != nil && p.live:
		content := Ended(p.content, label, key, p.id, now)
		flags := discord.MessageFlagSuppressEmbeds
		if _, err := m.discord.UpdateMessage(channel, p.msg, discord.MessageUpdate{Content: &content, AllowedMentions: noMentions, Flags: &flags}, opt); err != nil {
			if isUnknownMessage(err) {
				delete(m.posts, key)
				return true
			}
			m.failed(ctx, key, err, res)
			return false
		}
		p.live, p.ended, p.content = false, now, content
		res.Ended = append(res.Ended, name)
		m.posted.WithLabelValues(key.platform, "ended").Inc()
		m.logger.InfoContext(ctx, "streams: ended", "streamer", name, "stream", p.id, "message", p.msg.String())
		return true
	}
	return true
}

func (m *Module) failed(ctx context.Context, key streamer, err error, res *Result) {
	kind := classify(err)
	res.Failed = append(res.Failed, key.platform+"/"+key.login)
	m.posted.WithLabelValues(key.platform, kind).Inc()
	msg := "streams: a post failed, will retry"
	if kind == "forbidden" {
		msg = "streams: the bot may not post in the channel; it needs View Channel, Send Messages and Read Message History there"
	}
	m.warn(ctx, "post:"+key.platform+"/"+key.login, kind, msg, err)
}

// load reads the channel's newest messages for the module's own posts: each streamer's newest.
// A post found live is kept live until a platform says otherwise; one found ended counts its end
// from the message's last edit.
func (m *Module) load(opt rest.RequestOpt) error {
	msgs, err := m.discord.GetMessages(m.channel, 0, 0, 0, searchDepth, opt)
	if err != nil {
		return err
	}
	for _, msg := range msgs { // newest first
		if msg.Author.ID != m.self {
			continue
		}
		platform, login, id, ended, ok := Marker(msg.Content)
		if !ok {
			continue
		}
		key := streamer{platform: platform, login: login}
		if _, seen := m.posts[key]; seen {
			continue
		}
		p := &post{msg: msg.ID, id: id, content: msg.Content, live: !ended}
		if ended {
			p.ended = msg.CreatedAt
			if msg.EditedTimestamp != nil {
				p.ended = *msg.EditedTimestamp
			}
		}
		m.posts[key] = p
	}
	return nil
}

// warn logs a problem once until it changes or clears.
func (m *Module) warn(ctx context.Context, subject, kind, msg string, err error) {
	if m.warned[subject] == kind {
		return
	}
	m.warned[subject] = kind
	attrs := []any{"subject", subject}
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

func isUnknownMessage(err error) bool {
	var re *rest.Error
	return errors.As(err, &re) && re.Code == rest.JSONErrorCodeUnknownMessage
}

// Marker reads a post's marker: the platform, the login, the stream id and whether it ended.
func Marker(content string) (platform, login, id string, ended, ok bool) {
	g := marker.FindStringSubmatch(content)
	if g == nil {
		return "", "", "", false, false
	}
	return g[1], g[2], g[3], g[4] != "", true
}

// Post is what a go-live message shows.
type Post struct {
	Label  string
	Stream Stream
	Note   string
	// Role is pinged on a new post; zero on an edit.
	Role snowflake.ID
}

// platformNames are the platforms as a post names them.
var platformNames = map[string]string{"twitch": "Twitch", "kick": "Kick"}

// Link is the stream's page.
func Link(platform, login string) string {
	if platform == "kick" {
		return "https://kick.com/" + login
	}
	return "https://www.twitch.tv/" + login
}

// Render is a live post: who is live where, the title and the game, the link (Discord shows the
// platform's preview), the note, the role's ping and the marker. Names and titles are escaped.
func Render(p Post) string {
	s := p.Stream
	var b strings.Builder
	fmt.Fprintf(&b, "🔴 **%s** is live on %s\n", plain(p.Label), platformNames[s.Platform])
	title := cmpOr(plain(s.Title), "Live now")
	if game := plain(s.Game); game != "" {
		fmt.Fprintf(&b, "**%s** · %s\n", title, game)
	} else {
		fmt.Fprintf(&b, "**%s**\n", title)
	}
	b.WriteString(Link(s.Platform, s.Login) + "\n")
	if note := plain(p.Note); note != "" {
		b.WriteString(note + "\n")
	}
	if p.Role != 0 {
		fmt.Fprintf(&b, "<@&%s>\n", p.Role)
	}
	fmt.Fprintf(&b, "-# stream %s `%s` · %s", s.Platform, s.Login, s.ID)
	return b.String()
}

// Ended is a post after its stream: the first line says it was live and when it ended, the ping
// and the marker's live state go, and the rest of the post stays. A message this build didn't
// render keeps only the first line and the link.
func Ended(content, label string, key streamer, id string, at time.Time) string {
	lines := strings.Split(content, "\n")
	var out []string
	for i, l := range lines {
		switch {
		case i == 0:
			out = append(out, fmt.Sprintf("**%s** was live on %s · ended <t:%d:R>", plain(label), platformNames[key.platform], at.Unix()))
		case rolePing.MatchString(l), marker.MatchString(l):
		default:
			out = append(out, l)
		}
	}
	if len(lines) == 0 || !strings.HasPrefix(lines[0], "🔴 ") { // not a post this build rendered
		out = []string{out[0], Link(key.platform, key.login)}
	}
	out = append(out, fmt.Sprintf("-# stream %s `%s` · %s · ended", key.platform, key.login, id))
	return strings.Join(out, "\n")
}

var rolePing = regexp.MustCompile(`^<@&[0-9]{17,20}>$`)

func cmpOr(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// plain keeps a value a streamer or a host chose from being read as Discord markdown or a mention,
// on one line (as the server cards do).
func plain(s string) string { return strings.TrimSpace(markdown.Replace(s)) }

var markdown = strings.NewReplacer("\\", "\\\\", "*", "\\*", "_", "\\_", "~", "\\~", "`", "\\`", "|", "\\|", ">", "\\>", "#", "\\#",
	"@", "@\u200b", "\n", " ", "\r", " ", "[", "\\[", "]", "\\]", "<", "\\<")
