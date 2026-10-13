package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// IdentityProof is a row of identity_proofs: a one-time code a member proves an account with by
// putting it where only that account can (gravel#5).
type IdentityProof struct {
	ID       uuid.UUID
	UserID   uuid.UUID
	Channel  string // the way it is proven: "game-chat"
	Scope    string // where on the channel: a server id
	Provider string // the provider the account is at: "steam"
	// ExpectSubject is the account the code must come from; "" for any (a new link).
	ExpectSubject string
	CodeHash      []byte
	CreatedAt     time.Time
	ExpiresAt     time.Time
	UsedAt        *time.Time
	Result        string // "" while open, else ProofProven, ProofTaken or ProofReplaced
	ProvenSubject string
}

// What became of a proof.
const (
	ProofProven   = "proven"   // the account is linked to the member, or proven again
	ProofTaken    = "taken"    // the account is another member's; nothing moved
	ProofReplaced = "replaced" // the member asked for a new code on the channel
)

// Errors of identity_proofs.
var (
	ErrProofCodeExists = errors.New("store: a proof with that code exists")
	ErrProofUsed       = errors.New("store: the proof was used")
	ErrProofExpired    = errors.New("store: the proof expired")
	ErrProofMismatch   = errors.New("store: the proof is for another channel, place or account")
)

const proofColumns = `id, user_id, channel, scope, provider, expect_subject, code_hash, created_at, expires_at, used_at, result, proven_subject`

func scanProof(row pgx.Row) (IdentityProof, error) {
	var p IdentityProof
	err := row.Scan(&p.ID, &p.UserID, &p.Channel, &p.Scope, &p.Provider, &p.ExpectSubject, &p.CodeHash, &p.CreatedAt, &p.ExpiresAt,
		&p.UsedAt, &p.Result, &p.ProvenSubject)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, ErrNotFound
	}
	return p, err
}

// CreateIdentityProof stores a new proof and closes the member's open ones on the same channel as
// replaced, so one code at a time is good. ErrProofCodeExists when the code's hash is taken (the
// caller draws another).
func (s *Store) CreateIdentityProof(ctx context.Context, p IdentityProof) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: create proof: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `UPDATE identity_proofs SET used_at = $3, result = $4 WHERE user_id = $1 AND channel = $2 AND used_at IS NULL`,
		p.UserID, p.Channel, p.CreatedAt, ProofReplaced); err != nil {
		return fmt.Errorf("store: create proof: %w", err)
	}
	_, err = tx.Exec(ctx, `INSERT INTO identity_proofs (id, user_id, channel, scope, provider, expect_subject, code_hash, created_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		p.ID, p.UserID, p.Channel, p.Scope, p.Provider, p.ExpectSubject, p.CodeHash, p.CreatedAt, p.ExpiresAt)
	if isUniqueViolation(err) {
		return ErrProofCodeExists
	}
	if err != nil {
		return fmt.Errorf("store: create proof: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: create proof: %w", err)
	}
	return nil
}

// GetIdentityProof is a proof by id, or ErrNotFound.
func (s *Store) GetIdentityProof(ctx context.Context, id uuid.UUID) (IdentityProof, error) {
	p, err := scanProof(s.pool.QueryRow(ctx, `SELECT `+proofColumns+` FROM identity_proofs WHERE id = $1`, id))
	if err != nil && !errors.Is(err, ErrNotFound) {
		return p, fmt.Errorf("store: get proof: %w", err)
	}
	return p, err
}

// ProofRedemption is a code presented on a channel by an account: what the channel saw.
type ProofRedemption struct {
	Channel, Scope, Provider string
	Subject                  string // the account the channel says presented the code
	DisplayName              string // its name there, for a new link
	Method                   string // the verification method the identity records
	At                       time.Time
	// UserID, when set, is the member the proof must be: a channel the hub checks for a member
	// who brought the code back (an RSI bio), not one anyone could present it on.
	UserID *uuid.UUID
}

// ProofRedeemed is what a redeemed proof did: the proof as now spent, the identity, and whether
// it was linked by it (rather than proven again, or taken).
type ProofRedeemed struct {
	Proof    IdentityProof
	Identity Identity
	Linked   bool
}

// RedeemIdentityProof uses the proof whose code hashes to codeHash, in one transaction with what it
// proves. The proof must be open, unexpired, for the channel, scope and provider, and, when it
// names one, for the account. Then the account is linked to the proof's member (event linked), or
// proven again when it is already theirs (its method and time change; event proven), or, when it
// is another member's, nothing moves and the proof is spent as taken (ErrIdentityExists).
//
// ErrNotFound for no such code, ErrProofUsed for a code used before (a replay), ErrProofExpired,
// and ErrProofMismatch for another channel, place or account; none of these spends the proof.
func (s *Store) RedeemIdentityProof(ctx context.Context, codeHash []byte, r ProofRedemption) (ProofRedeemed, error) {
	var out ProofRedeemed
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return out, fmt.Errorf("store: redeem proof: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	p, err := scanProof(tx.QueryRow(ctx, `SELECT `+proofColumns+` FROM identity_proofs WHERE code_hash = $1 FOR UPDATE`, codeHash))
	switch {
	case errors.Is(err, ErrNotFound):
		return out, ErrNotFound
	case err != nil:
		return out, fmt.Errorf("store: redeem proof: %w", err)
	case p.UsedAt != nil:
		return out, ErrProofUsed
	case !r.At.Before(p.ExpiresAt):
		return out, ErrProofExpired
	case p.Channel != r.Channel || p.Scope != r.Scope || p.Provider != r.Provider || (p.ExpectSubject != "" && p.ExpectSubject != r.Subject) ||
		(r.UserID != nil && *r.UserID != p.UserID):
		return out, ErrProofMismatch
	}

	result := ProofProven
	ident, err := scanIdentity(tx.QueryRow(ctx, `SELECT `+identityColumns+` FROM identities WHERE provider = $1 AND subject = $2 FOR UPDATE`,
		r.Provider, r.Subject))
	switch {
	case err == nil && ident.UserID != p.UserID:
		result = ProofTaken
	case err == nil:
		ident, err = scanIdentity(tx.QueryRow(ctx, `UPDATE identities SET verification_method = $3, verified_at = $4,
			display_name = CASE WHEN $5 = '' THEN display_name ELSE $5 END
			WHERE provider = $1 AND subject = $2 RETURNING `+identityColumns, r.Provider, r.Subject, r.Method, r.At, r.DisplayName))
		if err != nil {
			return out, fmt.Errorf("store: redeem proof: %w", err)
		}
		if err := s.appendIdentityEvent(ctx, tx, p.UserID, r.Provider, r.Subject, IdentityEventProven, r.At); err != nil {
			return out, err
		}
	case errors.Is(err, ErrNotFound):
		ident = Identity{Provider: r.Provider, Subject: r.Subject, UserID: p.UserID, DisplayName: r.DisplayName,
			VerificationMethod: r.Method, VerifiedAt: r.At, LinkedAt: r.At}
		// A link made by someone else since the lock found nothing fails the insert and the
		// transaction; the proof stays open, and the next presentation finds the link.
		if err := s.insertIdentity(ctx, tx, ident, IdentityEventLinked); err != nil {
			return out, err
		}
		out.Linked = true
	default:
		return out, fmt.Errorf("store: redeem proof: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE identity_proofs SET used_at = $2, result = $3, proven_subject = $4 WHERE id = $1`,
		p.ID, r.At, result, r.Subject); err != nil {
		return out, fmt.Errorf("store: redeem proof: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return out, fmt.Errorf("store: redeem proof: %w", err)
	}
	at := r.At
	p.UsedAt, p.Result, p.ProvenSubject = &at, result, r.Subject
	out.Proof, out.Identity = p, ident
	if result == ProofTaken { // the other member's identity stays out of the answer
		return ProofRedeemed{Proof: p}, ErrIdentityExists
	}
	return out, nil
}

// DeleteIdentityProofs removes proofs that expired before the time.
func (s *Store) DeleteIdentityProofs(ctx context.Context, expiredBefore time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM identity_proofs WHERE expires_at < $1`, expiredBefore)
	if err != nil {
		return 0, fmt.Errorf("store: delete proofs: %w", err)
	}
	return tag.RowsAffected(), nil
}
