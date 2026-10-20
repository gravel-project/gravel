package api

import (
	"context"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
	"github.com/gravel-project/gravel/internal/stats"
)

// ListGaps is a server's feed gaps (ADR-0011 §5), with the boards' access.
func (s *StatsServer) ListGaps(ctx context.Context, req *connect.Request[hubv1.ListGapsRequest]) (*connect.Response[hubv1.ListGapsResponse], error) {
	if err := s.mayRead(ctx); err != nil {
		return nil, err
	}
	m := req.Msg
	q := stats.GapQuery{ServerID: m.GetServerId()}
	if m.GetFrom() != nil {
		q.From = m.GetFrom().AsTime()
	}
	if m.GetTo() != nil {
		q.To = m.GetTo().AsTime()
	}
	g, err := s.boards.Gaps(ctx, q)
	if err != nil {
		return nil, s.mapError(ctx, err)
	}
	return connect.NewResponse(GapsToProto(g)), nil
}

// GapsToProto is a gap list as the API answers it; the pages render the same message.
func GapsToProto(g stats.Gaps) *hubv1.ListGapsResponse {
	out := &hubv1.ListGapsResponse{From: timestamppb.New(g.From), To: timestamppb.New(g.To), More: g.More}
	for _, x := range g.Gaps {
		out.Gaps = append(out.Gaps, &hubv1.Gap{Reason: x.Reason, From: timestamppb.New(x.From), To: timestamppb.New(x.To), Events: int32(x.Events), //nolint:gosec // bounded by the server's counter
			MatchId: x.MatchID, Kills: int32(x.Kills), Deaths: int32(x.Deaths), Players: int32(x.Players)}) //nolint:gosec // bounded by a match
	}
	return out
}
