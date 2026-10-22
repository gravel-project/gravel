# ADR-0027: Capabilities, five roles as bundles, grants with a game scope, and ownership transfer

**Status:** accepted (2026-10-13, with #184 in #231) · **Changes:** how every procedure decides who may call it; supersedes ADR-0013 §1 and §3, amends ADR-0013 §4 and §5, ADR-0008 §4, ADR-0004 §6, ADR-0023 §7 and README "Human roles" · **Decided in:** John's design session *gravel — Design Session: Hub, Spines & Dashboard* (2026-10-10 to 12), handbook D35 · **Built in three steps:** #184 (§1, §2 and §7: capabilities, who holds them, the access matrix; no behaviour change), #185 (§3 to §5: five roles, game scope, granter rules; **Breaking**), #197 (§6: ownership transfer; as built below) · **Part of:** H1 (#182), framed by ADR-0025

## Context

The hub had an owner and one role. ADR-0013 gave the owner a way to grant `moderator`, and each service checked it by hand: `requireOwner`, `requireScope`, `requireModerator`, `requireMemberOrScope` and a host-only `owner()`. Apps had their own seven scopes (ADR-0008), and a member's token was narrowed by them (ADR-0015 §4). Three words for one question, and no way to say what John's design session asks for:

- An **operator** who restarts and parks servers but can't spend money or moderate people.
- An **admin** who can cause spend (create, resize, spin up) and run the hub, but can't give it away.
- Staff **for one game**: an operator of Counter-Strike who can't touch War Dogs.
- **Handing the hub to someone else**, which GRVL's customer hand-off needs and which the unplug test (ADR-0025) exercises.
- **Discord never grants power.** Owner and admin are gravel's alone; a synced Discord role may grant a low-trust role at most.

The design doc's role table is the target:

| Role | Purpose | Can spend money | Infrastructure | People |
|---|---|---|---|---|
| Owner | Holds the keys and the billing relationship; exactly one | Yes | Everything | Everything |
| Admin | Staff who can cause spend | Yes: create, resize, spin up | Full | — |
| Operator | Manages existing infrastructure | No | Restart, reboot existing servers; no creating | — |
| Moderator | Community and people management, disputes | No | None | Yes |
| Member | Everyday players | No | None | None |

## Decision

1. **Capabilities are the one vocabulary.** A capability is an `area:verb` name, and every procedure checks exactly one, through `authz.Principal.Check(capability, target)` (`internal/authz`). App scopes *are* capabilities: the seven names apps were issued keep their meaning, and each later capability becomes one an app may hold when a procedure lets an app use it, so `apps create` never accepts a scope that does nothing. Some capabilities are never an app's (`org:transfer`), and one is only an app's (`participation:write`: what an app saw, not what a person claims, ADR-0014). Host scopes (`host:…`) stay opaque: gravel admits nothing by them.

   | Capability | Admits | An app may hold it |
   |---|---|---|
   | `identity:read` | Members and their identities (`LookupUser`, `ListUsers`, `ListIdentityEvents`) | Yes |
   | `servers:read` | Who is on a server, its host (`ListServerPlayers`, `GetServerHost`, `ListServerHostEvents`) | Yes |
   | `servers:moderate` | Acting on players, bans, the audit log (`ModerationService`); park and unpark until #185 | Yes |
   | `servers:lease` | Taking a server for a match (`ServerLeaseService`) | Yes |
   | `servers:configure` | A server's configuration (`ServerConfigService`) | Yes |
   | `stats:read` | Boards, match lists and results while stats are private | Yes |
   | `participation:write` | Recording participation (`RecordParticipation`) | Only an app |
   | `servers:operate` | Start, park, restart existing servers at their size (#185) | With #185 |
   | `servers:provision` | Resize, pin, snapshot, destroy: anything that changes spend | Not yet |
   | `servers:manage` | Add, change, remove servers: the manifest (#195) | Not yet |
   | `members:manage` | Local accounts, set-password links, ending sessions (#186, #187) | Not yet |
   | `roles:grant` | Granting and revoking roles (`SetUserRole`) | With #185, moderator only |
   | `settings:write` | The Organization settings (`UpdateOrganizationSettings`) | Not yet |
   | `apps:manage` | Registering and revoking apps (#195) | Not yet |
   | `events:read` | The events log (#188) | With #188 |
   | `metrics:read` | Operator dashboards through the hub (#204) | With #204 |
   | `stats:erase` | Erasing a player from the stats (#195) | Not yet |
   | `org:transfer` | Handing the hub to another member (#197) | Never |

2. **Who holds what is resolved once per call.** The API builds a principal from the request (`api.Access`): the **owner** in person holds every capability a person can hold; a **member** in person holds the member base and their roles' bundles; a **member's token** holds what the member holds, narrowed to the token's scopes (ADR-0015 §4, unchanged); an **app** holds its scopes that an app may hold; **nobody** holds nothing. A refusal is `unauthenticated` for nobody and `permission_denied` for everyone else, an app included: an app is who it says it is, and lacking a capability is not a login problem. An app that names `on_behalf_of` acts only for a member who holds the capability themselves (ADR-0013 §4, kept); a lease names its holder, who must be a member but needn't hold `servers:lease` (ADR-0023). `GetMe` and `ListUsers` return each member's `capabilities`.

3. **Five roles are fixed bundles, in Go.** They are not host-editable, because "sync never grants admin" is unenforceable once a host can put `org:transfer` in a bundle. **Member** is the registered base, never a grant.

   | Role | Capabilities |
   |---|---|
   | Owner | Every capability a person can hold, `org:transfer` included. Exactly one, never a grant |
   | Admin | Everything but `org:transfer` |
   | Operator | `servers:read`, `servers:operate`, `servers:configure`, `servers:lease`, `events:read`, `stats:read`, `metrics:read` |
   | Moderator | `servers:read`, `servers:moderate`, `servers:lease`, `identity:read`, `events:read`, `stats:read` |
   | Member | `servers:read` (who is on a server: every member's server page shows it) |

   *Operate* is an existing host at its size; *provision* is anything that changes what the hub spends. There is no billing capability: billing is GRVL's, outside the hub. Until #185 the bundles are the ones ADR-0013 admitted (owner everything; moderator `servers:read`, `servers:moderate`, `servers:lease`; member `servers:read`), read from `users.roles`, so #184 changes nothing a caller sees.

4. **A grant is a role for a member, optionally for one game** (#185). `role_grants(user, role, game_id NULL)`: no game is instance-wide; a game covers every server of that game, those added later included, because the check resolves the server's game at call time. Only capabilities marked scopable (`servers:*`, `events:read`) are narrowed by a game; a game-scoped admin can't edit the settings or the servers manifest. Every grant and revocation is appended to `role_changes`.

5. **Who may grant what** (#185): the owner any role; an admin operator or moderator; a game-scoped admin only within that game; an app holding `roles:grant` moderator only (so Discord role sync can map a Discord role onto moderator and nothing higher); nobody their own grants. Owner and admin are gravel-native: no synced role, no app.

6. **Ownership moves in two steps** (#197). The owner nominates an existing member, in person; the nominee accepts, in person, within 7 days. Accepting is one transaction: exactly one owner at every moment, and the old owner becomes an admin. Apps and members' tokens can do neither.

7. **The access matrix is the contract.** `TestAccessMatrix` calls every procedure as every kind of caller and compares the answers with `internal/api/testdata/access_matrix.txt`. A change to who may call what is a diff of that file in the pull request that makes it; a new procedure fails the test until it is listed.

## Consequences

- #184: `internal/authz`; `api.Access` replaces the five hand-written checks; `apps.Scopes` derives from the capabilities an app may hold; `User.capabilities`. The matrix moves in one place: an app calling `UpdateOrganizationSettings` or `SetUserRole` is now `permission_denied`, not `unauthenticated`.
- #185 is **Breaking**: moderators lose park and unpark, which move to operator; the migration grants every existing moderator both moderator and operator, so Hidden Token Gaming's staff notice nothing until the owner prunes. Moderators gain `identity:read` and `stats:read`. Muster's staff check and the bot's `/wd` keep reading `User.roles`, which keeps the instance-wide role names.
- `users.roles` is read until #185 and dropped by #199 after the grants have run Hidden Token Gaming for a week.
- Not decided here: custom roles (no: bundles are fixed); per-server scope (no: a game is the grain); a capability for billing (GRVL's).

## As built (#185, 2026-10-13)

- **Grants** are rows of `role_grants` (migration 00025): a member, a role, an optional game that must be one the hub has, who granted it (a member, an app, or neither for the migration). `role_changes` gained the game and the app. `User.roles` is now the member's instance-wide grants by name; `User.grants` carries every grant with its game; `ListRoleGrants` lists them (`identity:read`).
- **A game-scoped grant gives only the bundle's scopable capabilities** (`servers:*`, `roles:grant`, `events:read`), for that game; the rest (`identity:read`, `stats:read`, `settings:write`…) needs an instance-wide grant. `roles:grant` is scopable so a game-scoped admin grants within their game, as §5 says.
- **The game is resolved at call time** from the server a procedure names, so a server added to a game later is covered. The two lists that span servers, the audit log and the lease list, admit a caller holding the capability for any game and show them that game's rows.
- **Leasing in person** needs `servers:lease` everywhere: a lease names a pool, not a server, and leases are an app's (Muster) in practice.
- **The member base keeps `servers:read`** (who is on a server, on every member's server page); §3's table says so.
- **App scopes:** `servers:operate` and `roles:grant` (moderator only) became scopes an app may hold. An app holding `servers:moderate` lost park and unpark, which moved to `servers:operate`; no first-party app of Hidden Token Gaming's called them.
- **The Members page** lists every member's grants with remove buttons and a grant form (role, every game or one) for anyone holding `roles:grant`; the Members link shows to them.

- **`users.roles` is dropped** (#199, 2026-10-22, migration 00039) after the grants ran Hidden Token Gaming's hub for a week (gravel 0.9.0, live there 2026-10-15). Going down puts the column back with `moderator` for every instance-wide moderator grant, the only role ADR-0013's column could hold.

## As built (#197, 2026-10-13): §6, ownership transfer

- **Three procedures, in person only:** `NominateOwner` checks `org:transfer` (the owner alone; no app may hold it); `AcceptOwnership` and `CancelOwnershipTransfer` admit any member through their session and the store decides (the nominee accepts or declines, the owner cancels). Members' tokens are refused for all three.
- **The nomination is three columns on `organizations`** (migration 00029), set and cleared together, and never the owner. A new nomination replaces the old one. An expired one is cleared only by a decline, a cancel or a new nomination: nothing runs at the expiry, and an accept after it is refused.
- **Accepting is one transaction:** the row is locked, one `UPDATE … WHERE owner_user_id = $old AND pending_owner_user_id = $me AND pending_owner_expires_at > $now` moves the owner, the old owner gets an instance-wide `admin` grant (by the new owner), and the events are written. Concurrent accepts serialise on the lock; the first wins and the rest find no nomination.
- **Who sees the nomination:** `GetOrganization` carries it to the owner and the nominee only.
- **Events:** `ownership.nominated`, `ownership.transfer_cancelled`, `ownership.declined` (notice) and `ownership.transferred` (warning, so a notifier's default filter carries it), each by the member who acted.
- **The pages:** the Members page has the owner's *Hand the hub to …*, the account page the nominee's Accept and Decline and the owner's Cancel. The admin dashboard's people page (H1-19) will carry the same handshake.
