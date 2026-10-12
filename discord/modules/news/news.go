// Package news posts Steam games' announcements in Discord: the Organization settings'
// discord.news.feeds name each game's Steam app id, the channel, the label its posts carry, Steam's
// feeds to read and the embeds' color. bot.yaml turns the module on and sets how often it runs.
//
// A pass reads the settings, then asks Steam's public ISteamNews/GetNewsForApp (no key) for each feed's ten latest items and
// posts the ones the channel doesn't have, oldest first and at most five a pass: one embed each,
// with the title, the store's news link, the notes turned from BBCode into Markdown and cut to
// fit, the first image and a footer naming the game. A post pings no one.
//
// The bot keeps nothing (ADR-0008): before it posts, it reads the channel's 100 newest messages and
// counts every announcement linked from an embed there (/news/app/<app>/view/<gid>) as posted,
// whoever posted it, so a bridge it replaces is never repeated. The first time it sees a feed in a
// process, it posts only what is newer than the newest of the game's announcements already in the
// channel or, when there is none, what is less than a day old, so turning it on never floods the
// channel with history. A post Discord fails is not in the channel and is tried again next pass.
package news

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
	// DefaultFeed is the feed a Feed reads when it names none: the game's own announcements.
	DefaultFeed = "steam_community_announcements"
	// DefaultColor is an embed's color when a Feed sets none: Steam's blue.
	DefaultColor = 0x66c0f4
	// PostsPerPass bounds a feed's posts in one pass; a burst beyond it waits for the next.
	PostsPerPass = 5
	// Fresh is how old an item may be and still be posted to a channel that has none of the game's.
	Fresh = 24 * time.Hour
	// searchDepth is how many of the channel's newest messages are read for what was posted.
	searchDepth = 100
	// titleMax is Discord's limit on an embed's title.
	titleMax = 256
)

// Feed is one game's announcements into one channel.
type Feed struct {
	// AppID is the game's Steam app id (730 for Counter-Strike 2).
	AppID   int
	Channel snowflake.ID
	// Label names the game in each post's footer.
	Label string
	// Feeds are Steam's feed names to read; empty is DefaultFeed.
	Feeds []string
	// Color is the embeds' color as 0xRRGGBB; 0 is DefaultColor.
	Color int
}

// Config is how the module runs; the feeds are in the Organization settings.
type Config struct {
	// Interval is the time between passes.
	Interval time.Duration
	// Steam is where the news is read; nil is Steam's own API.
	Steam *Steam
}

// Discord is the part of Discord's REST API the module uses; the runtime's client is one.
type Discord interface {
	GetMessages(channelID snowflake.ID, around snowflake.ID, before snowflake.ID, after snowflake.ID, limit int, opts ...rest.RequestOpt) ([]discord.Message, error)
	CreateMessage(channelID snowflake.ID, messageCreate discord.MessageCreate, opts ...rest.RequestOpt) (*discord.Message, error)
}

// storeLink finds an announcement's app and gid in a link to the store's news page.
var storeLink = regexp.MustCompile(`/news/app/([0-9]{1,10})/view/([0-9]{1,24})(?:[/?#]|$)`)

// noMentions keeps a post from pinging anyone, whatever the notes say.
var noMentions = &discord.AllowedMentions{Parse: []discord.AllowedMentionType{}, Roles: []snowflake.ID{}, Users: []snowflake.ID{}}

// Module is the news module.
type Module struct {
	hub   *hubclient.Client
	cfg   Config
	steam *Steam

	discord Discord
	logger  *slog.Logger
	now     func() time.Time

	mu        sync.Mutex
	since     map[feedKey]time.Time       // where each feed starts: set by its first channel read in this process
	posted    map[feedKey]map[string]bool // gids this process posted, among Steam's latest
	throttled time.Time                   // Steam's rate limit, until
	warned    map[string]string           // the problem being logged once, by subject
	idle      bool

	posts  *prometheus.CounterVec
	lastOK *prometheus.GaugeVec
}

// feedKey is one game in one channel.
type feedKey struct {
	app     int
	channel snowflake.ID
}

func (k feedKey) String() string { return strconv.Itoa(k.app) + "/" + k.channel.String() }

// New builds the module.
func New(hub *hubclient.Client, cfg Config) *Module {
	steam := cfg.Steam
	if steam == nil {
		steam = NewSteam("", nil)
	}
	return &Module{hub: hub, cfg: cfg, steam: steam, logger: slog.Default(), now: time.Now, since: map[feedKey]time.Time{},
		posted: map[feedKey]map[string]bool{}, warned: map[string]string{}}
}

// Name is the module's name.
func (m *Module) Name() string { return "news" }

// Register adds the job.
func (m *Module) Register(r *bot.Registry) error {
	m.discord = r.Rest()
	m.logger = r.Logger()
	m.posts = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "gravel_bot_news_posts_total", Help: "Steam announcements posted, by Steam app id and result (posted, failed)."}, []string{"app", "result"})
	m.lastOK = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "gravel_bot_news_last_success_timestamp_seconds", Help: "When the last pass in which Steam answered, the channel was read and every post went through completed, by Steam app id, as a Unix time."}, []string{"app"})
	for _, c := range []prometheus.Collector{m.posts, m.lastOK} {
		if err := r.Metrics().Register(c); err != nil {
			return fmt.Errorf("news: metrics: %w", err)
		}
	}
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
		m.logger.WarnContext(ctx, "news: the hub is unavailable, will retry", "error", err.Error())
	}
}

// Result is what a pass did.
type Result struct {
	Idle   bool     // no feed
	Posted []string // the gids posted, in order
	// Failed are the feeds ("<app>/<channel>") whose Steam call, channel read or post failed.
	Failed []string
	// Waiting are the feeds not asked because Steam is rate-limiting the bot.
	Waiting []string
}

// Pass checks every feed once. An error means the settings could not be read.
func (m *Module) Pass(ctx context.Context) (Result, error) {
	set, err := m.hub.Organization().GetOrganizationSettings(ctx, connect.NewRequest(&hubv1.GetOrganizationSettingsRequest{}))
	if err != nil {
		return Result{}, fmt.Errorf("read the settings: %w", err)
	}
	feeds := Feeds(set.Msg.GetSettings().GetDiscord().GetNews())
	m.mu.Lock()
	defer m.mu.Unlock()
	wasIdle := m.idle
	m.idle = len(feeds) == 0
	// A feed no longer listed drops what the module knew of it: listed again, it starts afresh.
	listed := map[feedKey]bool{}
	for _, f := range feeds {
		listed[feedKey{app: f.AppID, channel: f.Channel}] = true
	}
	maps.DeleteFunc(m.since, func(k feedKey, _ time.Time) bool { return !listed[k] })
	maps.DeleteFunc(m.posted, func(k feedKey, _ map[string]bool) bool { return !listed[k] })
	if m.idle {
		if !wasIdle {
			m.logger.InfoContext(ctx, "news idle: the organization settings list no feed")
		}
		return Result{Idle: true}, nil
	}
	var res Result
	ok := map[int]bool{}
	for _, f := range feeds {
		app := strconv.Itoa(f.AppID)
		if _, seen := ok[f.AppID]; !seen {
			ok[f.AppID] = true
			m.posts.WithLabelValues(app, "posted")
			m.posts.WithLabelValues(app, "failed")
		}
		if !m.feed(ctx, f, &res) {
			ok[f.AppID] = false
		}
	}
	for app, good := range ok {
		if good {
			m.lastOK.WithLabelValues(strconv.Itoa(app)).SetToCurrentTime()
		}
	}
	return res, nil
}

// Feeds are the settings' feeds with their defaults: DefaultFeed when one names no feed and
// DefaultColor when it sets no color. A feed whose channel doesn't parse is left out; the hub
// validated it.
func Feeds(n *hubv1.DiscordNews) []Feed {
	var out []Feed
	for _, f := range n.GetFeeds() {
		ch, err := snowflake.Parse(f.GetChannel())
		if err != nil || f.GetAppId() <= 0 {
			continue
		}
		feed := Feed{AppID: int(f.GetAppId()), Channel: ch, Label: f.GetLabel(), Feeds: f.GetFeeds(), Color: int(f.GetColor())}
		if len(feed.Feeds) == 0 {
			feed.Feeds = []string{DefaultFeed}
		}
		if feed.Color == 0 {
			feed.Color = DefaultColor
		}
		out = append(out, feed)
	}
	return out
}

// feed posts what one feed's channel lacks; false when anything failed or Steam is throttling.
func (m *Module) feed(ctx context.Context, f Feed, res *Result) bool {
	key := feedKey{app: f.AppID, channel: f.Channel}
	app := strconv.Itoa(f.AppID)
	now := m.now()
	if now.Before(m.throttled) {
		res.Waiting = append(res.Waiting, key.String())
		return false
	}
	items, err := m.steam.News(ctx, f.AppID, f.Feeds)
	var te *ThrottledError
	switch {
	case err == nil:
	case errors.As(err, &te):
		m.throttled = now.Add(te.RetryAfter)
		m.warn(ctx, "steam", "throttled", "news: Steam is rate-limiting the bot; waiting", err, "app", app)
		res.Waiting = append(res.Waiting, key.String())
		return false
	default:
		if ctx.Err() == nil {
			m.warn(ctx, "steam:"+app, "error", "news: Steam failed, will retry", err, "app", app)
		}
		res.Failed = append(res.Failed, key.String())
		return false
	}
	m.clear("steam")
	m.clear("steam:" + app)
	// The query already filters on both; this keeps another app or feed out if Steam ever doesn't.
	items = slices.DeleteFunc(items, func(i Item) bool { return i.AppID != f.AppID || !slices.Contains(f.Feeds, i.FeedName) })

	opt := rest.WithCtx(ctx)
	msgs, err := m.discord.GetMessages(f.Channel, 0, 0, 0, searchDepth, opt)
	if err != nil {
		kind := classify(err)
		msg := "news: can't read the channel, will retry"
		if kind == "forbidden" {
			msg = "news: the bot may not use the channel; it needs View Channel, Send Messages, Embed Links and Read Message History there"
		}
		m.warn(ctx, "channel:"+key.String(), kind, msg, err, "app", app, "channel", f.Channel.String())
		res.Failed = append(res.Failed, key.String())
		return false
	}
	m.clear("channel:" + key.String())
	inChannel, newest := Posted(msgs, f.AppID)

	// The first look sets where the feed starts, for the life of the process: after what the
	// channel has, or a day back when it has none. Nothing older is posted, so the channel never
	// fills with history.
	since, seen := m.since[key]
	if !seen {
		since = newest
		if len(inChannel) == 0 {
			since = now.Add(-Fresh)
		}
		m.since[key] = since
	}
	mine := m.posted[key]
	if mine == nil {
		mine = map[string]bool{}
		m.posted[key] = mine
	}
	var fresh []Item
	latest := map[string]bool{}
	for _, it := range items {
		latest[it.GID] = true
		if inChannel[it.GID] || mine[it.GID] || it.Date.Before(since) {
			continue
		}
		fresh = append(fresh, it)
	}
	for gid := range mine { // keep only what Steam still lists
		if !latest[gid] {
			delete(mine, gid)
		}
	}
	slices.SortStableFunc(fresh, func(a, b Item) int { return a.Date.Compare(b.Date) })
	if len(fresh) > PostsPerPass {
		fresh = fresh[:PostsPerPass]
	}
	for _, it := range fresh {
		msg, err := m.discord.CreateMessage(f.Channel, discord.MessageCreate{Embeds: []discord.Embed{Embed(f, it)}, AllowedMentions: noMentions}, opt)
		if err != nil {
			// The rest wait, so the channel stays in order.
			m.posts.WithLabelValues(app, "failed").Inc()
			kind := classify(err)
			text := "news: a post failed, will retry"
			if kind == "forbidden" {
				text = "news: the bot may not post in the channel; it needs View Channel, Send Messages, Embed Links and Read Message History there"
			}
			m.warn(ctx, "post:"+key.String(), kind, text, err, "app", app, "channel", f.Channel.String(), "gid", it.GID)
			res.Failed = append(res.Failed, key.String())
			return false
		}
		mine[it.GID] = true
		res.Posted = append(res.Posted, it.GID)
		m.posts.WithLabelValues(app, "posted").Inc()
		m.logger.InfoContext(ctx, "news: posted", "app", app, "gid", it.GID, "title", it.Title, "message", msg.ID.String())
	}
	m.clear("post:" + key.String())
	return true
}

// Posted reads the channel's messages for the app's announcements, whoever posted them: each gid
// linked from an embed, and the newest one's date (the embed's timestamp, else the message's).
func Posted(msgs []discord.Message, appID int) (gids map[string]bool, newest time.Time) {
	gids = map[string]bool{}
	app := strconv.Itoa(appID)
	for _, msg := range msgs {
		for _, e := range msg.Embeds {
			g := storeLink.FindStringSubmatch(e.URL)
			if g == nil || g[1] != app {
				continue
			}
			gids[g[2]] = true
			at := msg.CreatedAt
			if e.Timestamp != nil {
				at = *e.Timestamp
			}
			if at.After(newest) {
				newest = at
			}
		}
	}
	return gids, newest
}

// Link is an announcement's page in the Steam store.
func Link(appID int, gid string) string {
	return "https://store.steampowered.com/news/app/" + strconv.Itoa(appID) + "/view/" + gid
}

// Embed is an item's post: the title, the store link, the notes in Markdown cut to fit, the
// feed's color, the item's date, a footer naming the game and the first image.
func Embed(f Feed, it Item) discord.Embed {
	link := Link(f.AppID, it.GID)
	title := []rune(it.Title)
	if len(title) > titleMax {
		title = title[:titleMax]
	}
	color := f.Color
	if color == 0 {
		color = DefaultColor
	}
	at := it.Date.UTC()
	e := discord.Embed{
		Title:       string(title),
		URL:         link,
		Description: Fit(ToMarkdown(it.Contents), link),
		Color:       color,
		Timestamp:   &at,
		Footer:      &discord.EmbedFooter{Text: f.Label + " · Steam announcement"},
	}
	if img := FirstImage(it.Contents); img != "" {
		e.Image = &discord.EmbedResource{URL: img}
	}
	return e
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
