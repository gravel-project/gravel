// Package servercards keeps a live status card in Discord for each server the Organization
// settings place (discord.server_cards): one Components V2 message per server, edited in place.
//
// A pass reads the settings and the hub's public server list (ServerService.ListServers, one
// call for every card), renders each card and edits its message only when the card changed. The
// observation time on a live card is rounded to the minute, so a quiet server costs at most one
// edit a minute and a change shows within one interval. The bot keeps no state in the hub
// (ADR-0008): at start, and when a message is gone, the module finds its own card among the
// channel's recent messages by the server id in the card's footer, and posts a new one only when
// there is none. A card shows a state word's meaning, never error text: the server list carries
// none. Failures are logged once per card while they last, counted, and retried at the next pass.
package servercards

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
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

// Config is how the module runs; where the cards go is in the Organization settings.
type Config struct {
	// Interval is the time between passes.
	Interval time.Duration
}

// Discord is the part of Discord's REST API the cards use; the runtime's client is one.
type Discord interface {
	GetMessages(channelID snowflake.ID, around snowflake.ID, before snowflake.ID, after snowflake.ID, limit int, opts ...rest.RequestOpt) ([]discord.Message, error)
	CreateMessage(channelID snowflake.ID, messageCreate discord.MessageCreate, opts ...rest.RequestOpt) (*discord.Message, error)
	UpdateMessage(channelID snowflake.ID, messageID snowflake.ID, messageUpdate discord.MessageUpdate, opts ...rest.RequestOpt) (*discord.Message, error)
}

// searchDepth is how many of a channel's newest messages are read to find a card: Discord's
// maximum for one call. A card buried deeper is posted again.
const searchDepth = 100

// noMentions keeps a card from pinging anyone, whatever the host's note or a name says. The
// empty lists are sent as [] (a nil one would be null, which Discord reads as its default).
var noMentions = &discord.AllowedMentions{Parse: []discord.AllowedMentionType{}, Roles: []snowflake.ID{}, Users: []snowflake.ID{}}

// Accent colours by state: live, degraded, not yet observed (0 is no colour).
const (
	colourLive     = 0x3BA55D
	colourDegraded = 0xED4245
)

// Module is the server-cards module.
type Module struct {
	hub *hubclient.Client
	cfg Config

	discord Discord
	self    snowflake.ID // the bot's user id: a bot's is its application's
	logger  *slog.Logger

	mu      sync.Mutex
	cards   map[cardKey]*card
	idle    bool // the last pass found no card (logged once)
	hubDown bool // the last pass could not read the hub (logged once)

	updates    *prometheus.CounterVec
	lastPassOK prometheus.Gauge
}

type cardKey struct {
	server  string
	channel snowflake.ID
}

// card is what the module knows about one card's message.
type card struct {
	message  snowflake.ID // 0 until found or posted
	rendered string       // the last card sent, to skip an edit that changes nothing
	failing  string       // the failure being logged once ("" when the last update worked)
}

// New builds the module.
func New(hub *hubclient.Client, cfg Config) *Module {
	return &Module{hub: hub, cfg: cfg, cards: map[cardKey]*card{}, logger: slog.Default()}
}

// Name is the module's name.
func (m *Module) Name() string { return "servercards" }

// Register adds the job.
func (m *Module) Register(r *bot.Registry) error {
	m.discord = r.Rest()
	m.self = r.ApplicationID()
	m.logger = r.Logger()
	m.updates = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "gravel_bot_servercards_updates_total", Help: "Server card updates, by result (posted, edited, error, forbidden)."}, []string{"result"})
	m.lastPassOK = prometheus.NewGauge(prometheus.GaugeOpts{Name: "gravel_bot_servercards_last_success_timestamp_seconds", Help: "When the last pass that brought every card up to date completed, as a Unix time."})
	for _, c := range []prometheus.Collector{m.updates, m.lastPassOK} {
		if err := r.Metrics().Register(c); err != nil {
			return fmt.Errorf("servercards: metrics: %w", err)
		}
	}
	r.Job("update", m.Run)
	return nil
}

// Run updates the cards until ctx is done. A failed pass is logged and counted and the next one
// tries again: the hub or Discord being away for a while is not a reason to stop the bot.
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
	res, err := m.Pass(ctx)
	switch {
	case err != nil && ctx.Err() != nil:
		return // shutting down
	case err != nil:
		m.mu.Lock()
		first := !m.hubDown
		m.hubDown = true
		m.mu.Unlock()
		if first {
			m.logger.WarnContext(ctx, "server cards: the hub is unavailable, will retry", "error", err.Error())
		}
		return
	}
	m.mu.Lock()
	recovered := m.hubDown
	m.hubDown = false
	m.mu.Unlock()
	if recovered {
		m.logger.InfoContext(ctx, "server cards: the hub is back", "cards", res.Cards)
	}
}

// Result is what a pass did.
type Result struct {
	Idle      bool // no card placed
	Cards     int
	Posted    int
	Edited    int
	Unchanged int
	Unknown   int // cards whose server the hub does not list
	Forbidden int // Discord refused: the bot may not read or post in the channel
	Failed    int
}

// Pass updates every card once. An error means the hub could not be read; a card's own failure
// is counted in the result and logged, and the other cards still update.
func (m *Module) Pass(ctx context.Context) (Result, error) {
	set, err := m.hub.Organization().GetOrganizationSettings(ctx, connect.NewRequest(&hubv1.GetOrganizationSettingsRequest{}))
	if err != nil {
		return Result{}, fmt.Errorf("read the settings: %w", err)
	}
	placed, err := placementsFrom(set.Msg.GetSettings().GetDiscord())
	if err != nil {
		return Result{}, err
	}
	m.mu.Lock()
	wasIdle := m.idle
	m.idle = len(placed) == 0
	m.mu.Unlock()
	if len(placed) == 0 {
		if !wasIdle {
			m.logger.InfoContext(ctx, "server cards idle: the organization settings place no card")
		}
		m.lastPassOK.SetToCurrentTime()
		return Result{Idle: true}, nil
	}
	list, err := m.hub.Servers().ListServers(ctx, connect.NewRequest(&hubv1.ListServersRequest{}))
	if err != nil {
		return Result{}, fmt.Errorf("list the servers: %w", err)
	}
	servers := map[string]*hubv1.Server{}
	for _, s := range list.Msg.GetServers() {
		servers[s.GetId()] = s
	}
	res := Result{Cards: len(placed)}
	for _, p := range placed {
		m.update(ctx, p, servers[p.key.server], &res)
	}
	if res.Failed+res.Forbidden+res.Unknown == 0 {
		m.lastPassOK.SetToCurrentTime()
	}
	return res, nil
}

// placement is one card from the settings.
type placement struct {
	key  cardKey
	note string
}

// placementsFrom reads the settings' server cards. The hub validated them; a channel id that does
// not parse here is still refused rather than guessed at.
func placementsFrom(d *hubv1.DiscordSettings) ([]placement, error) {
	var out []placement
	for i, c := range d.GetServerCards() {
		id, err := strconv.ParseUint(c.GetChannel(), 10, 64)
		if err != nil || c.GetServer() == "" {
			return nil, fmt.Errorf("server_cards[%d]: %q in %q is not a server in a channel", i, c.GetServer(), c.GetChannel())
		}
		out = append(out, placement{key: cardKey{server: c.GetServer(), channel: snowflake.ID(id)}, note: c.GetNote()})
	}
	return out, nil
}

// update brings one card up to date: find or post its message, edit it when the card changed.
func (m *Module) update(ctx context.Context, p placement, s *hubv1.Server, res *Result) {
	m.mu.Lock()
	c, ok := m.cards[p.key]
	if !ok {
		c = &card{}
		m.cards[p.key] = c
	}
	m.mu.Unlock()

	if s == nil {
		res.Unknown++
		m.fail(ctx, p.key, c, "unknown", "server cards: the hub lists no such server; check discord.server_cards", nil)
		return
	}
	components := Render(s, p.note)
	rendered, err := json.Marshal(components)
	if err != nil {
		res.Failed++
		m.fail(ctx, p.key, c, "render", "server cards: cannot render the card", err)
		return
	}
	if c.message != 0 && string(rendered) == c.rendered {
		res.Unchanged++
		m.recover(ctx, p.key, c)
		return
	}

	opt := rest.WithCtx(ctx)
	if c.message == 0 {
		id, err := m.find(p.key, opt)
		if err != nil {
			m.failed(ctx, p.key, c, err, res)
			return
		}
		c.message = id
	}
	if c.message != 0 {
		_, err = m.discord.UpdateMessage(p.key.channel, c.message, discord.NewMessageUpdateV2(components...).WithAllowedMentions(noMentions), opt)
		if classify(err) == "gone" { // deleted since: post it again
			c.message = 0
			err = nil
		} else if err == nil {
			c.rendered = string(rendered)
			res.Edited++
			m.updates.WithLabelValues("edited").Inc()
			m.recover(ctx, p.key, c)
			return
		}
	}
	if err == nil {
		var msg *discord.Message
		msg, err = m.discord.CreateMessage(p.key.channel, discord.NewMessageCreateV2(components...).WithAllowedMentions(noMentions), opt)
		if err == nil {
			c.message, c.rendered = msg.ID, string(rendered)
			res.Posted++
			m.updates.WithLabelValues("posted").Inc()
			m.logger.InfoContext(ctx, "server cards: card posted", "server", p.key.server, "channel", p.key.channel.String(), "message", msg.ID.String())
			m.recover(ctx, p.key, c)
			return
		}
	}
	m.failed(ctx, p.key, c, err, res)
}

// find returns the message of the bot's own card for the server in the channel, 0 for none.
func (m *Module) find(k cardKey, opts ...rest.RequestOpt) (snowflake.ID, error) {
	msgs, err := m.discord.GetMessages(k.channel, 0, 0, 0, searchDepth, opts...)
	if err != nil {
		return 0, err
	}
	for _, msg := range msgs { // newest first
		if msg.Author.ID == m.self && msg.Flags.Has(discord.MessageFlagIsComponentsV2) && CardServer(msg.Components) == k.server {
			return msg.ID, nil
		}
	}
	return 0, nil
}

func (m *Module) failed(ctx context.Context, k cardKey, c *card, err error, res *Result) {
	if classify(err) == "forbidden" {
		res.Forbidden++
		m.updates.WithLabelValues("forbidden").Inc()
		m.fail(ctx, k, c, "forbidden", "server cards: the bot may not use the channel; it needs View Channel, Send Messages and Read Message History there", err)
		return
	}
	res.Failed++
	m.updates.WithLabelValues("error").Inc()
	m.fail(ctx, k, c, "error", "server cards: update failed, will retry", err)
}

// fail logs a card's failure once while it lasts.
func (m *Module) fail(ctx context.Context, k cardKey, c *card, kind, msg string, err error) {
	if c.failing == kind {
		return
	}
	c.failing = kind
	args := []any{"server", k.server, "channel", k.channel.String()}
	if err != nil {
		args = append(args, "error", err.Error())
	}
	m.logger.WarnContext(ctx, msg, args...)
}

func (m *Module) recover(ctx context.Context, k cardKey, c *card) {
	if c.failing == "" {
		return
	}
	c.failing = ""
	m.logger.InfoContext(ctx, "server cards: card updating again", "server", k.server, "channel", k.channel.String())
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

// footerPrefix starts the card's last line; the server id after it marks the card as the
// server's, which is how the module finds it again.
const footerPrefix = "-# `"

// CardServer is the server id a card's footer names, "" when the components are not a card.
func CardServer(components []discord.LayoutComponent) string {
	for _, lc := range components {
		box, ok := lc.(discord.ContainerComponent)
		if !ok {
			continue
		}
		for _, sc := range box.Components {
			if t, ok := sc.(discord.TextDisplayComponent); ok && strings.HasPrefix(t.Content, footerPrefix) {
				id, _, found := strings.Cut(strings.TrimPrefix(t.Content, footerPrefix), "`")
				if found {
					return id
				}
			}
		}
	}
	return ""
}

// Render is a server's card: one container with the name, the numbers or the degraded state,
// the host's note, and a footer naming the server. A field the game does not report is left out.
func Render(s *hubv1.Server, note string) []discord.LayoutComponent {
	st := s.GetStatus()
	var observed int64
	if t := st.GetObservedAt(); t != nil && t.IsValid() {
		observed = t.AsTime().Unix()
	}
	parts := []discord.ContainerSubComponent{discord.NewTextDisplay("## " + plain(s.GetName()))}
	colour, footer := colourDegraded, ""
	switch {
	case st.GetState() == "ok" && st.GetReachable():
		colour = colourLive
		parts = append(parts, discord.NewTextDisplay(liveLine(st)))
		if teams := teamLine(st.GetTeams()); teams != "" {
			parts = append(parts, discord.NewTextDisplay(teams))
		}
		footer = "Live"
		if observed > 0 {
			footer += fmt.Sprintf(" · updated <t:%d:R>", observed-observed%60)
		}
	case st.GetState() == "unknown" || st.GetState() == "":
		colour = 0
		parts = append(parts, discord.NewTextDisplay("Waiting for the first look at this server."))
		footer = "Not observed yet"
	case st.GetState() == "unreachable" || !st.GetReachable():
		line := "**Unreachable.**"
		if observed > 0 {
			line = fmt.Sprintf("**Unreachable** since <t:%d:R>.", observed)
		}
		parts = append(parts, discord.NewTextDisplay(line))
		footer = "Unreachable"
	default:
		parts = append(parts, discord.NewTextDisplay("**Status unavailable:** the hub cannot read this server right now."))
		footer = "Unavailable (" + plain(st.GetState()) + ")"
	}
	if note = strings.TrimSpace(note); note != "" {
		parts = append(parts, discord.NewTextDisplay(note))
	}
	parts = append(parts, discord.NewSmallSeparator(), discord.NewTextDisplay(footerPrefix+s.GetId()+"` · "+footer))
	box := discord.NewContainer(parts...)
	box.AccentColor = colour
	return []discord.LayoutComponent{box}
}

func liveLine(st *hubv1.ServerStatus) string {
	players := fmt.Sprintf("**%d** players", st.GetPlayers())
	if most := st.GetMaxPlayers(); most > 0 {
		players = fmt.Sprintf("**%d/%d** players", st.GetPlayers(), most)
	}
	out := []string{players}
	if m := plain(st.GetMap()); m != "" {
		if l := plain(st.GetLighting()); l != "" {
			m += " (" + l + ")"
		}
		out = append(out, m)
	}
	return strings.Join(out, " · ")
}

func teamLine(teams []*hubv1.TeamScore) string {
	var out []string
	for _, t := range teams {
		if name := plain(t.GetName()); name != "" {
			out = append(out, fmt.Sprintf("%s **%d**", name, t.GetScore()))
		}
	}
	return strings.Join(out, " · ")
}

// plain keeps a value the game or the host names from being read as Discord markdown or a
// mention: a server's name, a map's, a team's.
func plain(s string) string { return strings.TrimSpace(markdown.Replace(s)) }

// markdown escapes Discord's markdown characters, breaks a mention with a zero-width space and
// keeps a value on one line.
var markdown = strings.NewReplacer("\\", "\\\\", "*", "\\*", "_", "\\_", "~", "\\~", "`", "\\`", "|", "\\|", ">", "\\>", "#", "\\#",
	"@", "@\u200b", "\n", " ", "\r", " ")
