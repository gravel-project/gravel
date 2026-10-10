// Package modlog posts every moderation action to the channel the Organization settings name
// (discord.mod_log): the hub's audit log, whether the action came from the web or from Discord, so
// staff see one record in the place they already read. The audit log stays the record; this is a
// copy of it in Discord.
//
// A pass reads the settings, then the audit log newest first back to the last entry posted, and
// posts each finished entry in order, one message each. An entry still running (no outcome yet)
// waits for the next pass, unless it is older than StaleAfter, when it is posted as it stands. The
// bot keeps no state in the hub (ADR-0008): at start, or when the channel changes, it finds the
// last entry it posted from the marker in its own messages ("-# audit <id>") among the channel's
// newest; with none there it starts from the newest entry, so turning it on never floods the
// channel with history. Failures are logged once while they last, counted, and retried next pass.
package modlog

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
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

// StaleAfter is how long an entry without an outcome waits before it is posted as it stands (the
// hub stopped mid-call).
const StaleAfter = 2 * time.Minute

// searchDepth is how many of the channel's newest messages are read for the marker.
const searchDepth = 100

// maxPages bounds how far back one pass reads the audit log (pages of pageSize).
const (
	maxPages = 5
	pageSize = 100
)

// marker finds the audit id in a message the module posted.
var marker = regexp.MustCompile(`(?m)^-# audit ([0-9]+)\b`)

// noMentions keeps a post from pinging anyone, whatever a name or a reason says.
var noMentions = &discord.AllowedMentions{Parse: []discord.AllowedMentionType{}, Roles: []snowflake.ID{}, Users: []snowflake.ID{}}

// Config is how the module runs; where it posts is in the Organization settings.
type Config struct {
	Interval time.Duration
}

// Discord is the part of Discord's REST API the module uses; the runtime's client is one.
type Discord interface {
	GetMessages(channelID snowflake.ID, around snowflake.ID, before snowflake.ID, after snowflake.ID, limit int, opts ...rest.RequestOpt) ([]discord.Message, error)
	CreateMessage(channelID snowflake.ID, messageCreate discord.MessageCreate, opts ...rest.RequestOpt) (*discord.Message, error)
}

// Module is the mod-log module.
type Module struct {
	hub *hubclient.Client
	cfg Config

	discord Discord
	self    snowflake.ID
	logger  *slog.Logger
	now     func() time.Time

	mu      sync.Mutex
	channel snowflake.ID // the channel the cursor belongs to
	cursor  int64        // the last audit id posted (or skipped at start)
	idle    bool
	hubDown bool

	posts      *prometheus.CounterVec
	lastPassOK prometheus.Gauge
}

// New builds the module; the bot's app needs servers:moderate to read the audit log.
func New(hub *hubclient.Client, cfg Config) *Module {
	return &Module{hub: hub, cfg: cfg, logger: slog.Default(), now: time.Now}
}

// Name is the module's name.
func (*Module) Name() string { return "modlog" }

// Register adds the job.
func (m *Module) Register(r *bot.Registry) error {
	m.discord = r.Rest()
	m.self = r.ApplicationID()
	m.logger = r.Logger()
	m.posts = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "gravel_bot_modlog_posts_total", Help: "Audit-log entries posted to the mod log, by result (posted, forbidden, error)."}, []string{"result"})
	m.lastPassOK = prometheus.NewGauge(prometheus.GaugeOpts{Name: "gravel_bot_modlog_last_success_timestamp_seconds", Help: "When the last pass that posted everything finished completed, as a Unix time."})
	for _, c := range []prometheus.Collector{m.posts, m.lastPassOK} {
		if err := r.Metrics().Register(c); err != nil {
			return fmt.Errorf("modlog: metrics: %w", err)
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
	_, err := m.Pass(ctx)
	m.mu.Lock()
	was := m.hubDown
	m.hubDown = err != nil && ctx.Err() == nil
	now := m.hubDown
	m.mu.Unlock()
	switch {
	case now && !was:
		m.logger.WarnContext(ctx, "mod log: the hub is unavailable, will retry", "error", err.Error())
	case !now && was:
		m.logger.InfoContext(ctx, "mod log: the hub is back")
	}
}

// Result is what a pass did.
type Result struct {
	Idle      bool // no channel set
	Posted    int
	Waiting   int // entries still running, left for the next pass
	Forbidden bool
	Failed    bool
}

// Pass posts every finished entry since the last one posted. An error means the hub could not be
// read; a Discord failure is in the result, and the entry is tried again next pass.
func (m *Module) Pass(ctx context.Context) (Result, error) {
	set, err := m.hub.Organization().GetOrganizationSettings(ctx, connect.NewRequest(&hubv1.GetOrganizationSettingsRequest{}))
	if err != nil {
		return Result{}, fmt.Errorf("read the settings: %w", err)
	}
	raw := set.Msg.GetSettings().GetDiscord().GetModLog()
	m.mu.Lock()
	wasIdle := m.idle
	m.idle = raw == ""
	m.mu.Unlock()
	if raw == "" {
		if !wasIdle {
			m.logger.InfoContext(ctx, "mod log idle: the organization settings name no channel (discord.mod_log)")
		}
		m.lastPassOK.SetToCurrentTime()
		return Result{Idle: true}, nil
	}
	channel, err := snowflake.Parse(raw)
	if err != nil {
		return Result{}, fmt.Errorf("discord.mod_log %q: %w", raw, err)
	}

	m.mu.Lock()
	cursor, known := m.cursor, m.channel == channel
	m.mu.Unlock()
	if !known {
		found, ok, err := m.findCursor(channel)
		if err != nil {
			return m.discordFailed(ctx, channel, err), nil
		}
		if !ok { // nothing posted here yet: start from now
			newest, err := m.entries(ctx, 0, 1)
			if err != nil {
				return Result{}, err
			}
			found = 0
			if len(newest) > 0 {
				found = newest[len(newest)-1].GetId()
			}
		}
		cursor = found
		m.mu.Lock()
		m.channel, m.cursor = channel, cursor
		m.mu.Unlock()
	}

	todo, err := m.entries(ctx, cursor, maxPages)
	if err != nil {
		return Result{}, err
	}
	var res Result
	if len(todo) == 0 {
		m.lastPassOK.SetToCurrentTime()
		return res, nil
	}
	names, err := m.serverNames(ctx)
	if err != nil {
		return Result{}, err
	}
	now := m.now()
	for i, e := range todo {
		if e.GetOutcome() == "" && now.Sub(e.GetAt().AsTime()) < StaleAfter {
			res.Waiting = len(todo) - i
			break
		}
		if _, err := m.discord.CreateMessage(channel, discord.MessageCreate{Content: Line(e, names[e.GetServerId()]), AllowedMentions: noMentions}); err != nil {
			fail := m.discordFailed(ctx, channel, err)
			fail.Posted = res.Posted
			return fail, nil
		}
		res.Posted++
		m.posts.WithLabelValues("posted").Inc()
		m.mu.Lock()
		m.cursor = e.GetId()
		m.mu.Unlock()
	}
	if res.Waiting == 0 {
		m.lastPassOK.SetToCurrentTime()
	}
	return res, nil
}

// entries are the audit log's entries after cursor, oldest first, reading at most pages pages
// newest first (pages 1 with cursor 0 is the newest entry alone).
func (m *Module) entries(ctx context.Context, cursor int64, pages int) ([]*hubv1.AuditEntry, error) {
	var out []*hubv1.AuditEntry
	token := ""
	size := int32(pageSize)
	if cursor == 0 && pages == 1 {
		size = 1
	}
	for range pages {
		resp, err := m.hub.Moderation().ListAuditLog(ctx, connect.NewRequest(&hubv1.ListAuditLogRequest{PageSize: size, PageToken: token}))
		if err != nil {
			return nil, fmt.Errorf("read the audit log: %w", err)
		}
		done := false
		for _, e := range resp.Msg.GetEntries() {
			if cursor > 0 && e.GetId() <= cursor {
				done = true
				break
			}
			out = append(out, e)
		}
		token = resp.Msg.GetNextPageToken()
		if done || token == "" || (cursor == 0 && pages == 1) {
			break
		}
	}
	slices.Reverse(out)
	return out, nil
}

// findCursor is the highest audit id among the module's own posts in the channel's newest messages.
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

func (m *Module) serverNames(ctx context.Context) (map[string]string, error) {
	resp, err := m.hub.Servers().ListServers(ctx, connect.NewRequest(&hubv1.ListServersRequest{}))
	if err != nil {
		return nil, fmt.Errorf("list the servers: %w", err)
	}
	out := map[string]string{}
	for _, s := range resp.Msg.GetServers() {
		out[s.GetId()] = s.GetName()
	}
	return out, nil
}

// discordFailed counts and logs a Discord refusal or failure.
func (m *Module) discordFailed(ctx context.Context, channel snowflake.ID, err error) Result {
	var re *rest.Error
	if errors.As(err, &re) && (re.Code == rest.JSONErrorCodeMissingAccess || re.Code == rest.JSONErrorCodeLackPermissionsToPerformAction ||
		(re.Response != nil && re.Response.StatusCode == http.StatusForbidden)) {
		m.posts.WithLabelValues("forbidden").Inc()
		m.logger.WarnContext(ctx, "mod log: the bot may not read or post in the channel (View Channel, Send Messages, Read Message History)", "channel", channel.String())
		return Result{Forbidden: true}
	}
	m.posts.WithLabelValues("error").Inc()
	m.logger.WarnContext(ctx, "mod log: Discord failed, will retry", "channel", channel.String(), "error", err.Error())
	return Result{Failed: true}
}

// actionLabels name the audit log's actions.
var actionLabels = map[string]string{"kick": "Kick", "ban": "Ban", "unban": "Unban", "message": "Message", "broadcast": "Broadcast", "move_player": "Move"}

// Line is one entry as the mod log shows it: what, where, to whom, by whom, why, the result, and
// the marker the module finds its place by.
func Line(e *hubv1.AuditEntry, server string) string {
	if server == "" {
		server = e.GetServerId()
	}
	action := actionLabels[e.GetAction()]
	if action == "" {
		action = strings.ReplaceAll(e.GetAction(), "_", " ")
	}
	parts := []string{"**" + action + "**", escape(server)}
	if e.GetTargetSubject() != "" {
		parts = append(parts, "`"+strings.ReplaceAll(e.GetTargetSubject(), "`", "")+"`")
	}
	parts = append(parts, "by "+actor(e))
	switch {
	case e.GetReason() != "":
		parts = append(parts, escape(e.GetReason()))
	case e.GetMessage() != "":
		parts = append(parts, "“"+escape(e.GetMessage())+"”")
	case e.GetTeam() != "":
		parts = append(parts, "to "+escape(e.GetTeam()))
	}
	outcome := strings.ReplaceAll(e.GetOutcome(), "_", " ")
	if outcome == "" {
		outcome = "no result recorded"
	}
	parts = append(parts, outcome)
	return strings.Join(parts, " · ") + "\n-# audit " + strconv.FormatInt(e.GetId(), 10) + " · " + e.GetAt().AsTime().UTC().Format("Jan 2, 15:04 UTC")
}

// actor is who acted: a member by name, or an app and the account it acted for (a Discord account
// as a mention, which pings no one under noMentions).
func actor(e *hubv1.AuditEntry) string {
	if n := e.GetUserName(); n != "" {
		return "**" + escape(n) + "**"
	}
	if e.GetUserId() != "" {
		return "a member"
	}
	who := escape(e.GetAppName())
	if who == "" {
		who = "an app"
	}
	if ob := e.GetOnBehalfOf(); ob != nil {
		if ob.GetProvider() == "discord" {
			if _, err := snowflake.Parse(ob.GetSubject()); err == nil {
				return "<@" + ob.GetSubject() + "> (via " + who + ")"
			}
		}
		return escape(ob.GetProvider()+" "+ob.GetSubject()) + " (via " + who + ")"
	}
	return who
}

// escape keeps names and reasons from formatting Discord markdown or pinging anyone.
func escape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, "*", `\*`, "_", `\_`, "~", `\~`, "`", "'", "|", `\|`, ">", `\>`, "@", "@\u200b", "\n", " ")
	return r.Replace(s)
}
