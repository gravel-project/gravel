package api_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
	"github.com/gravel-project/gravel/gen/gravel/hub/v1/hubv1connect"
	"github.com/gravel-project/gravel/internal/identity"
	"github.com/gravel-project/gravel/internal/store"
)

func withLocalLinks(r *rig) identity.Option {
	return func(s *identity.Service) {
		for _, o := range []identity.Option{
			identity.WithLocalAccounts(r.idSt, func(context.Context) (identity.LocalPolicy, error) { return identity.LocalPolicy{}, nil }),
			identity.WithPasswordLinks(r.idSt, nil),
			identity.WithPasswordParams(identity.PasswordParams{MemoryKiB: 64, Passes: 1, Lanes: 1, SaltLen: 16, KeyLen: 32}),
		} {
			o(s)
		}
	}
}

// Staff with members:manage make a local account and get its set-password link; anyone else is
// refused (gravel#187).
func TestCreateLocalAccountAndReset(t *testing.T) {
	r := newRig(t, withLocalLinks)
	ctx := context.Background()
	owner := r.user(t, "Owner", "1")
	if _, err := r.org.Claim(ctx, r.token, store.Actor{UserID: &owner.ID}); err != nil {
		t.Fatal(err)
	}
	mod := r.user(t, "Mod", "2")
	r.grant(t, owner.ID, mod.ID, store.RoleModerator)
	asOwner := hubv1connect.NewIdentityServiceClient(cookieClient(http.DefaultClient, r.login(t, owner.ID)), r.srv.URL)
	asMod := hubv1connect.NewIdentityServiceClient(cookieClient(http.DefaultClient, r.login(t, mod.ID)), r.srv.URL)

	if _, err := asMod.CreateLocalAccount(ctx, connect.NewRequest(&hubv1.CreateLocalAccountRequest{Username: "vps.dad"})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("moderator: %v", err)
	}
	got, err := asOwner.CreateLocalAccount(ctx, connect.NewRequest(&hubv1.CreateLocalAccountRequest{Username: "VPS.Dad", Email: "dad@example.com"}))
	if err != nil {
		t.Fatal(err)
	}
	l := got.Msg.GetLink()
	if !strings.HasPrefix(l.GetUrl(), "https://hub.example.com/auth/local/reset/") || l.GetPurpose() != "set" || got.Msg.GetUser().GetDisplayName() != "vps.dad" ||
		time.Until(l.GetExpiresAt().AsTime()) > time.Hour || time.Until(l.GetExpiresAt().AsTime()) < 59*time.Minute {
		t.Fatalf("created: %v", got.Msg)
	}
	if ids := got.Msg.GetUser().GetIdentities(); len(ids) != 1 || ids[0].GetProvider() != "local" || ids[0].GetSubject() != "vps.dad" {
		t.Errorf("identities: %v", ids)
	}
	if _, err := asOwner.CreateLocalAccount(ctx, connect.NewRequest(&hubv1.CreateLocalAccountRequest{Username: "vps.dad"})); connect.CodeOf(err) != connect.CodeAlreadyExists {
		t.Errorf("taken: %v", err)
	}
	if _, err := asOwner.CreateLocalAccount(ctx, connect.NewRequest(&hubv1.CreateLocalAccountRequest{Username: "admin"})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("reserved: %v", err)
	}
	reset, err := asOwner.IssuePasswordReset(ctx, connect.NewRequest(&hubv1.IssuePasswordResetRequest{Username: "vps.dad"}))
	if err != nil || reset.Msg.GetLink().GetPurpose() != "reset" || reset.Msg.GetLink().GetUrl() == l.GetUrl() {
		t.Fatalf("reset: %v %v", reset, err)
	}
	if _, err := asOwner.IssuePasswordReset(ctx, connect.NewRequest(&hubv1.IssuePasswordResetRequest{Username: "nobody"})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("unknown: %v", err)
	}
	if _, err := asMod.IssuePasswordReset(ctx, connect.NewRequest(&hubv1.IssuePasswordResetRequest{Username: "vps.dad"})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("moderator reset: %v", err)
	}
}

// GetMe carries the recovery address of the caller's own local account to their session, never to
// an app acting for them (gravel#234).
func TestGetMeRecoveryAddress(t *testing.T) {
	r := newRig(t, withLocalLinks)
	ctx := context.Background()
	u, _, err := r.ids.CreateLocal(ctx, "vps.dad", "dad@example.com", nil)
	if err != nil {
		t.Fatal(err)
	}
	local := func(c hubv1connect.IdentityServiceClient) *hubv1.Identity {
		t.Helper()
		me, err := c.GetMe(ctx, connect.NewRequest(&hubv1.GetMeRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		for _, i := range me.Msg.GetUser().GetIdentities() {
			if i.GetProvider() == "local" {
				return i
			}
		}
		t.Fatalf("no local identity: %v", me.Msg)
		return nil
	}
	session := hubv1connect.NewIdentityServiceClient(cookieClient(http.DefaultClient, r.login(t, u.ID)), r.srv.URL)
	if i := local(session); i.GetRecoveryEmail() != "dad@example.com" || i.GetRecoveryEmailVerifiedAt() != nil {
		t.Errorf("unproved: %v", i)
	}
	at := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	c := r.idSt.Locals["vps.dad"]
	c.EmailVerifiedAt = &at
	r.idSt.Locals["vps.dad"] = c
	if i := local(session); i.GetRecoveryEmail() != "dad@example.com" || !i.GetRecoveryEmailVerifiedAt().AsTime().Equal(at) {
		t.Errorf("proved: %v", i)
	}
	app := hubv1connect.NewIdentityServiceClient(memberClient(r.memberToken(t, u.ID, nil)), r.srv.URL)
	if i := local(app); i.GetRecoveryEmail() != "" || i.GetRecoveryEmailVerifiedAt() != nil {
		t.Errorf("an app sees the address: %v", i)
	}
}
