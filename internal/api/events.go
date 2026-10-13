package api

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"

	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
	"github.com/gravel-project/gravel/internal/authz"
	"github.com/gravel-project/gravel/internal/events"
	"github.com/gravel-project/gravel/internal/store"
)

// EventReader reads the events log (store.Store).
type EventReader interface {
	ListEvents(ctx context.Context, orgID uuid.UUID, q store.EventQuery) ([]store.Event, int64, error)
}

// WithEvents lets the service read the events log (ADR-0028).
func (s *OrganizationServer) WithEvents(r EventReader) *OrganizationServer {
	s.events = r
	return s
}

// ListEvents reads the events log by cursor; a caller holding events:read for some games sees
// those games' events only.
func (s *OrganizationServer) ListEvents(ctx context.Context, req *connect.Request[hubv1.ListEventsRequest]) (*connect.Response[hubv1.ListEventsResponse], error) {
	p, err := s.access.RequireAny(ctx, authz.EventsRead)
	if err != nil {
		return nil, err
	}
	if s.events == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, errors.New("this hub keeps no events log"))
	}
	m := req.Msg
	if m.GetAfterId() < 0 || m.GetBeforeId() < 0 || m.GetLimit() < 0 || m.GetLimit() > 500 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("after_id and before_id are ids, limit at most 500"))
	}
	if sev := m.GetMinSeverity(); sev != "" && events.Rank(events.Severity(sev)) < 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("min_severity %q is not info, notice, warning or critical", sev))
	}
	q := store.EventQuery{AfterID: m.GetAfterId(), BeforeID: m.GetBeforeId(), Limit: int(m.GetLimit()), Kinds: m.GetKinds(),
		MinSeverity: events.Severity(m.GetMinSeverity()), ServerID: strings.TrimSpace(m.GetServerId())}
	if all, games := p.Scope(authz.EventsRead); !all {
		q.Games = append([]string{}, games...) // not nil: only these games' events
	}
	o, err := s.org.Get(ctx)
	if err != nil {
		return nil, mapError(ctx, s.logger, err)
	}
	evs, head, err := s.events.ListEvents(ctx, o.ID, q)
	if err != nil {
		return nil, mapError(ctx, s.logger, err)
	}
	limit := q.Limit
	if limit == 0 {
		limit = 100
	}
	out := &hubv1.ListEventsResponse{HeadId: head, NextAfterId: m.GetAfterId()}
	for _, e := range evs {
		out.Events = append(out.Events, eventMessage(e))
		if e.ID > out.NextAfterId {
			out.NextAfterId = e.ID
		}
	}
	if m.GetAfterId() == 0 && len(evs) == limit {
		out.NextBeforeId = evs[len(evs)-1].ID
	}
	return connect.NewResponse(out), nil
}

func eventMessage(e store.Event) *hubv1.Event {
	p := &hubv1.Event{Id: e.ID, At: timestamppb.New(e.At), Kind: e.Kind, Severity: e.Severity, OnBehalfProvider: e.OnBehalfProvider,
		OnBehalfSubject: e.OnBehalfSubject, ServerId: e.ServerID, GameId: e.GameID, Summary: e.Summary, RequestId: e.RequestID}
	if e.ActorUserID != nil {
		p.ActorUserId = e.ActorUserID.String()
	}
	if e.ActorAppID != nil {
		p.ActorAppId = e.ActorAppID.String()
	}
	if e.SubjectUserID != nil {
		p.SubjectUserId = e.SubjectUserID.String()
	}
	if len(e.Detail) > 0 {
		p.Detail = map[string]string{}
		for k, v := range e.Detail {
			p.Detail[k] = fmt.Sprint(v)
		}
	}
	return p
}
