package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"strings"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/gravel-project/gravel/drivers"
	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
	"github.com/gravel-project/gravel/gen/gravel/hub/v1/hubv1connect"
	"github.com/gravel-project/gravel/internal/apps"
	"github.com/gravel-project/gravel/internal/httpx"
	"github.com/gravel-project/gravel/internal/identity"
	"github.com/gravel-project/gravel/internal/org"
	"github.com/gravel-project/gravel/internal/servers"
	"github.com/gravel-project/gravel/internal/store"
)

// ModerationServer serves gravel.hub.v1.ModerationService: the owner, a moderator (ADR-0013), or
// an app with servers:moderate acting for a moderator or for itself.
type ModerationServer struct {
	hubv1connect.UnimplementedModerationServiceHandler
	mod     *servers.Moderation
	org     *org.Service
	members ModeratorLookup
	logger  *slog.Logger
}

// ModeratorLookup reads members and their roles (identity.Service).
type ModeratorLookup interface {
	MemberLookup
	Me(ctx context.Context, userID uuid.UUID) (identity.User, error)
}

// NewModerationServer wires the service.
func NewModerationServer(mod *servers.Moderation, orgSvc *org.Service, members ModeratorLookup, logger *slog.Logger) *ModerationServer {
	return &ModerationServer{mod: mod, org: orgSvc, members: members, logger: logger}
}

// KickPlayer removes a connected player.
func (s *ModerationServer) KickPlayer(ctx context.Context, req *connect.Request[hubv1.KickPlayerRequest]) (*connect.Response[hubv1.KickPlayerResponse], error) {
	m := req.Msg
	id, err := s.do(ctx, m.GetOnBehalfOf(), servers.Action{Server: m.GetServerId(), Kind: servers.ActionKick, Player: player(m.GetSubject()), Reason: m.GetReason()})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&hubv1.KickPlayerResponse{AuditId: id}), nil
}

// BanPlayer bans a connected player.
func (s *ModerationServer) BanPlayer(ctx context.Context, req *connect.Request[hubv1.BanPlayerRequest]) (*connect.Response[hubv1.BanPlayerResponse], error) {
	m := req.Msg
	id, err := s.do(ctx, m.GetOnBehalfOf(), servers.Action{Server: m.GetServerId(), Kind: servers.ActionBan, Player: player(m.GetSubject()), Reason: m.GetReason()})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&hubv1.BanPlayerResponse{AuditId: id}), nil
}

// UnbanPlayer lifts a ban.
func (s *ModerationServer) UnbanPlayer(ctx context.Context, req *connect.Request[hubv1.UnbanPlayerRequest]) (*connect.Response[hubv1.UnbanPlayerResponse], error) {
	m := req.Msg
	id, err := s.do(ctx, m.GetOnBehalfOf(), servers.Action{Server: m.GetServerId(), Kind: servers.ActionUnban, Player: player(m.GetSubject()), Reason: m.GetReason()})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&hubv1.UnbanPlayerResponse{AuditId: id}), nil
}

// MessagePlayer whispers to a connected player.
func (s *ModerationServer) MessagePlayer(ctx context.Context, req *connect.Request[hubv1.MessagePlayerRequest]) (*connect.Response[hubv1.MessagePlayerResponse], error) {
	m := req.Msg
	id, err := s.do(ctx, m.GetOnBehalfOf(), servers.Action{Server: m.GetServerId(), Kind: servers.ActionMessage, Player: player(m.GetSubject()), Message: m.GetMessage()})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&hubv1.MessagePlayerResponse{AuditId: id}), nil
}

// Broadcast sends a message to everyone on the server.
func (s *ModerationServer) Broadcast(ctx context.Context, req *connect.Request[hubv1.BroadcastRequest]) (*connect.Response[hubv1.BroadcastResponse], error) {
	m := req.Msg
	id, err := s.do(ctx, m.GetOnBehalfOf(), servers.Action{Server: m.GetServerId(), Kind: servers.ActionBroadcast, Message: m.GetMessage()})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&hubv1.BroadcastResponse{AuditId: id}), nil
}

// MovePlayer moves a connected player to a team.
func (s *ModerationServer) MovePlayer(ctx context.Context, req *connect.Request[hubv1.MovePlayerRequest]) (*connect.Response[hubv1.MovePlayerResponse], error) {
	m := req.Msg
	id, err := s.do(ctx, m.GetOnBehalfOf(), servers.Action{Server: m.GetServerId(), Kind: servers.ActionMovePlayer, Player: player(m.GetSubject()),
		Team: m.GetTeam(), Respawn: m.GetRespawn()})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&hubv1.MovePlayerResponse{AuditId: id}), nil
}

// ListServerBans are the bans a server holds.
func (s *ModerationServer) ListServerBans(ctx context.Context, req *connect.Request[hubv1.ListServerBansRequest]) (*connect.Response[hubv1.ListServerBansResponse], error) {
	if err := s.requireModerator(ctx, nil); err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.Msg.GetServerId()) == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("server_id is required"))
	}
	bans, err := s.mod.Bans(ctx, req.Msg.GetServerId())
	if err != nil {
		return nil, s.mapError(ctx, err)
	}
	out := &hubv1.ListServerBansResponse{}
	for _, b := range bans {
		p := &hubv1.Ban{Provider: b.Identity.Provider, Subject: b.Identity.Subject, BannedBy: b.By, Reason: b.Reason, Hub: b.Hub != nil}
		if !b.At.IsZero() {
			p.BannedAt = timestamppb.New(b.At)
		}
		if h := b.Hub; h != nil {
			if p.Reason == "" {
				p.Reason = h.Reason
			}
			if h.BanAuditID != nil {
				p.AuditId = *h.BanAuditID
			}
		}
		if b.Identity.Subject != "" {
			u, err := s.members.Lookup(ctx, b.Identity.Provider, b.Identity.Subject)
			switch {
			case err == nil:
				p.UserId = u.ID.String()
			case !errors.Is(err, store.ErrNotFound):
				return nil, mapError(ctx, s.logger, err)
			}
		}
		out.Bans = append(out.Bans, p)
	}
	return connect.NewResponse(out), nil
}

// ListAuditLog is the audit log, newest first.
func (s *ModerationServer) ListAuditLog(ctx context.Context, req *connect.Request[hubv1.ListAuditLogRequest]) (*connect.Response[hubv1.ListAuditLogResponse], error) {
	if err := s.requireModerator(ctx, nil); err != nil {
		return nil, err
	}
	before, err := decodeAuditToken(req.Msg.GetPageToken())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	entries, more, err := s.mod.Log(ctx, req.Msg.GetServerId(), before, int(req.Msg.GetPageSize()))
	if err != nil {
		return nil, mapError(ctx, s.logger, err)
	}
	out := &hubv1.ListAuditLogResponse{}
	names := map[uuid.UUID]string{} // each acting member's name, read once a page
	for _, e := range entries {
		p := auditEntry(e)
		if id := e.ActorUserID; id != nil {
			if _, ok := names[*id]; !ok {
				if u, err := s.members.Me(ctx, *id); err == nil {
					names[*id] = u.DisplayName
				} else {
					names[*id] = ""
				}
			}
			p.UserName = names[*id]
		}
		out.Entries = append(out.Entries, p)
	}
	if more && len(entries) > 0 {
		out.NextPageToken = encodeAuditToken(entries[len(entries)-1].ID)
	}
	return connect.NewResponse(out), nil
}

// do checks the caller, builds the actor from the credential and runs the action.
func (s *ModerationServer) do(ctx context.Context, onBehalf *hubv1.Actor, a servers.Action) (int64, error) {
	if err := s.requireModerator(ctx, onBehalf); err != nil {
		return 0, err
	}
	if strings.TrimSpace(a.Server) == "" {
		return 0, connect.NewError(connect.CodeInvalidArgument, errors.New("server_id is required"))
	}
	actor := servers.Actor{RequestID: httpx.RequestIDFromContext(ctx)}
	if app, ok := apps.FromContext(ctx); ok {
		actor.AppID = &app.ID
	} else {
		uid, err := caller(ctx) // requireModerator admitted the owner or a moderator
		if err != nil {
			return 0, err
		}
		actor.UserID = &uid
	}
	if onBehalf != nil {
		actor.OnBehalfOf = &drivers.Identity{Provider: onBehalf.GetProvider(), Subject: onBehalf.GetSubject()}
	}
	id, err := s.mod.Do(ctx, actor, a)
	if err != nil {
		return 0, s.mapError(ctx, err)
	}
	return id, nil
}

// mapError maps the moderation errors, then the rest as everywhere.
func (s *ModerationServer) mapError(ctx context.Context, err error) error {
	switch {
	case errors.Is(err, servers.ErrInvalidModeration), errors.Is(err, drivers.ErrRejected):
		return connect.NewError(connect.CodeInvalidArgument, publicError(err))
	case errors.Is(err, drivers.ErrInvalidConfig):
		var ie *drivers.InvalidConfigError
		msg := "the document is not acceptable"
		if errors.As(err, &ie) {
			parts := make([]string, 0, len(ie.Problems))
			for _, p := range ie.Problems {
				parts = append(parts, strings.TrimSpace(p.Section+" "+p.Key)+": "+p.Code+": "+p.Message)
			}
			msg += ": " + strings.Join(parts, "; ")
		}
		return connect.NewError(connect.CodeInvalidArgument, errors.New(msg))
	case errors.Is(err, drivers.ErrConfigRejected):
		return connect.NewError(connect.CodeInvalidArgument, errors.New("the server rejected the document; plan it to see why"))
	case errors.Is(err, drivers.ErrConfigConflict):
		return connect.NewError(connect.CodeAborted, errors.New("the configuration changed since it was planned; plan again"))
	case errors.Is(err, drivers.ErrNotSupported):
		return connect.NewError(connect.CodeFailedPrecondition, errors.New("the server does not offer this now"))
	case errors.Is(err, drivers.ErrPlayerNotFound):
		return connect.NewError(connect.CodeNotFound, errors.New("the player is not on the server"))
	case errors.Is(err, drivers.ErrBanNotFound):
		return connect.NewError(connect.CodeNotFound, errors.New("the server holds no ban for the player"))
	case errors.Is(err, servers.ErrRespawnFailed):
		return connect.NewError(connect.CodeUnknown, errors.New("the player was moved, but the respawn failed; see the audit log"))
	case errors.Is(err, servers.ErrNotObserved):
		return connect.NewError(connect.CodeUnavailable, errors.New("the hub has not reached the server yet"))
	case errors.Is(err, drivers.ErrCredentialRefused), errors.Is(err, drivers.ErrCredentialMissing):
		return connect.NewError(connect.CodeUnavailable, errors.New("the server does not accept the hub's credential"))
	case errors.Is(err, drivers.ErrRateLimited):
		return connect.NewError(connect.CodeUnavailable, errors.New("the server asked the hub to wait"))
	case errors.Is(err, store.ErrNotFound):
		return connect.NewError(connect.CodeNotFound, errors.New("no such server"))
	}
	var se interface{ Timeout() bool }
	if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &se) {
		return connect.NewError(connect.CodeUnavailable, errors.New("the server did not answer"))
	}
	return mapError(ctx, s.logger, err)
}

// publicError is the part of a validation or rejection a caller may see: the hub's own message
// for a request it refused, the game's error code for one the server refused (never the
// driver's text, which can name the server's address).
func publicError(err error) error {
	if errors.Is(err, servers.ErrInvalidModeration) {
		return err
	}
	msg := "the server rejected the request"
	if code := gameCode(err); code != "" {
		msg += " (" + code + ")"
	}
	return errors.New(msg)
}

// gameCode is the game's error code in a rejection ("message_too_long"), if it carries one.
func gameCode(err error) string {
	var r *drivers.RejectedError
	if errors.As(err, &r) {
		return r.Code
	}
	return ""
}

func player(subject string) drivers.Identity {
	return drivers.Identity{Subject: strings.TrimSpace(subject)}
}

func auditEntry(e store.AuditEntry) *hubv1.AuditEntry {
	p := &hubv1.AuditEntry{
		Id: e.ID, At: timestamppb.New(e.At), AppName: e.ActorAppName, ServerId: e.ServerID, Action: e.Action,
		TargetProvider: e.TargetProvider, TargetSubject: e.TargetSubject, Reason: e.Reason, RequestId: e.RequestID, Outcome: e.Outcome,
	}
	if e.ActorUserID != nil {
		p.UserId = e.ActorUserID.String()
	}
	if e.ActorAppID != nil {
		p.AppId = e.ActorAppID.String()
	}
	if e.OnBehalfProvider != "" || e.OnBehalfSubject != "" {
		p.OnBehalfOf = &hubv1.Actor{Provider: e.OnBehalfProvider, Subject: e.OnBehalfSubject}
	}
	if e.FinishedAt != nil {
		p.FinishedAt = timestamppb.New(*e.FinishedAt)
	}
	var d struct {
		Message string `json:"message"`
		Team    string `json:"team"`
		Respawn bool   `json:"respawn"`
	}
	if len(e.Detail) > 0 && json.Unmarshal(e.Detail, &d) == nil {
		p.Message, p.Team, p.Respawn = d.Message, d.Team, d.Respawn
	}
	return p
}

// An audit page token is the base64 of "audit|<id>", nothing a caller should build.
func encodeAuditToken(id int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte("audit|" + strconv.FormatInt(id, 10)))
}

func decodeAuditToken(token string) (int64, error) {
	if token == "" {
		return 0, nil
	}
	bad := errors.New("page_token is not one this hub issued")
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return 0, bad
	}
	n, ok := strings.CutPrefix(string(raw), "audit|")
	if !ok {
		return 0, bad
	}
	id, err := strconv.ParseInt(n, 10, 64)
	if err != nil || id <= 0 {
		return 0, bad
	}
	return id, nil
}

// requireModerator admits the owner, a member with the moderator role, or an app with
// servers:moderate. An app acting on_behalf_of someone must be acting for a member who could
// moderate themselves, so a bot can't lend its scope to anyone who asks it.
func (s *ModerationServer) requireModerator(ctx context.Context, onBehalf *hubv1.Actor) error {
	if a, ok := apps.FromContext(ctx); ok {
		if !a.Has(apps.ScopeServersModerate) {
			return connect.NewError(connect.CodePermissionDenied, errors.New("the app lacks the scope "+apps.ScopeServersModerate))
		}
		if onBehalf == nil {
			return nil
		}
		u, err := s.members.Lookup(ctx, onBehalf.GetProvider(), onBehalf.GetSubject())
		if errors.Is(err, store.ErrNotFound) {
			return connect.NewError(connect.CodePermissionDenied, errors.New("on_behalf_of is not a member of this hub"))
		}
		if err != nil {
			return mapError(ctx, s.logger, err)
		}
		return s.memberMayModerate(ctx, u.ID, "on_behalf_of is not a moderator")
	}
	uid, err := caller(ctx)
	if err != nil {
		return err
	}
	return s.memberMayModerate(ctx, uid, "moderators only")
}

// memberMayModerate admits the owner or a member holding the moderator role.
func (s *ModerationServer) memberMayModerate(ctx context.Context, uid uuid.UUID, refusal string) error {
	o, err := s.org.Get(ctx)
	if err != nil {
		return mapError(ctx, s.logger, err)
	}
	if o.OwnerUserID != nil && *o.OwnerUserID == uid {
		return nil
	}
	u, err := s.members.Me(ctx, uid)
	if errors.Is(err, store.ErrNotFound) {
		return connect.NewError(connect.CodePermissionDenied, errors.New(refusal))
	}
	if err != nil {
		return mapError(ctx, s.logger, err)
	}
	if !u.HasRole(store.RoleModerator) {
		return connect.NewError(connect.CodePermissionDenied, errors.New(refusal))
	}
	return nil
}
