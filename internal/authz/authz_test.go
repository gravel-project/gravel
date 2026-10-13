package authz_test

import (
	"errors"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/gravel-project/gravel/internal/authz"
)

// Every capability is one "area:verb" name, once; the app scopes are the seven gravel issued
// before capabilities, with their names unchanged.
func TestVocabulary(t *testing.T) {
	name := regexp.MustCompile(`^[a-z]+:[a-z]+$`)
	seen := map[authz.Capability]bool{}
	for _, s := range authz.Specs() {
		if !name.MatchString(string(s.Name)) {
			t.Errorf("%q is not area:verb", s.Name)
		}
		if seen[s.Name] {
			t.Errorf("%q twice", s.Name)
		}
		seen[s.Name] = true
		if s.About == "" {
			t.Errorf("%q says nothing about what it admits", s.Name)
		}
		if s.AppOnly && !s.Apps {
			t.Errorf("%q is an app's alone but no app may hold it", s.Name)
		}
		if got, ok := authz.Lookup(string(s.Name)); !ok || got.Name != s.Name {
			t.Errorf("Lookup(%q) = %v %v", s.Name, got, ok)
		}
	}
	want := []string{"identity:read", "participation:write", "roles:grant", "servers:configure", "servers:lease", "servers:moderate", "servers:operate", "servers:read", "stats:read"}
	if got := slices.Sorted(slices.Values(authz.AppScopes())); !slices.Equal(got, want) {
		t.Errorf("AppScopes = %v, want %v", got, want)
	}
	if _, ok := authz.Lookup("host:muster:write"); ok {
		t.Error("a host scope is not a capability")
	}
}

// ADR-0027 §3: the owner holds every capability a person can hold, an admin all but
// org:transfer, an operator and a moderator their bundles, a member the base.
func TestBundles(t *testing.T) {
	owner := authz.ForMember(uuid.New(), true, nil)
	admin := authz.ForMember(uuid.New(), false, []authz.Grant{{Role: "admin"}})
	for _, s := range authz.Specs() {
		if got := owner.Has(s.Name, authz.Target{}); got == s.AppOnly {
			t.Errorf("owner Has(%q) = %v", s.Name, got)
		}
		if got, want := admin.Has(s.Name, authz.Target{}), !s.AppOnly && s.Name != authz.OrgTransfer; got != want {
			t.Errorf("admin Has(%q) = %v", s.Name, got)
		}
	}
	member := authz.ForMember(uuid.New(), false, nil)
	if got := member.Capabilities(); !slices.Equal(got, []authz.Capability{authz.ServersRead}) {
		t.Errorf("member = %v", got)
	}
	for role, want := range map[string][]authz.Capability{
		"operator":  {authz.ServersRead, authz.ServersLease, authz.ServersConfigure, authz.StatsRead, authz.ServersOperate, authz.EventsRead, authz.MetricsRead},
		"moderator": {authz.IdentityRead, authz.ServersRead, authz.ServersModerate, authz.ServersLease, authz.StatsRead, authz.EventsRead},
	} {
		if got := authz.ForMember(uuid.New(), false, []authz.Grant{{Role: role}, {Role: "nonsense"}}).Capabilities(); !slices.Equal(got, want) {
			t.Errorf("%s = %v, want %v", role, got, want)
		}
	}
	for _, c := range []authz.Capability{authz.ServersProvision, authz.RolesGrant, authz.SettingsWrite, authz.ServersModerate} {
		if authz.ForMember(uuid.New(), false, []authz.Grant{{Role: "operator"}}).Has(c, authz.Target{}) {
			t.Errorf("an operator holds %q", c)
		}
	}
	if !authz.IsGrantable("admin") || authz.IsGrantable("owner") || authz.IsGrantable("member") {
		t.Error("grantable roles")
	}
}

// ADR-0027 §4: a game-scoped grant gives its scopable capabilities for that game only; the rest
// of the bundle not at all.
func TestGameScopedGrants(t *testing.T) {
	cs2 := authz.Target{ServerID: "htg-cs2-1", GameID: "cs2"}
	wd := authz.Target{ServerID: "htg-wardogs-1", GameID: "wardogs"}
	mod := authz.ForMember(uuid.New(), false, []authz.Grant{{Role: "moderator", GameID: "cs2"}, {Role: "operator", GameID: "cs2"}})
	if !mod.Has(authz.ServersModerate, cs2) || !mod.Has(authz.ServersOperate, cs2) || !mod.Has(authz.EventsRead, cs2) {
		t.Error("scoped grant on its game")
	}
	if mod.Has(authz.ServersModerate, wd) || mod.Has(authz.ServersModerate, authz.Target{}) || mod.Has(authz.ServersModerate, authz.Target{ServerID: "x"}) {
		t.Error("scoped grant elsewhere")
	}
	if mod.Has(authz.IdentityRead, cs2) || mod.Has(authz.StatsRead, cs2) {
		t.Error("a scoped grant gave a capability that isn't scopable")
	}
	if all, games := mod.Scope(authz.ServersModerate); all || !slices.Equal(games, []string{"cs2"}) {
		t.Errorf("scope = %v %v", all, games)
	}
	both := authz.ForMember(uuid.New(), false, []authz.Grant{{Role: "moderator"}, {Role: "operator", GameID: "cs2"}})
	if all, _ := both.Scope(authz.ServersModerate); !all {
		t.Error("an instance grant is everywhere")
	}
	if all, games := both.Scope(authz.ServersProvision); all || len(games) != 0 {
		t.Errorf("nowhere = %v %v", all, games)
	}
	admin := authz.ForMember(uuid.New(), false, []authz.Grant{{Role: "admin", GameID: "cs2"}})
	if !admin.Has(authz.ServersProvision, cs2) || !admin.Has(authz.RolesGrant, cs2) || admin.Has(authz.RolesGrant, authz.Target{}) || admin.Has(authz.SettingsWrite, cs2) {
		t.Error("a game-scoped admin: provision and grant in the game, never settings")
	}
	if tok := mod.Through(uuid.New(), "Muster", []string{"servers:moderate"}); !tok.Has(authz.ServersModerate, cs2) || tok.Has(authz.ServersOperate, cs2) {
		t.Error("a token narrows scoped capabilities too")
	}
}

// A member's token holds what the member holds and the token carries; never more than either.
func TestThroughNarrows(t *testing.T) {
	app := uuid.New()
	owner := authz.ForMember(uuid.New(), true, nil).Through(app, "Muster", []string{"identity:read", "servers:moderate", "participation:write", "host:x"})
	want := []authz.Capability{authz.IdentityRead, authz.ServersModerate}
	if got := owner.Capabilities(); !slices.Equal(got, want) || owner.Kind != authz.MemberToken || owner.AppID != app {
		t.Errorf("owner's token = %v %v", got, owner)
	}
	member := authz.ForMember(uuid.New(), false, nil).Through(app, "Muster", []string{"identity:read", "servers:read"})
	if got := member.Capabilities(); !slices.Equal(got, []authz.Capability{authz.ServersRead}) {
		t.Errorf("member's token = %v", got)
	}
}

// An app holds the scopes an app may hold; a host scope, an unknown name and a capability no app
// may hold admit nothing.
func TestForApp(t *testing.T) {
	p := authz.ForApp(uuid.New(), "bot", []string{"servers:moderate", "host:muster:write", "settings:write", "org:transfer", "nonsense"})
	if got := p.Capabilities(); !slices.Equal(got, []authz.Capability{authz.ServersModerate}) {
		t.Errorf("app = %v", got)
	}
	if err := p.Check(authz.SettingsWrite, authz.Target{}); !errors.Is(err, authz.ErrDenied) || !strings.Contains(err.Error(), "not a scope an app may hold") {
		t.Errorf("settings:write: %v", err)
	}
	if err := p.Check(authz.StatsRead, authz.Target{}); !errors.Is(err, authz.ErrDenied) || !strings.Contains(err.Error(), "lacks the scope") {
		t.Errorf("stats:read: %v", err)
	}
}

// Check says who is refused and why: nobody is unauthenticated, everyone else denied.
func TestCheck(t *testing.T) {
	if err := authz.Nobody().Check(authz.ServersRead, authz.Target{}); !errors.Is(err, authz.ErrUnauthenticated) || !strings.Contains(err.Error(), "call as an app") {
		t.Errorf("nobody, an app's capability: %v", err)
	}
	if err := authz.Nobody().Check(authz.SettingsWrite, authz.Target{}); !errors.Is(err, authz.ErrUnauthenticated) || err.Error() != "log in first" {
		t.Errorf("nobody, a person's capability: %v", err)
	}
	m := authz.ForMember(uuid.New(), false, nil)
	if err := m.Check(authz.ServersModerate, authz.Target{}); !errors.Is(err, authz.ErrDenied) || !strings.Contains(err.Error(), "you do not hold") {
		t.Errorf("member: %v", err)
	}
	owner := authz.ForMember(uuid.New(), true, nil)
	if err := owner.Check(authz.ParticipationWrite, authz.Target{}); !errors.Is(err, authz.ErrDenied) || !strings.Contains(err.Error(), "first-party app's own") {
		t.Errorf("owner, participation: %v", err)
	}
	if err := owner.Check(authz.OrgTransfer, authz.Target{}); err != nil {
		t.Errorf("owner, org:transfer: %v", err)
	}
	var r *authz.Refusal
	if err := m.Check(authz.RolesGrant, authz.Target{}); !errors.As(err, &r) || r.Capability != authz.RolesGrant {
		t.Errorf("refusal: %#v", err)
	}
}
