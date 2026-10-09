package identity_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gravel-project/gravel/internal/identity"
	"github.com/gravel-project/gravel/internal/identity/identitytest"
	"github.com/gravel-project/gravel/internal/store"
)

func TestListUsersAndEvents(t *testing.T) {
	st := identitytest.NewFakeStore()
	orgID := uuid.New()
	svc := identity.New(st, orgID, nil, time.Minute, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()
	base := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	var users []store.User
	for i, name := range []string{"A", "B", "C"} {
		u, err := st.RegisterUser(ctx, store.User{ID: uuid.New(), OrganizationID: orgID, DisplayName: name, CreatedAt: base.Add(time.Duration(i) * time.Minute)},
			store.Identity{Provider: "discord", Subject: name, VerificationMethod: identity.MethodOAuth2, VerifiedAt: base})
		if err != nil {
			t.Fatal(err)
		}
		u.CreatedAt = base.Add(time.Duration(i) * time.Minute)
		st.Users[u.ID] = u
		users = append(users, u)
	}
	// A user of another organization is not listed.
	if _, err := st.RegisterUser(ctx, store.User{ID: uuid.New(), OrganizationID: uuid.New(), DisplayName: "Z"}, store.Identity{Provider: "discord", Subject: "Z", VerifiedAt: base}); err != nil {
		t.Fatal(err)
	}
	if err := st.LinkIdentity(ctx, store.Identity{UserID: users[1].ID, Provider: "steam", Subject: "s", VerifiedAt: base, LinkedAt: base.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}

	page, err := svc.ListUsers(ctx, nil, 2)
	if err != nil || len(page) != 2 || page[0].ID != users[0].ID || page[1].ID != users[1].ID || len(page[1].Identities) != 2 {
		t.Fatalf("first page: %v %+v", err, page)
	}
	rest, err := svc.ListUsers(ctx, &store.UserCursor{CreatedAt: page[1].CreatedAt, ID: page[1].ID}, 2)
	if err != nil || len(rest) != 1 || rest[0].ID != users[2].ID || len(rest[0].Identities) != 1 {
		t.Fatalf("second page: %v %+v", err, rest)
	}
	if empty, err := svc.ListUsers(ctx, &store.UserCursor{CreatedAt: rest[0].CreatedAt, ID: rest[0].ID}, 2); err != nil || len(empty) != 0 {
		t.Errorf("past the end: %v %+v", err, empty)
	}

	events, err := svc.ListEvents(ctx, 0, 10)
	if err != nil || len(events) != 5 || events[0].ID != 1 || events[4].Event != store.IdentityEventLinked {
		t.Fatalf("events: %v %+v", err, events)
	}
	if later, err := svc.ListEvents(ctx, 3, 1); err != nil || len(later) != 1 || later[0].ID != 4 {
		t.Errorf("after 3, limit 1: %v %+v", err, later)
	}
	st.ErrOn = "ListIdentitiesForUsers"
	if _, err := svc.ListUsers(ctx, nil, 2); err == nil {
		t.Error("a store failure surfaces")
	}
}
