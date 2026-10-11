package api

import (
	"context"
	"strconv"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"

	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
	"github.com/gravel-project/gravel/internal/stats"
)

// GetSessionRecap is what one server's window held (ADR-0018), with the boards' access.
func (s *StatsServer) GetSessionRecap(ctx context.Context, req *connect.Request[hubv1.GetSessionRecapRequest]) (*connect.Response[hubv1.GetSessionRecapResponse], error) {
	if err := s.mayRead(ctx); err != nil {
		return nil, err
	}
	m := req.Msg
	q := stats.RecapQuery{ServerID: m.GetServerId(), Floor: int(m.GetFloor())}
	if m.GetFrom() != nil {
		q.From = m.GetFrom().AsTime()
	}
	if m.GetTo() != nil {
		q.To = m.GetTo().AsTime()
	}
	r, err := s.boards.Recap(ctx, q)
	if err != nil {
		return nil, s.mapError(ctx, err)
	}
	return connect.NewResponse(RecapToProto(r)), nil
}

// RecapToProto is a recap as the API answers it; the pages render the same message.
func RecapToProto(r stats.Recap) *hubv1.GetSessionRecapResponse {
	out := &hubv1.GetSessionRecapResponse{From: timestamppb.New(r.From), To: timestamppb.New(r.To), Floor: int32(r.Floor), //nolint:gosec // bounded by validation
		PeakPlayers: int32(r.PeakPlayers), Minutes: int32(r.Minutes), MinutesAtFloor: int32(r.AtFloor), Players: int32(r.Players)} //nolint:gosec // bounded by the store
	for _, m := range r.Matches {
		pm := &hubv1.RecapMatch{Id: strconv.FormatInt(m.ID, 10), StartedAt: timestamppb.New(m.StartedAt), Map: m.Map, Players: int32(m.Players)} //nolint:gosec // bounded by the store
		if m.EndedAt != nil {
			pm.EndedAt = timestamppb.New(*m.EndedAt)
		}
		for _, t := range m.Teams {
			pm.Teams = append(pm.Teams, &hubv1.TeamScore{Name: t.Name, Color: t.Color, Score: t.Score})
		}
		out.Matches = append(out.Matches, pm)
	}
	for _, e := range r.Top {
		pe := &hubv1.BoardEntry{Rank: int32(e.Rank), Name: e.Name, Pseudonymous: e.Pseudonymous, Kills: int32(e.Kills), Deaths: int32(e.Deaths), //nolint:gosec // bounded by the store
			Kd: e.KD, SecondsOn: int32(e.SecondsOn), Matches: int32(e.Matches)} //nolint:gosec // bounded by the store
		if e.UserID != uuid.Nil {
			pe.UserId = e.UserID.String()
		}
		out.Top = append(out.Top, pe)
	}
	return out
}
