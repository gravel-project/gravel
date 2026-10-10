package api

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"connectrpc.com/connect"

	"github.com/gravel-project/gravel/drivers"
	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
	"github.com/gravel-project/gravel/gen/gravel/hub/v1/hubv1connect"
	"github.com/gravel-project/gravel/internal/apps"
	"github.com/gravel-project/gravel/internal/httpx"
	"github.com/gravel-project/gravel/internal/org"
	"github.com/gravel-project/gravel/internal/servers"
)

// ConfigServer serves gravel.hub.v1.ServerConfigService: the owner, or an app with
// servers:configure. It shares moderation's error mapping and audit log.
type ConfigServer struct {
	hubv1connect.UnimplementedServerConfigServiceHandler
	mod    *servers.Moderation
	org    *org.Service
	errs   *ModerationServer // the error mapping
	logger *slog.Logger
}

// NewConfigServer wires the service.
func NewConfigServer(mod *servers.Moderation, orgSvc *org.Service, logger *slog.Logger) *ConfigServer {
	return &ConfigServer{mod: mod, org: orgSvc, errs: &ModerationServer{logger: logger}, logger: logger}
}

func (s *ConfigServer) check(ctx context.Context, server string) error {
	if err := requireScope(ctx, s.org, s.logger, apps.ScopeServersConfigure); err != nil {
		return err
	}
	if strings.TrimSpace(server) == "" {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("server_id is required"))
	}
	return nil
}

// GetServerConfig is the current document, redacted.
func (s *ConfigServer) GetServerConfig(ctx context.Context, req *connect.Request[hubv1.GetServerConfigRequest]) (*connect.Response[hubv1.GetServerConfigResponse], error) {
	if err := s.check(ctx, req.Msg.GetServerId()); err != nil {
		return nil, err
	}
	doc, err := s.mod.Config(ctx, req.Msg.GetServerId())
	if err != nil {
		return nil, s.errs.mapError(ctx, err)
	}
	out := &hubv1.GetServerConfigResponse{Revision: doc.Revision, Text: doc.Text, Writable: doc.Writable, Warnings: doc.Warnings}
	for _, sec := range doc.Sections {
		out.Sections = append(out.Sections, &hubv1.ConfigSection{Name: sec.Name, AppliesWhen: sec.AppliesWhen, Keys: sec.Keys, Locked: sec.Locked})
	}
	return connect.NewResponse(out), nil
}

// PlanServerConfig shows what applying a document would change.
func (s *ConfigServer) PlanServerConfig(ctx context.Context, req *connect.Request[hubv1.PlanServerConfigRequest]) (*connect.Response[hubv1.PlanServerConfigResponse], error) {
	if err := s.check(ctx, req.Msg.GetServerId()); err != nil {
		return nil, err
	}
	plan, err := s.mod.PlanConfig(ctx, req.Msg.GetServerId(), req.Msg.GetText())
	if err != nil {
		return nil, s.errs.mapError(ctx, err)
	}
	out := &hubv1.PlanServerConfigResponse{Revision: plan.Revision, Result: configResult(plan.Result)}
	for _, c := range plan.Changes {
		out.Changes = append(out.Changes, &hubv1.ConfigChange{Section: c.Section, Key: c.Key, Before: c.Before, After: c.After})
	}
	return connect.NewResponse(out), nil
}

// ApplyServerConfig writes a planned document.
func (s *ConfigServer) ApplyServerConfig(ctx context.Context, req *connect.Request[hubv1.ApplyServerConfigRequest]) (*connect.Response[hubv1.ApplyServerConfigResponse], error) {
	m := req.Msg
	if err := s.check(ctx, m.GetServerId()); err != nil {
		return nil, err
	}
	actor := servers.Actor{RequestID: httpx.RequestIDFromContext(ctx)}
	if app, ok := apps.FromContext(ctx); ok {
		actor.AppID = &app.ID
	} else {
		uid, err := caller(ctx)
		if err != nil {
			return nil, err
		}
		actor.UserID = &uid
	}
	if o := m.GetOnBehalfOf(); o != nil {
		actor.OnBehalfOf = &drivers.Identity{Provider: o.GetProvider(), Subject: o.GetSubject()}
	}
	id, res, err := s.mod.ApplyConfig(ctx, actor, m.GetServerId(), m.GetText(), m.GetRevision(), m.GetReason())
	if err != nil {
		return nil, s.errs.mapError(ctx, err)
	}
	return connect.NewResponse(&hubv1.ApplyServerConfigResponse{AuditId: id, Result: configResult(res)}), nil
}

func configResult(r drivers.ConfigResult) *hubv1.ConfigResult {
	out := &hubv1.ConfigResult{Ok: r.OK, Revision: r.Revision, Changed: r.Changed, Warnings: r.Warnings}
	for _, p := range r.Problems {
		out.Problems = append(out.Problems, &hubv1.ConfigProblem{Section: p.Section, Key: p.Key, Code: p.Code, Message: p.Message})
	}
	for _, o := range r.Outcomes {
		out.Outcomes = append(out.Outcomes, &hubv1.ConfigOutcome{Section: o.Section, State: o.State, Detail: o.Detail})
	}
	for _, x := range r.Shadowed {
		out.Shadowed = append(out.Shadowed, &hubv1.ConfigShadowed{Section: x.Section, Key: x.Key, Declared: x.Declared, Effective: x.Effective, Branch: x.Branch})
	}
	for _, x := range r.Stripped {
		out.Stripped = append(out.Stripped, &hubv1.ConfigStripped{Section: x.Section, Key: x.Key, Reason: x.Reason})
	}
	return out
}
