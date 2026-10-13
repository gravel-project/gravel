// Package authz is the hub's permission model (ADR-0027): the capabilities a procedure checks,
// the roles that bundle them, and what a caller holds.
//
// A capability is an "area:verb" name, and app scopes are the same names: one vocabulary for a
// member's role, a member's token and an app's credential. A procedure asks for one capability
// (Principal.Check); who the caller is decides whether they hold it:
//
//   - the owner holds every capability a person can hold;
//   - a member holds their roles' bundles (Bundle), and the base every member has;
//   - a member's token holds what the member holds, narrowed to the token's scopes (ADR-0015 §4);
//   - an app holds its scopes, only the capabilities an app may hold (Spec.Apps);
//   - nobody holds anything anonymously.
//
// The package resolves nothing itself: the API builds a Principal from the request (internal/api,
// Access) and asks it.
package authz

import (
	"errors"
	"fmt"
	"slices"

	"github.com/google/uuid"
)

// Capability is one thing a caller may do, named "area:verb".
type Capability string

// The capabilities. The first seven are the app scopes gravel had before ADR-0027, with their
// names unchanged.
const (
	IdentityRead       Capability = "identity:read"
	ServersRead        Capability = "servers:read"
	ServersModerate    Capability = "servers:moderate"
	ServersLease       Capability = "servers:lease"
	ServersConfigure   Capability = "servers:configure"
	StatsRead          Capability = "stats:read"
	ParticipationWrite Capability = "participation:write"

	ServersOperate   Capability = "servers:operate"
	ServersProvision Capability = "servers:provision"
	ServersManage    Capability = "servers:manage"
	MembersManage    Capability = "members:manage"
	RolesGrant       Capability = "roles:grant"
	SettingsWrite    Capability = "settings:write"
	AppsManage       Capability = "apps:manage"
	EventsRead       Capability = "events:read"
	MetricsRead      Capability = "metrics:read"
	StatsErase       Capability = "stats:erase"
	OrgTransfer      Capability = "org:transfer"
)

// Spec says what a capability admits and who may hold it.
type Spec struct {
	Name Capability
	// About is what it admits, in the operator's words: `apps create` and the docs show it.
	About string
	// Scopable capabilities may be granted for one game (ADR-0027 §4; grants arrive with #185).
	Scopable bool
	// Apps says an app may hold it as a scope. A capability becomes an app's when a procedure
	// lets an app use it, so `apps create` never accepts a scope that does nothing.
	Apps bool
	// AppOnly capabilities belong to apps alone: no role holds them, the owner included
	// (participation is what an app saw, not what a person claims, ADR-0014).
	AppOnly bool
}

var specs = []Spec{
	{Name: IdentityRead, Apps: true, About: "look up members and their identities (LookupUser, ListUsers, ListIdentityEvents)"},
	{Name: ServersRead, Apps: true, Scopable: true, About: "see who is on a server and its host: names and provider identities (ListServerPlayers, GetServerHost, ListServerHostEvents)"},
	{Name: ServersModerate, Apps: true, Scopable: true, About: "kick, ban, unban, message, broadcast to and move a server's players, list its bans, read the audit log, and park or unpark a cloud-VM server (ModerationService, SetServerHostDesired, SetServerHostExpectation)"},
	{Name: ServersLease, Apps: true, Scopable: true, About: "take a leasable server for a match and give it back (ServerLeaseService)"},
	{Name: ServersConfigure, Apps: true, Scopable: true, About: "read, plan and apply a server's configuration (ServerConfigService)"},
	{Name: StatsRead, Apps: true, About: "read the stats and participation boards, match lists and match results while stats are not public (StatsService)"},
	{Name: ParticipationWrite, Apps: true, AppOnly: true, About: "record participation events from a first-party app (StatsService RecordParticipation)"},

	{Name: ServersOperate, Scopable: true, About: "start, park and restart existing servers at their size (#185)"},
	{Name: ServersProvision, Scopable: true, About: "resize, pin, snapshot and destroy a cloud-VM server's host: anything that changes what the hub spends (ResizeServerHost, UnpinServerHostSize, SnapshotServerHostVolume, DestroyServerHost)"},
	{Name: ServersManage, About: "add, change and remove servers: the servers manifest (#195)"},
	{Name: MembersManage, About: "create local accounts, issue set-password links and end members' sessions (#186, #187)"},
	{Name: RolesGrant, About: "grant and revoke members' roles (SetUserRole)"},
	{Name: SettingsWrite, About: "change the Organization settings (UpdateOrganizationSettings)"},
	{Name: AppsManage, About: "register and revoke first-party apps (#195)"},
	{Name: EventsRead, Scopable: true, About: "read the hub's events log (#188)"},
	{Name: MetricsRead, About: "read the operator dashboards' metrics through the hub (#204)"},
	{Name: StatsErase, About: "erase a player's identity from the stats (#195)"},
	{Name: OrgTransfer, About: "hand the hub to another member (#197); the owner's alone, in person"},
}

// Specs is every capability, in a stable order.
func Specs() []Spec { return slices.Clone(specs) }

// Lookup returns a capability's Spec.
func Lookup(name string) (Spec, bool) {
	i := slices.IndexFunc(specs, func(s Spec) bool { return string(s.Name) == name })
	if i < 0 {
		return Spec{}, false
	}
	return specs[i], true
}

// AppScopes is every capability an app may hold, by name.
func AppScopes() []string {
	var out []string
	for _, s := range specs {
		if s.Apps {
			out = append(out, string(s.Name))
		}
	}
	return out
}

// Role names a bundle of capabilities.
type Role string

// The roles this package knows. ADR-0027 has five (owner, admin, operator, moderator, member);
// admin and operator arrive with role grants (#185), which also replace Moderator's transitional
// bundle with ADR-0027's.
const (
	Owner     Role = "owner"
	Moderator Role = "moderator"
	Member    Role = "member"
)

// bundles are each role's capabilities. Until #185 they are exactly what the hub admitted before
// capabilities (ADR-0013): a moderator moderates, parks and unparks, and leases; every member
// sees who is on a server.
var bundles = map[Role][]Capability{
	Moderator: {ServersRead, ServersModerate, ServersLease},
	Member:    {ServersRead},
}

// Bundle is a role's capabilities. The owner's is every capability a person can hold.
func Bundle(r Role) []Capability {
	if r == Owner {
		var out []Capability
		for _, s := range specs {
			if !s.AppOnly {
				out = append(out, s.Name)
			}
		}
		return out
	}
	return slices.Clone(bundles[r])
}

// Kind is how a caller reached the hub.
type Kind int

// The kinds of caller.
const (
	Anonymous   Kind = iota // no credential
	InPerson                // a member through their session
	MemberToken             // a member through a token an app holds for them (ADR-0015 §4)
	App                     // an app through its own token (ADR-0008)
)

func (k Kind) String() string {
	switch k {
	case InPerson:
		return "in person"
	case MemberToken:
		return "member token"
	case App:
		return "app"
	}
	return "anonymous"
}

// Target is what a capability is checked against. Its fields are for game-scoped grants (#185),
// which admit a capability on the servers of one game; an instance-wide check leaves it empty.
type Target struct {
	ServerID string
	GameID   string
}

// Principal is a caller and the capabilities they hold.
type Principal struct {
	Kind Kind
	// UserID is the member (InPerson, MemberToken).
	UserID uuid.UUID
	// AppID and AppName are the app (App), or the app holding a member's token (MemberToken).
	AppID   uuid.UUID
	AppName string
	// Owner says the member owns the hub.
	Owner bool
	caps  []Capability
}

// Nobody is the anonymous caller.
func Nobody() Principal { return Principal{} }

// ForMember is a member in person: the owner holds Bundle(Owner); anyone else the member base
// and their roles' bundles. A role this package doesn't know adds nothing.
func ForMember(userID uuid.UUID, owner bool, roles []string) Principal {
	p := Principal{Kind: InPerson, UserID: userID, Owner: owner}
	if owner {
		p.caps = Bundle(Owner)
		return p
	}
	set := Bundle(Member)
	for _, r := range roles {
		for _, c := range bundles[Role(r)] {
			if !slices.Contains(set, c) {
				set = append(set, c)
			}
		}
	}
	p.caps = set
	return p
}

// Through is the member acting through an app's token for them: their capabilities narrowed to
// the token's scopes.
func (p Principal) Through(appID uuid.UUID, appName string, scopes []string) Principal {
	q := p
	q.Kind, q.AppID, q.AppName = MemberToken, appID, appName
	q.caps = slices.DeleteFunc(slices.Clone(p.caps), func(c Capability) bool { return !slices.Contains(scopes, string(c)) })
	return q
}

// ForApp is an app acting for itself: the scopes it holds that an app may hold. Host scopes
// ("host:…") and unknown names admit nothing.
func ForApp(appID uuid.UUID, name string, scopes []string) Principal {
	p := Principal{Kind: App, AppID: appID, AppName: name}
	for _, sc := range scopes {
		if s, ok := Lookup(sc); ok && s.Apps {
			p.caps = append(p.caps, s.Name)
		}
	}
	return p
}

// Capabilities is what the caller holds, in Specs order.
func (p Principal) Capabilities() []Capability {
	var out []Capability
	for _, s := range specs {
		if slices.Contains(p.caps, s.Name) {
			out = append(out, s.Name)
		}
	}
	return out
}

// Has reports whether the caller holds c for t.
func (p Principal) Has(c Capability, _ Target) bool { return slices.Contains(p.caps, c) }

// The refusals Check returns, for the API to map: ErrUnauthenticated when nobody is calling,
// ErrDenied when the caller lacks the capability.
var (
	ErrUnauthenticated = errors.New("authz: unauthenticated")
	ErrDenied          = errors.New("authz: denied")
)

// Refusal is Check's error: why, in words a caller can act on.
type Refusal struct {
	Kind       error // ErrUnauthenticated or ErrDenied
	Capability Capability
	msg        string
}

func (r *Refusal) Error() string { return r.msg }
func (r *Refusal) Unwrap() error { return r.Kind }

// Check returns nil when the caller holds c for t, else a *Refusal.
func (p Principal) Check(c Capability, t Target) error {
	if p.Has(c, t) {
		return nil
	}
	r := &Refusal{Kind: ErrDenied, Capability: c}
	switch p.Kind {
	case Anonymous:
		r.Kind, r.msg = ErrUnauthenticated, "log in first"
		if s, ok := Lookup(string(c)); ok && s.Apps {
			r.msg = fmt.Sprintf("log in, or call as an app with %q", c)
		}
	case App:
		r.msg = fmt.Sprintf("the app %s lacks the scope %q", p.AppName, c)
		if s, ok := Lookup(string(c)); ok && !s.Apps {
			r.msg = fmt.Sprintf("%q is not a scope an app may hold: a member does this in person", c)
		}
	case InPerson, MemberToken:
		if s, ok := Lookup(string(c)); ok && s.AppOnly {
			r.msg = fmt.Sprintf("%q is a first-party app's own: an app records it, not a member", c)
			break
		}
		if p.Kind == MemberToken {
			r.msg = fmt.Sprintf("this needs %q, which this member does not hold or the token from %s does not carry", c, p.AppName)
			break
		}
		r.msg = fmt.Sprintf("this needs %q, which you do not hold", c)
	}
	return r
}
