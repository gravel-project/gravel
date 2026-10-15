// Package api implements the hub's Connect services over the domain packages. It maps domain
// errors to Connect codes and never leaks internal error text to clients.
package api

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"

	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
	"github.com/gravel-project/gravel/gen/gravel/hub/v1/hubv1connect"
	"github.com/gravel-project/gravel/internal/authz"
	"github.com/gravel-project/gravel/internal/httpx"
	"github.com/gravel-project/gravel/internal/notify"
	"github.com/gravel-project/gravel/internal/org"
	"github.com/gravel-project/gravel/internal/store"
)

// OrganizationServer serves gravel.hub.v1.OrganizationService.
type OrganizationServer struct {
	hubv1connect.UnimplementedOrganizationServiceHandler
	org    *org.Service
	access *Access
	events EventReader // nil: no events log (WithEvents)
	// notifiers tests a notifier (TestNotifier); nil: the hub sends no notifications.
	notifiers NotifierTester
	logger    *slog.Logger
}

// NotifierTester sends a notifier the test event (notify.Dispatcher).
type NotifierTester interface {
	Test(ctx context.Context, name, hub string) error
}

// WithNotifiers lets TestNotifier reach the notifiers (ADR-0028 §5).
func (s *OrganizationServer) WithNotifiers(t NotifierTester) *OrganizationServer {
	s.notifiers = t
	return s
}

// NewOrganizationServer wires the service.
func NewOrganizationServer(svc *org.Service, access *Access, logger *slog.Logger) *OrganizationServer {
	return &OrganizationServer{org: svc, access: access, logger: logger}
}

// GetOrganization returns the built-in organization.
func (s *OrganizationServer) GetOrganization(ctx context.Context, _ *connect.Request[hubv1.GetOrganizationRequest]) (*connect.Response[hubv1.GetOrganizationResponse], error) {
	o, err := s.org.Get(ctx)
	if err != nil {
		return nil, mapError(ctx, s.logger, err)
	}
	return connect.NewResponse(&hubv1.GetOrganizationResponse{Organization: s.withPending(ctx, o)}), nil
}

// withPending is the organization for the caller: the nomination (#197) only for the owner and
// the nominee, since who may take the hub over is theirs to know.
func (s *OrganizationServer) withPending(ctx context.Context, o store.Organization) *hubv1.Organization {
	p := toProto(o, s.org.Demo())
	if o.Pending == nil {
		return p
	}
	who, err := s.access.Principal(ctx)
	if err != nil || who.UserID == uuid.Nil || (who.UserID != o.Pending.UserID && (o.OwnerUserID == nil || who.UserID != *o.OwnerUserID)) {
		return p
	}
	p.PendingOwnerUserId, p.PendingOwnerName = o.Pending.UserID.String(), o.Pending.Name
	p.PendingOwnerExpiresAt = timestamppb.New(o.Pending.ExpiresAt)
	return p
}

// NominateOwner names the member who may take the hub over: org:transfer, the owner in person.
func (s *OrganizationServer) NominateOwner(ctx context.Context, req *connect.Request[hubv1.NominateOwnerRequest]) (*connect.Response[hubv1.NominateOwnerResponse], error) {
	if _, err := s.access.Require(ctx, authz.OrgTransfer, authz.Target{}); err != nil {
		return nil, err
	}
	uid, err := sessionCaller(ctx)
	if err != nil {
		return nil, err
	}
	nominee, err := uuid.Parse(strings.TrimSpace(req.Msg.GetUserId()))
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("user_id must be a member's id"))
	}
	o, err := s.org.NominateOwner(ctx, nominee, s.inPerson(ctx, uid))
	if err != nil {
		return nil, mapError(ctx, s.logger, err)
	}
	return connect.NewResponse(&hubv1.NominateOwnerResponse{Organization: s.withPending(ctx, o)}), nil
}

// AcceptOwnership makes the nominee, in person, the owner.
func (s *OrganizationServer) AcceptOwnership(ctx context.Context, _ *connect.Request[hubv1.AcceptOwnershipRequest]) (*connect.Response[hubv1.AcceptOwnershipResponse], error) {
	uid, err := sessionCaller(ctx)
	if err != nil {
		return nil, err
	}
	o, err := s.org.AcceptOwnership(ctx, s.inPerson(ctx, uid))
	if err != nil {
		return nil, mapError(ctx, s.logger, err)
	}
	return connect.NewResponse(&hubv1.AcceptOwnershipResponse{Organization: s.withPending(ctx, o)}), nil
}

// CancelOwnershipTransfer ends the nomination, by the owner or the nominee in person.
func (s *OrganizationServer) CancelOwnershipTransfer(ctx context.Context, _ *connect.Request[hubv1.CancelOwnershipTransferRequest]) (*connect.Response[hubv1.CancelOwnershipTransferResponse], error) {
	uid, err := sessionCaller(ctx)
	if err != nil {
		return nil, err
	}
	o, err := s.org.CancelOwnershipTransfer(ctx, s.inPerson(ctx, uid))
	if err != nil {
		return nil, mapError(ctx, s.logger, err)
	}
	return connect.NewResponse(&hubv1.CancelOwnershipTransferResponse{Organization: s.withPending(ctx, o)}), nil
}

// inPerson is a member acting through their session, as the events log records them.
func (s *OrganizationServer) inPerson(ctx context.Context, uid uuid.UUID) store.Actor {
	return store.Actor{UserID: &uid, Via: store.ViaAPI, RequestID: httpx.RequestIDFromContext(ctx)}
}

// ClaimOwnership consumes the one-time owner-claim token and makes the caller the owner.
func (s *OrganizationServer) ClaimOwnership(ctx context.Context, req *connect.Request[hubv1.ClaimOwnershipRequest]) (*connect.Response[hubv1.ClaimOwnershipResponse], error) {
	uid, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.Msg.GetToken()) == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("token is required"))
	}
	o, err := s.org.Claim(ctx, req.Msg.GetToken(), store.Actor{UserID: &uid, Via: store.ViaAPI, RequestID: httpx.RequestIDFromContext(ctx)})
	if err != nil {
		return nil, mapError(ctx, s.logger, err)
	}
	return connect.NewResponse(&hubv1.ClaimOwnershipResponse{Organization: toProto(o, s.org.Demo())}), nil
}

// GetOrganizationSettings returns the settings; public, the pages render the theme before login.
// The notifiers say where the hub's events go (an ntfy topic, email addresses), so only a caller
// who may change the settings reads them.
func (s *OrganizationServer) GetOrganizationSettings(ctx context.Context, _ *connect.Request[hubv1.GetOrganizationSettingsRequest]) (*connect.Response[hubv1.GetOrganizationSettingsResponse], error) {
	set, at, err := s.org.Settings(ctx)
	if err != nil {
		return nil, mapError(ctx, s.logger, err)
	}
	if p, err := s.access.Principal(ctx); err != nil || !p.Has(authz.SettingsWrite, authz.Target{}) {
		set.Notifiers = nil
	}
	return connect.NewResponse(&hubv1.GetOrganizationSettingsResponse{Settings: settingsToProto(set, at)}), nil
}

// UpdateOrganizationSettings replaces the settings; the owner only.
func (s *OrganizationServer) UpdateOrganizationSettings(ctx context.Context, req *connect.Request[hubv1.UpdateOrganizationSettingsRequest]) (*connect.Response[hubv1.UpdateOrganizationSettingsResponse], error) {
	p, err := s.access.Require(ctx, authz.SettingsWrite, authz.Target{})
	if err != nil {
		return nil, err
	}
	set, err := s.org.UpdateSettingsIf(ctx, settingsFromProto(req.Msg.GetSettings()), changedBy(ctx, p), ifUpdatedAt(req.Msg.GetIfUpdatedAt()))
	if err != nil {
		return nil, mapError(ctx, s.logger, err)
	}
	_, at, err := s.org.Settings(ctx)
	if err != nil {
		return nil, mapError(ctx, s.logger, err)
	}
	return connect.NewResponse(&hubv1.UpdateOrganizationSettingsResponse{Settings: settingsToProto(set, at)}), nil
}

// TestNotifier sends a notifier the test event: settings:write.
func (s *OrganizationServer) TestNotifier(ctx context.Context, req *connect.Request[hubv1.TestNotifierRequest]) (*connect.Response[hubv1.TestNotifierResponse], error) {
	if _, err := s.access.Require(ctx, authz.SettingsWrite, authz.Target{}); err != nil {
		return nil, err
	}
	if s.notifiers == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, errors.New("this hub sends no notifications"))
	}
	o, err := s.org.Get(ctx)
	if err != nil {
		return nil, mapError(ctx, s.logger, err)
	}
	err = s.notifiers.Test(ctx, strings.TrimSpace(req.Msg.GetName()), o.Name)
	switch {
	case errors.Is(err, notify.ErrNoNotifier):
		return nil, connect.NewError(connect.CodeNotFound, errors.New("the settings have no notifier by that name"))
	case err != nil:
		// A notifier's errors never carry its secret (notify.post), so the owner may see them.
		return connect.NewResponse(&hubv1.TestNotifierResponse{Error: err.Error()}), nil
	}
	return connect.NewResponse(&hubv1.TestNotifierResponse{Delivered: true}), nil
}

// ExportSettings is the stored settings as a manifest (gravel#201): settings:write.
func (s *OrganizationServer) ExportSettings(ctx context.Context, _ *connect.Request[hubv1.ExportSettingsRequest]) (*connect.Response[hubv1.ExportSettingsResponse], error) {
	if _, err := s.access.Require(ctx, authz.SettingsWrite, authz.Target{}); err != nil {
		return nil, err
	}
	set, at, err := s.org.Settings(ctx)
	if err != nil {
		return nil, mapError(ctx, s.logger, err)
	}
	b, err := set.Manifest()
	if err != nil {
		return nil, mapError(ctx, s.logger, err)
	}
	out := &hubv1.ExportSettingsResponse{Manifest: string(b)}
	if !at.IsZero() {
		out.UpdatedAt = timestamppb.New(at)
	}
	return connect.NewResponse(out), nil
}

// ifUpdatedAt is a request's if_updated_at: nil when the caller sent none.
func ifUpdatedAt(t *timestamppb.Timestamp) *time.Time {
	if t == nil {
		return nil
	}
	at := t.AsTime()
	return &at
}

// parseSettings reads and checks a manifest from a request: invalid_argument names every problem.
func (s *OrganizationServer) parseSettings(ctx context.Context, text string) (org.Settings, error) {
	set, err := org.ParseManifest([]byte(text))
	if err != nil {
		return org.Settings{}, connect.NewError(connect.CodeInvalidArgument, err)
	}
	if set, err = s.org.CheckSettings(set); err != nil {
		return org.Settings{}, mapError(ctx, s.logger, err)
	}
	return set, nil
}

// CheckSettings validates a manifest against this hub and changes nothing: settings:write.
func (s *OrganizationServer) CheckSettings(ctx context.Context, req *connect.Request[hubv1.CheckSettingsRequest]) (*connect.Response[hubv1.CheckSettingsResponse], error) {
	if _, err := s.access.Require(ctx, authz.SettingsWrite, authz.Target{}); err != nil {
		return nil, err
	}
	set, err := s.parseSettings(ctx, req.Msg.GetManifest())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&hubv1.CheckSettingsResponse{Summary: set.Summary()}), nil
}

// ApplySettings stores a manifest, or with dry_run names the sections it would change:
// settings:write. A manifest that matches the stored settings writes nothing, so applying the
// same one again records no settings.updated.
func (s *OrganizationServer) ApplySettings(ctx context.Context, req *connect.Request[hubv1.ApplySettingsRequest]) (*connect.Response[hubv1.ApplySettingsResponse], error) {
	p, err := s.access.Require(ctx, authz.SettingsWrite, authz.Target{})
	if err != nil {
		return nil, err
	}
	set, err := s.parseSettings(ctx, req.Msg.GetManifest())
	if err != nil {
		return nil, err
	}
	sections, err := s.org.PlanSettings(ctx, set)
	if err != nil {
		return nil, mapError(ctx, s.logger, err)
	}
	out := &hubv1.ApplySettingsResponse{Changes: sections, Summary: set.Summary()}
	if req.Msg.GetDryRun() || len(sections) == 0 {
		return connect.NewResponse(out), nil
	}
	if _, err := s.org.UpdateSettingsIf(ctx, set, changedBy(ctx, p), ifUpdatedAt(req.Msg.GetIfUpdatedAt())); err != nil {
		return nil, mapError(ctx, s.logger, err)
	}
	out.Applied = true
	return connect.NewResponse(out), nil
}

func settingsToProto(set org.Settings, at time.Time) *hubv1.OrganizationSettings {
	tokens := func(t org.Tokens) *hubv1.ThemeTokens {
		return &hubv1.ThemeTokens{Accent: t.Accent, Background: t.Background, Foreground: t.Foreground, Muted: t.Muted, Line: t.Line, Ok: t.OK, Err: t.Err}
	}
	p := &hubv1.OrganizationSettings{
		Version: int32(set.Version), //nolint:gosec // a small constant
		Theme:   &hubv1.Theme{Light: tokens(set.Theme.Light), Dark: tokens(set.Theme.Dark), Font: set.Theme.Font, LogoUrl: set.Theme.LogoURL, FaviconUrl: set.Theme.FaviconURL},
	}
	for _, l := range set.Nav {
		p.Nav = append(p.Nav, &hubv1.NavLink{Label: l.Label, Url: l.URL, Placement: l.Placement, Role: l.Role})
	}
	if d := set.Discord; !d.IsZero() {
		p.Discord = &hubv1.DiscordSettings{GuildId: d.GuildID, Roles: &hubv1.DiscordRoles{Linked: d.Roles.Linked, Providers: d.Roles.Providers}, ModLog: d.ModLog}
		for _, r := range d.Recognition {
			p.Discord.Recognition = append(p.Discord.Recognition, &hubv1.DiscordRecognition{Role: r.Role, Rule: r.Rule, Count: int32(r.Count), MinMembers: int32(r.MinMembers)}) //nolint:gosec // bounded by validation
		}
		for _, c := range d.ServerCards {
			p.Discord.ServerCards = append(p.Discord.ServerCards, &hubv1.DiscordServerCard{Server: c.Server, Channel: c.Channel, Note: c.Note})
		}
		for _, r := range d.Seeding {
			ps := &hubv1.DiscordSeeding{Server: r.Server, Channel: r.Channel, Role: r.Role, Below: int32(r.Below), //nolint:gosec // bounded by validation
				Cooldown: r.Cooldown, CallAtStart: r.CallAtStart, Note: r.Note}
			for _, h := range r.Hours {
				ps.Hours = append(ps.Hours, &hubv1.DiscordSeedingHours{Days: h.Days, From: h.From, To: h.To})
			}
			p.Discord.Seeding = append(p.Discord.Seeding, ps)
		}
		for _, r := range d.Recaps {
			pr := &hubv1.DiscordRecap{Server: r.Server, Channel: r.Channel, Title: r.Title, Floor: int32(r.Floor), Note: r.Note} //nolint:gosec // bounded by validation
			for _, h := range r.Hours {
				pr.Hours = append(pr.Hours, &hubv1.DiscordSeedingHours{Days: h.Days, From: h.From, To: h.To})
			}
			p.Discord.Recaps = append(p.Discord.Recaps, pr)
		}
		if st := d.Streams; !st.IsZero() {
			p.Discord.Streams = &hubv1.DiscordStreams{Channel: st.Channel, Role: st.Role, Note: st.Note}
			for _, s := range st.Streamers {
				p.Discord.Streams.Streamers = append(p.Discord.Streams.Streamers, &hubv1.DiscordStreamer{Platform: s.Platform, Login: s.Login, Label: s.Label})
			}
		}
		if n := d.News; !n.IsZero() {
			p.Discord.News = &hubv1.DiscordNews{}
			for _, f := range n.Feeds {
				pf := &hubv1.DiscordNewsFeed{AppId: int32(f.AppID), Channel: f.Channel, Label: f.Label, Feeds: f.Feeds, Color: int32(f.Color)} //nolint:gosec // bounded by validation
				p.Discord.News.Feeds = append(p.Discord.News.Feeds, pf)
			}
		}
		if r := d.MatchResults; !r.IsZero() {
			p.Discord.MatchResults = &hubv1.DiscordMatchResults{Channel: r.Channel, Servers: r.Servers}
		}
		if e := d.Events; !e.IsZero() {
			p.Discord.Events = &hubv1.DiscordEvents{Channel: e.Channel, MinSeverity: e.MinSeverity, Kinds: e.Kinds}
		}
		if pr := d.Presence; !pr.IsZero() {
			p.Discord.Presence = &hubv1.DiscordPresence{Channel: pr.Channel, Note: pr.Note}
			for _, g := range pr.Games {
				p.Discord.Presence.Games = append(p.Discord.Presence.Games, &hubv1.DiscordPresenceGame{Game: g.Game, Label: g.Label, Activities: g.Activities, ApplicationIds: g.ApplicationIDs})
			}
		}
	}
	if st := set.Stats; !st.IsZero() {
		p.Stats = &hubv1.StatsSettings{Public: st.Public, MinMatches: int32(st.MinMatches), Timezone: st.Timezone, //nolint:gosec // bounded by validation
			RawRetentionMonths: int32(st.RawRetentionMonths)} //nolint:gosec // bounded by validation
		for _, se := range st.Seasons {
			p.Stats.Seasons = append(p.Stats.Seasons, &hubv1.StatsSeason{Name: se.Name, Game: se.Game, From: se.From, To: se.To})
		}
	}
	if e := set.Events; !e.IsZero() {
		p.Events = &hubv1.EventsSettings{RetentionDays: int32(e.RetentionDays)} //nolint:gosec // bounded by validation
	}
	if l := set.LocalAccounts; !l.IsZero() {
		p.LocalAccounts = &hubv1.LocalAccountsSettings{Registration: l.Registration, MinPasswordLength: int32(l.MinPasswordLength)} //nolint:gosec // bounded by validation
	}
	for id, g := range set.Games {
		if p.Games == nil {
			p.Games = map[string]*hubv1.GameSettings{}
		}
		pg := &hubv1.GameSettings{Theme: &hubv1.GameTheme{Light: tokens(g.Theme.Light), Dark: tokens(g.Theme.Dark), BannerUrl: g.Theme.BannerURL, BannerAlt: g.Theme.BannerAlt}}
		for _, l := range g.Nav {
			pg.Nav = append(pg.Nav, &hubv1.NavLink{Label: l.Label, Url: l.URL, Placement: l.Placement, Role: l.Role})
		}
		pg.Maps, pg.MapFallback = g.Maps, g.MapFallback
		p.Games[id] = pg
	}
	for _, n := range set.Notifiers {
		p.Notifiers = append(p.Notifiers, &hubv1.Notifier{Name: n.Name, Kind: n.Kind, Disabled: n.Disabled, Url: n.URL, Flavour: n.Flavour, Topic: n.Topic,
			To: n.To, Events: &hubv1.NotifierEvents{Kinds: n.Events.Kinds, MinSeverity: n.Events.MinSeverity, Games: n.Events.Games, Servers: n.Events.Servers}})
	}
	if !at.IsZero() {
		p.UpdatedAt = timestamppb.New(at)
	}
	return p
}

func settingsFromProto(p *hubv1.OrganizationSettings) org.Settings {
	tokens := func(t *hubv1.ThemeTokens) org.Tokens {
		return org.Tokens{Accent: t.GetAccent(), Background: t.GetBackground(), Foreground: t.GetForeground(), Muted: t.GetMuted(), Line: t.GetLine(), OK: t.GetOk(), Err: t.GetErr()}
	}
	set := org.Settings{
		Version: int(p.GetVersion()),
		Theme:   org.Theme{Light: tokens(p.GetTheme().GetLight()), Dark: tokens(p.GetTheme().GetDark()), Font: p.GetTheme().GetFont(), LogoURL: p.GetTheme().GetLogoUrl(), FaviconURL: p.GetTheme().GetFaviconUrl()},
	}
	if set.Version == 0 {
		set.Version = org.SettingsVersion
	}
	for _, l := range p.GetNav() {
		set.Nav = append(set.Nav, org.NavLink{Label: l.GetLabel(), URL: l.GetUrl(), Placement: l.GetPlacement(), Role: l.GetRole()})
	}
	d := p.GetDiscord()
	set.Discord = org.Discord{GuildID: d.GetGuildId(), Roles: org.DiscordRoles{Linked: d.GetRoles().GetLinked(), Providers: d.GetRoles().GetProviders()}, ModLog: d.GetModLog()}
	for _, r := range d.GetRecognition() {
		set.Discord.Recognition = append(set.Discord.Recognition, org.Recognition{Role: r.GetRole(), Rule: r.GetRule(), Count: int(r.GetCount()), MinMembers: int(r.GetMinMembers())})
	}
	for _, c := range d.GetServerCards() {
		set.Discord.ServerCards = append(set.Discord.ServerCards, org.ServerCard{Server: c.GetServer(), Channel: c.GetChannel(), Note: c.GetNote()})
	}
	for _, r := range d.GetSeeding() {
		rule := org.SeedingRule{Server: r.GetServer(), Channel: r.GetChannel(), Role: r.GetRole(), Below: int(r.GetBelow()),
			Cooldown: r.GetCooldown(), CallAtStart: r.GetCallAtStart(), Note: r.GetNote()}
		for _, h := range r.GetHours() {
			rule.Hours = append(rule.Hours, org.SeedingHours{Days: h.GetDays(), From: h.GetFrom(), To: h.GetTo()})
		}
		set.Discord.Seeding = append(set.Discord.Seeding, rule)
	}
	for _, r := range d.GetRecaps() {
		rule := org.RecapRule{Server: r.GetServer(), Channel: r.GetChannel(), Title: r.GetTitle(), Floor: int(r.GetFloor()), Note: r.GetNote()}
		for _, h := range r.GetHours() {
			rule.Hours = append(rule.Hours, org.SeedingHours{Days: h.GetDays(), From: h.GetFrom(), To: h.GetTo()})
		}
		set.Discord.Recaps = append(set.Discord.Recaps, rule)
	}
	if st := d.GetStreams(); st != nil {
		set.Discord.Streams = org.Streams{Channel: st.GetChannel(), Role: st.GetRole(), Note: st.GetNote()}
		for _, s := range st.GetStreamers() {
			set.Discord.Streams.Streamers = append(set.Discord.Streams.Streamers, org.Streamer{Platform: s.GetPlatform(), Login: s.GetLogin(), Label: s.GetLabel()})
		}
	}
	for _, f := range d.GetNews().GetFeeds() {
		set.Discord.News.Feeds = append(set.Discord.News.Feeds, org.NewsFeed{AppID: int(f.GetAppId()), Channel: f.GetChannel(), Label: f.GetLabel(),
			Feeds: f.GetFeeds(), Color: int(f.GetColor())})
	}
	if r := d.GetMatchResults(); r != nil {
		set.Discord.MatchResults = org.MatchResults{Channel: r.GetChannel(), Servers: r.GetServers()}
	}
	if e := d.GetEvents(); e != nil {
		set.Discord.Events = org.DiscordEvents{Channel: e.GetChannel(), MinSeverity: e.GetMinSeverity(), Kinds: e.GetKinds()}
	}
	if pr := d.GetPresence(); pr != nil {
		set.Discord.Presence = org.Presence{Channel: pr.GetChannel(), Note: pr.GetNote()}
		for _, g := range pr.GetGames() {
			set.Discord.Presence.Games = append(set.Discord.Presence.Games, org.PresenceGame{Game: g.GetGame(), Label: g.GetLabel(), Activities: g.GetActivities(), ApplicationIDs: g.GetApplicationIds()})
		}
	}
	st := p.GetStats()
	set.Stats = org.Stats{Public: st.GetPublic(), MinMatches: int(st.GetMinMatches()), Timezone: st.GetTimezone(),
		RawRetentionMonths: int(st.GetRawRetentionMonths())}
	for _, se := range st.GetSeasons() {
		set.Stats.Seasons = append(set.Stats.Seasons, org.Season{Name: se.GetName(), Game: se.GetGame(), From: se.GetFrom(), To: se.GetTo()})
	}
	if e := p.GetEvents(); e != nil {
		set.Events = org.Events{RetentionDays: int(e.GetRetentionDays())}
	}
	if l := p.GetLocalAccounts(); l != nil {
		set.LocalAccounts = org.LocalAccounts{Registration: l.GetRegistration(), MinPasswordLength: int(l.GetMinPasswordLength())}
	}
	for id, g := range p.GetGames() {
		if set.Games == nil {
			set.Games = map[string]org.GameSettings{}
		}
		t := g.GetTheme()
		og := org.GameSettings{Theme: org.GameTheme{Light: tokens(t.GetLight()), Dark: tokens(t.GetDark()), BannerURL: t.GetBannerUrl(), BannerAlt: t.GetBannerAlt()}}
		for _, l := range g.GetNav() {
			og.Nav = append(og.Nav, org.NavLink{Label: l.GetLabel(), URL: l.GetUrl(), Placement: l.GetPlacement(), Role: l.GetRole()})
		}
		og.Maps, og.MapFallback = g.GetMaps(), g.GetMapFallback()
		set.Games[id] = og
	}
	for _, n := range p.GetNotifiers() {
		e := n.GetEvents()
		set.Notifiers = append(set.Notifiers, org.Notifier{Name: n.GetName(), Kind: n.GetKind(), Disabled: n.GetDisabled(), URL: n.GetUrl(),
			Flavour: n.GetFlavour(), Topic: n.GetTopic(), To: n.GetTo(),
			Events: org.NotifierEvents{Kinds: e.GetKinds(), MinSeverity: e.GetMinSeverity(), Games: e.GetGames(), Servers: e.GetServers()}})
	}
	return set
}

func toProto(o store.Organization, demo bool) *hubv1.Organization {
	p := &hubv1.Organization{
		Id:        o.ID.String(),
		Name:      o.Name,
		Owned:     o.Owned(),
		CreatedAt: timestamppb.New(o.CreatedAt),
		Demo:      demo,
	}
	if o.ClaimedAt != nil {
		p.ClaimedAt = timestamppb.New(*o.ClaimedAt)
	}
	if o.OwnerUserID != nil {
		p.OwnerUserId = o.OwnerUserID.String()
	}
	return p
}
