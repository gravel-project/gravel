// Package events mirrors the hub's events log into the channel the Organization settings name
// (discord.events, gravel#200): the bot is one consumer of the log (ADR-0028), and staff act from
// Discord within what the hub lets them do.
//
// A pass reads the settings, then the log after the last event posted (ListEvents with the
// settings' lowest severity and kinds, oldest first), and posts each event once. A trouble event
// about a server whose machine gravel runs (server.unreachable, host.failing on a cloud-VM server)
// carries Restart and Park buttons. A click asks the clicker privately to confirm; the action goes
// to the hub on behalf of their Discord account, and the hub admits it only if that member holds
// servers:operate for the server's game. The bot keeps no state in the hub (ADR-0008): at start, or
// when the channel changes, it finds the last event it posted from the marker in its own messages
// ("-# event <id>") among the channel's newest; with none it starts at the log's head, so turning
// it on never floods the channel with history. Failures are logged once while they last, counted,
// and retried next pass.
package events

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
	"github.com/disgoorg/disgo/handler"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/omit"
	"github.com/disgoorg/snowflake/v2"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/gravel-project/gravel/discord/bot"
	"github.com/gravel-project/gravel/discord/hubclient"
	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
)

// The reads a pass makes: at most maxPages of pageSize events, and searchDepth of the channel's
// newest messages for the marker.
const (
	pageSize    = 100
	maxPages    = 5
	searchDepth = 100
)

// The actions a button offers.
const (
	Restart = "restart"
	Park    = "park"
)

// actionable are the event kinds that carry the buttons, on a server whose machine gravel runs.
var actionable = map[string]bool{"server.unreachable": true, "host.failing": true}

// marker finds the event id in a message the module posted.
var marker = regexp.MustCompile(`(?m)^-# event ([0-9]+)\b`)

// serverID is a server's id as the hub writes it, checked before it goes in a custom id.
var serverID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// noMentions keeps a post from pinging anyone, whatever a summary says.
var noMentions = &discord.AllowedMentions{Parse: []discord.AllowedMentionType{}, Roles: []snowflake.ID{}, Users: []snowflake.ID{}}

// Config is how the module runs; where it posts is in the Organization settings.
type Config struct {
	Interval time.Duration
	// PublicURL is the hub's address, for a link to the event in the admin dashboard; empty is none.
	PublicURL string
}

// Hub is what the module needs from the hub; clientHub is the hub's API as the bot's app.
type Hub interface {
	// Feed is the settings' discord.events; nil when unset.
	Feed(ctx context.Context) (*hubv1.DiscordEvents, error)
	// Events are the log's events after afterID that the filter takes, oldest first; with afterID
	// 0, the newest event alone. head is the log's newest id.
	Events(ctx context.Context, afterID int64, minSeverity string, kinds []string, limit int) (evs []*hubv1.Event, next, head int64, err error)
	// Servers are the servers' names by id.
	Servers(ctx context.Context) (map[string]string, error)
	// HasHost reports whether gravel runs the server's machine (a cloud-VM server).
	HasHost(ctx context.Context, serverID string) (bool, error)
	// Act restarts or parks a server on behalf of a member.
	Act(ctx context.Context, action, serverID string, by *hubv1.Actor) error
}

// Discord is the part of Discord's REST API the module uses; the runtime's client is one.
type Discord interface {
	GetMessages(channelID snowflake.ID, around snowflake.ID, before snowflake.ID, after snowflake.ID, limit int, opts ...rest.RequestOpt) ([]discord.Message, error)
	CreateMessage(channelID snowflake.ID, messageCreate discord.MessageCreate, opts ...rest.RequestOpt) (*discord.Message, error)
}

// Module is the events module.
type Module struct {
	hub Hub
	cfg Config

	discord Discord
	self    snowflake.ID
	logger  *slog.Logger

	mu      sync.Mutex
	channel snowflake.ID // the channel the cursor belongs to
	cursor  int64        // the last event id posted (or the head at start)
	idle    bool
	hubDown bool

	posts      *prometheus.CounterVec
	actions    *prometheus.CounterVec
	lastPassOK prometheus.Gauge
}

// New builds the module over the hub's API; the bot's app needs events:read, and servers:read and
// servers:operate for the buttons.
func New(hub *hubclient.Client, cfg Config) *Module { return NewWithHub(clientHub{hub}, cfg) }

// NewWithHub builds the module over any Hub (tests use a fake).
func NewWithHub(hub Hub, cfg Config) *Module {
	return &Module{hub: hub, cfg: cfg, logger: slog.Default()}
}

// Name is the module's name.
func (*Module) Name() string { return "events" }

// Register adds the job and the buttons' handlers.
func (m *Module) Register(r *bot.Registry) error {
	m.discord = r.Rest()
	m.self = r.ApplicationID()
	m.logger = r.Logger()
	m.posts = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "gravel_bot_events_posts_total", Help: "Events posted to the events channel, by result (posted, forbidden, error)."}, []string{"result"})
	m.actions = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "gravel_bot_events_actions_total",
		Help: "Restart and Park from the events channel, by action and outcome (ok, refused, failed)."}, []string{"action", "outcome"})
	m.lastPassOK = prometheus.NewGauge(prometheus.GaugeOpts{Name: "gravel_bot_events_last_success_timestamp_seconds", Help: "When the last pass that posted everything completed, as a Unix time."})
	for _, c := range []prometheus.Collector{m.posts, m.actions, m.lastPassOK} {
		if err := r.Metrics().Register(c); err != nil {
			return fmt.Errorf("events: metrics: %w", err)
		}
	}
	r.Job("post", m.Run)
	r.Component("/events/ask/{action}/{server}", m.ask)
	r.Component("/events/do/{action}/{server}", m.do)
	r.Component("/events/cancel", m.cancel)
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
	_, err := m.Pass(ctx)
	m.mu.Lock()
	was := m.hubDown
	m.hubDown = err != nil && ctx.Err() == nil
	now := m.hubDown
	m.mu.Unlock()
	switch {
	case now && !was:
		m.logger.WarnContext(ctx, "events: the hub is unavailable, will retry", "error", err.Error())
	case !now && was:
		m.logger.InfoContext(ctx, "events: the hub is back")
	}
}

// Result is what a pass did.
type Result struct {
	Idle      bool // no channel set
	Posted    int
	Forbidden bool
	Failed    bool
}

// Pass posts every event the filter takes since the last one posted. An error means the hub could
// not be read; a Discord failure is in the result, and the event is tried again next pass.
func (m *Module) Pass(ctx context.Context) (Result, error) {
	feed, err := m.hub.Feed(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("read the settings: %w", err)
	}
	raw := feed.GetChannel()
	m.mu.Lock()
	wasIdle := m.idle
	m.idle = raw == ""
	m.mu.Unlock()
	if raw == "" {
		if !wasIdle {
			m.logger.InfoContext(ctx, "events idle: the organization settings name no channel (discord.events.channel)")
		}
		m.lastPassOK.SetToCurrentTime()
		return Result{Idle: true}, nil
	}
	channel, err := snowflake.Parse(raw)
	if err != nil {
		return Result{}, fmt.Errorf("discord.events.channel %q: %w", raw, err)
	}

	m.mu.Lock()
	cursor, known := m.cursor, m.channel == channel
	m.mu.Unlock()
	if !known {
		found, ok, err := m.findCursor(channel)
		if err != nil {
			return m.discordFailed(ctx, channel, err), nil
		}
		if !ok { // nothing posted here yet: start at the log's head
			_, _, head, err := m.hub.Events(ctx, 0, "", nil, 1)
			if err != nil {
				return Result{}, fmt.Errorf("read the events log: %w", err)
			}
			found = head
		}
		cursor = found
		m.mu.Lock()
		m.channel, m.cursor = channel, cursor
		m.mu.Unlock()
	}

	var res Result
	var names map[string]string
	for range maxPages {
		evs, next, _, err := m.hub.Events(ctx, cursor, feed.GetMinSeverity(), feed.GetKinds(), pageSize)
		if err != nil {
			return res, fmt.Errorf("read the events log: %w", err)
		}
		if len(evs) > 0 && names == nil {
			if names, err = m.hub.Servers(ctx); err != nil {
				return res, fmt.Errorf("list the servers: %w", err)
			}
		}
		for _, e := range evs {
			msg, err := m.message(ctx, e, names[e.GetServerId()])
			if err != nil {
				return res, err
			}
			if _, err := m.discord.CreateMessage(channel, msg); err != nil {
				fail := m.discordFailed(ctx, channel, err)
				fail.Posted = res.Posted
				return fail, nil
			}
			res.Posted++
			m.posts.WithLabelValues("posted").Inc()
			cursor = e.GetId()
			m.mu.Lock()
			m.cursor = cursor
			m.mu.Unlock()
		}
		if next > cursor {
			cursor = next
		}
		if len(evs) < pageSize {
			break
		}
	}
	m.lastPassOK.SetToCurrentTime()
	return res, nil
}

// message is an event's post: the line, and the buttons when the event is a cloud-VM server's
// trouble.
func (m *Module) message(ctx context.Context, e *hubv1.Event, server string) (discord.MessageCreate, error) {
	msg := discord.MessageCreate{Content: Line(e, server, m.link(e)), AllowedMentions: noMentions}
	if !actionable[e.GetKind()] || !serverID.MatchString(e.GetServerId()) {
		return msg, nil
	}
	host, err := m.hub.HasHost(ctx, e.GetServerId())
	if err != nil {
		return msg, fmt.Errorf("read the server's host: %w", err)
	}
	if host {
		msg.Components = []discord.LayoutComponent{discord.NewActionRow(
			discord.NewDangerButton("Restart", "/events/ask/"+Restart+"/"+e.GetServerId()),
			discord.NewSecondaryButton("Park", "/events/ask/"+Park+"/"+e.GetServerId()),
		)}
	}
	return msg, nil
}

// link is the event's server in the admin dashboard's events feed; "" without a public address.
func (m *Module) link(e *hubv1.Event) string {
	if m.cfg.PublicURL == "" {
		return ""
	}
	u := strings.TrimSuffix(m.cfg.PublicURL, "/") + "/admin/events"
	if s := e.GetServerId(); s != "" {
		u += "?server=" + url.QueryEscape(s)
	}
	return u
}

// ask is a click on Restart or Park: a private question with Confirm, which only the clicker sees.
func (m *Module) ask(e *handler.ComponentEvent) error {
	action, server := e.Vars["action"], e.Vars["server"]
	if (action != Restart && action != Park) || !serverID.MatchString(server) {
		return e.CreateMessage(ephemeral("That button is not one this bot made."))
	}
	return e.CreateMessage(discord.MessageCreate{Content: Question(action, server), Flags: discord.MessageFlagEphemeral, AllowedMentions: &discord.AllowedMentions{},
		Components: []discord.LayoutComponent{discord.NewActionRow(
			discord.NewDangerButton("Confirm", "/events/do/"+action+"/"+server),
			discord.NewSecondaryButton("Cancel", "/events/cancel"),
		)}})
}

// do is the confirmation: the action goes to the hub on behalf of the clicker's Discord account,
// and the private message says what happened.
func (m *Module) do(e *handler.ComponentEvent) error {
	action, server := e.Vars["action"], e.Vars["server"]
	if (action != Restart && action != Park) || !serverID.MatchString(server) {
		return e.CreateMessage(ephemeral("That button is not one this bot made."))
	}
	if err := e.DeferUpdateMessage(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	err := m.hub.Act(ctx, action, server, &hubv1.Actor{Provider: "discord", Subject: e.User().ID.String()})
	m.actions.WithLabelValues(action, outcome(err)).Inc()
	if err != nil && !isAnswer(err) {
		m.logger.Error("events: the hub failed", "action", action, "server", server, "error", err.Error())
	}
	_, uerr := e.UpdateInteractionResponse(discord.MessageUpdate{Content: omit.Ptr(Answer(action, server, err)), Components: &[]discord.LayoutComponent{}, AllowedMentions: &discord.AllowedMentions{}})
	return uerr
}

func (m *Module) cancel(e *handler.ComponentEvent) error {
	return e.UpdateMessage(discord.MessageUpdate{Content: omit.Ptr("Cancelled; nothing was done."), Components: &[]discord.LayoutComponent{}})
}

// Question asks the clicker to confirm.
func Question(action, server string) string {
	if action == Park {
		return "Park **" + server + "**? Its machine stops and its volume is kept; anyone on it is dropped. Start it again from the dashboard."
	}
	return "Restart **" + server + "**'s machine? Its instance stops and starts again; anyone on it is dropped."
}

// Answer says what the hub did with a confirmed action.
func Answer(action, server string, err error) string {
	if err == nil {
		if action == Park {
			return "Parking **" + server + "**. The events here say when it's parked."
		}
		return "Restarting **" + server + "**'s machine. The events here say when it's back."
	}
	switch connect.CodeOf(err) {
	case connect.CodePermissionDenied, connect.CodeUnauthenticated:
		return "Nothing done: the hub only lets an operator of this server's game do that, and needs your Discord account linked to your member account (/link)."
	case connect.CodeNotFound:
		return "Nothing done: gravel doesn't run that server's machine."
	case connect.CodeFailedPrecondition, connect.CodeInvalidArgument:
		var ce *connect.Error
		if errors.As(err, &ce) && ce.Message() != "" {
			return "Nothing done: " + strings.TrimSuffix(ce.Message(), ".") + "."
		}
		return "Nothing done: the hub refused it as things stand."
	case connect.CodeUnavailable, connect.CodeDeadlineExceeded:
		return "Nothing done: the hub isn't answering. Try again in a minute."
	}
	return "Nothing done: something went wrong reaching the hub. Try again, or use the dashboard."
}

// outcome is a result's metric label.
func outcome(err error) string {
	switch {
	case err == nil:
		return "ok"
	case isAnswer(err):
		return "refused"
	}
	return "failed"
}

// isAnswer reports whether an error is the hub's answer about the action (not a failure to ask).
func isAnswer(err error) bool {
	switch connect.CodeOf(err) {
	case connect.CodePermissionDenied, connect.CodeUnauthenticated, connect.CodeNotFound, connect.CodeFailedPrecondition, connect.CodeInvalidArgument:
		return true
	}
	return false
}

func ephemeral(s string) discord.MessageCreate {
	return discord.MessageCreate{Content: s, Flags: discord.MessageFlagEphemeral, AllowedMentions: &discord.AllowedMentions{}}
}

// findCursor is the highest event id among the module's own posts in the channel's newest messages.
func (m *Module) findCursor(channel snowflake.ID) (int64, bool, error) {
	msgs, err := m.discord.GetMessages(channel, 0, 0, 0, searchDepth)
	if err != nil {
		return 0, false, err
	}
	var best int64
	found := false
	for _, msg := range msgs {
		if msg.Author.ID != m.self {
			continue
		}
		if g := marker.FindStringSubmatch(msg.Content); g != nil {
			if id, err := strconv.ParseInt(g[1], 10, 64); err == nil && id > best {
				best, found = id, true
			}
		}
	}
	return best, found, nil
}

// discordFailed counts and logs a Discord refusal or failure.
func (m *Module) discordFailed(ctx context.Context, channel snowflake.ID, err error) Result {
	var re *rest.Error
	if errors.As(err, &re) && (re.Code == rest.JSONErrorCodeMissingAccess || re.Code == rest.JSONErrorCodeLackPermissionsToPerformAction ||
		(re.Response != nil && re.Response.StatusCode == http.StatusForbidden)) {
		m.posts.WithLabelValues("forbidden").Inc()
		m.logger.WarnContext(ctx, "events: the bot may not read or post in the channel (View Channel, Send Messages, Read Message History)", "channel", channel.String())
		return Result{Forbidden: true}
	}
	m.posts.WithLabelValues("error").Inc()
	m.logger.WarnContext(ctx, "events: Discord failed, will retry", "channel", channel.String(), "error", err.Error())
	return Result{Failed: true}
}

// severityLabels lead a post with how bad it is, in words.
var severityLabels = map[string]string{"critical": "🔴 **Critical**", "warning": "🟠 **Warning**", "notice": "🔵 **Notice**", "info": "⚪ Info"}

// Line is one event as the channel shows it: how bad, what happened, where, who acted, a link to
// it in the dashboard, and the marker the module finds its place by.
func Line(e *hubv1.Event, server, link string) string {
	sev := severityLabels[e.GetSeverity()]
	if sev == "" {
		sev = escape(e.GetSeverity())
	}
	parts := []string{sev, escape(e.GetSummary())}
	if s := e.GetServerId(); s != "" && server != "" && !strings.Contains(e.GetSummary(), server) {
		parts = append(parts, escape(server))
	}
	if a := e.GetActorName(); a != "" {
		parts = append(parts, "by "+escape(a))
	}
	line := strings.Join(parts, " · ")
	if link != "" {
		line += " · [events](<" + link + ">)"
	}
	return line + "\n-# event " + strconv.FormatInt(e.GetId(), 10) + " · " + e.GetKind() + " · " + e.GetAt().AsTime().UTC().Format("Jan 2, 15:04 UTC")
}

// escape keeps summaries and names from formatting Discord markdown or pinging anyone.
func escape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, "*", `\*`, "_", `\_`, "~", `\~`, "`", "'", "|", `\|`, ">", `\>`, "@", "@\u200b", "\n", " ")
	return r.Replace(s)
}

// clientHub is Hub over the hub's Connect API as the bot's app.
type clientHub struct{ hc *hubclient.Client }

func (c clientHub) Feed(ctx context.Context) (*hubv1.DiscordEvents, error) {
	resp, err := c.hc.Organization().GetOrganizationSettings(ctx, connect.NewRequest(&hubv1.GetOrganizationSettingsRequest{}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetSettings().GetDiscord().GetEvents(), nil
}

func (c clientHub) Events(ctx context.Context, afterID int64, minSeverity string, kinds []string, limit int) ([]*hubv1.Event, int64, int64, error) {
	req := &hubv1.ListEventsRequest{AfterId: afterID, Limit: int32(limit), MinSeverity: minSeverity, Kinds: kinds} //nolint:gosec // at most pageSize
	resp, err := c.hc.Organization().ListEvents(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, 0, 0, err
	}
	return resp.Msg.GetEvents(), resp.Msg.GetNextAfterId(), resp.Msg.GetHeadId(), nil
}

func (c clientHub) Servers(ctx context.Context) (map[string]string, error) {
	resp, err := c.hc.Servers().ListServers(ctx, connect.NewRequest(&hubv1.ListServersRequest{}))
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, s := range resp.Msg.GetServers() {
		out[s.GetId()] = s.GetName()
	}
	return out, nil
}

func (c clientHub) HasHost(ctx context.Context, serverID string) (bool, error) {
	_, err := c.hc.Hosts().GetServerHost(ctx, connect.NewRequest(&hubv1.GetServerHostRequest{ServerId: serverID}))
	switch connect.CodeOf(err) {
	case connect.CodeNotFound:
		return false, nil
	case connect.CodePermissionDenied: // the app has no servers:read: no buttons, rather than no feed
		return false, nil
	}
	return err == nil, err
}

func (c clientHub) Act(ctx context.Context, action, serverID string, by *hubv1.Actor) error {
	const reason = "from the Discord events channel"
	var err error
	switch action {
	case Restart:
		_, err = c.hc.Hosts().RebootServerHost(ctx, connect.NewRequest(&hubv1.RebootServerHostRequest{ServerId: serverID, Reason: reason, OnBehalfOf: by}))
	case Park:
		_, err = c.hc.Hosts().SetServerHostDesired(ctx, connect.NewRequest(&hubv1.SetServerHostDesiredRequest{ServerId: serverID, Desired: "parked", Reason: reason, OnBehalfOf: by}))
	default:
		err = connect.NewError(connect.CodeInvalidArgument, errors.New("unknown action"))
	}
	return err
}
