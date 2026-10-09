// Package rolesync is the role-sync reconciler (ADR-0008 §4): it keeps the Discord roles the
// Organization settings map (the discord section) in step with the hub's members. A member whose
// identities include a provider besides Discord gets the linked role, a provider's role where one
// is mapped, and the first N members by registration a recognition role.
//
// A pass reads the mapping, every hub member (ListUsers) and every guild member, and adds or
// removes only the roles the mapping names: a role it does not name is never touched, and a
// recognition role, once earned, is never removed. Passes are idempotent, so the schedule is
// simple: one at start, one every interval, and one as soon as the hub's identity log shows a
// registration, a link or an unlink, or a member joins the guild. Discord's rate limits are the
// REST client's business (it waits and retries); a role the bot may not manage (above its own) is
// logged and skipped; a member who left between the list and the change is skipped.
package rolesync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"

	"connectrpc.com/connect"
	disgobot "github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/gravel-project/gravel/discord/bot"
	"github.com/gravel-project/gravel/discord/hubclient"
	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
)

// Config is how the reconciler runs; what it maps is in the Organization settings.
type Config struct {
	// Interval is the full pass's period.
	Interval time.Duration
	// PollInterval is how often the identity log is read.
	PollInterval time.Duration
	// DryRun logs the changes and makes none.
	DryRun bool
}

// Discord is the part of Discord's REST API role sync uses; the runtime's client is one.
type Discord interface {
	GetMembers(guildID snowflake.ID, limit int, after snowflake.ID, opts ...rest.RequestOpt) ([]discord.Member, error)
	AddMemberRole(guildID snowflake.ID, userID snowflake.ID, roleID snowflake.ID, opts ...rest.RequestOpt) error
	RemoveMemberRole(guildID snowflake.ID, userID snowflake.ID, roleID snowflake.ID, opts ...rest.RequestOpt) error
}

// Page sizes: Discord's maximum for the member list, the hub's for ListUsers and its events.
const (
	memberPage = 1000
	userPage   = 500
	eventPage  = 1000
)

// Module is the role-sync module.
type Module struct {
	hub *hubclient.Client
	cfg Config

	discord Discord
	logger  *slog.Logger
	trigger chan struct{}

	mu     sync.Mutex
	guild  snowflake.ID // the mapped guild, for the join listener
	cursor int64        // the identity log position; -1 until read
	idle   bool         // the last pass found no mapping (logged once)

	passes     *prometheus.CounterVec
	changes    *prometheus.CounterVec
	lastPassOK prometheus.Gauge
}

// New builds the module.
func New(hub *hubclient.Client, cfg Config) *Module {
	return &Module{hub: hub, cfg: cfg, trigger: make(chan struct{}, 1), cursor: -1, logger: slog.Default()}
}

// Name is the module's name.
func (m *Module) Name() string { return "rolesync" }

// Register adds the reconciler job and the member-join listener.
func (m *Module) Register(r *bot.Registry) error {
	m.discord = r.Rest()
	m.logger = r.Logger()
	m.passes = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "gravel_bot_rolesync_passes_total", Help: "Role-sync passes, by result (ok, idle, error)."}, []string{"result"})
	m.changes = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "gravel_bot_rolesync_changes_total", Help: "Role changes, by action (add, remove) and result (ok, dry_run, forbidden, gone)."}, []string{"action", "result"})
	m.lastPassOK = prometheus.NewGauge(prometheus.GaugeOpts{Name: "gravel_bot_rolesync_last_success_timestamp_seconds", Help: "When the last pass completed, as a Unix time."})
	for _, c := range []prometheus.Collector{m.passes, m.changes, m.lastPassOK} {
		if err := r.Metrics().Register(c); err != nil {
			return fmt.Errorf("rolesync: metrics: %w", err)
		}
	}
	r.Listen(disgobot.NewListenerFunc(func(e *events.GuildMemberJoin) {
		m.mu.Lock()
		guild := m.guild
		m.mu.Unlock()
		if e.GuildID == guild {
			m.Trigger()
		}
	}))
	r.Job("reconcile", m.Run)
	return nil
}

// Trigger asks for a pass soon; triggers while one is pending coalesce.
func (m *Module) Trigger() {
	select {
	case m.trigger <- struct{}{}:
	default:
	}
}

// Run reconciles until ctx is done. A failed pass is logged and counted and the next one tries
// again: the hub or Discord being away for a while is not a reason to stop the bot.
func (m *Module) Run(ctx context.Context) error {
	m.readHead(ctx)
	m.runPass(ctx)
	full := time.NewTicker(m.cfg.Interval)
	defer full.Stop()
	poll := time.NewTicker(m.cfg.PollInterval)
	defer poll.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-full.C:
			m.runPass(ctx)
		case <-m.trigger:
			m.runPass(ctx)
		case <-poll.C:
			if m.poll(ctx) {
				m.runPass(ctx)
			}
		}
	}
}

// readHead starts the identity log cursor at its newest event; the pass that follows covers
// everything before it.
func (m *Module) readHead(ctx context.Context) {
	resp, err := m.hub.Identity().ListIdentityEvents(ctx, connect.NewRequest(&hubv1.ListIdentityEventsRequest{AfterId: 0, Limit: 1}))
	if err != nil {
		m.logger.WarnContext(ctx, "role sync: identity log unavailable, will retry", "error", err.Error())
		return
	}
	m.mu.Lock()
	m.cursor = resp.Msg.GetHeadId()
	m.mu.Unlock()
}

// poll reads the identity log after the cursor and reports whether anything in it changes
// roles: a registration (recognition), a link or an unlink. Logins do not.
func (m *Module) poll(ctx context.Context) bool {
	m.mu.Lock()
	cursor := m.cursor
	m.mu.Unlock()
	if cursor < 0 {
		m.readHead(ctx)
		return true // the head was unknown, so a pass may have missed events
	}
	changed := false
	for {
		resp, err := m.hub.Identity().ListIdentityEvents(ctx, connect.NewRequest(&hubv1.ListIdentityEventsRequest{AfterId: cursor, Limit: eventPage}))
		if err != nil {
			if ctx.Err() == nil {
				m.logger.WarnContext(ctx, "role sync: identity log unavailable", "error", err.Error())
			}
			return changed
		}
		for _, e := range resp.Msg.GetEvents() {
			switch e.GetEvent() {
			case "registered", "linked", "unlinked":
				changed = true
			}
		}
		cursor = resp.Msg.GetNextAfterId()
		m.mu.Lock()
		m.cursor = cursor
		m.mu.Unlock()
		if len(resp.Msg.GetEvents()) < eventPage {
			return changed
		}
	}
}

func (m *Module) runPass(ctx context.Context) {
	start := time.Now()
	res, err := m.Pass(ctx)
	switch {
	case err != nil && ctx.Err() != nil:
		return // shutting down
	case err != nil:
		m.passes.WithLabelValues("error").Inc()
		m.logger.ErrorContext(ctx, "role sync pass failed", "error", err.Error())
		return
	case res.Idle:
		m.passes.WithLabelValues("idle").Inc()
	default:
		m.passes.WithLabelValues("ok").Inc()
	}
	m.lastPassOK.SetToCurrentTime()
	if res.Idle {
		return
	}
	level := slog.LevelDebug
	if res.Added+res.Removed+res.Forbidden+res.Gone > 0 {
		level = slog.LevelInfo
	}
	m.logger.Log(ctx, level, "role sync pass", "users", res.Users, "members", res.Members, "added", res.Added, "removed", res.Removed,
		"forbidden", res.Forbidden, "gone", res.Gone, "dry_run", m.cfg.DryRun, "duration", time.Since(start).Round(time.Millisecond).String())
}

// Result is what a pass did. In a dry run, Added and Removed count what it would have done.
type Result struct {
	Idle      bool // no mapping: nothing to do
	Users     int  // hub members read
	Members   int  // guild members read
	Added     int
	Removed   int
	Forbidden int // changes Discord refused: a role above the bot's
	Gone      int // members who left before their change
}

// Pass reconciles once.
func (m *Module) Pass(ctx context.Context) (Result, error) {
	set, err := m.hub.Organization().GetOrganizationSettings(ctx, connect.NewRequest(&hubv1.GetOrganizationSettingsRequest{}))
	if err != nil {
		return Result{}, fmt.Errorf("read the mapping: %w", err)
	}
	mapping, err := MappingFrom(set.Msg.GetSettings().GetDiscord())
	if err != nil {
		return Result{}, err
	}
	m.mu.Lock()
	m.guild = mapping.Guild
	wasIdle := m.idle
	m.idle = mapping.Idle()
	m.mu.Unlock()
	if mapping.Idle() {
		if !wasIdle {
			m.logger.InfoContext(ctx, "role sync idle: the organization settings map no Discord roles")
		}
		return Result{Idle: true}, nil
	}

	users, err := m.listUsers(ctx)
	if err != nil {
		return Result{}, err
	}
	members, err := m.listMembers(ctx, mapping.Guild)
	if err != nil {
		return Result{}, err
	}
	res := Result{Users: len(users), Members: len(members)}
	forbidden := map[snowflake.ID]int{}
	for _, c := range Plan(mapping, Desired(mapping, users), members) {
		action, done := "remove", "removed"
		if c.Add {
			action, done = "add", "added"
		}
		if m.cfg.DryRun {
			m.changes.WithLabelValues(action, "dry_run").Inc()
			m.logger.InfoContext(ctx, "role sync would "+action+" a role (dry run)", "user", c.User.String(), "role", c.Role.String())
			res.count(c.Add)
			continue
		}
		opt := rest.WithCtx(ctx)
		if c.Add {
			err = m.discord.AddMemberRole(mapping.Guild, c.User, c.Role, opt)
		} else {
			err = m.discord.RemoveMemberRole(mapping.Guild, c.User, c.Role, opt)
		}
		switch classify(err) {
		case "ok":
			m.changes.WithLabelValues(action, "ok").Inc()
			m.logger.InfoContext(ctx, "role sync: role "+done, "user", c.User.String(), "role", c.Role.String())
			res.count(c.Add)
		case "forbidden":
			m.changes.WithLabelValues(action, "forbidden").Inc()
			forbidden[c.Role]++
			res.Forbidden++
		case "gone":
			m.changes.WithLabelValues(action, "gone").Inc()
			res.Gone++
		default:
			return res, fmt.Errorf("%s role %s for %s: %w", action, c.Role, c.User, err)
		}
	}
	for _, role := range slices.Sorted(maps.Keys(forbidden)) {
		m.logger.WarnContext(ctx, "role sync may not manage a role: move the bot's role above it", "role", role.String(), "changes", forbidden[role])
	}
	return res, nil
}

func (r *Result) count(add bool) {
	if add {
		r.Added++
	} else {
		r.Removed++
	}
}

// classify sorts a REST error into what a pass does with it.
func classify(err error) string {
	if err == nil {
		return "ok"
	}
	var re *rest.Error
	if !errors.As(err, &re) {
		return "error"
	}
	switch {
	case re.Code == rest.JSONErrorCodeUnknownMember:
		return "gone"
	case re.Code == rest.JSONErrorCodeLackPermissionsToPerformAction, re.Code == rest.JSONErrorCodeMissingAccess,
		re.Response != nil && re.Response.StatusCode == http.StatusForbidden:
		return "forbidden"
	}
	return "error"
}

func (m *Module) listUsers(ctx context.Context) ([]*hubv1.User, error) {
	var users []*hubv1.User
	token := ""
	for {
		resp, err := m.hub.Identity().ListUsers(ctx, connect.NewRequest(&hubv1.ListUsersRequest{PageSize: userPage, PageToken: token}))
		if err != nil {
			return nil, fmt.Errorf("list the hub's members: %w", err)
		}
		users = append(users, resp.Msg.GetUsers()...)
		if token = resp.Msg.GetNextPageToken(); token == "" {
			return users, nil
		}
	}
}

func (m *Module) listMembers(ctx context.Context, guild snowflake.ID) ([]discord.Member, error) {
	var members []discord.Member
	var after snowflake.ID
	for {
		page, err := m.discord.GetMembers(guild, memberPage, after, rest.WithCtx(ctx))
		if err != nil {
			if classify(err) == "forbidden" {
				return nil, fmt.Errorf("list guild %s's members: %w (the bot must be in the guild, and the application needs the Server Members intent: Developer Portal, Bot, Privileged Gateway Intents)", guild, err)
			}
			return nil, fmt.Errorf("list guild %s's members: %w", guild, err)
		}
		members = append(members, page...)
		if len(page) < memberPage {
			return members, nil
		}
		after = page[len(page)-1].User.ID
	}
}

// Mapping is the settings' discord section as ids.
type Mapping struct {
	Guild       snowflake.ID
	Linked      snowflake.ID // 0 for none
	Providers   map[string]snowflake.ID
	Recognition []Recognition
}

// Recognition is a role for the first Count members by registration.
type Recognition struct {
	Role  snowflake.ID
	Count int
}

// MappingFrom reads the settings' discord section. The hub validated it; an id that does not
// parse here is still refused rather than guessed at.
func MappingFrom(d *hubv1.DiscordSettings) (Mapping, error) {
	parse := func(field, s string) (snowflake.ID, error) {
		if s == "" {
			return 0, nil
		}
		id, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("mapping: %s: %q is not a Discord id", field, s)
		}
		return snowflake.ID(id), nil
	}
	var m Mapping
	var err error
	if m.Guild, err = parse("guild_id", d.GetGuildId()); err != nil {
		return Mapping{}, err
	}
	if m.Linked, err = parse("roles.linked", d.GetRoles().GetLinked()); err != nil {
		return Mapping{}, err
	}
	for p, s := range d.GetRoles().GetProviders() {
		id, err := parse("roles.providers."+p, s)
		if err != nil {
			return Mapping{}, err
		}
		if m.Providers == nil {
			m.Providers = map[string]snowflake.ID{}
		}
		m.Providers[p] = id
	}
	for i, r := range d.GetRecognition() {
		if r.GetRule() != "first_members" {
			return Mapping{}, fmt.Errorf("mapping: recognition[%d]: rule %q is not one this bot knows", i, r.GetRule())
		}
		id, err := parse(fmt.Sprintf("recognition[%d].role", i), r.GetRole())
		if err != nil {
			return Mapping{}, err
		}
		m.Recognition = append(m.Recognition, Recognition{Role: id, Count: int(r.GetCount())})
	}
	return m, nil
}

// Idle reports whether there is nothing to sync: no guild, or no role.
func (m Mapping) Idle() bool {
	return m.Guild == 0 || (m.Linked == 0 && len(m.Providers) == 0 && len(m.Recognition) == 0)
}

// managed is every mapped role, true when role sync may remove it (false for recognition).
func (m Mapping) managed() map[snowflake.ID]bool {
	out := map[snowflake.ID]bool{}
	if m.Linked != 0 {
		out[m.Linked] = true
	}
	for _, id := range m.Providers {
		out[id] = true
	}
	for _, r := range m.Recognition {
		out[r.Role] = false
	}
	return out
}

// Desired is the mapped roles each hub member with a Discord identity should have, by Discord
// user id. users are in registration order, oldest first, as ListUsers returns them; the first
// Count of them earn a recognition role whether or not they are in the guild.
func Desired(m Mapping, users []*hubv1.User) map[snowflake.ID]map[snowflake.ID]bool {
	out := map[snowflake.ID]map[snowflake.ID]bool{}
	for i, u := range users {
		var discordID snowflake.ID
		var others []string
		for _, id := range u.GetIdentities() {
			if id.GetProvider() == "discord" {
				if v, err := strconv.ParseUint(id.GetSubject(), 10, 64); err == nil {
					discordID = snowflake.ID(v)
				}
			} else {
				others = append(others, id.GetProvider())
			}
		}
		if discordID == 0 {
			continue
		}
		roles := map[snowflake.ID]bool{}
		if len(others) > 0 && m.Linked != 0 {
			roles[m.Linked] = true
		}
		for _, id := range u.GetIdentities() {
			if role, ok := m.Providers[id.GetProvider()]; ok {
				roles[role] = true
			}
		}
		for _, r := range m.Recognition {
			if i < r.Count {
				roles[r.Role] = true
			}
		}
		out[discordID] = roles
	}
	return out
}

// Change is one role to add to or remove from one member.
type Change struct {
	User snowflake.ID
	Role snowflake.ID
	Add  bool
}

// Plan is the changes that bring the guild's members to the desired roles, in member order then
// role order. Bots are left alone; a member the hub does not know desires no mapped role, so
// they lose the linked and provider roles and keep a recognition role.
func Plan(m Mapping, desired map[snowflake.ID]map[snowflake.ID]bool, members []discord.Member) []Change {
	managed := m.managed()
	roles := slices.Sorted(maps.Keys(managed))
	var out []Change
	for _, mem := range members {
		if mem.User.Bot {
			continue
		}
		want := desired[mem.User.ID]
		for _, role := range roles {
			has := slices.Contains(mem.RoleIDs, role)
			switch {
			case want[role] && !has:
				out = append(out, Change{User: mem.User.ID, Role: role, Add: true})
			case !want[role] && has && managed[role]:
				out = append(out, Change{User: mem.User.ID, Role: role})
			}
		}
	}
	return out
}
