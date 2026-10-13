package identity

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/mail"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/gravel-project/gravel/internal/store"
)

// Password links (gravel#187, ADR-0026): with registration closed, staff make a local account and
// hand its owner a one-time link to choose a password; a forgotten password gets the same kind of
// link. The token is shown once and only its SHA-256 is stored.

// PasswordLinkTTL is how long a link works.
const PasswordLinkTTL = time.Hour

// MaxEmail bounds a recovery address.
const MaxEmail = 254

// Errors of password links.
var (
	// ErrLinkInvalid is one answer to an unknown, used or expired link.
	ErrLinkInvalid  = errors.New("this link is unknown, used or expired; ask for a new one")
	ErrNoLocal      = errors.New("no member has that username")
	ErrInvalidEmail = errors.New("invalid email address")
)

// PasswordLink is an issued link: Token goes in /auth/local/reset/<token>, once.
type PasswordLink struct {
	Token     string
	Username  string
	Purpose   string // store.PasswordLinkSet or store.PasswordLinkReset
	ExpiresAt time.Time
}

// Path is the link's path on the hub.
func (l PasswordLink) Path() string { return "/auth/local/reset/" + l.Token }

// LinkStore is what password links need from the database.
type LinkStore interface {
	CreatePasswordLink(ctx context.Context, l store.PasswordLink) error
	GetPasswordLink(ctx context.Context, tokenHash []byte, now time.Time) (store.PasswordLink, error)
	UsePasswordLink(ctx context.Context, tokenHash []byte, now time.Time, passwordHash string) (store.PasswordLink, uuid.UUID, error)
	DeleteExpiredPasswordLinks(ctx context.Context, cutoff time.Time) (int64, error)
}

// WithPasswordLinks lets the service issue and spend password links over st; observe, when set,
// is told "issued", "used", "invalid" (an unknown, used or expired link) or "refused" (a password
// the rules turned down). Links need local accounts on (WithLocalAccounts).
func WithPasswordLinks(st LinkStore, observe func(result string)) Option {
	return func(s *Service) {
		s.links, s.observeLink = st, observe
	}
}

func (s *Service) linkObserved(result string) {
	if s.observeLink != nil {
		s.observeLink(result)
	}
}

// CreateLocal makes a member with a local account and no usable password yet, and issues the link
// that sets one. by is the staff member who asked (nil: the host). email is optional, for recovery
// only. ErrLocalDisabled, ErrInvalidUsername, ErrInvalidEmail, ErrIdentityTaken.
func (s *Service) CreateLocal(ctx context.Context, username, email string, by *uuid.UUID) (store.User, PasswordLink, error) {
	if s.local == nil || s.links == nil {
		return store.User{}, PasswordLink{}, ErrLocalDisabled
	}
	subject, err := CanonicalUsername(username)
	if err != nil {
		return store.User{}, PasswordLink{}, err
	}
	var addr *string
	if e := strings.TrimSpace(email); e != "" {
		a, err := mail.ParseAddress(e)
		if err != nil || a.Address != e || a.Name != "" || len(e) > MaxEmail {
			return store.User{}, PasswordLink{}, fmt.Errorf("%w: %q", ErrInvalidEmail, e)
		}
		addr = &e
	}
	// A password nobody knows, so the account can't log in until its link is used.
	secret := make([]byte, 32)
	if _, err := io.ReadFull(s.rand, secret); err != nil {
		return store.User{}, PasswordLink{}, err
	}
	hash, err := HashPassword(base64.RawURLEncoding.EncodeToString(secret), s.pwParams, s.rand)
	if err != nil {
		return store.User{}, PasswordLink{}, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return store.User{}, PasswordLink{}, fmt.Errorf("identity: new id: %w", err)
	}
	now := s.now()
	ident := store.Identity{Provider: ProviderLocal, Subject: subject, DisplayName: subject, VerificationMethod: MethodPassword, VerifiedAt: now,
		Credential: &store.LocalCredential{PasswordHash: hash, PasswordUpdatedAt: now, Email: addr}}
	u, err := s.st.RegisterUser(ctx, store.User{ID: id, OrganizationID: s.orgID, DisplayName: subject}, ident)
	if errors.Is(err, store.ErrIdentityExists) {
		return store.User{}, PasswordLink{}, ErrIdentityTaken
	}
	if err != nil {
		return store.User{}, PasswordLink{}, err
	}
	s.logger.InfoContext(ctx, "local account created", "user_id", u.ID, "by", by)
	link, err := s.issueLink(ctx, subject, store.PasswordLinkSet, by)
	if err != nil {
		return store.User{}, PasswordLink{}, err
	}
	return u, link, nil
}

// IssueReset issues a link that resets a local account's password; the account's earlier unused
// links stop working. ErrLocalDisabled, ErrNoLocal.
func (s *Service) IssueReset(ctx context.Context, username string, by *uuid.UUID) (PasswordLink, error) {
	if s.local == nil || s.links == nil {
		return PasswordLink{}, ErrLocalDisabled
	}
	subject, err := CanonicalUsername(username)
	if err != nil {
		return PasswordLink{}, ErrNoLocal
	}
	return s.issueLink(ctx, subject, store.PasswordLinkReset, by)
}

func (s *Service) issueLink(ctx context.Context, subject, purpose string, by *uuid.UUID) (PasswordLink, error) {
	raw := make([]byte, 32)
	if _, err := io.ReadFull(s.rand, raw); err != nil {
		return PasswordLink{}, err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	id, err := uuid.NewV7()
	if err != nil {
		return PasswordLink{}, fmt.Errorf("identity: new id: %w", err)
	}
	now := s.now()
	l := store.PasswordLink{ID: id, TokenHash: hashToken(token), Subject: subject, Purpose: purpose, IssuedBy: by, IssuedAt: now, ExpiresAt: now.Add(PasswordLinkTTL)}
	if err := s.links.CreatePasswordLink(ctx, l); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return PasswordLink{}, ErrNoLocal
		}
		return PasswordLink{}, err
	}
	s.linkObserved("issued")
	// The token is never logged: the link is a password for the next hour.
	s.logger.InfoContext(ctx, "password link issued", "subject", subject, "purpose", purpose, "by", by, "expires_at", l.ExpiresAt.UTC().Format(time.RFC3339))
	return PasswordLink{Token: token, Username: subject, Purpose: purpose, ExpiresAt: l.ExpiresAt}, nil
}

// PasswordLinkFor reads an open link, for the page that asks for the new password; it spends
// nothing. ErrLinkInvalid for an unknown, used or expired one.
func (s *Service) PasswordLinkFor(ctx context.Context, token string) (PasswordLink, error) {
	if s.local == nil || s.links == nil || token == "" {
		return PasswordLink{}, ErrLinkInvalid
	}
	l, err := s.links.GetPasswordLink(ctx, hashToken(token), s.now())
	if errors.Is(err, store.ErrNotFound) {
		return PasswordLink{}, ErrLinkInvalid
	}
	if err != nil {
		return PasswordLink{}, err
	}
	return PasswordLink{Username: l.Subject, Purpose: l.Purpose, ExpiresAt: l.ExpiresAt}, nil
}

// UsePasswordLink sets the account's password from its link, once, and clears its lockout. The
// caller ends the member's other sessions. ErrLinkInvalid, ErrWeakPassword.
func (s *Service) UsePasswordLink(ctx context.Context, token, password string) (store.User, PasswordLink, error) {
	open, err := s.PasswordLinkFor(ctx, token)
	if err != nil {
		if errors.Is(err, ErrLinkInvalid) {
			s.linkObserved("invalid")
		}
		return store.User{}, PasswordLink{}, err
	}
	policy, err := s.LocalPolicy(ctx)
	if err != nil {
		return store.User{}, PasswordLink{}, err
	}
	if err := checkPassword(open.Username, password, policy.minPassword()); err != nil {
		s.linkObserved("refused")
		return store.User{}, PasswordLink{}, err
	}
	hash, err := HashPassword(password, s.pwParams, s.rand)
	if err != nil {
		return store.User{}, PasswordLink{}, err
	}
	l, userID, err := s.links.UsePasswordLink(ctx, hashToken(token), s.now(), hash)
	if errors.Is(err, store.ErrNotFound) { // spent between the read and now
		s.linkObserved("invalid")
		return store.User{}, PasswordLink{}, ErrLinkInvalid
	}
	if err != nil {
		return store.User{}, PasswordLink{}, err
	}
	u, err := s.st.GetUser(ctx, userID)
	if err != nil {
		return store.User{}, PasswordLink{}, err
	}
	s.linkObserved("used")
	s.logger.InfoContext(ctx, "password set from a link", "user_id", userID, "subject", l.Subject, "purpose", l.Purpose)
	return u, PasswordLink{Username: l.Subject, Purpose: l.Purpose, ExpiresAt: l.ExpiresAt}, nil
}
