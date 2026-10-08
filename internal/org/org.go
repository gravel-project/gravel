// Package org owns the built-in organization and the one-time owner claim.
//
// A fresh hub has no owner and every later action needs one. On each start while it is unowned
// the hub mints a one-time owner-claim token, stores only its hash, and prints the token once to
// its log. ClaimOwnership consumes it: single-use, expiring, and refused forever once the hub is
// owned (ADR-0002). Binding the claim to a logged-in user is gravel#13's job.
package org

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/gravel-project/gravel/internal/store"
)

// Errors the API maps to Connect codes.
var (
	ErrAlreadyOwned = errors.New("the organization is already owned")
	ErrInvalidToken = errors.New("the owner-claim token is invalid or has expired")
)

// Store is what the service needs from the database.
type Store interface {
	GetBuiltinOrganization(ctx context.Context) (store.Organization, error)
	CreateBuiltinOrganization(ctx context.Context, id uuid.UUID, name string) (store.Organization, error)
	UpdateOrganizationName(ctx context.Context, id uuid.UUID, name string) error
	SetClaimToken(ctx context.Context, id uuid.UUID, hash []byte, expiresAt time.Time) error
	ClaimOrganization(ctx context.Context, id uuid.UUID, hash []byte, now time.Time) (store.Organization, error)
}

// Service is the organization domain.
type Service struct {
	st     Store
	ttl    time.Duration
	logger *slog.Logger

	now  func() time.Time
	rand io.Reader
}

// New builds a service whose claim tokens live for ttl.
func New(st Store, ttl time.Duration, logger *slog.Logger) *Service {
	return &Service{st: st, ttl: ttl, logger: logger, now: time.Now, rand: rand.Reader}
}

// EnsureBuiltin creates the built-in organization if it does not exist, keeps its name in step
// with the configuration and, while it is unowned, mints a fresh owner-claim token. The token is
// returned exactly once, for the start-up log; it is empty when the hub is owned.
func (s *Service) EnsureBuiltin(ctx context.Context, name string) (store.Organization, string, error) {
	name = strings.TrimSpace(name)
	o, err := s.st.GetBuiltinOrganization(ctx)
	switch {
	case errors.Is(err, store.ErrNotFound):
		id, err := uuid.NewV7()
		if err != nil {
			return store.Organization{}, "", fmt.Errorf("org: new id: %w", err)
		}
		if o, err = s.st.CreateBuiltinOrganization(ctx, id, name); err != nil {
			return store.Organization{}, "", err
		}
		s.logger.Info("created the built-in organization", "organization_id", o.ID, "name", o.Name)
	case err != nil:
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

// Get returns the built-in organization.
func (s *Service) Get(ctx context.Context) (store.Organization, error) {
	return s.st.GetBuiltinOrganization(ctx)
}

// Claim consumes the owner-claim token.
func (s *Service) Claim(ctx context.Context, token string) (store.Organization, error) {
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
	claimed, err := s.st.ClaimOrganization(ctx, o.ID, hash, s.now())
	switch {
	case errors.Is(err, store.ErrOwned):
		return store.Organization{}, ErrAlreadyOwned
	case errors.Is(err, store.ErrClaimRejected):
		return store.Organization{}, ErrInvalidToken
	case err != nil:
		return store.Organization{}, err
	}
	s.logger.Info("ownership claimed", "organization_id", claimed.ID)
	return claimed, nil
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
