// Package proof is proof of control (gravel#5): a member proves they control an account at a
// provider by putting a one-time code where only that account can. Typing it in a game server's
// chat is one way: the server's log carries the account's id with the line. Putting it in a
// profile the hub can read is another (an RSI bio, P5).
//
// A code is drawn from crypto/rand, eight symbols of Crockford's base 32 (40 bits), shown to the
// member once and stored as its SHA-256. It is bound to the member, the channel, the channel's
// scope (the server) and, when the member is proving an account they have linked already, that
// account. It expires after ten minutes and is used once. Asking for a new one replaces the open
// one. Two rate limits apply: codes a member may ask for, and codes presented per account and per
// scope, so a chat can't be used to guess.
//
// A proven account is linked by the rules every link follows: an account another member holds
// stays theirs (identity.ErrIdentityTaken), and one the member holds already is proven again (its
// verification method and time change). Nothing is merged or moved.
package proof

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/gravel-project/gravel/internal/identity"
	"github.com/gravel-project/gravel/internal/ratelimit"
	"github.com/gravel-project/gravel/internal/store"
)

// Channel is a way an account shows a code.
type Channel interface {
	// Name is the channel's id: "game-chat".
	Name() string
	// Provider is the identity provider the accounts it proves are at: "steam".
	Provider() string
	// Method is the verification method a proven identity records: "game-chat".
	Method() string
	// Code finds a code in what the channel carried ("!link 7K3M-9QXA" in a chat line); ok is
	// false when there is none.
	Code(text string) (code string, ok bool)
	// Instructions tell the member what to do with the code.
	Instructions(code string) string
}

// Checker is a channel the hub asks: the member says which account and brings the code back, and
// the checker looks for it where only that account can put it (a profile's bio).
type Checker interface {
	Channel
	// Check reports the account at subject showing the code, with its name; ErrNotShown when it
	// doesn't.
	Check(ctx context.Context, subject, code string) (Account, error)
}

// Account is an account at a channel's provider.
type Account struct {
	Subject     string
	DisplayName string
}

// Evidence is a code an account presented on a channel: a chat line from the server's log.
type Evidence struct {
	Channel string
	Scope   string // the server it came from
	Account
	Text string
}

// Errors.
var (
	ErrUnknownChannel = errors.New("proof: no such channel")
	ErrNoCode         = errors.New("proof: no code in what was presented")
	// ErrBadCode is a code that is unknown, used, expired, or for another member, place or
	// account: one answer, so a guess learns nothing.
	ErrBadCode   = errors.New("proof: that code is unknown, used or expired, or not for here")
	ErrNotShown  = errors.New("proof: the account doesn't show the code")
	ErrNotLinked = identity.ErrNotLinked
	ErrNotFound  = errors.New("proof: no such proof")
)

// TooManyError is a refusal by a rate limit, with when to try again.
type TooManyError struct{ RetryAfter time.Duration }

func (e *TooManyError) Error() string {
	return fmt.Sprintf("proof: too many tries; try again in %s", e.RetryAfter.Round(time.Second))
}

// Limits.
const (
	// TTL is how long a code is good for.
	TTL = 10 * time.Minute
	// Keep is how long a proof stays after it expired, for the member's page and the logs.
	Keep = 30 * 24 * time.Hour
	// CodesPerHour is how many codes a member may ask for in an hour.
	CodesPerHour = 5
	// TriesPerAccount and TriesPerScope bound codes presented every ten minutes, per account and
	// per scope (a server's chat).
	TriesPerAccount = 10
	TriesPerScope   = 60
)

// Store is what the service needs from the database.
type Store interface {
	CreateIdentityProof(ctx context.Context, p store.IdentityProof) error
	GetIdentityProof(ctx context.Context, id uuid.UUID) (store.IdentityProof, error)
	RedeemIdentityProof(ctx context.Context, codeHash []byte, r store.ProofRedemption) (store.ProofRedeemed, error)
	DeleteIdentityProofs(ctx context.Context, expiredBefore time.Time) (int64, error)
	ListIdentities(ctx context.Context, userID uuid.UUID) ([]store.Identity, error)
}

// Service issues and redeems proofs.
type Service struct {
	st       Store
	channels map[string]Channel
	logger   *slog.Logger
	now      func() time.Time
	rand     io.Reader

	codes, perAccount, perScope *ratelimit.Limiter
	counted                     *prometheus.CounterVec
}

// Option tunes a Service.
type Option func(*Service)

// WithClock replaces the clock, the rate limits' included.
func WithClock(now func() time.Time) Option { return func(s *Service) { s.now = now } }

// WithRand replaces the code source (tests).
func WithRand(r io.Reader) Option { return func(s *Service) { s.rand = r } }

// New builds a service over the channels.
func New(st Store, logger *slog.Logger, channels []Channel, opts ...Option) *Service {
	s := &Service{st: st, logger: logger, channels: map[string]Channel{}, now: time.Now, rand: rand.Reader}
	for _, c := range channels {
		s.channels[c.Name()] = c
	}
	for _, o := range opts {
		o(s)
	}
	clock := func() time.Time { return s.now() }
	s.codes = ratelimit.NewEvery(CodesPerHour, time.Hour, CodesPerHour).WithClock(clock)
	s.perAccount = ratelimit.NewEvery(TriesPerAccount, 10*time.Minute, TriesPerAccount).WithClock(clock)
	s.perScope = ratelimit.NewEvery(TriesPerScope, 10*time.Minute, TriesPerScope).WithClock(clock)
	return s
}

// Channel looks a channel up.
func (s *Service) Channel(name string) (Channel, bool) {
	c, ok := s.channels[name]
	return c, ok
}

// Started is a new code: shown to the member once, never again.
type Started struct {
	ID           uuid.UUID
	Code         string // "7K3M-9QXA"
	Channel      string
	Scope        string
	ExpiresAt    time.Time
	Instructions string
}

// Start draws a code for the member on a channel and scope. expectSubject, when set, is an account
// at the channel's provider the member has linked already, which the code must come from (proving
// it again); ErrNotLinked otherwise. The member's open code on the channel is replaced.
func (s *Service) Start(ctx context.Context, userID uuid.UUID, channel, scope, expectSubject string) (Started, error) {
	ch, ok := s.channels[channel]
	if !ok {
		return Started{}, ErrUnknownChannel
	}
	if expectSubject != "" {
		ids, err := s.st.ListIdentities(ctx, userID)
		if err != nil {
			return Started{}, err
		}
		found := false
		for _, i := range ids {
			found = found || (i.Provider == ch.Provider() && i.Subject == expectSubject)
		}
		if !found {
			return Started{}, ErrNotLinked
		}
	}
	if ok, retry := s.codes.Allow(userID.String()); !ok {
		return Started{}, &TooManyError{RetryAfter: retry}
	}
	now := s.now().UTC()
	for range 3 {
		code, err := newCode(s.rand)
		if err != nil {
			return Started{}, err
		}
		p := store.IdentityProof{ID: uuid.New(), UserID: userID, Channel: ch.Name(), Scope: scope, Provider: ch.Provider(), ExpectSubject: expectSubject,
			CodeHash: hashCode(code), CreatedAt: now, ExpiresAt: now.Add(TTL)}
		switch err := s.st.CreateIdentityProof(ctx, p); {
		case errors.Is(err, store.ErrProofCodeExists):
			continue
		case err != nil:
			return Started{}, err
		}
		s.logger.InfoContext(ctx, "proof started", "user_id", userID, "channel", ch.Name(), "scope", scope, "proof_id", p.ID)
		s.count(ch.Name(), resultStarted)
		return Started{ID: p.ID, Code: code, Channel: ch.Name(), Scope: scope, ExpiresAt: p.ExpiresAt, Instructions: ch.Instructions(code)}, nil
	}
	return Started{}, errors.New("proof: no free code after three draws")
}

// Outcome is what a presented code did.
type Outcome struct {
	UserID   uuid.UUID
	Identity store.Identity
	// Linked is true for an account linked by the proof, false for one proven again.
	Linked bool
}

// Present redeems a code an account showed on a channel (a chat line). ErrNoCode when the text
// holds none; ErrBadCode for a code that doesn't redeem; identity.ErrIdentityTaken when the account
// is another member's (the code is spent; nothing moved); a *TooManyError past a rate limit.
func (s *Service) Present(ctx context.Context, e Evidence) (Outcome, error) {
	ch, ok := s.channels[e.Channel]
	if !ok {
		return Outcome{}, ErrUnknownChannel
	}
	code, ok := ch.Code(e.Text)
	if !ok {
		return Outcome{}, ErrNoCode
	}
	if e.Subject == "" {
		return Outcome{}, ErrBadCode // a line the log couldn't tie to an account
	}
	if ok, retry := s.perAccount.Allow(e.Scope + "\x00" + e.Subject); !ok {
		s.count(ch.Name(), resultLimited)
		return Outcome{}, &TooManyError{RetryAfter: retry}
	}
	if ok, retry := s.perScope.Allow(e.Scope); !ok {
		s.count(ch.Name(), resultLimited)
		return Outcome{}, &TooManyError{RetryAfter: retry}
	}
	return s.redeem(ctx, ch, code, e.Scope, e.Account, nil)
}

// Check redeems a code the member brought back for an account on a channel the hub asks (a
// profile's bio): the checker must find the code at the account. ErrNotShown when it doesn't.
func (s *Service) Check(ctx context.Context, userID uuid.UUID, channel, scope, subject, code string) (Outcome, error) {
	ch, ok := s.channels[channel]
	if !ok {
		return Outcome{}, ErrUnknownChannel
	}
	c, ok := ch.(Checker)
	if !ok {
		return Outcome{}, fmt.Errorf("%w: %s is not checked by the hub", ErrUnknownChannel, channel)
	}
	if ok, retry := s.perAccount.Allow(scope + "\x00" + subject); !ok {
		s.count(ch.Name(), resultLimited)
		return Outcome{}, &TooManyError{RetryAfter: retry}
	}
	acct, err := c.Check(ctx, subject, code)
	if err != nil {
		return Outcome{}, err
	}
	return s.redeem(ctx, ch, code, scope, acct, &userID)
}

func (s *Service) redeem(ctx context.Context, ch Channel, code, scope string, acct Account, userID *uuid.UUID) (Outcome, error) {
	r, err := s.st.RedeemIdentityProof(ctx, hashCode(code), store.ProofRedemption{Channel: ch.Name(), Scope: scope, Provider: ch.Provider(),
		Subject: acct.Subject, DisplayName: acct.DisplayName, Method: ch.Method(), At: s.now().UTC(), UserID: userID})
	switch {
	case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrProofUsed), errors.Is(err, store.ErrProofExpired), errors.Is(err, store.ErrProofMismatch):
		s.logger.InfoContext(ctx, "a proof code did not redeem", "channel", ch.Name(), "scope", scope, "why", err.Error())
		s.count(ch.Name(), resultBadCode)
		return Outcome{}, ErrBadCode
	case errors.Is(err, store.ErrIdentityExists):
		s.logger.InfoContext(ctx, "a proof found the account linked to another member", "channel", ch.Name(), "scope", scope,
			"proof_id", r.Proof.ID, "user_id", r.Proof.UserID)
		s.count(ch.Name(), resultTaken)
		return Outcome{UserID: r.Proof.UserID}, identity.ErrIdentityTaken
	case err != nil:
		return Outcome{}, err
	}
	s.logger.InfoContext(ctx, "account proven", "channel", ch.Name(), "scope", scope, "proof_id", r.Proof.ID, "user_id", r.Proof.UserID,
		"provider", ch.Provider(), "linked", r.Linked)
	if r.Linked {
		s.count(ch.Name(), resultLinked)
	} else {
		s.count(ch.Name(), resultProven)
	}
	return Outcome{UserID: r.Proof.UserID, Identity: r.Identity, Linked: r.Linked}, nil
}

// Proof is a member's proof as they see it.
type Proof struct {
	ID        uuid.UUID
	Channel   string
	Scope     string
	Provider  string
	ExpiresAt time.Time
	// State is "open", "expired", or the store's result: "proven", "taken", "replaced".
	State         string
	ProvenSubject string
}

// Get is one of the member's proofs; ErrNotFound for none, or another member's.
func (s *Service) Get(ctx context.Context, userID, id uuid.UUID) (Proof, error) {
	p, err := s.st.GetIdentityProof(ctx, id)
	if errors.Is(err, store.ErrNotFound) || (err == nil && p.UserID != userID) {
		return Proof{}, ErrNotFound
	}
	if err != nil {
		return Proof{}, err
	}
	out := Proof{ID: p.ID, Channel: p.Channel, Scope: p.Scope, Provider: p.Provider, ExpiresAt: p.ExpiresAt, State: p.Result, ProvenSubject: p.ProvenSubject}
	switch {
	case out.State != "":
	case !s.now().Before(p.ExpiresAt):
		out.State = "expired"
	default:
		out.State = "open"
	}
	return out, nil
}

// Prune deletes proofs that expired more than Keep ago, and sweeps the rate limits.
func (s *Service) Prune(ctx context.Context) (int64, error) {
	s.codes.Sweep()
	s.perAccount.Sweep()
	s.perScope.Sweep()
	return s.st.DeleteIdentityProofs(ctx, s.now().Add(-Keep))
}

// crockford is Crockford's base 32: no I, L, O or U, so a code reads back unambiguously.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// newCode is eight symbols from 40 random bits, shown as two groups of four.
func newCode(r io.Reader) (string, error) {
	var b [5]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return "", fmt.Errorf("proof: random: %w", err)
	}
	n := uint64(b[0])<<32 | uint64(b[1])<<24 | uint64(b[2])<<16 | uint64(b[3])<<8 | uint64(b[4])
	out := make([]byte, 8)
	for i := 7; i >= 0; i-- {
		out[i] = crockford[n&31]
		n >>= 5
	}
	return string(out[:4]) + "-" + string(out[4:]), nil
}

// normalize is a code as typed, in the form it is hashed in: upper case, no hyphen, I and L read
// as 1 and O as 0.
func normalize(code string) string {
	return strings.NewReplacer("-", "", "I", "1", "L", "1", "O", "0").Replace(strings.ToUpper(strings.TrimSpace(code)))
}

func hashCode(code string) []byte {
	h := sha256.Sum256([]byte(normalize(code)))
	return h[:]
}

// GameChat is a code typed in a game server's chat, which the server's log carries with the
// player's account (Counter-Strike 2's say): "!link 7K3M-9QXA". Its scope is the server.
type GameChat struct {
	// ProviderName is the provider the game's log names accounts at: "steam".
	ProviderName string
}

// GameChatName is GameChat's channel name and verification method.
const GameChatName = "game-chat"

var chatCode = regexp.MustCompile(`(?i)^\s*[!.]link\s+([0-9a-z]{4})-?([0-9a-z]{4})\s*$`)

// Name implements Channel.
func (GameChat) Name() string { return GameChatName }

// Provider implements Channel.
func (g GameChat) Provider() string { return g.ProviderName }

// Method implements Channel.
func (GameChat) Method() string { return GameChatName }

// Code implements Channel: the whole line is "!link CODE" (or ".link"), with or without its hyphen.
func (GameChat) Code(text string) (string, bool) {
	m := chatCode.FindStringSubmatch(text)
	if m == nil {
		return "", false
	}
	return strings.ToUpper(m[1] + "-" + m[2]), true
}

// Instructions implements Channel.
func (GameChat) Instructions(code string) string {
	return "Join the server, then type in chat: !link " + code
}
