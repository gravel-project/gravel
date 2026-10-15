package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
)

const (
	aliceID = "8d4c0f5e-0000-4000-8000-000000000001"
	bobID   = "8d4c0f5e-0000-4000-8000-000000000002"
)

// adminState is what the fake hub's operator procedures answer from and record.
type adminState struct {
	users     []*hubv1.User
	pending   string // the nominee's id
	apps      []*hubv1.App
	settings  string // the stored settings manifest
	servers   string // the stored servers manifest
	notifiers map[string]string
	calls     []string // each procedure and what it was asked, in order
}

func newAdminState() *adminState {
	return &adminState{
		users: []*hubv1.User{
			{Id: aliceID, DisplayName: "Alice", Owner: true, LastLoginAt: timestamppb.New(t0),
				Identities: []*hubv1.Identity{{Provider: "local", Subject: "alice"}}},
			{Id: bobID, DisplayName: "Bob",
				Grants:     []*hubv1.RoleGrant{{Role: "operator", GameId: "cs2"}},
				Identities: []*hubv1.Identity{{Provider: "local", Subject: "bob"}, {Provider: "discord", Subject: "222"}}},
		},
		apps: []*hubv1.App{
			{ClientId: "gravel_bot", Name: "HTG Bot", Scopes: []string{"identity:read", "roles:grant"}, CreatedAt: timestamppb.New(t0.Add(-48 * time.Hour))},
			{ClientId: "gravel_old", Name: "Muster", Scopes: []string{"stats:read"}, RedirectUris: []string{"https://muster.example/cb"},
				CreatedAt: timestamppb.New(t0.Add(-72 * time.Hour)), RevokedAt: timestamppb.New(t0.Add(-24 * time.Hour))},
		},
		settings:  "version: 1\ntheme: {}\n",
		servers:   "games:\n    - id: wardogs\nservers:\n    - id: wd-1\n      name: War Dogs\n      endpoint: http://10.0.0.5:8080\n",
		notifiers: map[string]string{"john": "", "ops-mail": "smtp: 550 mailbox unavailable"},
	}
}

func (h *fakeHub) record(format string, a ...any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.adm.calls = append(h.adm.calls, fmt.Sprintf(format, a...))
}

// staff is a member who may: alice, logged in. An app is refused, as the hub refuses one in person.
func (h *fakeHub) staff(hdr http.Header) error {
	who := h.who(hdr)
	switch {
	case who == "":
		return connect.NewError(connect.CodeUnauthenticated, errors.New("log in first"))
	case strings.HasPrefix(who, "app:"):
		return connect.NewError(connect.CodePermissionDenied, errors.New("needs settings:write in person"))
	}
	return nil
}

func (h *fakeHub) userByID(id string) *hubv1.User {
	for _, u := range h.adm.users {
		if u.GetId() == id {
			return u
		}
	}
	return nil
}

func (h *fakeHub) ListUsers(_ context.Context, req *connect.Request[hubv1.ListUsersRequest]) (*connect.Response[hubv1.ListUsersResponse], error) {
	if err := h.staff(req.Header()); err != nil {
		return nil, err
	}
	// One member a page, so the command must follow the pages.
	i := 0
	if t := req.Msg.GetPageToken(); t != "" {
		_, _ = fmt.Sscanf(t, "p%d", &i)
	}
	res := &hubv1.ListUsersResponse{Users: h.adm.users[i : i+1]}
	if i+1 < len(h.adm.users) {
		res.NextPageToken = fmt.Sprintf("p%d", i+1)
	}
	return connect.NewResponse(res), nil
}

func (h *fakeHub) LookupUser(_ context.Context, req *connect.Request[hubv1.LookupUserRequest]) (*connect.Response[hubv1.LookupUserResponse], error) {
	if err := h.staff(req.Header()); err != nil {
		return nil, err
	}
	for _, u := range h.adm.users {
		for _, id := range u.GetIdentities() {
			if id.GetProvider() == req.Msg.GetProvider() && id.GetSubject() == req.Msg.GetSubject() {
				return connect.NewResponse(&hubv1.LookupUserResponse{User: u}), nil
			}
		}
	}
	return nil, connect.NewError(connect.CodeNotFound, errors.New("no such member"))
}

func (h *fakeHub) SetUserRole(_ context.Context, req *connect.Request[hubv1.SetUserRoleRequest]) (*connect.Response[hubv1.SetUserRoleResponse], error) {
	if err := h.staff(req.Header()); err != nil {
		return nil, err
	}
	m := req.Msg
	h.record("SetUserRole %s %s granted=%t game=%q", m.GetUserId(), m.GetRole(), m.GetGranted(), m.GetGameId())
	u := h.userByID(m.GetUserId())
	if u == nil {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("no such member"))
	}
	g := &hubv1.RoleGrant{Role: m.GetRole(), GameId: m.GetGameId()}
	same := func(x *hubv1.RoleGrant) bool { return x.GetRole() == g.GetRole() && x.GetGameId() == g.GetGameId() }
	u.Grants = slices.DeleteFunc(u.Grants, same)
	if m.GetGranted() {
		u.Grants = append(u.Grants, g)
	}
	return connect.NewResponse(&hubv1.SetUserRoleResponse{User: u}), nil
}

func (h *fakeHub) RevokeUserSessions(_ context.Context, req *connect.Request[hubv1.RevokeUserSessionsRequest]) (*connect.Response[hubv1.RevokeUserSessionsResponse], error) {
	if err := h.staff(req.Header()); err != nil {
		return nil, err
	}
	h.record("RevokeUserSessions %s", req.Msg.GetUserId())
	return connect.NewResponse(&hubv1.RevokeUserSessionsResponse{Revoked: 2}), nil
}

func (h *fakeHub) organizationMsg() *hubv1.Organization {
	o := &hubv1.Organization{Name: "Hidden Token Gaming", Owned: true, OwnerUserId: aliceID, ClaimedAt: timestamppb.New(t0.Add(-time.Hour))}
	if h.adm.pending != "" {
		o.PendingOwnerUserId, o.PendingOwnerName, o.PendingOwnerExpiresAt = h.adm.pending, h.userByID(h.adm.pending).GetDisplayName(), timestamppb.New(t0.Add(7*24*time.Hour))
	}
	return o
}

func (h *fakeHub) GetOrganization(context.Context, *connect.Request[hubv1.GetOrganizationRequest]) (*connect.Response[hubv1.GetOrganizationResponse], error) {
	return connect.NewResponse(&hubv1.GetOrganizationResponse{Organization: h.organizationMsg()}), nil
}

func (h *fakeHub) NominateOwner(_ context.Context, req *connect.Request[hubv1.NominateOwnerRequest]) (*connect.Response[hubv1.NominateOwnerResponse], error) {
	if err := h.staff(req.Header()); err != nil {
		return nil, err
	}
	h.record("NominateOwner %s", req.Msg.GetUserId())
	if h.userByID(req.Msg.GetUserId()) == nil {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("not a member"))
	}
	h.adm.pending = req.Msg.GetUserId()
	return connect.NewResponse(&hubv1.NominateOwnerResponse{Organization: h.organizationMsg()}), nil
}

func (h *fakeHub) AcceptOwnership(_ context.Context, req *connect.Request[hubv1.AcceptOwnershipRequest]) (*connect.Response[hubv1.AcceptOwnershipResponse], error) {
	if err := h.staff(req.Header()); err != nil {
		return nil, err
	}
	// Alice owns the hub; only a nominee may accept, and she is never one here.
	return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("no nomination"))
}

func (h *fakeHub) CancelOwnershipTransfer(_ context.Context, req *connect.Request[hubv1.CancelOwnershipTransferRequest]) (*connect.Response[hubv1.CancelOwnershipTransferResponse], error) {
	if err := h.staff(req.Header()); err != nil {
		return nil, err
	}
	if h.adm.pending == "" {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("no nomination"))
	}
	h.adm.pending = ""
	return connect.NewResponse(&hubv1.CancelOwnershipTransferResponse{Organization: h.organizationMsg()}), nil
}

func (h *fakeHub) CreateLocalAccount(_ context.Context, req *connect.Request[hubv1.CreateLocalAccountRequest]) (*connect.Response[hubv1.CreateLocalAccountResponse], error) {
	if err := h.staff(req.Header()); err != nil {
		return nil, err
	}
	h.record("CreateLocalAccount %s email=%q", req.Msg.GetUsername(), req.Msg.GetEmail())
	// A hub without auth.base_url answers the path only.
	return connect.NewResponse(&hubv1.CreateLocalAccountResponse{Link: &hubv1.PasswordLink{Url: "/auth/local/reset/tok-set", Purpose: "set", ExpiresAt: timestamppb.New(t0.Add(time.Hour))}}), nil
}

func (h *fakeHub) IssuePasswordReset(_ context.Context, req *connect.Request[hubv1.IssuePasswordResetRequest]) (*connect.Response[hubv1.IssuePasswordResetResponse], error) {
	if err := h.staff(req.Header()); err != nil {
		return nil, err
	}
	return connect.NewResponse(&hubv1.IssuePasswordResetResponse{Link: &hubv1.PasswordLink{Url: "https://hub.example.org/auth/local/reset/tok-reset", Purpose: "reset", ExpiresAt: timestamppb.New(t0.Add(time.Hour))}}), nil
}

func (h *fakeHub) CreateApp(_ context.Context, req *connect.Request[hubv1.CreateAppRequest]) (*connect.Response[hubv1.CreateAppResponse], error) {
	if err := h.staff(req.Header()); err != nil {
		return nil, err
	}
	m := req.Msg
	h.record("CreateApp %s scopes=%v redirects=%v", m.GetName(), m.GetScopes(), m.GetRedirectUris())
	app := &hubv1.App{ClientId: "gravel_new", Name: m.GetName(), Scopes: m.GetScopes(), RedirectUris: m.GetRedirectUris(), CreatedAt: timestamppb.New(t0)}
	return connect.NewResponse(&hubv1.CreateAppResponse{App: app, ClientSecret: "shown-once"}), nil
}

func (h *fakeHub) ListApps(_ context.Context, req *connect.Request[hubv1.ListAppsRequest]) (*connect.Response[hubv1.ListAppsResponse], error) {
	if err := h.staff(req.Header()); err != nil {
		return nil, err
	}
	return connect.NewResponse(&hubv1.ListAppsResponse{Apps: h.adm.apps}), nil
}

func (h *fakeHub) RevokeApp(_ context.Context, req *connect.Request[hubv1.RevokeAppRequest]) (*connect.Response[hubv1.RevokeAppResponse], error) {
	if err := h.staff(req.Header()); err != nil {
		return nil, err
	}
	for _, a := range h.adm.apps {
		if a.GetClientId() == req.Msg.GetClientId() && a.GetRevokedAt() == nil {
			a.RevokedAt = timestamppb.New(t0)
			return connect.NewResponse(&hubv1.RevokeAppResponse{}), nil
		}
	}
	return nil, connect.NewError(connect.CodeNotFound, errors.New("no such app"))
}

// The fake's manifests: a document is invalid when it says "bad:", and what differs from the
// stored one is the "sections" (settings) or "changes" (servers) a line starting "# change "
// names.
func manifestCheck(text string) error {
	if strings.Contains(text, "bad:") {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("invalid settings: bad: unknown field\ninvalid settings: theme.dark.accent: not a colour"))
	}
	return nil
}

func manifestChanges(text string) []string {
	var out []string
	for line := range strings.SplitSeq(text, "\n") {
		if c, ok := strings.CutPrefix(line, "# change "); ok {
			out = append(out, c)
		}
	}
	return out
}

func (h *fakeHub) ExportSettings(_ context.Context, req *connect.Request[hubv1.ExportSettingsRequest]) (*connect.Response[hubv1.ExportSettingsResponse], error) {
	if err := h.staff(req.Header()); err != nil {
		return nil, err
	}
	return connect.NewResponse(&hubv1.ExportSettingsResponse{Manifest: h.adm.settings}), nil
}

func (h *fakeHub) CheckSettings(_ context.Context, req *connect.Request[hubv1.CheckSettingsRequest]) (*connect.Response[hubv1.CheckSettingsResponse], error) {
	if err := h.staff(req.Header()); err != nil {
		return nil, err
	}
	if err := manifestCheck(req.Msg.GetManifest()); err != nil {
		return nil, err
	}
	return connect.NewResponse(&hubv1.CheckSettingsResponse{Summary: "1 nav links, theme default, discord unmapped"}), nil
}

func (h *fakeHub) ApplySettings(_ context.Context, req *connect.Request[hubv1.ApplySettingsRequest]) (*connect.Response[hubv1.ApplySettingsResponse], error) {
	if err := h.staff(req.Header()); err != nil {
		return nil, err
	}
	m := req.Msg
	h.record("ApplySettings dry_run=%t", m.GetDryRun())
	if err := manifestCheck(m.GetManifest()); err != nil {
		return nil, err
	}
	res := &hubv1.ApplySettingsResponse{Changes: manifestChanges(m.GetManifest()), Summary: "1 nav links, theme default, discord unmapped"}
	if !m.GetDryRun() && len(res.Changes) > 0 {
		h.adm.settings, res.Applied = m.GetManifest(), true
	}
	return connect.NewResponse(res), nil
}

func (h *fakeHub) ExportServers(_ context.Context, req *connect.Request[hubv1.ExportServersRequest]) (*connect.Response[hubv1.ExportServersResponse], error) {
	if err := h.staff(req.Header()); err != nil {
		return nil, err
	}
	return connect.NewResponse(&hubv1.ExportServersResponse{Manifest: h.adm.servers}), nil
}

func (h *fakeHub) CheckServers(_ context.Context, req *connect.Request[hubv1.CheckServersRequest]) (*connect.Response[hubv1.CheckServersResponse], error) {
	if err := h.staff(req.Header()); err != nil {
		return nil, err
	}
	if err := manifestCheck(req.Msg.GetManifest()); err != nil {
		return nil, err
	}
	return connect.NewResponse(&hubv1.CheckServersResponse{Summary: "1 games [wardogs], 1 servers [wd-1]"}), nil
}

func (h *fakeHub) ApplyServers(_ context.Context, req *connect.Request[hubv1.ApplyServersRequest]) (*connect.Response[hubv1.ApplyServersResponse], error) {
	if err := h.staff(req.Header()); err != nil {
		return nil, err
	}
	m := req.Msg
	h.record("ApplyServers dry_run=%t", m.GetDryRun())
	if err := manifestCheck(m.GetManifest()); err != nil {
		return nil, err
	}
	if strings.Contains(m.GetManifest(), "credential_file:") {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("only the hub's host changes these, with gravel-hub servers apply (they name files on the host, or where their secrets go): server wd-1: its credential_file"))
	}
	res := &hubv1.ApplyServersResponse{Changes: manifestChanges(m.GetManifest())}
	if !m.GetDryRun() && len(res.Changes) > 0 {
		h.adm.servers, res.Applied = m.GetManifest(), true
	}
	return connect.NewResponse(res), nil
}

func (h *fakeHub) EraseIdentity(_ context.Context, req *connect.Request[hubv1.EraseIdentityRequest]) (*connect.Response[hubv1.EraseIdentityResponse], error) {
	if err := h.staff(req.Header()); err != nil {
		return nil, err
	}
	h.record("EraseIdentity %s %s", req.Msg.GetProvider(), req.Msg.GetSubject())
	return connect.NewResponse(&hubv1.EraseIdentityResponse{MatchRows: 12, MonthlyRows: 3, ParticipationRows: 4, ResultRows: 5, SessionRows: 6, ActivityMonthlyRows: 1, IngestBatches: 2, PseudonymDeleted: true}), nil
}

func (h *fakeHub) TestNotifier(_ context.Context, req *connect.Request[hubv1.TestNotifierRequest]) (*connect.Response[hubv1.TestNotifierResponse], error) {
	if err := h.staff(req.Header()); err != nil {
		return nil, err
	}
	e, ok := h.adm.notifiers[req.Msg.GetName()]
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("no notifier"))
	}
	return connect.NewResponse(&hubv1.TestNotifierResponse{Delivered: e == "", Error: e}), nil
}

// adminRun is one command against the fake hub as alice, logged in; it returns the exit code and
// stdout, failing the test on an unexpected code.
func adminRun(t *testing.T, want int, stdin string, args ...string) (stdout, stderr string) {
	t.Helper()
	code, stdout, stderr := cli{t: t, stdin: stdin}.run(context.Background(), args...)
	if code != want {
		t.Fatalf("gravel %s: exit %d, want %d; stdout %q, stderr %q", strings.Join(args, " "), code, want, stdout, stderr)
	}
	return stdout, stderr
}

func adminHub(t *testing.T) *fakeHub {
	t.Helper()
	isolate(t)
	h := newFakeHub(t)
	login(t, h)
	return h
}

func TestMembers(t *testing.T) {
	h := adminHub(t)
	out, _ := adminRun(t, exitOK, "", "members", "list")
	golden(t, h, "members-list", out)
	if out, _ := adminRun(t, exitOK, "", "members", "list", "--game", "cs2"); !strings.Contains(out, "Bob") || strings.Contains(out, "Alice") {
		t.Errorf("--game cs2:\n%s", out)
	}

	// A member by an identity, a flag after the arguments.
	out, _ = adminRun(t, exitOK, "", "members", "grant", "discord:222", "moderator", "--game", "wardogs")
	if out != "Bob holds moderator of wardogs now (operator of cs2, moderator of wardogs).\n" {
		t.Errorf("grant: %q", out)
	}
	out, _ = adminRun(t, exitOK, "", "members", "revoke", bobID, "operator", "--game=cs2")
	if out != "Bob no longer holds operator of cs2 (moderator of wardogs).\n" {
		t.Errorf("revoke: %q", out)
	}
	out, _ = adminRun(t, exitOK, "", "members", "logout", "local:bob")
	if out != "Ended 2 sessions of local:bob.\n" {
		t.Errorf("logout: %q", out)
	}
	want := []string{
		`SetUserRole ` + bobID + ` moderator granted=true game="wardogs"`,
		`SetUserRole ` + bobID + ` operator granted=false game="cs2"`,
		`RevokeUserSessions ` + bobID,
	}
	if !slices.Equal(h.adm.calls, want) {
		t.Errorf("calls:\n%s\nwant:\n%s", strings.Join(h.adm.calls, "\n"), strings.Join(want, "\n"))
	}

	if _, errOut := adminRun(t, exitFailed, "", "members", "grant", "discord:999", "admin"); !strings.Contains(errOut, "discord:999 is not a member of") {
		t.Errorf("an unknown identity: %q", errOut)
	}
	if _, errOut := adminRun(t, exitUsage, "", "members", "grant", "bob", "admin"); !strings.Contains(errOut, "provider:subject") {
		t.Errorf("a bare name: %q", errOut)
	}
	adminRun(t, exitUsage, "", "members", "grant", "local:bob")
	adminRun(t, exitUsage, "", "members", "promote")
}

func TestOwner(t *testing.T) {
	h := adminHub(t)
	out, _ := adminRun(t, exitOK, "", "owner", "nominate", "local:bob")
	if !strings.HasPrefix(out, "Bob may take Hidden Token Gaming over until 2026-10-21T02:00:00Z") {
		t.Errorf("nominate: %q", out)
	}
	out, _ = adminRun(t, exitOK, "", "owner", "status")
	golden(t, h, "owner-status", out)
	adminRun(t, exitOK, "", "owner", "cancel")
	if _, errOut := adminRun(t, exitFailed, "", "owner", "cancel"); !strings.Contains(errOut, "no nomination to end") {
		t.Errorf("cancel twice: %q", errOut)
	}
	if _, errOut := adminRun(t, exitFailed, "", "owner", "accept"); !strings.Contains(errOut, "no nomination for you") {
		t.Errorf("accept without one: %q", errOut)
	}
}

func TestUsers(t *testing.T) {
	h := adminHub(t)
	out, _ := adminRun(t, exitOK, "", "users", "create-local", "--username", "carol", "--email", "carol@example.org")
	golden(t, h, "users-create-local", out)
	out, _ = adminRun(t, exitOK, "", "users", "reset-password", "--username", "bob")
	if !strings.Contains(out, "https://hub.example.org/auth/local/reset/tok-reset\n") {
		t.Errorf("reset: %q", out)
	}
	adminRun(t, exitUsage, "", "users", "create-local")
	adminRun(t, exitUsage, "", "users", "reset-password", "--username", "bob", "--email", "x@example.org")
}

func TestApps(t *testing.T) {
	h := adminHub(t)
	out, _ := adminRun(t, exitOK, "", "apps", "list")
	golden(t, h, "apps-list", out)
	out, _ = adminRun(t, exitOK, "", "apps", "create", "--name", "Muster", "--scopes", "stats:read, participation:write", "--redirect-uri", "https://a.example/cb", "--redirect-uri", "https://b.example/cb")
	golden(t, h, "apps-create", out)
	if want := "CreateApp Muster scopes=[stats:read participation:write] redirects=[https://a.example/cb https://b.example/cb]"; h.adm.calls[0] != want {
		t.Errorf("asked %q, want %q", h.adm.calls[0], want)
	}
	if out, _ := adminRun(t, exitOK, "", "apps", "revoke", "--client-id", "gravel_bot"); out != "app revoked: gravel_bot (its tokens are gone; no new one will be issued)\n" {
		t.Errorf("revoke: %q", out)
	}
	if _, errOut := adminRun(t, exitFailed, "", "apps", "revoke", "--client-id", "gravel_bot"); !strings.Contains(errOut, "already revoked") {
		t.Errorf("revoke twice: %q", errOut)
	}
	adminRun(t, exitUsage, "", "apps", "create", "--name", "x")
}

func TestSettingsManifest(t *testing.T) {
	h := adminHub(t)
	out, _ := adminRun(t, exitOK, "", "settings", "export")
	if out != h.adm.settings {
		t.Errorf("export: %q", out)
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "organization.yaml")
	if err := os.WriteFile(file, []byte("version: 1\nnav:\n  - {label: Discord, url: https://discord.gg/x}\n# change nav\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, _ := adminRun(t, exitOK, "", "settings", "check", file); out != "settings valid (1 nav links, theme default, discord unmapped)\n" {
		t.Errorf("check: %q", out)
	}
	if out, _ := adminRun(t, exitWouldChange, "", "settings", "apply", file, "--dry-run"); out != "settings would change: nav (1 nav links, theme default, discord unmapped)\n" {
		t.Errorf("dry run: %q", out)
	}
	if out, _ := adminRun(t, exitOK, "", "settings", "apply", file); out != "settings applied: nav (1 nav links, theme default, discord unmapped)\n" {
		t.Errorf("apply: %q", out)
	}
	// From stdin; nothing to change exits 0 on a dry run too.
	if out, _ := adminRun(t, exitOK, "version: 1\n", "settings", "apply", "-", "--dry-run"); out != "settings unchanged (1 nav links, theme default, discord unmapped)\n" {
		t.Errorf("unchanged: %q", out)
	}
	if want := []string{"ApplySettings dry_run=true", "ApplySettings dry_run=false", "ApplySettings dry_run=true"}; !slices.Equal(h.adm.calls, want) {
		t.Errorf("calls %q, want %q", h.adm.calls, want)
	}
	_, errOut := adminRun(t, exitFailed, "bad: 1\n", "settings", "check", "-")
	golden(t, h, "settings-invalid", errOut)
	if _, errOut := adminRun(t, exitFailed, "", "settings", "apply", filepath.Join(dir, "missing.yaml")); !strings.Contains(errOut, "no such file") {
		t.Errorf("a missing file: %q", errOut)
	}
	adminRun(t, exitUsage, "", "settings", "apply")
}

func TestServersManifest(t *testing.T) {
	h := adminHub(t)
	stored := h.adm.servers
	if out, _ := adminRun(t, exitOK, "", "servers", "export"); out != stored {
		t.Errorf("export: %q", out)
	}
	renamed := stored + "# change server wd-1 changed\n"
	if out, _ := adminRun(t, exitOK, renamed, "servers", "check", "-"); out != "servers valid (1 games [wardogs], 1 servers [wd-1])\n" {
		t.Errorf("check: %q", out)
	}
	if out, _ := adminRun(t, exitWouldChange, renamed, "servers", "apply", "--dry-run", "-"); out != "servers would change:\n  server wd-1 changed\n" {
		t.Errorf("dry run: %q", out)
	}
	if out, _ := adminRun(t, exitOK, renamed, "servers", "apply", "-"); out != "servers applied:\n  server wd-1 changed\n" {
		t.Errorf("apply: %q", out)
	}
	if out, _ := adminRun(t, exitOK, stored, "servers", "apply", "-"); out != "servers unchanged\n" {
		t.Errorf("unchanged: %q", out)
	}
	_, errOut := adminRun(t, exitFailed, renamed+"      credential_file: /run/secrets/x\n", "servers", "apply", "-")
	if !strings.Contains(errOut, "only the hub's host changes these, with gravel-hub servers apply") || !strings.Contains(errOut, "its credential_file") {
		t.Errorf("a host-only change: %q", errOut)
	}
}

func TestStatsEraseAndNotifiers(t *testing.T) {
	h := adminHub(t)
	out, _ := adminRun(t, exitOK, "", "stats", "erase", "--provider", "steam", "--subject", "76561190000000001")
	if out != "erased: 12 match rows, 3 monthly rows, 4 participation events, 0 participation monthly rows, 5 match result rows, 6 sessions, 1 activity monthly rows, 2 ingest batches re-keyed; pseudonym deleted: true\n" ||
		h.adm.calls[0] != "EraseIdentity steam 76561190000000001" {
		t.Errorf("erase: %q (%q)", out, h.adm.calls)
	}
	adminRun(t, exitUsage, "", "stats", "erase", "--provider", "steam")

	if out, _ := adminRun(t, exitOK, "", "notifiers", "test", "john"); out != "john delivered the test event.\n" {
		t.Errorf("delivered: %q", out)
	}
	if _, errOut := adminRun(t, exitFailed, "", "notifiers", "test", "ops-mail"); !strings.Contains(errOut, "ops-mail did not deliver the test event: smtp: 550 mailbox unavailable") {
		t.Errorf("not delivered: %q", errOut)
	}
	if _, errOut := adminRun(t, exitFailed, "", "notifiers", "test", "nobody"); !strings.Contains(errOut, `no notifier "nobody"`) {
		t.Errorf("unknown: %q", errOut)
	}
}

// The commands exit as the others that call a hub do: 4 to log in, 1 when refused.
func TestAdminLoginAndRefusal(t *testing.T) {
	isolate(t)
	h := newFakeHub(t)
	for _, args := range [][]string{
		{"members", "list"}, {"settings", "export"}, {"servers", "export"}, {"apps", "list"},
		{"stats", "erase", "--provider", "steam", "--subject", "1"}, {"notifiers", "test", "john"},
	} {
		code, _, errOut := cli{t: t}.run(context.Background(), append(args, "--hub", h.srv.URL)...)
		if code != exitLoggedIn || !strings.Contains(errOut, "not logged in") {
			t.Errorf("%v logged out: exit %d, %q; want %d", args, code, errOut, exitLoggedIn)
		}
	}
	// An app is refused what is a member's in person.
	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secret, []byte(h.secret), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := (cli{t: t}).run(context.Background(), "login", "--app", "--hub", h.srv.URL, "--client-id", h.clientID, "--secret-file", secret); code != exitOK {
		t.Fatalf("login --app: %d %s", code, errOut)
	}
	if code, _, errOut := (cli{t: t}).run(context.Background(), "settings", "export"); code != exitFailed || !strings.Contains(errOut, "refused: needs settings:write in person") {
		t.Errorf("as an app: exit %d, %q", code, errOut)
	}
}

func TestAdminUsage(t *testing.T) {
	isolate(t)
	for _, cmd := range []string{"members", "owner", "users", "apps", "settings", "stats", "notifiers"} {
		if code, _, errOut := (cli{t: t}).run(context.Background(), cmd); code != exitUsage || !strings.HasPrefix(errOut, "usage: gravel "+cmd) {
			t.Errorf("gravel %s: exit %d, %q", cmd, code, errOut)
		}
		if code, out, _ := (cli{t: t}).run(context.Background(), cmd, "help"); code != exitOK || !strings.HasPrefix(out, "usage: gravel "+cmd) {
			t.Errorf("gravel %s help: exit %d, %q", cmd, code, out)
		}
	}
}
