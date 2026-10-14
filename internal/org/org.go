// Package org owns the built-in organization and the one-time owner claim.
//
// A fresh hub has no owner and every later action needs one. On each start while it is unowned
// the hub mints a one-time owner-claim token, stores only its hash, and prints the token once to
// its log. ClaimOwnership consumes it: single-use, expiring, and refused forever once the hub is
// owned (ADR-0002). The claim binds to the logged-in user who makes it; that user is the owner.
package org

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/gravel-project/gravel/internal/store"
)

// Errors the API maps to Connect codes.
var (
	ErrAlreadyOwned = errors.New("the organization is already owned")
	ErrInvalidToken = errors.New("the owner-claim token is invalid or has expired")

	// The ownership transfer's (#197).
	ErrNotOwner          = errors.New("only the owner hands the hub over")
	ErrSelfNomination    = errors.New("you already own the hub: nominate another member")
	ErrNotMember         = errors.New("the nominee is not a member of this hub")
	ErrNoNomination      = errors.New("nobody nominated you to take the hub over")
	ErrNominationExpired = errors.New("the nomination expired: the owner can nominate you again")
)

// NominationTTL is how long a nominee has to accept the hub (ADR-0027 §6).
const NominationTTL = 7 * 24 * time.Hour

// Store is what the service needs from the database.
type Store interface {
	GetBuiltinOrganization(ctx context.Context) (store.Organization, error)
	CreateBuiltinOrganization(ctx context.Context, id uuid.UUID, name string) (store.Organization, error)
	UpdateOrganizationName(ctx context.Context, id uuid.UUID, name string) error
	SetClaimToken(ctx context.Context, id uuid.UUID, hash []byte, expiresAt time.Time) error
	ClaimOrganization(ctx context.Context, id uuid.UUID, hash []byte, by store.Actor, now time.Time) (store.Organization, error)
	UpdateOrganizationSettings(ctx context.Context, id uuid.UUID, settings json.RawMessage, at time.Time, by store.Actor, sections []string) (store.Organization, error)
	NominateOwner(ctx context.Context, id, nominee uuid.UUID, by store.Actor, at, expires time.Time) (store.Organization, error)
	CancelOwnershipTransfer(ctx context.Context, id uuid.UUID, by store.Actor, at time.Time) (store.Organization, error)
	AcceptOwnership(ctx context.Context, id uuid.UUID, by store.Actor, at time.Time) (store.Organization, uuid.UUID, error)
}

// Service is the organization domain.
type Service struct {
	st     Store
	ttl    time.Duration
	logger *slog.Logger

	now  func() time.Time
	rand io.Reader
	// games are the game ids UpdateSettings accepts in the settings (Settings.CheckGames); nil
	// skips the check.
	games []string
	// demo is a demo hub's (hub.yaml demo: true, gravel#112): its data is made up.
	demo bool
}

// New builds a service whose claim tokens live for ttl.
func New(st Store, ttl time.Duration, logger *slog.Logger) *Service {
	return &Service{st: st, ttl: ttl, logger: logger, now: time.Now, rand: rand.Reader}
}

// WithGames makes UpdateSettings refuse a game that is not one of ids, the games this hub ships
// (Settings.CheckGames), and returns s.
func (s *Service) WithGames(ids []string) *Service {
	s.games = slices.Sorted(slices.Values(ids))
	return s
}

// WithDemo marks the organization as a demo hub's, whose members, matches and stats are made up
// (gravel#112), and returns s.
func (s *Service) WithDemo(demo bool) *Service {
	s.demo = demo
	return s
}

// Demo reports whether this is a demo hub.
func (s *Service) Demo() bool { return s.demo }

// EnsureBuiltin creates the built-in organization if it does not exist, keeps its name in step
// with the configuration and, while it is unowned, mints a fresh owner-claim token. The token is
// returned exactly once, for the start-up log; it is empty when the hub is owned.
func (s *Service) EnsureBuiltin(ctx context.Context, name string) (store.Organization, string, error) {
	name = strings.TrimSpace(name)
	o, err := s.CreateBuiltinIfAbsent(ctx, name)
	if err != nil {
		return store.Organization{}, "", err
	}
	if o.Name != name {
		if err := s.st.UpdateOrganizationName(ctx, o.ID, name); err != nil {
			return store.Organization{}, "", err
		}
		s.logger.Info("renamed the built-in organization", "from", o.Name, "to", name)
		o.Name = name
	}
	if o.Owned() {
		return o, "", nil
	}
	token, hash, err := s.newToken()
	if err != nil {
		return store.Organization{}, "", err
	}
	expires := s.now().Add(s.ttl)
	if err := s.st.SetClaimToken(ctx, o.ID, hash, expires); err != nil {
		if errors.Is(err, store.ErrOwned) { // claimed between the read and the write
			o, err = s.st.GetBuiltinOrganization(ctx)
			return o, "", err
		}
		return store.Organization{}, "", err
	}
	o.ClaimTokenHash = hash
	o.ClaimTokenExpiresAt = &expires
	return o, token, nil
}

// CreateBuiltinIfAbsent creates the built-in organization with name if it does not exist and
// returns it. It writes nothing else: an existing organization keeps its name, and the owner
// claim is never touched, so a command that needs the row (`gravel-hub settings apply` on a
// database that was migrated but never served) does not rotate the token `serve` printed. The
// renaming and the token are EnsureBuiltin's, at start.
func (s *Service) CreateBuiltinIfAbsent(ctx context.Context, name string) (store.Organization, error) {
	o, err := s.st.GetBuiltinOrganization(ctx)
	if !errors.Is(err, store.ErrNotFound) {
		return o, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return store.Organization{}, fmt.Errorf("org: new id: %w", err)
	}
	if o, err = s.st.CreateBuiltinOrganization(ctx, id, strings.TrimSpace(name)); err != nil {
		return store.Organization{}, err
	}
	s.logger.Info("created the built-in organization", "organization_id", o.ID, "name", o.Name)
	return o, nil
}

// Get returns the built-in organization.
func (s *Service) Get(ctx context.Context) (store.Organization, error) {
	return s.st.GetBuiltinOrganization(ctx)
}

// Claim consumes the owner-claim token and makes the member by names (by.UserID) the owner; the
// events log records it (ownership.claimed).
func (s *Service) Claim(ctx context.Context, token string, by store.Actor) (store.Organization, error) {
	if by.UserID == nil {
		return store.Organization{}, errors.New("org: a claim needs a member")
	}
	token = strings.TrimSpace(token)
	o, err := s.st.GetBuiltinOrganization(ctx)
	if err != nil {
		return store.Organization{}, err
	}
	if o.Owned() {
		return store.Organization{}, ErrAlreadyOwned
	}
	hash := hashToken(token)
	// Constant-time compare first, so a wrong token costs the same as a right one; the store's
	// predicate then makes the claim atomic. An empty stored hash never matches a real token.
	if token == "" || len(o.ClaimTokenHash) != len(hash) || subtle.ConstantTimeCompare(o.ClaimTokenHash, hash) != 1 {
		return store.Organization{}, ErrInvalidToken
	}
	claimed, err := s.st.ClaimOrganization(ctx, o.ID, hash, by, s.now())
	switch {
	case errors.Is(err, store.ErrOwned):
		return store.Organization{}, ErrAlreadyOwned
	case errors.Is(err, store.ErrClaimRejected):
		return store.Organization{}, ErrInvalidToken
	case err != nil:
		return store.Organization{}, err
	}
	s.logger.Info("ownership claimed", "organization_id", claimed.ID, "owner_user_id", *by.UserID)
	return claimed, nil
}

// NominateOwner makes nominee the hub's pending owner for NominationTTL, replacing any earlier
// nomination; by must be the owner (ADR-0027 §6). The caller checks that by acts in person.
func (s *Service) NominateOwner(ctx context.Context, nominee uuid.UUID, by store.Actor) (store.Organization, error) {
	o, err := s.st.GetBuiltinOrganization(ctx)
	if err != nil {
		return store.Organization{}, err
	}
	now := s.now()
	n, err := s.st.NominateOwner(ctx, o.ID, nominee, by, now, now.Add(NominationTTL))
	if err != nil {
		return store.Organization{}, transferError(err)
	}
	s.logger.Info("ownership nominated", "organization_id", o.ID, "nominee_user_id", nominee, "expires_at", n.Pending.ExpiresAt)
	return n, nil
}

// CancelOwnershipTransfer ends the nomination: the owner cancels it, the nominee declines it.
func (s *Service) CancelOwnershipTransfer(ctx context.Context, by store.Actor) (store.Organization, error) {
	o, err := s.st.GetBuiltinOrganization(ctx)
	if err != nil {
		return store.Organization{}, err
	}
	c, err := s.st.CancelOwnershipTransfer(ctx, o.ID, by, s.now())
	if err != nil {
		return store.Organization{}, transferError(err)
	}
	s.logger.Info("ownership nomination ended", "organization_id", o.ID)
	return c, nil
}

// AcceptOwnership makes by, the unexpired nominee, the owner, and the old owner an admin.
func (s *Service) AcceptOwnership(ctx context.Context, by store.Actor) (store.Organization, error) {
	o, err := s.st.GetBuiltinOrganization(ctx)
	if err != nil {
		return store.Organization{}, err
	}
	a, old, err := s.st.AcceptOwnership(ctx, o.ID, by, s.now())
	if err != nil {
		return store.Organization{}, transferError(err)
	}
	s.logger.Info("ownership transferred", "organization_id", o.ID, "owner_user_id", *by.UserID, "old_owner_user_id", old)
	return a, nil
}

// transferError turns the store's transfer errors into the service's.
func transferError(err error) error {
	switch {
	case errors.Is(err, store.ErrNotOwner):
		return ErrNotOwner
	case errors.Is(err, store.ErrSelfNomination):
		return ErrSelfNomination
	case errors.Is(err, store.ErrNotFound):
		return ErrNotMember
	case errors.Is(err, store.ErrNoNomination):
		return ErrNoNomination
	case errors.Is(err, store.ErrNominationExpired):
		return ErrNominationExpired
	}
	return err
}

func (s *Service) newToken() (token string, hash []byte, err error) {
	raw := make([]byte, 32)
	if _, err := io.ReadFull(s.rand, raw); err != nil {
		return "", nil, fmt.Errorf("org: token: %w", err)
	}
	token = base64.RawURLEncoding.EncodeToString(raw)
	return token, hashToken(token), nil
}

func hashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}
