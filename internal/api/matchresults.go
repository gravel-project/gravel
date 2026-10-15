package api

import (
	"context"
	"errors"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"

	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
	"github.com/gravel-project/gravel/internal/results"
	"github.com/gravel-project/gravel/internal/store"
)

// Match results from a server's match plugin (ADR-0024), read like the boards.

// SetResults gives the service the match results; without them the procedures are unimplemented.
func (s *StatsServer) SetResults(r *results.Service) { s.results = r }

// ListMatchResults is the series, newest first.
func (s *StatsServer) ListMatchResults(ctx context.Context, req *connect.Request[hubv1.ListMatchResultsRequest]) (*connect.Response[hubv1.ListMatchResultsResponse], error) {
	if err := s.mayReadResults(ctx); err != nil {
		return nil, err
	}
	m := req.Msg
	f := store.MatchResultFilter{ServerID: m.GetServerId(), EndedOnly: m.GetEndedOnly(), Limit: min(int(m.GetPageSize()), 200)}
	if m.GetPageSize() < 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("page_size must not be negative"))
	}
	if id := m.GetLeaseId(); id != "" {
		u, err := uuid.Parse(id)
		if err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("lease_id is not a lease's id"))
		}
		f.LeaseID = &u
	}
	if t := m.GetEndedAfter(); t != nil {
		at := t.AsTime()
		f.EndedAfter = &at
	}
	rs, err := s.results.List(ctx, f)
	if err != nil {
		return nil, mapError(ctx, s.logger, err)
	}
	out := &hubv1.ListMatchResultsResponse{}
	for _, r := range rs {
		out.Results = append(out.Results, resultMessage(r))
	}
	return connect.NewResponse(out), nil
}

// GetMatchResult is a series with its maps and players.
func (s *StatsServer) GetMatchResult(ctx context.Context, req *connect.Request[hubv1.GetMatchResultRequest]) (*connect.Response[hubv1.GetMatchResultResponse], error) {
	if err := s.mayReadResults(ctx); err != nil {
		return nil, err
	}
	id, err := uuid.Parse(req.Msg.GetId())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("id is not a result's id"))
	}
	r, err := s.results.Get(ctx, id)
	if err != nil {
		return nil, mapError(ctx, s.logger, err)
	}
	m := resultMessage(r)
	for _, mp := range r.Maps {
		m.Maps = append(m.Maps, &hubv1.MatchResultMap{MapNumber: int32(mp.MapNumber), MapName: mp.MapName, Team1Score: int32(mp.Team1Score), //nolint:gosec // small counts
			Team2Score: int32(mp.Team2Score), Winner: mp.Winner, StartedAt: ts(mp.StartedAt), EndedAt: ts(mp.EndedAt), HasDemo: mp.DemoKey != ""}) //nolint:gosec // small counts
	}
	keys := make([]store.PlayerKey, len(r.Players))
	for i, p := range r.Players {
		keys[i] = store.PlayerKey{Provider: p.Provider, Subject: p.Subject}
	}
	shown, err := s.boards.ShowAs(ctx, keys)
	if err != nil {
		return nil, mapError(ctx, s.logger, err)
	}
	exts, err := s.boards.ServerExtensions(ctx, r.ServerID)
	if err != nil {
		return nil, mapError(ctx, s.logger, err)
	}
	for i, p := range r.Players {
		mp := &hubv1.MatchResultPlayer{MapNumber: int32(p.MapNumber), Name: shown[i].Name, Pseudonymous: shown[i].Pseudonymous, Team: p.Team, //nolint:gosec // small counts
			Kills: int32(p.Kills), Deaths: int32(p.Deaths)} //nolint:gosec // game counters
		// The game's declared stats only (ADR-0033): what a row holds beyond them stays unshown.
		for _, e := range exts {
			if v, ok := p.Extensions[e.Name]; ok {
				if mp.Extensions == nil {
					mp.Extensions = map[string]float64{}
				}
				mp.Extensions[e.Name] = v
			}
		}
		if shown[i].UserID != uuid.Nil {
			mp.UserId = shown[i].UserID.String()
		}
		m.Players = append(m.Players, mp)
	}
	return connect.NewResponse(&hubv1.GetMatchResultResponse{Result: m, StatExtensions: statExtensionMessages(exts)}), nil
}

// GetMatchDemo is a link to a map's demo.
func (s *StatsServer) GetMatchDemo(ctx context.Context, req *connect.Request[hubv1.GetMatchDemoRequest]) (*connect.Response[hubv1.GetMatchDemoResponse], error) {
	if err := s.mayReadResults(ctx); err != nil {
		return nil, err
	}
	id, err := uuid.Parse(req.Msg.GetId())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("id is not a result's id"))
	}
	url, err := s.results.DemoURL(ctx, id, int(req.Msg.GetMapNumber()))
	if errors.Is(err, results.ErrNoDemo) {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("the map has no demo"))
	}
	if err != nil {
		return nil, mapError(ctx, s.logger, err)
	}
	return connect.NewResponse(&hubv1.GetMatchDemoResponse{Url: url, ExpiresAt: timestamppb.New(time.Now().Add(results.DemoTTL))}), nil
}

func (s *StatsServer) mayReadResults(ctx context.Context) error {
	if s.results == nil {
		return connect.NewError(connect.CodeUnimplemented, errors.New("this hub keeps no match results"))
	}
	return s.mayRead(ctx)
}

func resultMessage(r store.MatchResult) *hubv1.MatchResult {
	m := &hubv1.MatchResult{Id: r.ID.String(), ServerId: r.ServerID, Team1Name: r.Team1Name, Team2Name: r.Team2Name, NumMaps: int32(r.NumMaps), //nolint:gosec // small counts
		Team1Score: int32(r.Team1Score), Team2Score: int32(r.Team2Score), Winner: r.Winner, StartedAt: timestamppb.New(r.StartedAt), EndedAt: ts(r.EndedAt)} //nolint:gosec // small counts
	if r.LeaseID != nil {
		m.LeaseId = r.LeaseID.String()
	}
	return m
}

func ts(t *time.Time) *timestamppb.Timestamp {
	if t == nil {
		return nil
	}
	return timestamppb.New(*t)
}
