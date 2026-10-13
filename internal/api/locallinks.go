package api

import (
	"context"
	"errors"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
	"github.com/gravel-project/gravel/internal/authz"
	"github.com/gravel-project/gravel/internal/identity"
)

// WithBaseURL is the origin password links are built on (hub.yaml auth.base_url); without it a
// link is its path alone.
func (s *IdentityServer) WithBaseURL(base string) *IdentityServer {
	s.baseURL = strings.TrimSuffix(base, "/")
	return s
}

// CreateLocalAccount makes a member with a local account and returns the link that sets its
// password (ADR-0026, gravel#187).
func (s *IdentityServer) CreateLocalAccount(ctx context.Context, req *connect.Request[hubv1.CreateLocalAccountRequest]) (*connect.Response[hubv1.CreateLocalAccountResponse], error) {
	p, err := s.access.Require(ctx, authz.MembersManage, authz.Target{})
	if err != nil {
		return nil, err
	}
	by := p.UserID
	u, link, err := s.ids.CreateLocal(ctx, req.Msg.GetUsername(), req.Msg.GetEmail(), &by)
	if err != nil {
		return nil, s.linkError(ctx, err)
	}
	pu, err := s.user(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&hubv1.CreateLocalAccountResponse{User: pu, Link: s.linkMessage(link)}), nil
}

// IssuePasswordReset returns a link that resets a local account's password.
func (s *IdentityServer) IssuePasswordReset(ctx context.Context, req *connect.Request[hubv1.IssuePasswordResetRequest]) (*connect.Response[hubv1.IssuePasswordResetResponse], error) {
	p, err := s.access.Require(ctx, authz.MembersManage, authz.Target{})
	if err != nil {
		return nil, err
	}
	by := p.UserID
	link, err := s.ids.IssueReset(ctx, req.Msg.GetUsername(), &by)
	if err != nil {
		return nil, s.linkError(ctx, err)
	}
	return connect.NewResponse(&hubv1.IssuePasswordResetResponse{Link: s.linkMessage(link)}), nil
}

func (s *IdentityServer) linkMessage(l identity.PasswordLink) *hubv1.PasswordLink {
	return &hubv1.PasswordLink{Url: s.baseURL + l.Path(), Purpose: l.Purpose, ExpiresAt: timestamppb.New(l.ExpiresAt)}
}

func (s *IdentityServer) linkError(ctx context.Context, err error) error {
	switch {
	case errors.Is(err, identity.ErrLocalDisabled):
		return connect.NewError(connect.CodeFailedPrecondition, errors.New("local accounts are off on this hub (hub.yaml auth.local.enabled)"))
	case errors.Is(err, identity.ErrInvalidUsername), errors.Is(err, identity.ErrInvalidEmail):
		return connect.NewError(connect.CodeInvalidArgument, err)
	case errors.Is(err, identity.ErrIdentityTaken):
		return connect.NewError(connect.CodeAlreadyExists, errors.New("that username is taken"))
	case errors.Is(err, identity.ErrNoLocal):
		return connect.NewError(connect.CodeNotFound, err)
	}
	return mapError(ctx, s.logger, err)
}
