package api

import (
	"context"
	"encoding/base64"
	"errors"
	"log/slog"
	"strconv"
	"strings"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"

	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
	"github.com/gravel-project/gravel/gen/gravel/hub/v1/hubv1connect"
	"github.com/gravel-project/gravel/internal/authz"
	"github.com/gravel-project/gravel/internal/identity"
	"github.com/gravel-project/gravel/internal/results"
	"github.com/gravel-project/gravel/internal/stats"
	"github.com/gravel-project/gravel/internal/store"
)

// StatsServer is StatsService (ADR-0012): boards and match lists, public when the Organization
// settings say so and otherwise the owner's or an app's with stats:read; a member's own choice
// to be shown by name; and participation from first-party apps (ADR-0014), recorded by apps with
// participation:write and read like the boards.
type StatsServer struct {
	hubv1connect.UnimplementedStatsServiceHandler
	boards        *stats.Boards
	participation *stats.Participation
	access        *Access
	members       MemberReader
	logger        *slog.Logger
	results       *results.Service // match results (ADR-0024); nil: none
	erase         Eraser           // EraseIdentity (#195); nil: none
}

// MemberReader reads a member with their linked identities (identity.Service).
type MemberReader interface {
	Me(ctx context.Context, userID uuid.UUID) (identity.User, error)
}

// NewStatsServer builds the handler.
func NewStatsServer(boards *stats.Boards, participation *stats.Participation, access *Access, members MemberReader, logger *slog.Logger) *StatsServer {
	return &StatsServer{boards: boards, participation: participation, access: access, members: members, logger: logger}
}

// mayRead admits anyone when stats are public, else the owner or an app with stats:read.
func (s *StatsServer) mayRead(ctx context.Context) error {
	public, err := s.boards.Public(ctx)
	if err != nil {
		return mapError(ctx, s.logger, err)
	}
	if public {
		return nil
	}
	_, err = s.access.Require(ctx, authz.StatsRead, authz.Target{})
	return err
}

func (s *StatsServer) mapError(ctx context.Context, err error) error {
	if errors.Is(err, stats.ErrInvalidQuery) {
		return connect.NewError(connect.CodeInvalidArgument, errors.New(strings.TrimPrefix(err.Error(), stats.ErrInvalidQuery.Error()+": ")))
	}
	return mapError(ctx, s.logger, err)
}

// GetBoard ranks the players of a server, a game or the organization over a window.
func (s *StatsServer) GetBoard(ctx context.Context, req *connect.Request[hubv1.GetBoardRequest]) (*connect.Response[hubv1.GetBoardResponse], error) {
	if err := s.mayRead(ctx); err != nil {
		return nil, err
	}
	m := req.Msg
	if m.GetPageSize() < 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("page_size must not be negative"))
	}
	offset, err := decodeOffsetToken(m.GetPageToken())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	b, err := s.boards.Board(ctx, stats.Query{ServerID: m.GetServerId(), GameID: m.GetGameId(), Window: m.GetWindow(), Season: m.GetSeason(),
		Metric: m.GetMetric(), PageSize: int(m.GetPageSize()), Offset: offset})
	if err != nil {
		return nil, s.mapError(ctx, err)
	}
	resp := &hubv1.GetBoardResponse{}
	for _, e := range b.Entries {
		pe := &hubv1.BoardEntry{Rank: int32(e.Rank), Name: e.Name, Pseudonymous: e.Pseudonymous, Kills: int32(e.Kills), Deaths: int32(e.Deaths), //nolint:gosec // bounded by the store
			Kd: e.KD, SecondsOn: int32(e.SecondsOn), Matches: int32(e.Matches)} //nolint:gosec // bounded by the store
		if e.UserID != uuid.Nil {
			pe.UserId = e.UserID.String()
		}
		resp.Entries = append(resp.Entries, pe)
	}
	if b.More {
		resp.NextPageToken = encodeOffsetToken(offset + len(b.Entries))
	}
	if !b.From.IsZero() {
		resp.From, resp.To = timestamppb.New(b.From), timestamppb.New(b.To)
	}
	return connect.NewResponse(resp), nil
}

// ListMatches is a server's matches, newest first.
func (s *StatsServer) ListMatches(ctx context.Context, req *connect.Request[hubv1.ListMatchesRequest]) (*connect.Response[hubv1.ListMatchesResponse], error) {
	if err := s.mayRead(ctx); err != nil {
		return nil, err
	}
	m := req.Msg
	if m.GetPageSize() < 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("page_size must not be negative"))
	}
	before, err := decodeMatchToken(m.GetPageToken())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	ms, more, err := s.boards.Matches(ctx, m.GetServerId(), before, int(m.GetPageSize()))
	if err != nil {
		return nil, s.mapError(ctx, err)
	}
	resp := &hubv1.ListMatchesResponse{}
	for _, x := range ms {
		pm := &hubv1.MatchSummary{Id: strconv.FormatInt(x.ID, 10), ServerId: x.ServerID, GameId: x.GameID, StartedAt: timestamppb.New(x.StartedAt),
			Map: x.Map, Players: int32(x.Players), Kills: int32(x.Kills)} //nolint:gosec // bounded by the store
		if x.EndedAt != nil {
			pm.EndedAt = timestamppb.New(*x.EndedAt)
		}
		resp.Matches = append(resp.Matches, pm)
	}
	if more && len(ms) > 0 {
		resp.NextPageToken = encodeMatchToken(ms[len(ms)-1].ID)
	}
	return connect.NewResponse(resp), nil
}

// GetBoardName is the logged-in member's choice.
func (s *StatsServer) GetBoardName(ctx context.Context, _ *connect.Request[hubv1.GetBoardNameRequest]) (*connect.Response[hubv1.GetBoardNameResponse], error) {
	uid, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	show, err := s.boards.ShowName(ctx, uid)
	if err != nil {
		return nil, mapError(ctx, s.logger, err)
	}
	return connect.NewResponse(&hubv1.GetBoardNameResponse{ShowName: show}), nil
}

// SetBoardName changes the logged-in member's choice; an app cannot make it for them.
func (s *StatsServer) SetBoardName(ctx context.Context, req *connect.Request[hubv1.SetBoardNameRequest]) (*connect.Response[hubv1.SetBoardNameResponse], error) {
	uid, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.boards.SetShowName(ctx, uid, req.Msg.GetShowName()); err != nil {
		return nil, mapError(ctx, s.logger, err)
	}
	return connect.NewResponse(&hubv1.SetBoardNameResponse{ShowName: req.Msg.GetShowName()}), nil
}

// A board page token is the base64 of "board|<offset>"; a match page token "matches|<id>".
// Nothing a caller should build.
func encodeOffsetToken(offset int) string {
	return base64.RawURLEncoding.EncodeToString([]byte("board|" + strconv.Itoa(offset)))
}

func decodeOffsetToken(token string) (int, error) {
	if token == "" {
		return 0, nil
	}
	n, err := decodePrefixed(token, "board|")
	return int(n), err
}

func encodeMatchToken(id int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte("matches|" + strconv.FormatInt(id, 10)))
}

func decodeMatchToken(token string) (int64, error) {
	if token == "" {
		return 0, nil
	}
	return decodePrefixed(token, "matches|")
}

func decodePrefixed(token, prefix string) (int64, error) {
	bad := errors.New("page_token is not one this hub issued")
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return 0, bad
	}
	rest, ok := strings.CutPrefix(string(raw), prefix)
	if !ok {
		return 0, bad
	}
	n, err := strconv.ParseInt(rest, 10, 64)
	if err != nil || n < 0 || n > 1<<31 {
		return 0, bad
	}
	return n, nil
}

// GetMemberProfile is a member's stats across their linked identities, for whoever may see them.
func (s *StatsServer) GetMemberProfile(ctx context.Context, req *connect.Request[hubv1.GetMemberProfileRequest]) (*connect.Response[hubv1.GetMemberProfileResponse], error) {
	var self uuid.UUID
	if uid, err := caller(ctx); err == nil {
		self = uid
	}
	uid := self
	if req.Msg.GetUserId() != "" {
		id, err := uuid.Parse(req.Msg.GetUserId())
		if err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("user_id is not a member id"))
		}
		uid = id
	}
	if uid == uuid.Nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("log in, or name a member"))
	}
	notFound := connect.NewError(connect.CodeNotFound, errors.New("no such profile"))
	u, err := s.members.Me(ctx, uid)
	if errors.Is(err, store.ErrNotFound) {
		return nil, notFound
	}
	if err != nil {
		return nil, mapError(ctx, s.logger, err)
	}
	shows, err := s.boards.ShowName(ctx, uid)
	if err != nil {
		return nil, mapError(ctx, s.logger, err)
	}
	if err := s.mayReadProfile(ctx, uid == self, shows); err != nil {
		return nil, notFound // never say whether the member exists, or plays under a pseudonym
	}
	keys := make([]store.PlayerKey, 0, len(u.Identities))
	for _, i := range u.Identities {
		keys = append(keys, store.PlayerKey{Provider: i.Provider, Subject: i.Subject})
	}
	p, err := s.boards.Profile(ctx, keys)
	if err != nil {
		return nil, s.mapError(ctx, err)
	}
	resp := &hubv1.GetMemberProfileResponse{UserId: uid.String(), DisplayName: u.DisplayName, ShowName: shows, Self: uid == self}
	for _, t := range p.Totals {
		pt := &hubv1.ProfileTotals{Window: t.Window, Season: t.Season, Kills: int32(t.Totals.Kills), Deaths: int32(t.Totals.Deaths), //nolint:gosec // bounded by the store
			Kd: t.KD, SecondsOn: int32(t.Totals.SecondsOn), Matches: int32(t.Totals.Matches)} //nolint:gosec // bounded by the store
		if !t.From.IsZero() {
			pt.From, pt.To = timestamppb.New(t.From), timestamppb.New(t.To)
		}
		resp.Totals = append(resp.Totals, pt)
		for _, k := range t.Participation {
			pp := &hubv1.ParticipationTotals{Window: t.Window, Season: t.Season, Kind: k.Kind, Events: int32(k.Events), Seconds: int32(k.Seconds)} //nolint:gosec // clamped by stats
			pp.From, pp.To = pt.GetFrom(), pt.GetTo()
			resp.Participation = append(resp.Participation, pp)
		}
	}
	for _, m := range p.Recent {
		pm := &hubv1.PlayerMatch{MatchId: strconv.FormatInt(m.ID, 10), ServerId: m.ServerID, ServerName: m.ServerName, StartedAt: timestamppb.New(m.StartedAt),
			Map: m.Map, Kills: int32(m.Kills), Deaths: int32(m.Deaths), SecondsOn: int32(m.SecondsOn)} //nolint:gosec // bounded by the store
		if m.EndedAt != nil {
			pm.EndedAt = timestamppb.New(*m.EndedAt)
		}
		resp.Recent = append(resp.Recent, pm)
	}
	return connect.NewResponse(resp), nil
}

// mayReadProfile admits the member, the owner, an app with stats:read, or anyone when the member
// shows their name and stats are public.
func (s *StatsServer) mayReadProfile(ctx context.Context, isSelf, showsName bool) error {
	if isSelf {
		return nil
	}
	if showsName {
		public, err := s.boards.Public(ctx)
		if err != nil {
			return err
		}
		if public {
			return nil
		}
	}
	_, err := s.access.Require(ctx, authz.StatsRead, authz.Target{})
	return err
}

// RecordParticipation stores participation events for the calling app (ADR-0014). Only an app
// with participation:write records them: a member's session, the owner's included, is refused,
// since an event says what an app saw, not what a person claims.
func (s *StatsServer) RecordParticipation(ctx context.Context, req *connect.Request[hubv1.RecordParticipationRequest]) (*connect.Response[hubv1.RecordParticipationResponse], error) {
	a, err := s.access.Require(ctx, authz.ParticipationWrite, authz.Target{}) // an app's alone (Spec.AppOnly)
	if err != nil {
		return nil, err
	}
	events := make([]stats.ParticipationEvent, 0, len(req.Msg.GetEvents()))
	for _, e := range req.Msg.GetEvents() {
		ev := stats.ParticipationEvent{Key: e.GetKey(), Kind: e.GetKind(), GameID: e.GetGameId(), ServerID: e.GetServerId(), Session: e.GetSession(),
			DurationSeconds: int(e.GetDurationSeconds()), SchemaVersion: int(e.GetSchemaVersion())}
		if id := e.GetIdentity(); id != nil {
			ev.Identity = &store.PlayerKey{Provider: id.GetProvider(), Subject: id.GetSubject()}
		}
		if at := e.GetOccurredAt(); at != nil && at.IsValid() {
			ev.OccurredAt = at.AsTime()
		}
		events = append(events, ev)
	}
	results, err := s.participation.Record(ctx, a.AppID, events)
	if errors.Is(err, stats.ErrInvalidParticipation) {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New(strings.TrimPrefix(err.Error(), stats.ErrInvalidParticipation.Error()+": ")))
	}
	if err != nil {
		return nil, mapError(ctx, s.logger, err)
	}
	resp := &hubv1.RecordParticipationResponse{}
	for _, r := range results {
		resp.Results = append(resp.Results, &hubv1.ParticipationResult{Key: r.Key, Outcome: r.Outcome})
	}
	return connect.NewResponse(resp), nil
}

// GetParticipationBoard ranks the players who took part in one kind over a window, with the same
// access as GetBoard.
func (s *StatsServer) GetParticipationBoard(ctx context.Context, req *connect.Request[hubv1.GetParticipationBoardRequest]) (*connect.Response[hubv1.GetParticipationBoardResponse], error) {
	if err := s.mayRead(ctx); err != nil {
		return nil, err
	}
	m := req.Msg
	if m.GetPageSize() < 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("page_size must not be negative"))
	}
	offset, err := decodeOffsetToken(m.GetPageToken())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	b, err := s.boards.ParticipationBoard(ctx, stats.ParticipationQuery{Kind: m.GetKind(), GameID: m.GetGameId(), Window: m.GetWindow(), Season: m.GetSeason(),
		PageSize: int(m.GetPageSize()), Offset: offset})
	if err != nil {
		return nil, s.mapError(ctx, err)
	}
	resp := &hubv1.GetParticipationBoardResponse{Kind: b.Kind, Kinds: b.Kinds}
	for _, e := range b.Entries {
		pe := &hubv1.ParticipationEntry{Rank: int32(e.Rank), Name: e.Name, Pseudonymous: e.Pseudonymous, Events: int32(e.Events), Seconds: int32(e.Seconds)} //nolint:gosec // clamped by stats
		if e.UserID != uuid.Nil {
			pe.UserId = e.UserID.String()
		}
		resp.Entries = append(resp.Entries, pe)
	}
	if b.More {
		resp.NextPageToken = encodeOffsetToken(offset + len(b.Entries))
	}
	if !b.From.IsZero() {
		resp.From, resp.To = timestamppb.New(b.From), timestamppb.New(b.To)
	}
	return connect.NewResponse(resp), nil
}

// GetActivity is when people play and who stopped (ADR-0035): stats:read held everywhere, in
// person. A game-scoped grant isn't enough: the activity is the whole hub's.
func (s *StatsServer) GetActivity(ctx context.Context, req *connect.Request[hubv1.GetActivityRequest]) (*connect.Response[hubv1.GetActivityResponse], error) {
	if _, err := s.access.Require(ctx, authz.StatsRead, authz.Target{}); err != nil {
		return nil, err
	}
	if _, err := sessionCaller(ctx); err != nil {
		return nil, err
	}
	a, err := s.boards.Activity(ctx, req.Msg.GetWindow())
	if err != nil {
		return nil, s.mapError(ctx, err)
	}
	out := &hubv1.GetActivityResponse{Window: a.Window, From: timestamppb.New(a.From), To: timestamppb.New(a.To), Timezone: a.Timezone,
		Sessions: int32(a.Sessions), Players: int32(a.Players), MedianSessionSeconds: int32(a.MedianSession.Seconds()), //nolint:gosec // counts
		ReturningPlayers: int32(a.Returning), MeanActiveDays: a.MeanDays, NewPlayers: int32(a.New), LostPlayers: int32(a.Lost), //nolint:gosec // counts
		RegularDays: int32(a.RegularDays), RegularWeeks: int32(a.RegularWeeks), Partial: a.Rolled} //nolint:gosec // small constants
	for d := range a.Hours {
		for h, c := range a.Hours[d] {
			out.Hours = append(out.Hours, &hubv1.ActivityHour{Day: int32(d + 1), Hour: int32(h), Average: c.Average, Peak: int32(c.Peak)}) //nolint:gosec // small counts
		}
	}
	for _, w := range a.Weeks {
		out.Weeks = append(out.Weeks, &hubv1.ActivityWeek{Start: timestamppb.New(w.Start), Players: int32(w.Players), NewPlayers: int32(w.New)}) //nolint:gosec // counts
	}
	for _, q := range a.Quiet {
		m := &hubv1.QuietRegular{Name: q.Name, Pseudonymous: q.Pseudonymous, DaysBefore: int32(q.DaysBefore), LastSeen: timestamppb.New(q.LastSeen), //nolint:gosec // counts
			LastServerId: q.LastServer, Hours: q.Hours}
		if q.UserID != uuid.Nil {
			m.UserId = q.UserID.String()
		}
		out.Quiet = append(out.Quiet, m)
	}
	return connect.NewResponse(out), nil
}
