package api

import (
	"context"
	"errors"
	"log/slog"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	"github.com/gravel-project/gravel/internal/httpx"
	"github.com/gravel-project/gravel/internal/identity"
	"github.com/gravel-project/gravel/internal/org"
	"github.com/gravel-project/gravel/internal/session"
	"github.com/gravel-project/gravel/internal/store"
)

// caller returns the logged-in user the session middleware found, or CodeUnauthenticated.
func caller(ctx context.Context) (uuid.UUID, error) {
	s, ok := session.FromContext(ctx)
	if !ok {
		return uuid.Nil, connect.NewError(connect.CodeUnauthenticated, errors.New("log in first"))
	}
	return s.UserID, nil
}

// requireOwner refuses anyone but the organization's owner: CodeUnauthenticated without a
// session, CodePermissionDenied for a member.
func requireOwner(ctx context.Context, orgSvc *org.Service, logger *slog.Logger) error {
	uid, err := caller(ctx)
	if err != nil {
		return err
	}
	o, err := orgSvc.Get(ctx)
	if err != nil {
		return mapError(ctx, logger, err)
	}
	if o.OwnerUserID == nil || *o.OwnerUserID != uid {
		return connect.NewError(connect.CodePermissionDenied, errors.New("the owner only"))
	}
	return nil
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
