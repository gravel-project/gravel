package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	"github.com/gravel-project/gravel/drivers"
	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
	"github.com/gravel-project/gravel/gen/gravel/hub/v1/hubv1connect"
	"github.com/gravel-project/gravel/internal/apps"
	"github.com/gravel-project/gravel/internal/authz"
	"github.com/gravel-project/gravel/internal/httpx"
	"github.com/gravel-project/gravel/internal/identity"
	"github.com/gravel-project/gravel/internal/org"
	"github.com/gravel-project/gravel/internal/servers"
	"github.com/gravel-project/gravel/internal/session"
	"github.com/gravel-project/gravel/internal/store"
)

// Who calls a procedure: a member through their session cookie; an app through its own bearer
// token (apps.FromContext), within the token's scopes; or a member through a token an app holds
// for them (apps.MemberFromContext, ADR-0015 §4), with the member's own rights narrowed to the
// token's scopes. A member's token is checked twice: the MemberTokens interceptor admits only the
// procedures its scopes cover (memberProcedures), and the procedure then applies the member's own
// right exactly as for their session, through caller.

// memberProcedures is every procedure a member's token may call, with the scope it must carry on
// top of the member's own right to the call; "" needs none. GetMe is how an app learns who signed
// in, so it needs no scope (ADR-0015 §4); the other "" entries are public and do not look at the
// caller. A procedure missing here is refused to a member's token: the account's own controls
// (unlinking, sessions, the leaderboard name, ownership, roles, settings, the apps a member
// allowed) are the member's to use in person, and a new procedure is refused until it is added.
var memberProcedures = map[string]string{
	hubv1connect.IdentityServiceGetMeProcedure:                       "",
	hubv1connect.IdentityServiceLookupUserProcedure:                  apps.ScopeIdentityRead,
	hubv1connect.IdentityServiceListUsersProcedure:                   apps.ScopeIdentityRead,
	hubv1connect.IdentityServiceListIdentityEventsProcedure:          apps.ScopeIdentityRead,
	hubv1connect.IdentityServiceListRoleGrantsProcedure:              apps.ScopeIdentityRead,
	hubv1connect.OrganizationServiceGetOrganizationProcedure:         "",
	hubv1connect.OrganizationServiceGetOrganizationSettingsProcedure: "",
	hubv1connect.OrganizationServiceListEventsProcedure:              apps.ScopeEventsRead,
	hubv1connect.OrganizationServiceListEventKindsProcedure:          apps.ScopeEventsRead,
	hubv1connect.ServerServiceListGamesProcedure:                     "",
	hubv1connect.ServerServiceListServersProcedure:                   "",
	hubv1connect.ServerServiceGetServerStatusProcedure:               "",
	hubv1connect.ServerServiceListServerPlayersProcedure:             apps.ScopeServersRead,
	hubv1connect.ServerServiceGetFleetSummaryProcedure:               apps.ScopeServersRead,
	hubv1connect.ModerationServiceKickPlayerProcedure:                apps.ScopeServersModerate,
	hubv1connect.ModerationServiceBanPlayerProcedure:                 apps.ScopeServersModerate,
	hubv1connect.ModerationServiceUnbanPlayerProcedure:               apps.ScopeServersModerate,
	hubv1connect.ModerationServiceMessagePlayerProcedure:             apps.ScopeServersModerate,
	hubv1connect.ModerationServiceBroadcastProcedure:                 apps.ScopeServersModerate,
	hubv1connect.ModerationServiceMovePlayerProcedure:                apps.ScopeServersModerate,
	hubv1connect.ModerationServiceListServerBansProcedure:            apps.ScopeServersModerate,
	hubv1connect.ModerationServiceListAuditLogProcedure:              apps.ScopeServersModerate,
	hubv1connect.ServerHostServiceGetServerHostProcedure:             apps.ScopeServersRead,
	hubv1connect.ServerHostServiceListServerHostEventsProcedure:      apps.ScopeServersRead,
	hubv1connect.ServerHostServiceSetServerHostDesiredProcedure:      apps.ScopeServersOperate,
	hubv1connect.ServerHostServiceRebootServerHostProcedure:          apps.ScopeServersOperate,
	hubv1connect.ServerHostServiceSetServerHostExpectationProcedure:  apps.ScopeServersOperate,
	// ResizeServerHost, UnpinServerHostSize, SnapshotServerHostVolume and DestroyServerHost are the owner's, in person, and stay out.
	// StartProof and GetProof (gravel#5) are the member's, in person: an app never proves an account for them.
	// CreateLocalAccount and IssuePasswordReset (gravel#187) are staff's, in person: a link is a password for an hour.
	// TestNotifier (gravel#196) is the owner's, in person, like the settings it tests; so are ExportSettings, CheckSettings and ApplySettings (gravel#201).
	// NominateOwner, AcceptOwnership and CancelOwnershipTransfer (gravel#197) are the owner's and the nominee's, in person: a token never hands a hub over.
	// The operator's admin procedures (gravel#195: CreateApp, ListApps, RevokeApp, ExportServers, ApplyServers, CheckServers, EraseIdentity, RevokeUserSessions) are staff's, in person.
	// ServerLeaseService (#20) is an app's (servers:lease) or a moderator's in person; a member's token never leases.
	hubv1connect.ServerConfigServiceGetServerConfigProcedure:   apps.ScopeServersConfigure,
	hubv1connect.ServerConfigServicePlanServerConfigProcedure:  apps.ScopeServersConfigure,
	hubv1connect.ServerConfigServiceApplyServerConfigProcedure: apps.ScopeServersConfigure,
	hubv1connect.StatsServiceGetBoardProcedure:                 apps.ScopeStatsRead,
	hubv1connect.StatsServiceListMatchesProcedure:              apps.ScopeStatsRead,
	hubv1connect.StatsServiceListMatchResultsProcedure:         apps.ScopeStatsRead,
	hubv1connect.StatsServiceGetMatchResultProcedure:           apps.ScopeStatsRead,
	hubv1connect.StatsServiceGetMatchDemoProcedure:             apps.ScopeStatsRead,
	hubv1connect.StatsServiceGetMemberProfileProcedure:         apps.ScopeStatsRead,
	hubv1connect.StatsServiceGetParticipationBoardProcedure:    apps.ScopeStatsRead,
	hubv1connect.StatsServiceGetSessionRecapProcedure:          apps.ScopeStatsRead,
	// RecordParticipation is an app's own (ADR-0014) and stays out: a member's token never
	// stands in for the app.
}

// MemberTokenScope says whether a member's token may call a procedure and the scope it must
// carry to ("" for none).
func MemberTokenScope(procedure string) (scope string, allowed bool) {
	scope, allowed = memberProcedures[procedure]
	return scope, allowed
}

// MemberTokens is the interceptor that holds a member's token to its scopes; the hub puts it on
// every service. A call without a member's token passes untouched.
func MemberTokens() connect.Interceptor { return memberTokens{} }

type memberTokens struct{}

type memberCheckedKey struct{}

// admit refuses a member's token a procedure its scopes do not cover, and marks the context as
// checked, which caller requires before it lets a member's token act as the member.
func (memberTokens) admit(ctx context.Context, procedure string) (context.Context, error) {
	m, ok := apps.MemberFromContext(ctx)
	if !ok {
		return ctx, nil
	}
	scope, allowed := memberProcedures[procedure]
	if !allowed {
		return ctx, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("a member's token from %s cannot call %s", m.App.Name, procedure))
	}
	if scope != "" && !m.Has(scope) {
		return ctx, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("the token from %s lacks the scope %q", m.App.Name, scope))
	}
	return context.WithValue(ctx, memberCheckedKey{}, true), nil
}

func (i memberTokens) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		ctx, err := i.admit(ctx, req.Spec().Procedure)
		if err != nil {
			return nil, err
		}
		return next(ctx, req)
	}
}

func (memberTokens) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (i memberTokens) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		ctx, err := i.admit(ctx, conn.Spec().Procedure)
		if err != nil {
			return err
		}
		return next(ctx, conn)
	}
}

// caller returns the member calling: the member a member's token names (the token wins over a
// session cookie sent with it), else the session's. CodeUnauthenticated without either. A member's
// token that the MemberTokens interceptor did not check is refused, so a service mounted without
// it fails closed.
func caller(ctx context.Context) (uuid.UUID, error) {
	if m, ok := apps.MemberFromContext(ctx); ok {
		if checked, _ := ctx.Value(memberCheckedKey{}).(bool); !checked {
			return uuid.Nil, connect.NewError(connect.CodePermissionDenied, errors.New("a member's token is not accepted here"))
		}
		return m.UserID, nil
	}
	s, ok := session.FromContext(ctx)
	if !ok {
		return uuid.Nil, connect.NewError(connect.CodeUnauthenticated, errors.New("log in first"))
	}
	return s.UserID, nil
}

// sessionCaller returns the member of the session cookie, refusing any bearer token: for the
// procedures a member uses only in person (the apps they allowed, consent).
func sessionCaller(ctx context.Context) (uuid.UUID, error) {
	if _, ok := apps.MemberFromContext(ctx); ok {
		return uuid.Nil, connect.NewError(connect.CodePermissionDenied, errors.New("a member's token cannot call this"))
	}
	if _, ok := apps.FromContext(ctx); ok {
		return uuid.Nil, connect.NewError(connect.CodePermissionDenied, errors.New("an app cannot call this"))
	}
	s, ok := session.FromContext(ctx)
	if !ok {
		return uuid.Nil, connect.NewError(connect.CodeUnauthenticated, errors.New("log in first"))
	}
	return s.UserID, nil
}

// changedBy is who a change the events log records was made by (ADR-0028): the member in person,
// an app, or a member through an app's token (both), through the API.
func changedBy(ctx context.Context, p authz.Principal) store.Actor {
	a := store.Actor{Via: store.ViaAPI, RequestID: httpx.RequestIDFromContext(ctx)}
	if p.UserID != uuid.Nil {
		uid := p.UserID
		a.UserID = &uid
	}
	if p.AppID != uuid.Nil {
		app := p.AppID
		a.AppID = &app
	}
	return a
}

// actor is who a moderation or configuration call is recorded as: an app acting for itself (or
// for someone it names in onBehalf), a member in person, or a member through an app's token, who
// is recorded with the app (ADR-0015 §4). A member's token never acts on behalf of anyone.
func actor(ctx context.Context, onBehalf *drivers.Identity) (servers.Actor, error) {
	a := servers.Actor{RequestID: httpx.RequestIDFromContext(ctx), OnBehalfOf: onBehalf}
	if app, ok := apps.FromContext(ctx); ok {
		a.AppID = &app.ID
		return a, nil
	}
	if _, ok := apps.MemberFromContext(ctx); ok && onBehalf != nil {
		return servers.Actor{}, connect.NewError(connect.CodePermissionDenied, errors.New("on_behalf_of is an app's; a member's token acts as that member"))
	}
	uid, err := caller(ctx) // a member in person with on_behalf_of is refused by the action's validation
	if err != nil {
		return servers.Actor{}, err
	}
	a.UserID = &uid
	if m, ok := apps.MemberFromContext(ctx); ok {
		a.AppID = &m.App.ID
	}
	return a, nil
}

// Members is what Access reads of members: one by id, for their roles, and one by a provider
// identity, for an app's on_behalf_of (identity.Service).
type Members interface {
	MemberLookup
	Me(ctx context.Context, userID uuid.UUID) (identity.User, error)
}

// ServerGames finds a server's game, so a game-scoped grant applies to the servers of its game,
// those added later included (ADR-0027 §4).
type ServerGames interface {
	Server(ctx context.Context, id string) (servers.Server, error)
}

// Access resolves who is calling into an authz.Principal and checks a capability against it
// (ADR-0027). Every service that checks a capability shares one.
type Access struct {
	org     *org.Service
	members Members
	servers ServerGames // nil: no game-scoped check can match a server
	logger  *slog.Logger
}

// WithServers lets a check on a server match a grant for its game.
func (a *Access) WithServers(s ServerGames) *Access {
	a.servers = s
	return a
}

// target fills in the game of t's server, read at call time. An unknown server leaves it empty:
// only an instance-wide grant admits, and the procedure says not found.
func (a *Access) target(ctx context.Context, t authz.Target) authz.Target {
	if t.GameID != "" || t.ServerID == "" || a.servers == nil {
		return t
	}
	if srv, err := a.servers.Server(ctx, t.ServerID); err == nil {
		t.GameID = srv.Game
	}
	return t
}

// RequireAny returns the caller when they hold c instance-wide or for at least one game: a list
// that is not per server, which Visible then filters.
func (a *Access) RequireAny(ctx context.Context, c authz.Capability) (authz.Principal, error) {
	p, err := a.Principal(ctx)
	if err != nil {
		return authz.Principal{}, err
	}
	if all, games := p.Scope(c); all || len(games) > 0 {
		return p, nil
	}
	return authz.Principal{}, refused(p.Check(c, authz.Target{}))
}

// Visible reports whether p holds c for the server: a row of an instance-wide list a game-scoped
// caller may see.
func (a *Access) Visible(ctx context.Context, p authz.Principal, c authz.Capability, serverID string) bool {
	return p.Has(c, a.target(ctx, authz.Target{ServerID: serverID}))
}

// NewAccess builds the resolver over the organization (who owns it) and the members (their roles).
func NewAccess(orgSvc *org.Service, members Members, logger *slog.Logger) *Access {
	return &Access{org: orgSvc, members: members, logger: logger}
}

// Principal is who is calling: a member's token (which wins over a session cookie sent with it,
// and which the MemberTokens interceptor must have checked), an app's own token, a session, or
// nobody.
func (a *Access) Principal(ctx context.Context) (authz.Principal, error) {
	if m, ok := apps.MemberFromContext(ctx); ok {
		if checked, _ := ctx.Value(memberCheckedKey{}).(bool); !checked {
			return authz.Principal{}, connect.NewError(connect.CodePermissionDenied, errors.New("a member's token is not accepted here"))
		}
		p, err := a.member(ctx, m.UserID)
		if err != nil {
			return authz.Principal{}, err
		}
		return p.Through(m.App.ID, m.App.Name, m.App.Scopes), nil
	}
	if app, ok := apps.FromContext(ctx); ok {
		return authz.ForApp(app.ID, app.Name, app.Scopes), nil
	}
	if s, ok := session.FromContext(ctx); ok {
		return a.member(ctx, s.UserID)
	}
	return authz.Nobody(), nil
}

// member is a member in person: the owner, or a member with the roles they were granted. A member
// the store no longer has holds the member base only.
func (a *Access) member(ctx context.Context, uid uuid.UUID) (authz.Principal, error) {
	o, err := a.org.Get(ctx)
	if err != nil {
		return authz.Principal{}, mapError(ctx, a.logger, err)
	}
	if o.OwnerUserID != nil && *o.OwnerUserID == uid {
		return authz.ForMember(uid, true, nil), nil
	}
	u, err := a.members.Me(ctx, uid)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return authz.ForMember(uid, false, nil), nil
	case err != nil:
		return authz.Principal{}, mapError(ctx, a.logger, err)
	}
	return authz.ForMember(uid, false, u.AuthzGrants()), nil
}

// Require returns the caller when they hold c for t; else CodeUnauthenticated for nobody and
// CodePermissionDenied for anyone else.
func (a *Access) Require(ctx context.Context, c authz.Capability, t authz.Target) (authz.Principal, error) {
	p, err := a.Principal(ctx)
	if err != nil {
		return authz.Principal{}, err
	}
	t = a.target(ctx, t)
	if err := p.Check(c, t); err != nil {
		return authz.Principal{}, refused(err)
	}
	return p, nil
}

// RequireFor is Require for a procedure an app may call on behalf of a member it names: the app
// must hold c, and so must the member, so an app can't lend its scope to anyone who asks it
// (ADR-0013 §4, kept by ADR-0027). A member's token never acts on behalf of anyone; a member in
// person who sends on_behalf_of is refused by the action's own validation.
func (a *Access) RequireFor(ctx context.Context, c authz.Capability, t authz.Target, onBehalf *hubv1.Actor) (authz.Principal, error) {
	if _, ok := apps.MemberFromContext(ctx); ok && onBehalf != nil {
		return authz.Principal{}, connect.NewError(connect.CodePermissionDenied, errors.New("on_behalf_of is an app's; a member's token acts as that member"))
	}
	p, err := a.Require(ctx, c, t)
	if err != nil || p.Kind != authz.App || onBehalf == nil {
		return p, err
	}
	u, err := a.members.Lookup(ctx, onBehalf.GetProvider(), onBehalf.GetSubject())
	if errors.Is(err, store.ErrNotFound) {
		return authz.Principal{}, connect.NewError(connect.CodePermissionDenied, errors.New("on_behalf_of is not a member of this hub"))
	}
	if err != nil {
		return authz.Principal{}, mapError(ctx, a.logger, err)
	}
	m, err := a.member(ctx, u.ID)
	if err != nil {
		return authz.Principal{}, err
	}
	if !m.Has(c, a.target(ctx, t)) {
		return authz.Principal{}, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("on_behalf_of does not hold %q", c))
	}
	return p, nil
}

// refused turns Check's refusal into a Connect error.
func refused(err error) error {
	code := connect.CodePermissionDenied
	if errors.Is(err, authz.ErrUnauthenticated) {
		code = connect.CodeUnauthenticated
	}
	return connect.NewError(code, errors.New(err.Error()))
}

// mapError turns a domain error into a Connect error. Anything unmapped is logged with the
// request id and answered as an opaque internal error.
func mapError(ctx context.Context, logger *slog.Logger, err error) error {
	switch {
	case errors.Is(err, org.ErrAlreadyOwned):
		return connect.NewError(connect.CodeFailedPrecondition, err)
	case errors.Is(err, org.ErrInvalidToken):
		return connect.NewError(connect.CodePermissionDenied, err)
	case errors.Is(err, org.ErrInvalidSettings):
		return connect.NewError(connect.CodeInvalidArgument, err)
	case errors.Is(err, org.ErrNotOwner):
		return connect.NewError(connect.CodePermissionDenied, err)
	case errors.Is(err, org.ErrSelfNomination):
		return connect.NewError(connect.CodeInvalidArgument, err)
	case errors.Is(err, org.ErrNotMember):
		return connect.NewError(connect.CodeNotFound, err)
	case errors.Is(err, org.ErrNoNomination), errors.Is(err, org.ErrNominationExpired):
		return connect.NewError(connect.CodeFailedPrecondition, err)
	case errors.Is(err, identity.ErrLastIdentity):
		return connect.NewError(connect.CodeFailedPrecondition, err)
	case errors.Is(err, identity.ErrNotLinked):
		return connect.NewError(connect.CodeNotFound, err)
	case errors.Is(err, store.ErrNotFound):
		return connect.NewError(connect.CodeNotFound, errors.New("not found"))
	}
	logger.ErrorContext(ctx, "internal error", "error", err.Error(), "request_id", httpx.RequestIDFromContext(ctx))
	return connect.NewError(connect.CodeInternal, errors.New("internal error"))
}
