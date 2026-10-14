package main

// The operator's commands over the API (gravel#201, ADR-0031 §5): what `gravel-hub` does on the
// host, from any machine with a credential for the hub, so an operator never needs a shell on the
// hub's host after bootstrap. They take gravel-hub's flags where it has the command, and exit as it
// does: 3 when a --dry-run would change something. The hub checks every call; most are a member's
// in person, so an app's credential is refused.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
	"github.com/gravel-project/gravel/gen/gravel/hub/v1/hubv1connect"
)

func (c *hubClient) apps() hubv1connect.AppServiceClient {
	return hubv1connect.NewAppServiceClient(c.http, c.hub, connect.WithProtoJSON())
}

func (c *hubClient) stats() hubv1connect.StatsServiceClient {
	return hubv1connect.NewStatsServiceClient(c.http, c.hub, connect.WithProtoJSON())
}

// parseArgs parses the flags wherever they come among the arguments, so `apply x.yaml --dry-run`
// works as it does for gravel-hub, and returns the others in order.
func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return pos, nil
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

// subcommands runs the named one of a command's subcommands.
func subcommands(ctx context.Context, name, usage string, cmds map[string]func(context.Context, []string) int, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return exitUsage
	}
	if args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprint(stdout, usage)
		return exitOK
	}
	run, ok := cmds[args[0]]
	if !ok {
		fmt.Fprintf(stderr, "gravel %s: unknown command %q\n", name, args[0])
		fmt.Fprint(stderr, usage)
		return exitUsage
	}
	return run(ctx, args[1:])
}

// admin is one invocation of an operator's command: its flags, then its hub.
type admin struct {
	name   string // "members grant"
	usage  string // the usage line, without "usage: "
	fs     *flag.FlagSet
	hub    hubFlag
	stderr io.Writer
}

func newAdmin(name, usage string, stderr io.Writer) *admin {
	fs := newFlagSet(name, stderr)
	return &admin{name: name, usage: usage, fs: fs, hub: addHubFlag(fs), stderr: stderr}
}

// args parses the flags and returns n positional arguments; ok false has printed the usage.
func (a *admin) args(args []string, n int) ([]string, bool) {
	pos, err := parseArgs(a.fs, args)
	if err != nil {
		return nil, false
	}
	if len(pos) != n {
		fmt.Fprintln(a.stderr, "usage: "+a.usage)
		return nil, false
	}
	return pos, true
}

// open is the hub the flags name; ok false has printed why.
func (a *admin) open() (*hubClient, bool) {
	c, err := openHub(a.hub)
	if err != nil {
		fmt.Fprintf(a.stderr, "gravel %s: %v\n", a.name, err)
		return nil, false
	}
	return c, true
}

func (a *admin) usageError(msg string) int {
	fmt.Fprintf(a.stderr, "gravel %s: %s\nusage: %s\n", a.name, msg, a.usage)
	return exitUsage
}

// readManifest reads a manifest from a file, or from stdin for "-".
func readManifest(path string, stdin io.Reader) (string, error) {
	var (
		b   []byte
		err error
	)
	if path == "-" {
		b, err = io.ReadAll(stdin)
	} else {
		b, err = os.ReadFile(path)
	}
	return string(b), err
}

// ---- members ----

const membersUsage = `usage: gravel members <command> [--hub URL]

  list [--game GAME]                    every member: their roles, identities and last login
  grant MEMBER ROLE [--game GAME]       grant admin, operator or moderator, instance-wide or for one game
  revoke MEMBER ROLE [--game GAME]      take a grant back
  logout MEMBER                         end every session the member has

  MEMBER is a member's id, or one of their identities as provider:subject (local:alice, discord:1234).
`

func membersCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return subcommands(ctx, "members", membersUsage, map[string]func(context.Context, []string) int{
		"list":   func(ctx context.Context, args []string) int { return membersList(ctx, args, stdout, stderr) },
		"grant":  func(ctx context.Context, args []string) int { return membersSetRole(ctx, args, true, stdout, stderr) },
		"revoke": func(ctx context.Context, args []string) int { return membersSetRole(ctx, args, false, stdout, stderr) },
		"logout": func(ctx context.Context, args []string) int { return membersLogout(ctx, args, stdout, stderr) },
	}, args, stdout, stderr)
}

// member resolves MEMBER: an id as it is, or provider:subject through LookupUser.
func (c *hubClient) member(ctx context.Context, ref string) (string, error) {
	if _, err := uuid.Parse(ref); err == nil {
		return ref, nil
	}
	provider, subject, ok := strings.Cut(ref, ":")
	if !ok || provider == "" || subject == "" {
		return "", errMemberRef
	}
	res, err := c.identity().LookupUser(ctx, connect.NewRequest(&hubv1.LookupUserRequest{Provider: provider, Subject: subject}))
	if err != nil {
		return "", err
	}
	return res.Msg.GetUser().GetId(), nil
}

var errMemberRef = errors.New("name a member by their id, or by an identity as provider:subject (local:alice, discord:1234)")

// memberFail is c.fail, with the answers that name the member said plainly.
func (c *hubClient) memberFail(stderr io.Writer, cmd, ref string, err error) int {
	switch {
	case errors.Is(err, errMemberRef):
		fmt.Fprintf(stderr, "gravel %s: %v\n", cmd, err)
		return exitUsage
	case connect.CodeOf(err) == connect.CodeNotFound:
		fmt.Fprintf(stderr, "gravel %s: %s is not a member of %s\n", cmd, ref, c.hub)
		return exitFailed
	}
	return c.fail(stderr, cmd, err)
}

func membersList(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	a := newAdmin("members list", "gravel members list [--game GAME] [--hub URL]", stderr)
	game := a.fs.String("game", "", "only the members holding a role for this game")
	if _, ok := a.args(args, 0); !ok {
		return exitUsage
	}
	c, ok := a.open()
	if !ok {
		return exitFailed
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	var users []*hubv1.User
	for token := ""; ; {
		res, err := c.identity().ListUsers(ctx, connect.NewRequest(&hubv1.ListUsersRequest{PageSize: 500, PageToken: token}))
		if err != nil {
			return c.fail(stderr, a.name, err)
		}
		users = append(users, res.Msg.GetUsers()...)
		if token = res.Msg.GetNextPageToken(); token == "" {
			break
		}
	}
	tw := table(stdout)
	fmt.Fprintln(tw, "MEMBER\tNAME\tROLES\tIDENTITIES\tLAST LOGIN")
	for _, u := range users {
		if *game != "" && !slices.ContainsFunc(u.GetGrants(), func(g *hubv1.RoleGrant) bool { return g.GetGameId() == *game }) {
			continue
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", u.GetId(), u.GetDisplayName(), dash(strings.Join(roles(u), ", ")), dash(strings.Join(identities(u), " ")), stamp(u.GetLastLoginAt()))
	}
	return flush(tw, stderr)
}

// roles is "owner", then each grant: "admin", "operator of cs2".
func roles(u *hubv1.User) []string {
	var out []string
	if u.GetOwner() {
		out = append(out, "owner")
	}
	for _, g := range u.GetGrants() {
		out = append(out, grantName(g.GetRole(), g.GetGameId()))
	}
	return out
}

func grantName(role, game string) string {
	if game == "" {
		return role
	}
	return role + " of " + game
}

// identities are provider:subject, as MEMBER takes them.
func identities(u *hubv1.User) []string {
	var out []string
	for _, id := range u.GetIdentities() {
		out = append(out, id.GetProvider()+":"+id.GetSubject())
	}
	return out
}

func membersSetRole(ctx context.Context, args []string, granted bool, stdout, stderr io.Writer) int {
	verb := "revoke"
	if granted {
		verb = "grant"
	}
	a := newAdmin("members "+verb, "gravel members "+verb+" MEMBER ROLE [--game GAME] [--hub URL]   (ROLE: admin, operator or moderator)", stderr)
	game := a.fs.String("game", "", "the game the role is for; instance-wide when unset")
	pos, ok := a.args(args, 2)
	if !ok {
		return exitUsage
	}
	c, ok := a.open()
	if !ok {
		return exitFailed
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	id, err := c.member(ctx, pos[0])
	if err != nil {
		return c.memberFail(stderr, a.name, pos[0], err)
	}
	res, err := c.identity().SetUserRole(ctx, connect.NewRequest(&hubv1.SetUserRoleRequest{UserId: id, Role: pos[1], Granted: granted, GameId: *game}))
	if err != nil {
		return c.memberFail(stderr, a.name, pos[0], err)
	}
	u := res.Msg.GetUser()
	if granted {
		fmt.Fprintf(stdout, "%s holds %s now (%s).\n", u.GetDisplayName(), grantName(pos[1], *game), orNone(roles(u)))
	} else {
		fmt.Fprintf(stdout, "%s no longer holds %s (%s).\n", u.GetDisplayName(), grantName(pos[1], *game), orNone(roles(u)))
	}
	return exitOK
}

func membersLogout(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	a := newAdmin("members logout", "gravel members logout MEMBER [--hub URL]", stderr)
	pos, ok := a.args(args, 1)
	if !ok {
		return exitUsage
	}
	c, ok := a.open()
	if !ok {
		return exitFailed
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	id, err := c.member(ctx, pos[0])
	if err != nil {
		return c.memberFail(stderr, a.name, pos[0], err)
	}
	res, err := c.identity().RevokeUserSessions(ctx, connect.NewRequest(&hubv1.RevokeUserSessionsRequest{UserId: id}))
	if err != nil {
		return c.memberFail(stderr, a.name, pos[0], err)
	}
	fmt.Fprintf(stdout, "Ended %d sessions of %s.\n", res.Msg.GetRevoked(), pos[0])
	return exitOK
}

// ---- owner ----

const ownerUsage = `usage: gravel owner <command> [--hub URL]

  status            who owns the hub, and the nomination the owner or the nominee may see
  nominate MEMBER   name the member who may take the hub over, for 7 days (the owner)
  accept            take the hub over; the old owner stays an admin (the nominee)
  cancel            end the nomination: the owner cancels it, the nominee declines it

  MEMBER is a member's id, or one of their identities as provider:subject (local:alice, discord:1234).
`

func ownerCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return subcommands(ctx, "owner", ownerUsage, map[string]func(context.Context, []string) int{
		"status":   func(ctx context.Context, args []string) int { return ownerStatus(ctx, args, stdout, stderr) },
		"nominate": func(ctx context.Context, args []string) int { return ownerNominate(ctx, args, stdout, stderr) },
		"accept":   func(ctx context.Context, args []string) int { return ownerAccept(ctx, args, stdout, stderr) },
		"cancel":   func(ctx context.Context, args []string) int { return ownerCancel(ctx, args, stdout, stderr) },
	}, args, stdout, stderr)
}

func printOrganization(w io.Writer, hub string, o *hubv1.Organization, stderr io.Writer) int {
	tw := table(w)
	fmt.Fprintf(tw, "hub\t%s (%s)\n", o.GetName(), hub)
	if o.GetOwned() {
		fmt.Fprintf(tw, "owner\t%s since %s\n", o.GetOwnerUserId(), stamp(o.GetClaimedAt()))
	} else {
		fmt.Fprintln(tw, "owner\tnone: the hub is unowned (gravel claim)")
	}
	if o.GetPendingOwnerUserId() != "" {
		fmt.Fprintf(tw, "nominated\t%s (%s) until %s\n", o.GetPendingOwnerName(), o.GetPendingOwnerUserId(), stamp(o.GetPendingOwnerExpiresAt()))
	}
	return flush(tw, stderr)
}

func ownerStatus(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	a := newAdmin("owner status", "gravel owner status [--hub URL]", stderr)
	if _, ok := a.args(args, 0); !ok {
		return exitUsage
	}
	c, ok := a.open()
	if !ok {
		return exitFailed
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	res, err := c.organization().GetOrganization(ctx, connect.NewRequest(&hubv1.GetOrganizationRequest{}))
	if err != nil {
		return c.fail(stderr, a.name, err)
	}
	return printOrganization(stdout, c.hub, res.Msg.GetOrganization(), stderr)
}

func ownerNominate(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	a := newAdmin("owner nominate", "gravel owner nominate MEMBER [--hub URL]", stderr)
	pos, ok := a.args(args, 1)
	if !ok {
		return exitUsage
	}
	c, ok := a.open()
	if !ok {
		return exitFailed
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	id, err := c.member(ctx, pos[0])
	if err != nil {
		return c.memberFail(stderr, a.name, pos[0], err)
	}
	res, err := c.organization().NominateOwner(ctx, connect.NewRequest(&hubv1.NominateOwnerRequest{UserId: id}))
	if err != nil {
		return c.memberFail(stderr, a.name, pos[0], err)
	}
	o := res.Msg.GetOrganization()
	fmt.Fprintf(stdout, "%s may take %s over until %s: they run gravel owner accept, or accept on their account page.\n",
		o.GetPendingOwnerName(), o.GetName(), stamp(o.GetPendingOwnerExpiresAt()))
	return exitOK
}

func ownerAccept(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	a := newAdmin("owner accept", "gravel owner accept [--hub URL]", stderr)
	if _, ok := a.args(args, 0); !ok {
		return exitUsage
	}
	c, ok := a.open()
	if !ok {
		return exitFailed
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	res, err := c.organization().AcceptOwnership(ctx, connect.NewRequest(&hubv1.AcceptOwnershipRequest{}))
	if connect.CodeOf(err) == connect.CodeFailedPrecondition {
		fmt.Fprintf(stderr, "gravel %s: there is no nomination for you on %s, or it has expired\n", a.name, c.hub)
		return exitFailed
	}
	if err != nil {
		return c.fail(stderr, a.name, err)
	}
	fmt.Fprintf(stdout, "You own %s (%s) now.\n", res.Msg.GetOrganization().GetName(), c.hub)
	return exitOK
}

func ownerCancel(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	a := newAdmin("owner cancel", "gravel owner cancel [--hub URL]", stderr)
	if _, ok := a.args(args, 0); !ok {
		return exitUsage
	}
	c, ok := a.open()
	if !ok {
		return exitFailed
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	_, err := c.organization().CancelOwnershipTransfer(ctx, connect.NewRequest(&hubv1.CancelOwnershipTransferRequest{}))
	if connect.CodeOf(err) == connect.CodeFailedPrecondition {
		fmt.Fprintf(stderr, "gravel %s: %s has no nomination to end\n", a.name, c.hub)
		return exitFailed
	}
	if err != nil {
		return c.fail(stderr, a.name, err)
	}
	fmt.Fprintln(stdout, "The nomination is over; nobody may take the hub over.")
	return exitOK
}

// ---- users ----

const usersUsage = `usage: gravel users <command> [--hub URL]

  create-local --username NAME [--email ADDRESS]   a local account, and the one-time link that sets its password
  reset-password --username NAME                   a one-time link that sets a new password
`

func usersCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return subcommands(ctx, "users", usersUsage, map[string]func(context.Context, []string) int{
		"create-local": func(ctx context.Context, args []string) int {
			return usersLink(ctx, "create-local", args, stdout, stderr)
		},
		"reset-password": func(ctx context.Context, args []string) int {
			return usersLink(ctx, "reset-password", args, stdout, stderr)
		},
	}, args, stdout, stderr)
}

func usersLink(ctx context.Context, sub string, args []string, stdout, stderr io.Writer) int {
	usage := "gravel users reset-password --username NAME [--hub URL]"
	if sub == "create-local" {
		usage = "gravel users create-local --username NAME [--email ADDRESS] [--hub URL]"
	}
	a := newAdmin("users "+sub, usage, stderr)
	username := a.fs.String("username", "", "the local account's username (required)")
	email := a.fs.String("email", "", "create-local: an optional address, only ever for recovery")
	if _, ok := a.args(args, 0); !ok {
		return exitUsage
	}
	if *username == "" {
		return a.usageError("--username is required")
	}
	if sub != "create-local" && *email != "" {
		return a.usageError("--email is for create-local")
	}
	c, ok := a.open()
	if !ok {
		return exitFailed
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	var link *hubv1.PasswordLink
	if sub == "create-local" {
		res, err := c.identity().CreateLocalAccount(ctx, connect.NewRequest(&hubv1.CreateLocalAccountRequest{Username: *username, Email: *email}))
		if err != nil {
			return c.fail(stderr, a.name, err)
		}
		link = res.Msg.GetLink()
		fmt.Fprintf(stdout, "local account created: %s\n", *username)
	} else {
		res, err := c.identity().IssuePasswordReset(ctx, connect.NewRequest(&hubv1.IssuePasswordResetRequest{Username: *username}))
		if err != nil {
			return c.fail(stderr, a.name, err)
		}
		link = res.Msg.GetLink()
		fmt.Fprintf(stdout, "password reset for: %s (its earlier links no longer work)\n", *username)
	}
	url := link.GetUrl()
	if strings.HasPrefix(url, "/") { // a hub without auth.base_url answers the path only
		url = c.hub + url
	}
	fmt.Fprintf(stdout, "password link (shown once, works once, until %s):\n%s\n", link.GetExpiresAt().AsTime().UTC().Format("2006-01-02 15:04 UTC"), url)
	return exitOK
}

// ---- apps ----

const appsUsage = `usage: gravel apps <command> [--hub URL]

  create --name NAME --scopes SCOPES [--redirect-uri URL]...   register an app; its secret is shown once
  list                                                         every app, revoked ones too
  revoke --client-id ID                                        revoke an app and its tokens
`

func appsCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return subcommands(ctx, "apps", appsUsage, map[string]func(context.Context, []string) int{
		"create": func(ctx context.Context, args []string) int { return appsCreate(ctx, args, stdout, stderr) },
		"list":   func(ctx context.Context, args []string) int { return appsList(ctx, args, stdout, stderr) },
		"revoke": func(ctx context.Context, args []string) int { return appsRevoke(ctx, args, stdout, stderr) },
	}, args, stdout, stderr)
}

// repeated is a flag that may be given more than once.
type repeated []string

func (r *repeated) String() string     { return strings.Join(*r, ",") }
func (r *repeated) Set(v string) error { *r = append(*r, v); return nil }

func appsCreate(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	a := newAdmin("apps create", "gravel apps create --name NAME --scopes SCOPES [--redirect-uri URL]... [--hub URL]", stderr)
	name := a.fs.String("name", "", "the app's name (required)")
	scopes := a.fs.String("scopes", "", "comma-separated scopes: capabilities an app may hold, or host:<name> (required)")
	var redirects repeated
	a.fs.Var(&redirects, "redirect-uri", "where the app's pages receive a member who signed in; repeatable")
	if _, ok := a.args(args, 0); !ok {
		return exitUsage
	}
	if *name == "" || *scopes == "" {
		return a.usageError("--name and --scopes are required")
	}
	c, ok := a.open()
	if !ok {
		return exitFailed
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	var list []string
	for s := range strings.SplitSeq(*scopes, ",") {
		if s = strings.TrimSpace(s); s != "" {
			list = append(list, s)
		}
	}
	res, err := c.apps().CreateApp(ctx, connect.NewRequest(&hubv1.CreateAppRequest{Name: *name, Scopes: list, RedirectUris: redirects}))
	if err != nil {
		return c.fail(stderr, a.name, err)
	}
	app := res.Msg.GetApp()
	fmt.Fprintf(stdout, "app registered: %s\nclient_id: %s\nclient_secret: %s\nscopes: %s\n", app.GetName(), app.GetClientId(), res.Msg.GetClientSecret(), strings.Join(app.GetScopes(), ","))
	for _, u := range app.GetRedirectUris() {
		fmt.Fprintf(stdout, "redirect_uri: %s\n", u)
	}
	fmt.Fprintln(stdout, "\nThe secret is shown once; the hub keeps its hash. Give it to the app as a secret file.")
	return exitOK
}

func appsList(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	a := newAdmin("apps list", "gravel apps list [--hub URL]", stderr)
	if _, ok := a.args(args, 0); !ok {
		return exitUsage
	}
	c, ok := a.open()
	if !ok {
		return exitFailed
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	res, err := c.apps().ListApps(ctx, connect.NewRequest(&hubv1.ListAppsRequest{}))
	if err != nil {
		return c.fail(stderr, a.name, err)
	}
	if len(res.Msg.GetApps()) == 0 {
		fmt.Fprintln(stdout, "no apps registered")
		return exitOK
	}
	tw := table(stdout)
	fmt.Fprintln(tw, "CLIENT_ID\tNAME\tSCOPES\tREDIRECT_URIS\tCREATED\tREVOKED")
	for _, app := range res.Msg.GetApps() {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", app.GetClientId(), app.GetName(), strings.Join(app.GetScopes(), ","),
			dash(strings.Join(app.GetRedirectUris(), ",")), stamp(app.GetCreatedAt()), stamp(app.GetRevokedAt()))
	}
	return flush(tw, stderr)
}

func appsRevoke(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	a := newAdmin("apps revoke", "gravel apps revoke --client-id ID [--hub URL]", stderr)
	clientID := a.fs.String("client-id", "", "the app's client id (required)")
	if _, ok := a.args(args, 0); !ok {
		return exitUsage
	}
	if *clientID == "" {
		return a.usageError("--client-id is required")
	}
	c, ok := a.open()
	if !ok {
		return exitFailed
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	if _, err := c.apps().RevokeApp(ctx, connect.NewRequest(&hubv1.RevokeAppRequest{ClientId: *clientID})); err != nil {
		if connect.CodeOf(err) == connect.CodeNotFound {
			fmt.Fprintf(stderr, "gravel %s: no app with client id %s, or it is already revoked\n", a.name, *clientID)
			return exitFailed
		}
		return c.fail(stderr, a.name, err)
	}
	fmt.Fprintf(stdout, "app revoked: %s (its tokens are gone; no new one will be issued)\n", *clientID)
	return exitOK
}

// ---- settings and the servers manifest ----

const settingsUsage = `usage: gravel settings <command> [--hub URL]

  export                        print the Organization settings as a manifest
  check MANIFEST                validate a manifest against the hub; change nothing
  apply MANIFEST [--dry-run]    store a manifest; --dry-run says which sections would change (exit 3 when some would)

  MANIFEST is a file, or - for stdin.
`

func settingsCmd(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	return subcommands(ctx, "settings", settingsUsage, map[string]func(context.Context, []string) int{
		"export": func(ctx context.Context, args []string) int { return settingsExport(ctx, args, stdout, stderr) },
		"check":  func(ctx context.Context, args []string) int { return settingsCheck(ctx, args, stdin, stdout, stderr) },
		"apply":  func(ctx context.Context, args []string) int { return settingsApply(ctx, args, stdin, stdout, stderr) },
	}, args, stdout, stderr)
}

func settingsExport(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	a := newAdmin("settings export", "gravel settings export [--hub URL]", stderr)
	if _, ok := a.args(args, 0); !ok {
		return exitUsage
	}
	c, ok := a.open()
	if !ok {
		return exitFailed
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	res, err := c.organization().ExportSettings(ctx, connect.NewRequest(&hubv1.ExportSettingsRequest{}))
	if err != nil {
		return c.fail(stderr, a.name, err)
	}
	fmt.Fprint(stdout, res.Msg.GetManifest())
	return exitOK
}

// manifestCall reads the one MANIFEST argument and opens the hub; ok false has printed why.
func (a *admin) manifestCall(args []string, stdin io.Reader) (*hubClient, string, int, bool) {
	pos, ok := a.args(args, 1)
	if !ok {
		return nil, "", exitUsage, false
	}
	text, err := readManifest(pos[0], stdin)
	if err != nil {
		fmt.Fprintf(a.stderr, "gravel %s: %v\n", a.name, err)
		return nil, "", exitFailed, false
	}
	c, ok := a.open()
	if !ok {
		return nil, "", exitFailed, false
	}
	return c, text, exitOK, true
}

// invalid prints the hub's refusal of a manifest: each problem on its own line.
func (c *hubClient) invalid(stderr io.Writer, cmd string, err error) int {
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		return c.fail(stderr, cmd, err)
	}
	var ce *connect.Error
	errors.As(err, &ce)
	fmt.Fprintf(stderr, "gravel %s: the hub refused the manifest:\n", cmd)
	for line := range strings.SplitSeq(ce.Message(), "\n") {
		fmt.Fprintf(stderr, "  %s\n", line)
	}
	return exitFailed
}

func settingsCheck(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	a := newAdmin("settings check", "gravel settings check MANIFEST [--hub URL]", stderr)
	c, text, code, ok := a.manifestCall(args, stdin)
	if !ok {
		return code
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	res, err := c.organization().CheckSettings(ctx, connect.NewRequest(&hubv1.CheckSettingsRequest{Manifest: text}))
	if err != nil {
		return c.invalid(stderr, a.name, err)
	}
	fmt.Fprintf(stdout, "settings valid (%s)\n", res.Msg.GetSummary())
	return exitOK
}

func settingsApply(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	a := newAdmin("settings apply", "gravel settings apply MANIFEST [--dry-run] [--hub URL]", stderr)
	dryRun := a.fs.Bool("dry-run", false, "say which sections would change; change nothing (exit 3 when some would)")
	c, text, code, ok := a.manifestCall(args, stdin)
	if !ok {
		return code
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	res, err := c.organization().ApplySettings(ctx, connect.NewRequest(&hubv1.ApplySettingsRequest{Manifest: text, DryRun: *dryRun}))
	if err != nil {
		return c.invalid(stderr, a.name, err)
	}
	m := res.Msg
	switch {
	case len(m.GetChanges()) == 0:
		fmt.Fprintf(stdout, "settings unchanged (%s)\n", m.GetSummary())
	case !m.GetApplied():
		fmt.Fprintf(stdout, "settings would change: %s (%s)\n", strings.Join(m.GetChanges(), ", "), m.GetSummary())
		return exitWouldChange
	default:
		fmt.Fprintf(stdout, "settings applied: %s (%s)\n", strings.Join(m.GetChanges(), ", "), m.GetSummary())
	}
	return exitOK
}

func serversExport(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	a := newAdmin("servers export", "gravel servers export [--hub URL]", stderr)
	if _, ok := a.args(args, 0); !ok {
		return exitUsage
	}
	c, ok := a.open()
	if !ok {
		return exitFailed
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	res, err := c.servers().ExportServers(ctx, connect.NewRequest(&hubv1.ExportServersRequest{}))
	if err != nil {
		return c.fail(stderr, a.name, err)
	}
	fmt.Fprint(stdout, res.Msg.GetManifest())
	return exitOK
}

func serversCheck(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	a := newAdmin("servers check", "gravel servers check MANIFEST [--hub URL]", stderr)
	c, text, code, ok := a.manifestCall(args, stdin)
	if !ok {
		return code
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	res, err := c.servers().CheckServers(ctx, connect.NewRequest(&hubv1.CheckServersRequest{Manifest: text}))
	if err != nil {
		return c.invalid(stderr, a.name, err)
	}
	fmt.Fprintf(stdout, "servers valid (%s)\n", res.Msg.GetSummary())
	return exitOK
}

func serversApply(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	a := newAdmin("servers apply", "gravel servers apply MANIFEST [--dry-run] [--hub URL]", stderr)
	dryRun := a.fs.Bool("dry-run", false, "say what would change; change nothing (exit 3 when something would)")
	c, text, code, ok := a.manifestCall(args, stdin)
	if !ok {
		return code
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	res, err := c.servers().ApplyServers(ctx, connect.NewRequest(&hubv1.ApplyServersRequest{Manifest: text, DryRun: *dryRun}))
	if connect.CodeOf(err) == connect.CodeFailedPrecondition {
		// What only the host may change: the hub names each, and the command that may.
		fmt.Fprintf(stderr, "gravel %s: %s\n", a.name, message(err))
		return exitFailed
	}
	if err != nil {
		return c.invalid(stderr, a.name, err)
	}
	m := res.Msg
	switch {
	case len(m.GetChanges()) == 0:
		fmt.Fprintln(stdout, "servers unchanged")
		return exitOK
	case !m.GetApplied():
		fmt.Fprintln(stdout, "servers would change:")
	default:
		fmt.Fprintln(stdout, "servers applied:")
	}
	for _, ch := range m.GetChanges() {
		fmt.Fprintf(stdout, "  %s\n", ch)
	}
	if !m.GetApplied() {
		return exitWouldChange
	}
	return exitOK
}

// ---- stats and notifiers ----

const statsUsage = `usage: gravel stats <command> [--hub URL]

  erase --provider PROVIDER --subject SUBJECT   erase a player's identity from the stats (re-keyed, not deleted)
`

func statsCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return subcommands(ctx, "stats", statsUsage, map[string]func(context.Context, []string) int{
		"erase": func(ctx context.Context, args []string) int { return statsErase(ctx, args, stdout, stderr) },
	}, args, stdout, stderr)
}

func statsErase(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	a := newAdmin("stats erase", "gravel stats erase --provider PROVIDER --subject SUBJECT [--hub URL]", stderr)
	provider := a.fs.String("provider", "", "the identity's provider (steam)")
	subject := a.fs.String("subject", "", "the identity's subject at the provider (a SteamID64)")
	if _, ok := a.args(args, 0); !ok {
		return exitUsage
	}
	if *provider == "" || *subject == "" {
		return a.usageError("--provider and --subject are required")
	}
	c, ok := a.open()
	if !ok {
		return exitFailed
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	res, err := c.stats().EraseIdentity(ctx, connect.NewRequest(&hubv1.EraseIdentityRequest{Provider: *provider, Subject: *subject}))
	if err != nil {
		return c.fail(stderr, a.name, err)
	}
	m := res.Msg
	fmt.Fprintf(stdout, "erased: %d match rows, %d monthly rows, %d participation events, %d participation monthly rows, %d ingest batches re-keyed; pseudonym deleted: %t\n",
		m.GetMatchRows(), m.GetMonthlyRows(), m.GetParticipationRows(), m.GetParticipationMonthlyRows(), m.GetIngestBatches(), m.GetPseudonymDeleted())
	return exitOK
}

const notifiersUsage = `usage: gravel notifiers <command> [--hub URL]

  test NAME   send the settings' notifier NAME a test event; exit 1 when it is not delivered
`

func notifiersCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return subcommands(ctx, "notifiers", notifiersUsage, map[string]func(context.Context, []string) int{
		"test": func(ctx context.Context, args []string) int { return notifiersTest(ctx, args, stdout, stderr) },
	}, args, stdout, stderr)
}

func notifiersTest(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	a := newAdmin("notifiers test", "gravel notifiers test NAME [--hub URL]", stderr)
	pos, ok := a.args(args, 1)
	if !ok {
		return exitUsage
	}
	c, ok := a.open()
	if !ok {
		return exitFailed
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	res, err := c.organization().TestNotifier(ctx, connect.NewRequest(&hubv1.TestNotifierRequest{Name: pos[0]}))
	if connect.CodeOf(err) == connect.CodeNotFound {
		fmt.Fprintf(stderr, "gravel %s: the settings have no notifier %q (gravel settings export lists them)\n", a.name, pos[0])
		return exitFailed
	}
	if err != nil {
		return c.fail(stderr, a.name, err)
	}
	if !res.Msg.GetDelivered() {
		fmt.Fprintf(stderr, "gravel %s: %s did not deliver the test event: %s\n", a.name, pos[0], res.Msg.GetError())
		return exitFailed
	}
	fmt.Fprintf(stdout, "%s delivered the test event.\n", pos[0])
	return exitOK
}
