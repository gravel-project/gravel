package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Ownership transfer in two steps (gravel#197, ADR-0027 §6): the owner nominates a member, the
// member accepts. Each step is one transaction with its event; accepting also makes the old owner
// an admin.

// Errors of the transfer.
var (
	// ErrNotOwner is a nomination or a cancel by someone who is neither the owner nor, for a
	// cancel, the nominee.
	ErrNotOwner = errors.New("not the owner")
	// ErrSelfNomination is the owner nominating themselves.
	ErrSelfNomination = errors.New("the owner can't nominate themselves")
	// ErrNoNomination is an accept or a cancel with no nomination for the caller, or an accept
	// after it expired.
	ErrNoNomination = errors.New("no nomination")
	// ErrNominationExpired is an accept after the nomination's 7 days.
	ErrNominationExpired = errors.New("the nomination has expired")
)

// ownershipState is the organization's owner and nomination, read under a row lock.
type ownershipState struct {
	owner   *uuid.UUID
	pending *uuid.UUID
	expires *time.Time
}

func (s *Store) lockOwnership(ctx context.Context, q rowQuerier, orgID uuid.UUID) (ownershipState, error) {
	var st ownershipState
	err := q.QueryRow(ctx, `SELECT owner_user_id, pending_owner_user_id, pending_owner_expires_at FROM organizations WHERE id = $1 FOR UPDATE`, orgID).
		Scan(&st.owner, &st.pending, &st.expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return st, ErrNotFound
	}
	if err != nil {
		return st, fmt.Errorf("store: ownership: %w", err)
	}
	return st, nil
}

// NominateOwner makes nominee the pending owner until expires, replacing any earlier nomination,
// when by.UserID is the owner. ErrNotOwner otherwise, ErrSelfNomination for the owner,
// ErrNotFound for a nominee who isn't a member of the organization.
func (s *Store) NominateOwner(ctx context.Context, orgID, nominee uuid.UUID, by Actor, at, expires time.Time) (Organization, error) {
	if by.UserID == nil {
		return Organization{}, ErrNotOwner
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Organization{}, fmt.Errorf("store: nominate owner: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	st, err := s.lockOwnership(ctx, tx, orgID)
	if err != nil {
		return Organization{}, err
	}
	switch {
	case st.owner == nil || *st.owner != *by.UserID:
		return Organization{}, ErrNotOwner
	case nominee == *by.UserID:
		return Organization{}, ErrSelfNomination
	}
	var member bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE id = $1 AND organization_id = $2)`, nominee, orgID).Scan(&member); err != nil {
		return Organization{}, fmt.Errorf("store: nominate owner: %w", err)
	}
	if !member {
		return Organization{}, ErrNotFound
	}
	o, err := scanOrganization(tx.QueryRow(ctx, `
		UPDATE organizations SET pending_owner_user_id = $2, pending_owner_nominated_at = $3, pending_owner_expires_at = $4
		 WHERE id = $1 RETURNING `+organizationColumns, orgID, nominee, at, expires))
	if err != nil {
		return Organization{}, fmt.Errorf("store: nominate owner: %w", err)
	}
	detail := map[string]any{"expires_at": expires.UTC().Format(time.RFC3339)}
	if st.pending != nil {
		detail["replaces_user_id"] = st.pending.String()
	}
	if err := s.appendEvent(ctx, tx, by.event(Event{OrganizationID: orgID, At: at, Kind: "ownership.nominated", SubjectUserID: &nominee,
		Summary: "{user} was nominated to take the hub over", Detail: detail, Source: "organizations.transfer",
		SourceID: fmt.Sprintf("%s:nominated:%d", orgID, at.UnixMicro())})); err != nil {
		return Organization{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Organization{}, fmt.Errorf("store: nominate owner: %w", err)
	}
	return o, nil
}

// CancelOwnershipTransfer ends the nomination: the owner cancels it, the nominee declines it (an
// expired one too). ErrNoNomination when there is none, ErrNotOwner for anyone else.
func (s *Store) CancelOwnershipTransfer(ctx context.Context, orgID uuid.UUID, by Actor, at time.Time) (Organization, error) {
	if by.UserID == nil {
		return Organization{}, ErrNotOwner
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Organization{}, fmt.Errorf("store: cancel ownership transfer: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	st, err := s.lockOwnership(ctx, tx, orgID)
	if err != nil {
		return Organization{}, err
	}
	declined := st.pending != nil && *st.pending == *by.UserID
	switch {
	case st.pending == nil:
		return Organization{}, ErrNoNomination
	case !declined && (st.owner == nil || *st.owner != *by.UserID):
		return Organization{}, ErrNotOwner
	}
	o, err := scanOrganization(tx.QueryRow(ctx, `
		UPDATE organizations SET pending_owner_user_id = NULL, pending_owner_nominated_at = NULL, pending_owner_expires_at = NULL
		 WHERE id = $1 RETURNING `+organizationColumns, orgID))
	if err != nil {
		return Organization{}, fmt.Errorf("store: cancel ownership transfer: %w", err)
	}
	kind, summary := "ownership.transfer_cancelled", "The nomination of {user} to take the hub over was cancelled"
	if declined {
		kind, summary = "ownership.declined", "{user} declined to take the hub over"
	}
	if err := s.appendEvent(ctx, tx, by.event(Event{OrganizationID: orgID, At: at, Kind: kind, SubjectUserID: st.pending, Summary: summary,
		Source: "organizations.transfer", SourceID: fmt.Sprintf("%s:%s:%d", orgID, kind, at.UnixMicro())})); err != nil {
		return Organization{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Organization{}, fmt.Errorf("store: cancel ownership transfer: %w", err)
	}
	return o, nil
}

// AcceptOwnership makes by.UserID the owner when they are the unexpired nominee, and the old owner
// an admin, in one transaction: the owner moves in one guarded UPDATE, so the hub has exactly one
// owner at every moment and a second accept finds no nomination. It returns the organization and
// the old owner. ErrNoNomination when the caller isn't the nominee, ErrNominationExpired after the
// 7 days.
func (s *Store) AcceptOwnership(ctx context.Context, orgID uuid.UUID, by Actor, at time.Time) (Organization, uuid.UUID, error) {
	if by.UserID == nil {
		return Organization{}, uuid.Nil, ErrNoNomination
	}
	me := *by.UserID
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Organization{}, uuid.Nil, fmt.Errorf("store: accept ownership: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	st, err := s.lockOwnership(ctx, tx, orgID)
	if err != nil {
		return Organization{}, uuid.Nil, err
	}
	switch {
	case st.pending == nil || *st.pending != me || st.owner == nil:
		return Organization{}, uuid.Nil, ErrNoNomination
	case !st.expires.After(at):
		return Organization{}, uuid.Nil, ErrNominationExpired
	}
	old := *st.owner
	o, err := scanOrganization(tx.QueryRow(ctx, `
		UPDATE organizations
		   SET owner_user_id = $3, pending_owner_user_id = NULL, pending_owner_nominated_at = NULL, pending_owner_expires_at = NULL
		 WHERE id = $1 AND owner_user_id = $2 AND pending_owner_user_id = $3 AND pending_owner_expires_at > $4
		 RETURNING `+organizationColumns, orgID, old, me, at))
	if errors.Is(err, ErrNotFound) {
		return Organization{}, uuid.Nil, ErrNoNomination
	}
	if err != nil {
		return Organization{}, uuid.Nil, fmt.Errorf("store: accept ownership: %w", err)
	}
	if err := s.appendEvent(ctx, tx, by.event(Event{OrganizationID: orgID, At: at, Kind: "ownership.transferred", SubjectUserID: &me,
		Summary: "{user} accepted the hub and is its owner now; the old owner is an admin", Detail: map[string]any{"from_user_id": old.String()},
		Source: "organizations.transfer", SourceID: fmt.Sprintf("%s:transferred:%d", orgID, at.UnixMicro())})); err != nil {
		return Organization{}, uuid.Nil, err
	}
	// The old owner keeps running things, as an admin: everything but handing the hub over.
	if _, err := s.setRoleGrant(ctx, tx, old, RoleAdmin, "", true, RoleChange{By: &me, At: at}); err != nil {
		return Organization{}, uuid.Nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Organization{}, uuid.Nil, fmt.Errorf("store: accept ownership: %w", err)
	}
	return o, old, nil
}
