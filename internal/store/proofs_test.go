package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gravel-project/gravel/internal/identity/proof/prooftest"
	"github.com/gravel-project/gravel/internal/store"
	"github.com/gravel-project/gravel/internal/store/storetest"
)

// The proof-of-control conformance suite (gravel#5) on Postgres: the same rules as the fake, with
// the row lock deciding the race of two accounts typing one code.
func TestIdentityProofsConformance(t *testing.T) {
	st := storetest.Open(t)
	prooftest.Run(t, func(t *testing.T) prooftest.Backend {
		storetest.Reset(t, st)
		o := newOrg(t, st)
		return prooftest.Backend{
			Store: st,
			NewUser: func(t testing.TB, idents ...store.Identity) uuid.UUID {
				t.Helper()
				ctx := context.Background()
				now := time.Now().UTC().Truncate(time.Microsecond)
				first := idents[0]
				first.VerifiedAt = now
				if first.VerificationMethod == "" {
					first.VerificationMethod = "oauth2"
				}
				u, err := st.RegisterUser(ctx, store.User{ID: uuid.New(), OrganizationID: o.ID, DisplayName: "Member"}, first)
				if err != nil {
					t.Fatal(err)
				}
				for _, i := range idents[1:] {
					i.UserID, i.VerifiedAt = u.ID, now
					if err := st.LinkIdentity(ctx, i); err != nil {
						t.Fatal(err)
					}
				}
				return u.ID
			},
			Identity: func(t testing.TB, provider, subject string) (store.Identity, bool) {
				i, err := st.GetIdentity(context.Background(), provider, subject)
				if errors.Is(err, store.ErrNotFound) {
					return i, false
				}
				if err != nil {
					t.Fatal(err)
				}
				return i, true
			},
		}
	})
}

// A proven account writes its identity event: linked for a new link, proven when proven again.
func TestIdentityProofEvents(t *testing.T) {
	st := storetest.Open(t)
	storetest.Reset(t, st)
	ctx := context.Background()
	o := newOrg(t, st)
	now := time.Now().UTC().Truncate(time.Microsecond)
	u, err := st.RegisterUser(ctx, store.User{ID: uuid.New(), OrganizationID: o.ID, DisplayName: "Jo"}, ident("discord", "1", now))
	if err != nil {
		t.Fatal(err)
	}
	head, err := st.IdentityEventsHead(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i, hash := range [][]byte{[]byte("h1-xxxxxxxxxxxxxxxxxxxxxxxxxxxxx"), []byte("h2-xxxxxxxxxxxxxxxxxxxxxxxxxxxxx")} {
		at := now.Add(time.Duration(i) * time.Second)
		if err := st.CreateIdentityProof(ctx, store.IdentityProof{ID: uuid.New(), UserID: u.ID, Channel: "game-chat", Scope: "cs2-1", Provider: "steam",
			CodeHash: hash, CreatedAt: at, ExpiresAt: at.Add(10 * time.Minute)}); err != nil {
			t.Fatal(err)
		}
		r, err := st.RedeemIdentityProof(ctx, hash, store.ProofRedemption{Channel: "game-chat", Scope: "cs2-1", Provider: "steam", Subject: "765",
			DisplayName: "Jo in game", Method: "game-chat", At: at})
		if err != nil || r.Linked != (i == 0) || r.Proof.Result != store.ProofProven || r.Identity.UserID != u.ID {
			t.Fatalf("redeem %d: %+v %v", i, r, err)
		}
	}
	ev, err := st.ListIdentityEventsAfter(ctx, head, 10)
	if err != nil || len(ev) != 2 || ev[0].Event != store.IdentityEventLinked || ev[1].Event != store.IdentityEventProven || ev[1].Subject != "765" {
		t.Errorf("events %+v %v", ev, err)
	}
	if err := st.CreateIdentityProof(ctx, store.IdentityProof{ID: uuid.New(), UserID: u.ID, Channel: "game-chat", Provider: "steam",
		CodeHash: []byte("h1-xxxxxxxxxxxxxxxxxxxxxxxxxxxxx"), CreatedAt: now, ExpiresAt: now}); !errors.Is(err, store.ErrProofCodeExists) {
		t.Errorf("a code hash twice: %v", err)
	}
}
