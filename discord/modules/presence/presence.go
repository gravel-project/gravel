// Package presence keeps a "who's playing now" card in Discord (ADR-0017): one Components V2
// message in the channel the Organization settings place (discord.presence), with how many members
// are playing each of the settings' games right now. Counts only: nothing is stored, and the card
// never names a member.
//
// The module asks the gateway for guild presences, a privileged intent the application must be
// allowed in the Developer Portal. It keeps a live view in memory, each online member's playing
// activities (the name and the application id, nothing else), from the guild's presences when the
// gateway delivers the guild, each presence update after it, and a member leaving. A pass reads
// the settings, counts the view against the settings' games and edits the card only when a count
// changed, so a quiet guild costs one hub call a pass and no Discord call. Only members who share
// their activity on Discord are seen at all.
//
// Without the intent (the runtime leaves it out when the application isn't allowed it) the card
// says the bot can't see what members are playing, rather than showing counts of nobody. Before
// the gateway has delivered the guild, the card is left as it is. As with the server cards, the
// module finds its own card again after a restart by the marker in the card's footer.
package presence

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	disgobot "github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/disgo/gateway"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/gravel-project/gravel/discord/bot"
	"github.com/gravel-project/gravel/discord/hubclient"
	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
)

// Config is how the module runs; where the card goes and which games it counts are in the
// Organization settings.
type Config struct {
	// Interval is the time between passes: the longest a changed count waits to show.
	Interval time.Duration
}

// Settings reads the Organization settings' Discord section; the hub's OrganizationService is one.
type Settings func(ctx context.Context) (*hubv1.DiscordSettings, error)

// HubSettings reads the settings from the hub.
func HubSettings(hub *hubclient.Client) Settings {
	return func(ctx context.Context) (*hubv1.DiscordSettings, error) {
		res, err := hub.Organization().GetOrganizationSettings(ctx, connect.NewRequest(&hubv1.GetOrganizationSettingsRequest{}))
		if err != nil {
			return nil, err
		}
		return res.Msg.GetSettings().GetDiscord(), nil
	}
}

// Discord is the part of Discord's REST API the card uses; the runtime's client is one.
type Discord interface {
	GetMessages(channelID snowflake.ID, around snowflake.ID, before snowflake.ID, after snowflake.ID, limit int, opts ...rest.RequestOpt) ([]discord.Message, error)
	CreateMessage(channelID snowflake.ID, messageCreate discord.MessageCreate, opts ...rest.RequestOpt) (*discord.Message, error)
	UpdateMessage(channelID snowflake.ID, messageID snowflake.ID, messageUpdate discord.MessageUpdate, opts ...rest.RequestOpt) (*discord.Message, error)
}

// searchDepth is how many of the channel's newest messages are read to find the card.
const searchDepth = 100

// noMentions keeps the card from pinging anyone, whatever a label or the note says.
var noMentions = &discord.AllowedMentions{Parse: []discord.AllowedMentionType{}, Roles: []snowflake.ID{}, Users: []snowflake.ID{}}

// Marker starts the card's footer; it is how the module finds its card again.
const Marker = "-# `presence`"

// accent is the card's colour.
const accent = 0x5865F2

// Module is the presence module.
type Module struct {
	settings Settings
	cfg      Config

	discord Discord
	self    snowflake.ID
	logger  *slog.Logger
	now     func() time.Time
	// live is whether the gateway delivers presences: the intent was asked for and granted.
	live bool

	mu     sync.Mutex                                   // guards guilds and state: the listener writes the view, the job reads it
	guilds map[snowflake.ID]map[snowflake.ID][]activity // the live view: guild → member → playing
	state  string                                       // what the last pass logged: "", "idle", "hub", "waiting"
	card   card                                         // the job's alone

	playing    *prometheus.GaugeVec
	tracked    prometheus.Gauge
	updates    *prometheus.CounterVec
	lastPassOK prometheus.Gauge
}

// activity is what the view keeps of one playing activity: enough to match a game, nothing more.
type activity struct {
	name string // lower-cased
	app  string // the activity's Discord application id, "" when it has none
}

// card is what the module knows about the card's message.
type card struct {
	channel snowflake.ID
	message snowflake.ID // 0 until found or posted
	shown   string       // the counts the message shows, to skip an edit that changes nothing
	failing string       // the failure being logged once
}

// New builds the module with the hub's settings.
func New(hub *hubclient.Client, cfg Config) *Module { return NewWith(HubSettings(hub), cfg) }

// NewWith builds the module with any settings source (tests).
func NewWith(s Settings, cfg Config) *Module {
	return &Module{settings: s, cfg: cfg, logger: slog.Default(), now: time.Now, guilds: map[snowflake.ID]map[snowflake.ID][]activity{}}
}

// Name is the module's name.
func (m *Module) Name() string { return "presence" }

// Intents asks for guild presences (privileged), beside the guilds the runtime always asks for.
func (m *Module) Intents() gateway.Intents {
	return gateway.IntentGuilds | gateway.IntentGuildPresences
}

// Register adds the listener and the job.
func (m *Module) Register(r *bot.Registry) error {
	m.discord = r.Rest()
	m.self = r.ApplicationID()
	m.logger = r.Logger()
	m.live = r.HasIntent(gateway.IntentGuildPresences)
	if !m.live {
		m.logger.Warn("presence: the gateway delivers no presences (the gateway is off, or the application isn't allowed the Presence Intent), so the card says it can't see who is playing")
	}
	m.playing = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "gravel_bot_presence_playing", Help: "Members Discord shows playing each game on the presence card, at the last pass."}, []string{"game"})
	m.tracked = prometheus.NewGauge(prometheus.GaugeOpts{Name: "gravel_bot_presence_members_tracked", Help: "Members in the live view: online and playing something, in any guild. Held in memory only."})
	m.updates = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "gravel_bot_presence_updates_total", Help: "Presence card updates, by result (posted, edited, error, forbidden)."}, []string{"result"})
	m.lastPassOK = prometheus.NewGauge(prometheus.GaugeOpts{Name: "gravel_bot_presence_last_success_timestamp_seconds", Help: "When the last pass that brought the card up to date completed, as a Unix time."})
	for _, c := range []prometheus.Collector{m.playing, m.tracked, m.updates, m.lastPassOK} {
		if err := r.Metrics().Register(c); err != nil {
			return fmt.Errorf("presence: metrics: %w", err)
		}
	}
	r.Listen(disgobot.NewListenerFunc(m.OnEvent))
	r.Job("card", m.Run)
	return nil
}

// OnEvent keeps the live view: a guild's presences when the gateway delivers it, then each update.
func (m *Module) OnEvent(e disgobot.Event) {
	switch e := e.(type) {
	case *events.GuildReady:
		m.LoadGuild(e.GuildID, e.Guild.Presences)
	case *events.GuildAvailable:
		m.LoadGuild(e.GuildID, e.Guild.Presences)
	case *events.GuildJoin:
		m.LoadGuild(e.GuildID, e.Guild.Presences)
	case *events.GuildUnavailable:
		m.ForgetGuild(e.GuildID)
	case *events.GuildLeave:
		m.ForgetGuild(e.GuildID)
	case *events.PresenceUpdate:
		m.Observe(e.Presence)
	case *events.GuildMemberLeave:
		m.ForgetMember(e.GuildID, e.User.ID)
	}
}

// LoadGuild replaces a guild's view with its presences, as the gateway delivers them when the
// guild becomes available: every online member's, offline ones left out.
func (m *Module) LoadGuild(guildID snowflake.ID, presences []discord.Presence) {
	view := map[snowflake.ID][]activity{}
	for _, p := range presences {
		if a := playing(p); len(a) > 0 {
			view[p.PresenceUser.ID] = a
		}
	}
	m.mu.Lock()
	m.guilds[guildID] = view
	m.setTracked()
	m.mu.Unlock()
}

// Observe applies one presence update: a member who goes offline or stops playing leaves the view.
func (m *Module) Observe(p discord.Presence) {
	m.mu.Lock()
	defer m.mu.Unlock()
	view, ok := m.guilds[p.GuildID]
	if !ok {
		return // a guild not delivered yet: its own presences will come with it
	}
	if a := playing(p); len(a) > 0 {
		view[p.PresenceUser.ID] = a
	} else {
		delete(view, p.PresenceUser.ID)
	}
	m.setTracked()
}

// ForgetMember drops a member who left the guild.
func (m *Module) ForgetMember(guildID, userID snowflake.ID) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.guilds[guildID], userID)
	m.setTracked()
}

// ForgetGuild drops a guild the bot left or lost: until it is delivered again it has no counts.
func (m *Module) ForgetGuild(guildID snowflake.ID) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.guilds, guildID)
	m.setTracked()
}

func (m *Module) setTracked() {
	if m.tracked == nil {
		return
	}
	n := 0
	for _, v := range m.guilds {
		n += len(v)
	}
	m.tracked.Set(float64(n))
}

// playing is what a presence is playing: its game and competing activities, deduplicated.
func playing(p discord.Presence) []activity {
	if p.Status == discord.OnlineStatusOffline {
		return nil
	}
	var out []activity
	for _, a := range p.Activities {
		if a.Type != discord.ActivityTypeGame && a.Type != discord.ActivityTypeCompeting {
			continue
		}
		act := activity{name: strings.ToLower(strings.TrimSpace(a.Name)), app: a.ApplicationID.String()}
		if !slices.Contains(out, act) {
			out = append(out, act)
		}
	}
	return out
}

// Run updates the card until ctx is done. A failed pass is logged and the next one tries again.
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
		m.logState(ctx, "hub", "presence: the hub is unavailable, will retry", "error", err.Error())
	}
}

// logState logs a pass's state once while it lasts, and its end.
func (m *Module) logState(ctx context.Context, state, msg string, args ...any) {
	m.mu.Lock()
	prev := m.state
	m.state = state
	m.mu.Unlock()
	if state == prev {
		return
	}
	switch state {
	case "":
		m.logger.InfoContext(ctx, "presence: card updating", "after", prev)
	case "hub":
		m.logger.WarnContext(ctx, msg, args...)
	default:
		m.logger.InfoContext(ctx, msg, args...)
	}
}

// Result is what a pass did.
type Result struct {
	Idle      bool // no card placed
	Waiting   bool // the gateway hasn't delivered the guild yet; the card was left alone
	Live      bool // counted from presences (false: the card says it can't see)
	Counts    map[string]int
	Posted    bool
	Edited    bool
	Unchanged bool
	Forbidden bool
	Failed    bool
}

// Pass updates the card once. An error means the settings could not be read.
func (m *Module) Pass(ctx context.Context) (Result, error) {
	d, err := m.settings(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("read the settings: %w", err)
	}
	pr := d.GetPresence()
	channel, perr := strconv.ParseUint(pr.GetChannel(), 10, 64)
	guild, gerr := strconv.ParseUint(d.GetGuildId(), 10, 64)
	if pr == nil || len(pr.GetGames()) == 0 || perr != nil || gerr != nil {
		m.playing.Reset()
		m.logState(ctx, "idle", "presence idle: the organization settings place no presence card")
		m.lastPassOK.SetToCurrentTime()
		return Result{Idle: true}, nil
	}

	res := Result{Live: m.live}
	var counts []int
	if m.live {
		var ok bool
		counts, ok = m.count(snowflake.ID(guild), pr.GetGames())
		if !ok {
			m.logState(ctx, "waiting", "presence: waiting for the gateway to deliver the guild", "guild", d.GetGuildId())
			return Result{Waiting: true, Live: true}, nil
		}
		res.Counts = map[string]int{}
		m.playing.Reset()
		for i, g := range pr.GetGames() {
			res.Counts[g.GetGame()] = counts[i]
			m.playing.WithLabelValues(g.GetGame()).Set(float64(counts[i]))
		}
	} else {
		m.playing.Reset()
	}

	view := View{Games: pr.GetGames(), Counts: counts, Live: m.live, Note: pr.GetNote()}
	m.update(ctx, snowflake.ID(channel), view, &res)
	if !res.Failed && !res.Forbidden {
		m.lastPassOK.SetToCurrentTime()
		m.logState(ctx, "", "")
	}
	return res, nil
}

// count is each game's count in a guild's view, false when the gateway hasn't delivered the guild.
// A member counts once for a game however many of their activities match it.
func (m *Module) count(guild snowflake.ID, games []*hubv1.DiscordPresenceGame) ([]int, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	view, ok := m.guilds[guild]
	if !ok {
		return nil, false
	}
	counts := make([]int, len(games))
	for _, acts := range view {
		for i, g := range games {
			if matches(g, acts) {
				counts[i]++
			}
		}
	}
	return counts, true
}

func matches(g *hubv1.DiscordPresenceGame, acts []activity) bool {
	for _, a := range acts {
		if a.app != "" && slices.Contains(g.GetApplicationIds(), a.app) {
			return true
		}
		for _, n := range g.GetActivities() {
			if a.name != "" && strings.EqualFold(strings.TrimSpace(n), a.name) {
				return true
			}
		}
	}
	return false
}

// update brings the card up to date: find or post its message, edit it when a count changed.
func (m *Module) update(ctx context.Context, channel snowflake.ID, v View, res *Result) {
	c := &m.card
	if c.channel != channel { // moved: find or post it in the new channel
		*c = card{channel: channel}
	}

	shown, err := json.Marshal(v.key())
	if err != nil {
		res.Failed = true
		return
	}
	if c.message != 0 && string(shown) == c.shown {
		res.Unchanged = true
		return
	}
	components := Render(v, m.now())
	opt := rest.WithCtx(ctx)
	if c.message == 0 {
		id, err := m.find(channel, opt)
		if err != nil {
			m.failed(ctx, c, err, res)
			return
		}
		c.message = id
	}
	if c.message != 0 {
		_, err = m.discord.UpdateMessage(channel, c.message, discord.NewMessageUpdateV2(components...).WithAllowedMentions(noMentions), opt)
		if classify(err) == "gone" {
			c.message, err = 0, nil
		} else if err == nil {
			c.shown = string(shown)
			res.Edited = true
			m.updates.WithLabelValues("edited").Inc()
			m.recover(ctx, c)
			return
		}
	}
	if err == nil {
		var msg *discord.Message
		msg, err = m.discord.CreateMessage(channel, discord.NewMessageCreateV2(components...).WithAllowedMentions(noMentions), opt)
		if err == nil {
			c.message, c.shown = msg.ID, string(shown)
			res.Posted = true
			m.updates.WithLabelValues("posted").Inc()
			m.logger.InfoContext(ctx, "presence: card posted", "channel", channel.String(), "message", msg.ID.String())
			m.recover(ctx, c)
			return
		}
	}
	m.failed(ctx, c, err, res)
}

// find returns the bot's own card in the channel, 0 for none.
func (m *Module) find(channel snowflake.ID, opts ...rest.RequestOpt) (snowflake.ID, error) {
	msgs, err := m.discord.GetMessages(channel, 0, 0, 0, searchDepth, opts...)
	if err != nil {
		return 0, err
	}
	for _, msg := range msgs { // newest first
		if msg.Author.ID == m.self && msg.Flags.Has(discord.MessageFlagIsComponentsV2) && IsCard(msg.Components) {
			return msg.ID, nil
		}
	}
	return 0, nil
}

func (m *Module) failed(ctx context.Context, c *card, err error, res *Result) {
	kind, msg := "error", "presence: card update failed, will retry"
	if classify(err) == "forbidden" {
		kind, msg = "forbidden", "presence: the bot may not use the channel; it needs View Channel, Send Messages and Read Message History there"
		res.Forbidden = true
	} else {
		res.Failed = true
	}
	m.updates.WithLabelValues(kind).Inc()
	if c.failing == kind {
		return
	}
	c.failing = kind
	m.logger.WarnContext(ctx, msg, "channel", c.channel.String(), "error", err.Error())
}

func (m *Module) recover(ctx context.Context, c *card) {
	if c.failing == "" {
		return
	}
	c.failing = ""
	m.logger.InfoContext(ctx, "presence: card updating again", "channel", c.channel.String())
}

// classify sorts a REST error into what an update does with it.
func classify(err error) string {
	if err == nil {
		return "ok"
	}
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

// View is what the card shows.
type View struct {
	Games  []*hubv1.DiscordPresenceGame
	Counts []int // by game, in order; nil when not Live
	Live   bool  // false: the bot can't see presences
	Note   string
}

// key is what decides whether the card needs an edit: everything it shows but the time.
func (v View) key() any {
	labels := make([]string, len(v.Games))
	for i, g := range v.Games {
		labels[i] = g.GetLabel()
	}
	return struct {
		Labels []string
		Counts []int
		Live   bool
		Note   string
	}{labels, v.Counts, v.Live, strings.TrimSpace(v.Note)}
}

// Render is the card: a line per game, the host's note, and a footer with the marker and when the
// counts last changed (Discord shows the time relative to the reader's clock).
func Render(v View, at time.Time) []discord.LayoutComponent {
	var lines []string
	if v.Live {
		for i, g := range v.Games {
			n := 0
			if i < len(v.Counts) {
				n = v.Counts[i]
			}
			what := "nobody right now"
			if n > 0 {
				what = strconv.Itoa(n) + " playing"
			}
			lines = append(lines, "**"+escape(g.GetLabel())+"** · "+what)
		}
	} else {
		lines = append(lines, "The bot can't see what members are playing right now, so there are no counts to show.")
	}
	parts := []discord.ContainerSubComponent{
		discord.NewTextDisplay("## Who's playing now"),
		discord.NewTextDisplay(strings.Join(lines, "\n")),
	}
	if note := strings.TrimSpace(v.Note); note != "" {
		parts = append(parts, discord.NewTextDisplay(escape(note)))
	}
	parts = append(parts, discord.NewTextDisplay(fmt.Sprintf("%s · members who share their activity on Discord · changed <t:%d:R>", Marker, at.Unix())))
	return []discord.LayoutComponent{discord.NewContainer(parts...).WithAccentColor(accent)}
}

// IsCard reports whether components are a presence card, by the marker in its footer.
func IsCard(components []discord.LayoutComponent) bool {
	for _, lc := range components {
		box, ok := lc.(discord.ContainerComponent)
		if !ok {
			continue
		}
		for _, sc := range box.Components {
			if t, ok := sc.(discord.TextDisplayComponent); ok && strings.HasPrefix(t.Content, Marker) {
				return true
			}
		}
	}
	return false
}

// escape keeps a host's label or note from being read as Discord markdown or a mention, on one
// line (as the server cards do).
func escape(s string) string { return strings.TrimSpace(markdown.Replace(s)) }

var markdown = strings.NewReplacer("\\", "\\\\", "*", "\\*", "_", "\\_", "~", "\\~", "`", "\\`", "|", "\\|", ">", "\\>", "#", "\\#",
	"@", "@\u200b", "\n", " ", "\r", " ")
