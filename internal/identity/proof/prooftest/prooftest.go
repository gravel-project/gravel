// Package prooftest holds an in-memory proof.Store and the conformance suite every proof store
// and channel passes: the fake here, and Postgres in internal/store's tests.
package prooftest

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gravel-project/gravel/internal/identity"
	"github.com/gravel-project/gravel/internal/identity/proof"
	"github.com/gravel-project/gravel/internal/store"
)

// FakeStore is an in-memory proof.Store with the database's rules.
type FakeStore struct {
	mu         sync.Mutex
	proofs     map[uuid.UUID]store.IdentityProof
	identities map[[2]string]store.Identity
	// Events are the identity events written, as "event provider subject".
	Events []string
}

// NewFakeStore is an empty store.
func NewFakeStore() *FakeStore {
	return &FakeStore{proofs: map[uuid.UUID]store.IdentityProof{}, identities: map[[2]string]store.Identity{}}
}

// Link adds an identity, as a login or a link would have.
func (f *FakeStore) Link(i store.Identity) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.identities[[2]string{i.Provider, i.Subject}] = i
}

// Identity reads one back.
func (f *FakeStore) Identity(provider, subject string) (store.Identity, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i, ok := f.identities[[2]string{provider, subject}]
	return i, ok
}

// CreateIdentityProof implements proof.Store.
func (f *FakeStore) CreateIdentityProof(_ context.Context, p store.IdentityProof) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, q := range f.proofs {
		if bytes.Equal(q.CodeHash, p.CodeHash) {
			return store.ErrProofCodeExists
		}
	}
	for id, q := range f.proofs {
		if q.UserID == p.UserID && q.Channel == p.Channel && q.UsedAt == nil {
			at := p.CreatedAt
			q.UsedAt, q.Result = &at, store.ProofReplaced
			f.proofs[id] = q
		}
	}
	f.proofs[p.ID] = p
	return nil
}

// GetIdentityProof implements proof.Store.
func (f *FakeStore) GetIdentityProof(_ context.Context, id uuid.UUID) (store.IdentityProof, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.proofs[id]
	if !ok {
		return p, store.ErrNotFound
	}
	return p, nil
}

// RedeemIdentityProof implements proof.Store with store.Store's rules.
func (f *FakeStore) RedeemIdentityProof(_ context.Context, codeHash []byte, r store.ProofRedemption) (store.ProofRedeemed, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var p store.IdentityProof
	found := false
	for _, q := range f.proofs {
		if bytes.Equal(q.CodeHash, codeHash) {
			p, found = q, true
		}
	}
	switch {
	case !found:
		return store.ProofRedeemed{}, store.ErrNotFound
	case p.UsedAt != nil:
		return store.ProofRedeemed{}, store.ErrProofUsed
	case !r.At.Before(p.ExpiresAt):
		return store.ProofRedeemed{}, store.ErrProofExpired
	case p.Channel != r.Channel || p.Scope != r.Scope || p.Provider != r.Provider || (p.ExpectSubject != "" && p.ExpectSubject != r.Subject) ||
		(r.UserID != nil && *r.UserID != p.UserID):
		return store.ProofRedeemed{}, store.ErrProofMismatch
	}
	key := [2]string{r.Provider, r.Subject}
	ident, linked := f.identities[key]
	result, out := store.ProofProven, store.ProofRedeemed{}
	switch {
	case linked && ident.UserID != p.UserID:
		result = store.ProofTaken
	case linked:
		ident.VerificationMethod, ident.VerifiedAt = r.Method, r.At
		if r.DisplayName != "" {
			ident.DisplayName = r.DisplayName
		}
		f.identities[key] = ident
		f.Events = append(f.Events, store.IdentityEventProven+" "+r.Provider+" "+r.Subject)
	default:
		ident = store.Identity{Provider: r.Provider, Subject: r.Subject, UserID: p.UserID, DisplayName: r.DisplayName,
			VerificationMethod: r.Method, VerifiedAt: r.At, LinkedAt: r.At}
		f.identities[key] = ident
		f.Events = append(f.Events, store.IdentityEventLinked+" "+r.Provider+" "+r.Subject)
		out.Linked = true
	}
	at := r.At
	p.UsedAt, p.Result, p.ProvenSubject = &at, result, r.Subject
	f.proofs[p.ID] = p
	if result == store.ProofTaken {
		return store.ProofRedeemed{Proof: p}, store.ErrIdentityExists
	}
	out.Proof, out.Identity = p, ident
	return out, nil
}

// DeleteIdentityProofs implements proof.Store.
func (f *FakeStore) DeleteIdentityProofs(_ context.Context, before time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int64
	for id, p := range f.proofs {
		if p.ExpiresAt.Before(before) {
			delete(f.proofs, id)
			n++
		}
	}
	return n, nil
}

// ListIdentities implements proof.Store.
func (f *FakeStore) ListIdentities(_ context.Context, userID uuid.UUID) ([]store.Identity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.Identity
	for _, i := range f.identities {
		if i.UserID == userID {
			out = append(out, i)
		}
	}
	return out, nil
}

// Backend is a proof store under test and how to set its users up.
type Backend struct {
	Store proof.Store
	// NewUser makes a member with these identities linked (their UserID is ignored).
	NewUser func(t testing.TB, idents ...store.Identity) uuid.UUID
	// Identity reads an identity back; false when nothing has it.
	Identity func(t testing.TB, provider, subject string) (store.Identity, bool)
}

// Bio is a Checker channel for the suite: accounts at the provider "rsi" show what Shown says.
type Bio struct {
	mu    sync.Mutex
	Shown map[string]string // subject → text in the bio
}

// Name implements proof.Channel.
func (*Bio) Name() string { return "bio" }

// Provider implements proof.Channel.
func (*Bio) Provider() string { return "rsi" }

// Method implements proof.Channel.
func (*Bio) Method() string { return "bio" }

// Code implements proof.Channel.
func (*Bio) Code(text string) (string, bool) { return strings.TrimSpace(text), text != "" }

// Instructions implements proof.Channel.
func (*Bio) Instructions(code string) string { return "Put " + code + " in your bio." }

// Show puts text in an account's bio.
func (b *Bio) Show(subject, text string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.Shown == nil {
		b.Shown = map[string]string{}
	}
	b.Shown[subject] = text
}

// Check implements proof.Checker.
func (b *Bio) Check(_ context.Context, subject, code string) (proof.Account, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !strings.Contains(b.Shown[subject], code) {
		return proof.Account{}, proof.ErrNotShown
	}
	return proof.Account{Subject: subject, DisplayName: "Citizen " + subject}, nil
}

// Run is the conformance suite: a fresh backend per case, game chat on Steam and a bio checker.
func Run(t *testing.T, newBackend func(t *testing.T) Backend) {
	type rig struct {
		Backend
		svc  *proof.Service
		bio  *Bio
		now  *time.Time
		ctx  context.Context
		chat proof.Channel
	}
	setup := func(t *testing.T) *rig {
		b := newBackend(t)
		now := time.Date(2026, 10, 13, 18, 0, 0, 0, time.UTC)
		r := &rig{Backend: b, bio: &Bio{}, now: &now, ctx: context.Background(), chat: proof.GameChat{ProviderName: "steam"}}
		r.svc = proof.New(b.Store, slog.New(slog.NewTextHandler(io.Discard, nil)), []proof.Channel{r.chat, r.bio},
			proof.WithClock(func() time.Time { return *r.now }))
		return r
	}
	start := func(t *testing.T, r *rig, user uuid.UUID, scope, expect string) proof.Started {
		t.Helper()
		s, err := r.svc.Start(r.ctx, user, proof.GameChatName, scope, expect)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	say := func(r *rig, scope, subject, text string) (proof.Outcome, error) {
		return r.svc.Present(r.ctx, proof.Evidence{Channel: proof.GameChatName, Scope: scope, Account: proof.Account{Subject: subject, DisplayName: "Player " + subject}, Text: text})
	}
	const s1, s2 = "76561190000000001", "76561190000000002"

	t.Run("a code links the account that typed it, once", func(t *testing.T) {
		r := setup(t)
		a := r.NewUser(t, store.Identity{Provider: "discord", Subject: "1"})
		st := start(t, r, a, "cs2-1", "")
		if len(st.Code) != 9 || st.Code[4] != '-' || !strings.Contains(st.Instructions, "!link "+st.Code) || !st.ExpiresAt.Equal(r.now.Add(proof.TTL)) {
			t.Fatalf("started %+v", st)
		}
		out, err := say(r, "cs2-1", s1, "!link "+st.Code)
		if err != nil || out.UserID != a || !out.Linked || out.Identity.Subject != s1 {
			t.Fatalf("present: %+v %v", out, err)
		}
		if i, ok := r.Identity(t, "steam", s1); !ok || i.UserID != a || i.VerificationMethod != proof.GameChatName || i.DisplayName != "Player "+s1 {
			t.Errorf("the identity: %+v %v", i, ok)
		}
		if p, err := r.svc.Get(r.ctx, a, st.ID); err != nil || p.State != store.ProofProven || p.ProvenSubject != s1 {
			t.Errorf("get: %+v %v", p, err)
		}
		if _, err := say(r, "cs2-1", s1, "!link "+st.Code); !errors.Is(err, proof.ErrBadCode) {
			t.Errorf("a replay: %v", err)
		}
	})

	t.Run("as typed: lower case, no hyphen, the dot form", func(t *testing.T) {
		r := setup(t)
		a := r.NewUser(t, store.Identity{Provider: "discord", Subject: "1"})
		st := start(t, r, a, "cs2-1", "")
		if _, err := say(r, "cs2-1", s1, "  .LINK "+strings.ToLower(strings.ReplaceAll(st.Code, "-", ""))+" "); err != nil {
			t.Errorf("present: %v", err)
		}
		for _, text := range []string{"hello", "gg !link " + st.Code, "!link", "!link ABC"} {
			if _, err := say(r, "cs2-1", s2, text); !errors.Is(err, proof.ErrNoCode) {
				t.Errorf("%q: %v", text, err)
			}
		}
	})

	t.Run("an account linked already is proven again", func(t *testing.T) {
		r := setup(t)
		a := r.NewUser(t, store.Identity{Provider: "discord", Subject: "1"}, store.Identity{Provider: "steam", Subject: s1, VerificationMethod: "openid"})
		st := start(t, r, a, "cs2-1", s1)
		if _, err := say(r, "cs2-1", s2, "!link "+st.Code); !errors.Is(err, proof.ErrBadCode) {
			t.Errorf("from another account: %v", err)
		}
		out, err := say(r, "cs2-1", s1, "!link "+st.Code)
		if err != nil || out.Linked || out.UserID != a {
			t.Fatalf("present: %+v %v", out, err)
		}
		if i, _ := r.Identity(t, "steam", s1); i.VerificationMethod != proof.GameChatName || !i.VerifiedAt.Equal(*r.now) {
			t.Errorf("proven again: %+v", i)
		}
		if _, err := r.svc.Start(r.ctx, a, proof.GameChatName, "cs2-1", s2); !errors.Is(err, proof.ErrNotLinked) {
			t.Errorf("expecting an account the member hasn't linked: %v", err)
		}
	})

	t.Run("another member's account stays theirs", func(t *testing.T) {
		r := setup(t)
		b := r.NewUser(t, store.Identity{Provider: "discord", Subject: "2"}, store.Identity{Provider: "steam", Subject: s1, VerificationMethod: "openid"})
		a := r.NewUser(t, store.Identity{Provider: "discord", Subject: "1"})
		st := start(t, r, a, "cs2-1", "")
		out, err := say(r, "cs2-1", s1, "!link "+st.Code)
		if !errors.Is(err, identity.ErrIdentityTaken) || out.UserID != a || out.Identity.UserID != uuid.Nil {
			t.Fatalf("present: %+v %v", out, err)
		}
		if i, _ := r.Identity(t, "steam", s1); i.UserID != b || i.VerificationMethod != "openid" {
			t.Errorf("the account moved or changed: %+v", i)
		}
		if p, _ := r.svc.Get(r.ctx, a, st.ID); p.State != store.ProofTaken {
			t.Errorf("state %q", p.State)
		}
		if _, err := say(r, "cs2-1", s2, "!link "+st.Code); !errors.Is(err, proof.ErrBadCode) {
			t.Errorf("the code is spent: %v", err)
		}
	})

	t.Run("a code is good for its server and ten minutes", func(t *testing.T) {
		r := setup(t)
		a := r.NewUser(t, store.Identity{Provider: "discord", Subject: "1"})
		st := start(t, r, a, "cs2-1", "")
		if _, err := say(r, "cs2-2", s1, "!link "+st.Code); !errors.Is(err, proof.ErrBadCode) {
			t.Errorf("on another server: %v", err)
		}
		*r.now = r.now.Add(proof.TTL)
		if _, err := say(r, "cs2-1", s1, "!link "+st.Code); !errors.Is(err, proof.ErrBadCode) {
			t.Errorf("at ten minutes: %v", err)
		}
		if p, _ := r.svc.Get(r.ctx, a, st.ID); p.State != "expired" {
			t.Errorf("state %q", p.State)
		}
		if _, err := say(r, "cs2-1", "", "!link "+st.Code); !errors.Is(err, proof.ErrBadCode) {
			t.Errorf("a line with no account: %v", err)
		}
	})

	t.Run("a new code replaces the open one", func(t *testing.T) {
		r := setup(t)
		a := r.NewUser(t, store.Identity{Provider: "discord", Subject: "1"})
		first := start(t, r, a, "cs2-1", "")
		second := start(t, r, a, "cs2-1", "")
		if _, err := say(r, "cs2-1", s1, "!link "+first.Code); !errors.Is(err, proof.ErrBadCode) {
			t.Errorf("the first: %v", err)
		}
		if p, _ := r.svc.Get(r.ctx, a, first.ID); p.State != store.ProofReplaced {
			t.Errorf("the first's state %q", p.State)
		}
		if _, err := say(r, "cs2-1", s1, "!link "+second.Code); err != nil {
			t.Errorf("the second: %v", err)
		}
	})

	t.Run("only the member sees their proof", func(t *testing.T) {
		r := setup(t)
		a := r.NewUser(t, store.Identity{Provider: "discord", Subject: "1"})
		b := r.NewUser(t, store.Identity{Provider: "discord", Subject: "2"})
		st := start(t, r, a, "cs2-1", "")
		for _, c := range []struct {
			user, id uuid.UUID
		}{{b, st.ID}, {a, uuid.New()}} {
			if _, err := r.svc.Get(r.ctx, c.user, c.id); !errors.Is(err, proof.ErrNotFound) {
				t.Errorf("get: %v", err)
			}
		}
		if _, err := r.svc.Start(r.ctx, a, "nope", "x", ""); !errors.Is(err, proof.ErrUnknownChannel) {
			t.Errorf("unknown channel: %v", err)
		}
	})

	t.Run("rate limits: codes per member, tries per account and per server", func(t *testing.T) {
		r := setup(t)
		a := r.NewUser(t, store.Identity{Provider: "discord", Subject: "1"})
		for range proof.CodesPerHour {
			start(t, r, a, "cs2-1", "")
		}
		var tm *proof.TooManyError
		if _, err := r.svc.Start(r.ctx, a, proof.GameChatName, "cs2-1", ""); !errors.As(err, &tm) || tm.RetryAfter <= 0 {
			t.Errorf("one code too many: %v", err)
		}
		for i := range proof.TriesPerAccount {
			if _, err := say(r, "cs2-1", s1, "!link 0000-000"+string(rune('0'+i%10))); !errors.Is(err, proof.ErrBadCode) {
				t.Fatalf("try %d: %v", i, err)
			}
		}
		if _, err := say(r, "cs2-1", s1, "!link 0000-0000"); !errors.As(err, &tm) {
			t.Errorf("one try too many for the account: %v", err)
		}
		if _, err := say(r, "cs2-2", s1, "!link 0000-0000"); !errors.Is(err, proof.ErrBadCode) {
			t.Errorf("the same account on another server has its own tries: %v", err)
		}
		for i := range proof.TriesPerScope - proof.TriesPerAccount {
			_, _ = say(r, "cs2-1", "7656119000000"+string(rune('a'+i%26))+string(rune('a'+i/26)), "!link 0000-0000")
		}
		if _, err := say(r, "cs2-1", "76561190000009999", "!link 0000-0000"); !errors.As(err, &tm) {
			t.Errorf("one try too many for the server: %v", err)
		}
		if _, err := say(r, "cs2-1", s2, "hello"); !errors.Is(err, proof.ErrNoCode) {
			t.Errorf("chat without a code is not a try: %v", err)
		}
	})

	t.Run("two accounts typing one code: one wins", func(t *testing.T) {
		r := setup(t)
		a := r.NewUser(t, store.Identity{Provider: "discord", Subject: "1"})
		st := start(t, r, a, "cs2-1", "")
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for i, sub := range []string{s1, s2} {
			wg.Go(func() { _, errs[i] = say(r, "cs2-1", sub, "!link "+st.Code) })
		}
		wg.Wait()
		ok := slices.IndexFunc(errs, func(e error) bool { return e == nil })
		bad := slices.IndexFunc(errs, func(e error) bool { return errors.Is(e, proof.ErrBadCode) })
		if ok < 0 || bad < 0 {
			t.Errorf("results %v: one linked, one refused", errs)
		}
	})

	t.Run("a checked channel: the member brings the code back", func(t *testing.T) {
		r := setup(t)
		a := r.NewUser(t, store.Identity{Provider: "discord", Subject: "1"})
		b := r.NewUser(t, store.Identity{Provider: "discord", Subject: "2"})
		st, err := r.svc.Start(r.ctx, a, "bio", "", "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.svc.Check(r.ctx, a, "bio", "", "Handle", st.Code); !errors.Is(err, proof.ErrNotShown) {
			t.Errorf("not in the bio yet: %v", err)
		}
		r.bio.Show("Handle", "Fly safe. "+st.Code)
		if _, err := r.svc.Check(r.ctx, b, "bio", "", "Handle", st.Code); !errors.Is(err, proof.ErrBadCode) {
			t.Errorf("another member with the code: %v", err)
		}
		out, err := r.svc.Check(r.ctx, a, "bio", "", "Handle", st.Code)
		if err != nil || !out.Linked || out.Identity.Provider != "rsi" || out.Identity.DisplayName != "Citizen Handle" {
			t.Errorf("checked: %+v %v", out, err)
		}
		if _, err := r.svc.Check(r.ctx, a, proof.GameChatName, "cs2-1", s1, st.Code); !errors.Is(err, proof.ErrUnknownChannel) {
			t.Errorf("game chat is presented, not checked: %v", err)
		}
	})

	t.Run("prune keeps a month of expired proofs", func(t *testing.T) {
		r := setup(t)
		a := r.NewUser(t, store.Identity{Provider: "discord", Subject: "1"})
		st := start(t, r, a, "cs2-1", "")
		*r.now = r.now.Add(proof.TTL + proof.Keep - time.Minute)
		if n, err := r.svc.Prune(r.ctx); err != nil || n != 0 {
			t.Errorf("prune before a month: %d %v", n, err)
		}
		*r.now = r.now.Add(2 * time.Minute)
		if n, err := r.svc.Prune(r.ctx); err != nil || n != 1 {
			t.Errorf("prune: %d %v", n, err)
		}
		if _, err := r.svc.Get(r.ctx, a, st.ID); !errors.Is(err, proof.ErrNotFound) {
			t.Errorf("pruned: %v", err)
		}
	})
}

// FakeBackend is the fake store as a Backend.
func FakeBackend(t *testing.T) Backend {
	f := NewFakeStore()
	return Backend{
		Store: f,
		NewUser: func(_ testing.TB, idents ...store.Identity) uuid.UUID {
			id := uuid.New()
			for _, i := range idents {
				i.UserID = id
				f.Link(i)
			}
			return id
		},
		Identity: func(_ testing.TB, provider, subject string) (store.Identity, bool) {
			return f.Identity(provider, subject)
		},
	}
}
