# The hub

One Go binary, `gravel-hub`, beside Postgres. This page is the operator reference: configuration,
endpoints, login, the owner claim, migrations, running it, observability and tests. ADR-0001 is the
why, ADR-0004 the login design, ADR-0008 the app credentials; `deploy/README.md` is the container how-to.

## Commands

| Command | Does |
|---|---|
| `gravel-hub serve` (default) | Serve the API. Stops cleanly on SIGINT/SIGTERM, draining for `server.shutdown_timeout`. |
| `gravel-hub migrate` | Apply pending migrations, then print the schema version. |
| `gravel-hub migrate --status` | Print the schema version and pending count; exit 3 when migrations are pending. |
| `gravel-hub config check` | Load and validate the configuration, print a summary with the login providers (never a secret). |
| `gravel-hub healthcheck [--url …]` | GET `/healthz` and exit 0 when it answers 200. The container healthcheck. |
| `gravel-hub settings export` | Print the Organization settings as a YAML manifest. |
| `gravel-hub settings apply <manifest.yaml> [--dry-run]` | Validate and store a manifest; "unchanged" or "applied". With `--dry-run`, exit 3 when it would change something. |
| `gravel-hub settings check <manifest.yaml>` | Validate a manifest with no configuration and no database: exit 0 and print its summary, 1 with every problem named, 2 on a usage error. |
| `gravel-hub apps create --name NAME --scopes SCOPES` | Register a first-party app (ADR-0008) and print its client id and secret once; the hub keeps the secret's hash. |
| `gravel-hub apps list` | The registered apps: client id, name, scopes, created, revoked. |
| `gravel-hub apps revoke --client-id ID` | Revoke an app: its tokens are deleted and no new one is issued. |
| `gravel-hub servers export` | Print the games and servers as a `servers.yaml` manifest ([Servers](#servers-adr-0010)). |
| `gravel-hub servers apply <servers.yaml> [--dry-run]` | Validate a manifest, check each credential file is readable where it runs, and store it; prints each change. With `--dry-run`, exit 3 when something would change. A running hub picks it up within 30 s. |
| `gravel-hub servers check <servers.yaml>` | Validate a manifest with no configuration, no database and no secret: exit 0 with its summary, 1 with every problem named, 2 on a usage error. |
| `gravel-hub ingest export --server ID [--after ID] [--limit N]` | Print a server's stored ingest batches, oldest first, one JSON object per line: a JSON body as JSON, anything else as base64 ([Ingestion](#ingestion-adr-0011)). They hold players' SteamIDs: redact before they leave the host. |
| `gravel-hub stats erase --provider P --subject S` | Erase a player from the stats store ([Stats](#stats-adr-0012), Erasure): their identity becomes a random token in every stats table and the stored ingest batches, their pseudonym is deleted, the numbers stay. For an operator acting on a request they have verified. Prints the counts, never the identity. |
| `gravel-hub wardogs record --base-url URL [--token-file FILE] [--dir DIR]` | Record a War Dogs server's read-only answers as test fixtures in `<dir>/<build>/` (default `games/wardogs/testdata`), with people, addresses and secrets replaced (ADR-0010; `games/wardogs/README.md`). One token, never retried; without one, only the public routes. |
| `gravel-hub version` | Print the build version. |

The config path is `--config`, else `$GRAVEL_CONFIG`, else `/etc/gravel/hub.yaml`.

## Configuration

One YAML file, strict (an unknown key is an error) and versioned. Every key except the first three
has a default.

```yaml
version: 1                       # required; this build reads version 1
organization:
  name: Hidden Token Gaming      # required; the built-in organization's display name
database:
  url_file: /run/secrets/gravel-db-url   # a podman secret holding postgres://user:pw@host:5432/db
  # url: postgres://…            # development only; never commit a password
  migrate: auto                  # auto: apply pending migrations at start · manual: refuse to serve until `gravel-hub migrate` ran
  max_conns: 8
  connect_timeout: 60s           # how long to wait for Postgres to accept connections at start
server:
  listen: 127.0.0.1:8080         # API, pages, /healthz, /readyz; put TLS termination (cloudflared) in front
  internal_listen: 127.0.0.1:9090 # /metrics and /debug/pprof/; keep it off the public network
  shutdown_timeout: 15s
  client_ip_header: ""           # CF-Connecting-IP behind cloudflared; see Login below
log:
  level: info                    # debug · info · warn · error
  format: json                   # json · text
claim:
  token_ttl: 15m                 # how long an owner-claim token lives
auth:
  base_url: https://app.example.com   # the origin members use; required once a provider is enabled
  session_ttl: 720h              # a login lasts 30 days; no idle timeout
  attempt_ttl: 10m               # how long a member has to come back from a provider
  discord:
    enabled: true
    client_id: "1234567890"
    client_secret_file: /run/secrets/gravel-discord-client-secret   # or GRAVEL_DISCORD_CLIENT_SECRET
    login: true                  # Discord is the primary login
  steam:
    enabled: true
    api_key_file: /run/secrets/gravel-steam-api-key   # optional: persona name and avatar; or GRAVEL_STEAM_API_KEY
    login: false                 # link-only by default
rate_limit:
  per_ip:   {requests_per_minute: 120, burst: 40}    # anonymous requests, per client address
  per_user: {requests_per_minute: 600, burst: 100}   # authenticated requests, per user
  per_app:  {requests_per_minute: 600, burst: 100}   # requests carrying an app's bearer token, per app
apps:
  token_ttl: 1h                  # how long a bearer token from /oauth/token lives
stats:
  pseudonym_key_file: /run/secrets/gravel-stats-pseudonym-key   # the key pseudonyms are chosen with ([Stats](#stats-adr-0012))
```

**Secrets.** Four, each with the same precedence: the environment variable, then the `*_file`
key (a podman secret mount), then the inline key, which is for development only.

| Secret | Environment | File key | Inline key |
|---|---|---|---|
| Database URL | `GRAVEL_DATABASE_URL` | `database.url_file` | `database.url` |
| Discord OAuth2 client secret | `GRAVEL_DISCORD_CLIENT_SECRET` | `auth.discord.client_secret_file` | `auth.discord.client_secret` |
| Steam Web API key (optional) | `GRAVEL_STEAM_API_KEY` | `auth.steam.api_key_file` | `auth.steam.api_key` |
| Stats pseudonym key (optional; at least 32 characters, `openssl rand -hex 32`) | `GRAVEL_STATS_PSEUDONYM_KEY` | `stats.pseudonym_key_file` | `stats.pseudonym_key` |

Production uses podman secrets (`Secret=gravel-db-url` in the quadlet unit lands at
`/run/secrets/gravel-db-url`, and so on). The hub logs the database URL with the password
redacted and, for every secret, which source it came from. There is no session key: sessions are
random tokens whose hashes live in Postgres (ADR-0004).

## Endpoints

Public listener (`server.listen`), HTTP/1.1 and unencrypted HTTP/2 so gRPC works behind TLS termination:

| Path | What |
|---|---|
| `/gravel.hub.v1.OrganizationService/GetOrganization` | Connect procedure; POST JSON `{}` or call it over gRPC / gRPC-Web |
| `/gravel.hub.v1.OrganizationService/ClaimOwnership` | Connect procedure; `{"token": "…"}`; needs a session, binds the caller as owner |
| `/gravel.hub.v1.OrganizationService/GetOrganizationSettings` | the Organization settings (theme, navigation); public |
| `/gravel.hub.v1.OrganizationService/UpdateOrganizationSettings` | replace them; the owner only; `invalid_argument` names every invalid field |
| `/gravel.hub.v1.IdentityService/GetMe` | the caller and their linked identities; needs a session |
| `/gravel.hub.v1.IdentityService/UnlinkIdentity` | `{"provider": "steam", "subject": "…"}`; the last identity is refused (`failed_precondition`) |
| `/gravel.hub.v1.IdentityService/Logout` | end the calling session and clear its cookie |
| `/gravel.hub.v1.IdentityService/RevokeSessions` | log out everywhere; answers how many sessions ended and clears the cookie |
| `/gravel.hub.v1.IdentityService/LookupUser` | `{"provider": "discord", "subject": "…"}` → the user and their identities, for role sync; an app with `identity:read`, or the owner |
| `/gravel.hub.v1.IdentityService/ListUsers` | `{"page_size": 100, "page_token": "…"}` → members with their identities, oldest first, and the next page's token; role sync's full pass; `identity:read` or the owner |
| `/gravel.hub.v1.IdentityService/SetUserRole` | `{"user_id": "…", "role": "moderator", "granted": true}` → the member with their roles ([Roles](#roles-adr-0013)); every change recorded; the owner only |
| `/gravel.hub.v1.IdentityService/ListIdentityEvents` | `{"after_id": 0, "limit": 100}` → the identity log after a position, oldest first, the id to continue from, and the log's newest id (`head_id`, where a reader that just made a full pass starts); role sync's incremental pass; `identity:read` or the owner |
| `/gravel.hub.v1.ServerService/ListGames` | the enabled games, their identity provider, bands, layout and `no_paid_perks`; public |
| `/gravel.hub.v1.ServerService/ListServers` | every server with its last observation (state, reachable, players and max, map, teams, capabilities, build, `observed_at`); public, no personal data, never a control address |
| `/gravel.hub.v1.ServerService/GetServerStatus` | `{"server_id": "…"}` → one server; public; `not_found` for an unknown id |
| `/gravel.hub.v1.StatsService/GetMemberProfile` | `{"user_id": "…"}` (empty: the caller) → a member's totals by window and recent matches ([Pages](#pages-gravel18)); the member, the owner, `stats:read`, or anyone when they show their name and stats are public; else `not_found` |
| `/gravel.hub.v1.StatsService/GetBoard` | `{"server_id"/"game_id", "window": "week", "metric": "kd"}` → a ranked page of players under pseudonyms or opted-in names ([Stats](#stats-adr-0012)); public when the settings make stats public, else the owner or `stats:read` |
| `/gravel.hub.v1.StatsService/ListMatches` | `{"server_id": "…"}` → a server's matches, newest first; the same access |
| `/gravel.hub.v1.StatsService/GetBoardName`, `SetBoardName` | a logged-in member's choice to be shown by name on boards (off by default) |
| `/gravel.hub.v1.ServerService/ListServerPlayers` | `{"server_id": "…"}` → who was on at the last poll (name, provider identity, team, kills, deaths, ping, and the member's user id when that identity is linked); a logged-in member, the owner, or an app with `servers:read` |
| `/grpc.health.v1.Health/Check` | gRPC health |
| `/grpc.reflection.v1.ServerReflection/…` (and v1alpha) | gRPC reflection, so `grpcurl` and `buf curl` discover the API |
| `GET /healthz` | 200 `{"status":"ok","version":"…"}` while the process runs |
| `GET /readyz` | 200 `{"status":"ready"}` when Postgres answers and no migration is pending, else 503 with a reason |
| `POST /oauth/token` | the token endpoint for first-party apps (ADR-0008): client credentials in, a Bearer token out; see [Service credentials](#service-credentials-adr-0008) |

Pages and flows, on the same listener (ADR-0005: the pages are templ components that call the
procedures above in process; htmx, served from the binary, swaps the main content on forms, and
every page works without it):

| Path | What |
|---|---|
| `GET /` | to `/account`, or `/login` when anonymous |
| `GET /login` | the login providers |
| `GET /account` | the member's identities, link buttons, unlink forms, "log out everywhere", and the claim form while the hub is unowned |
| `GET /auth/{provider}/start` | begin a login (providers with `login: true`) |
| `GET /auth/{provider}/link` | begin a link, logged in |
| `GET /auth/{provider}/roles` | begin Discord's Linked Roles verification: a login that also publishes the member's linked accounts to Discord (below) |
| `GET /auth/{provider}/callback` | the provider's return |
| `POST /auth/logout` | this session (`Logout`), or every session with `everywhere=1` (`RevokeSessions`) |
| `POST /account/unlink`, `POST /account/claim` | forms; every POST carries the session's CSRF token in `_csrf`; with `HX-Request: true` the answer is the page's content, else a redirect |
| `GET /theme.css` | the theme's tokens as custom properties, with an ETag |
| `GET /static/app.css`, `GET /static/vendor/htmx.min.js` | the stylesheet and htmx |

The pages send `Content-Security-Policy: default-src 'none'; script-src 'self'; style-src 'self';
img-src https: data:; connect-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors
'none'`: nothing inline, nothing from a third party. A host's look comes from a theme (tokens
for the light and dark schemes, the font, a logo, a favicon, navigation links in the header or
the footer, optionally owner-only); gravel#7 stores it on the Organization settings, and the
defaults apply until then. Editing a page means editing its `.templ` file under
`internal/web/templates/` and running `make generate`; the generated Go is committed.

Internal listener (`server.internal_listen`): `GET /metrics` (Prometheus) and `GET /debug/pprof/`.

The protobuf source is `proto/gravel/hub/v1/`; the generated Go is `gen/`; the OpenAPI document
for the JSON side is `gen/openapi/hub.openapi.yaml`. `make generate` rebuilds them.

```sh
# JSON
curl -X POST -H 'Content-Type: application/json' -d '{}' http://127.0.0.1:8080/gravel.hub.v1.OrganizationService/GetOrganization
# gRPC, via reflection
grpcurl -plaintext 127.0.0.1:8080 gravel.hub.v1.OrganizationService/GetOrganization
```

## Organization settings (ADR-0007)

Every host-configurable knob is one document on the organization: the theme (tokens for the
light and dark schemes, the font, a logo, a favicon), the navigation links and the Discord role
mapping now; token lifetimes and layout overrides later. Zero values mean the default. The pages
read it through the API; the owner writes it through `UpdateOrganizationSettings`, or a
deployment commits a manifest and applies it:

```yaml
# organization.yaml
version: 1
theme:
  dark:                      # colours as plain CSS colours; omit a token to keep the default
    background: "#070b17"
    foreground: "#e8eef6"
    muted: "#93a3bb"
    line: "#1f2a44"
    accent: "#3ee07a"
  light:
    background: "#f2f5f9"
    foreground: "#0b1222"
    accent: "#0a7a3a"        # meets AA on the light background; the dark accent would not
  font: Archivo, system-ui, sans-serif   # the system font when omitted; the hub serves no webfont
  logo_url: https://example.com/brand/mark.svg   # https or site-relative; shown beside the name
  favicon_url: https://example.com/favicon.svg
nav:                         # at most 12; label at most 40 characters
  - label: Discord
    url: https://discord.gg/…
  - label: Rules
    url: https://example.com/rules/
    placement: footer        # header (default) or footer
  - label: Admin
    url: /admin
    role: owner              # shown to the owner only
discord:                     # what the bot manages (docs/bot.md); ids as quoted strings
  guild_id: "519496143298756611"
  roles:
    linked: "…"              # a member with any account besides Discord
    providers:               # a member who linked this provider (discord, steam)
      steam: "…"
  recognition:               # earned once, never removed; at most 10
    - role: "…"
      rule: first_members    # the first `count` members by registration
      count: 50
  server_cards:              # a live status card per server, edited in place; at most 10
    - server: htg-wardogs-1  # the server's id in servers.yaml, one card each
      channel: "…"           # the bot needs View Channel, Send Messages, Read Message History
      note: Matches start at 20 players.   # optional, one line of at most 200 characters
stats:                       # the boards ([Stats](#stats-adr-0012))
  public: false              # true once the host's privacy policy covers stats; until then the owner and stats:read
  min_matches: 3             # a player's matches before a K/D board shows them (default 3)
  timezone: America/Chicago  # weeks, months and season dates (default UTC)
  raw_retention_months: 13   # per-match rows older than this roll up into monthly totals (default 13, at most 120)
  seasons:                   # named windows; at most 50; ending one deletes nothing
    - name: Season 02
      game: wardogs          # optional: one game's matches
      from: "2026-10-15"     # included
      to: "2027-01-15"       # excluded
```

```sh
podman exec gravel-hub /ko-app/gravel-hub settings apply /etc/gravel/organization.yaml --config /etc/gravel/hub.yaml
podman exec gravel-hub /ko-app/gravel-hub settings export --config /etc/gravel/hub.yaml
```

`apply` is idempotent; `--dry-run` exits 3 when the manifest differs from what is stored, so a
converge can check before writing. An invalid manifest is refused with every problem named, and
nothing is stored. On a database that was migrated but never served, `apply` and `export` create
the built-in organization with the configured name, as `serve` would, and leave the owner claim
alone: the token `serve` prints stays the one that claims the hub. A `--dry-run` there compares
with the defaults and creates nothing.

`settings check` runs the same strict parse and validation as `apply`, without a configuration or
a database, so a deployment's CI can gate its committed manifest on the pinned hub image: exit 0
with the summary `apply` would print (`settings valid (2 nav links, theme 5 tokens, font, discord
unmapped)`), 1 with the offending fields named (an unknown key by name and line, a bad value by
its path, such as `theme.dark.accent`), 2 on a usage error.

```sh
podman run --rm -v ./hub/organization.yaml:/m.yaml:ro,Z ghcr.io/gravel-project/gravel-hub:<version> settings check /m.yaml
``` The default tokens meet WCAG AA contrast in both schemes; a host's tokens are
the host's responsibility (`make a11y` checks the defaults only).

The `discord` section holds ids, never a credential, and is as public as the rest. Ids are Discord
snowflakes as quoted strings (YAML would read a bare one as a number); a role may be mapped once,
never to the guild's own id (`@everyone`), and roles need a guild. The bot reads the section at
every role-sync pass, so a mapping change is a manifest change and lands within one pass.

## Service credentials (ADR-0008)

A first-party service (a host's Discord bot, later Muster) calls the API as an **app**, not as a
member: a registered client id and secret, traded at the token endpoint for a short-lived opaque
bearer token that carries the app's scopes. The secret and every token are stored as SHA-256
hashes, like session tokens. This is the client-credentials slice of #6; the authorization-code
flow for people comes when a first-party web app needs it.

```sh
# Register (on the hub's host, no API credential needed; the secret is printed once)
gravel-hub apps create --name htg-bot --scopes identity:read --config /etc/gravel/hub.yaml
# Trade the credentials for a token (RFC 6749 §4.4; the id and secret go through HTTP Basic)
curl -u "$CLIENT_ID:$CLIENT_SECRET" -d grant_type=client_credentials https://app.example.com/oauth/token
# {"access_token":"…","token_type":"Bearer","expires_in":3600,"scope":"identity:read"}
# Call the API with it
curl -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' -d '{"provider":"discord","subject":"1234"}' \
  https://app.example.com/gravel.hub.v1.IdentityService/LookupUser
```

| Scope | Admits |
|---|---|
| `identity:read` | `LookupUser`, `ListUsers`, `ListIdentityEvents`: members and their identities, for role sync |
| `servers:read` | `ListServerPlayers`: who is on a server, for a live card (the rest of `ServerService` is public) |
| `servers:moderate` | `ModerationService`: kick, ban, unban, message, broadcast and move players, list a server's bans, read the audit log |
| `servers:configure` | `ServerConfigService`: read, plan and apply a server's configuration |
| `stats:read` | `StatsService` `GetBoard` and `ListMatches` while the settings keep stats private |

Rules: a token lives `apps.token_ttl` (an hour by default) and is pruned after; `scope` at the
token endpoint may narrow a token to a subset of the app's scopes, never widen it; an unknown,
expired or revoked token is answered 401 with `WWW-Authenticate: Bearer error="invalid_token"`
before any procedure runs; a procedure an app may not call answers `permission_denied`, and one a
member may not call still does. The owner's session is admitted wherever a scope is, so the owner
can do by hand what the app does. Requests carrying a token are rate-limited per app
(`rate_limit.per_app`), the token endpoint per client address. To rotate a secret, create a new
app and revoke the old one once the service has moved; `apps revoke` deletes the tokens at once.

## Servers (ADR-0010)

The hub knows the game servers it controls as resources: a **game** is a Game spec gravel ships
(`games/<game>/game.yaml`: who its players are keyed by, which drivers control it, the
publisher's rules), and a **server** is one a driver controls. A deployment declares both
in `servers.yaml` and applies it like the Organization settings; the hub stores them (migration 6)
and never stores a credential, only the file to read it from.

**Game specs and publisher rules (gravel#8).** gravel ships five specs. War Dogs has a driver.
Counter-Strike 2, Sea of Thieves, Star Citizen and PUBG are **catalog entries** (`drivers: []`):
their rules are known before gravel can run a server for them. A manifest may enable a catalog
game, but a server for one is refused. A spec's rules:

| Rule | What it is | Enforced |
|---|---|---|
| `bands` | One per configuration key, of one kind: a range (`min`, `max`), a longest value (`max_length`, in characters) or the hosts a URL may point at (`hosts`: https, the host or a subdomain of one; an empty value passes). `unless_set` lifts a band while another key of its section has a value. | By the driver on every config plan and apply, before anything is sent (problems `out_of_band`, `too_long`, `host_not_allowed`). `servers.yaml` may tighten a band of the same kind (a narrower range, a shorter length, a subset of hosts), never loosen one or change its `unless_set` |
| `plugins` | `deny`: rules, each a `match` and the publisher's `reason`; a plugin whose name contains a match is refused. `allow`: when set, the only plugins permitted. Names compare by letters and digits only, case-insensitively. The denylist wins over the allow list. | `Spec.CheckPlugins` refuses a set naming every refused plugin and why; the Counter-Strike 2 image build and driver call it (P4) |
| `layout` | How the pages show the game, as data: `board` is a board's columns in order (`kills`, `deaths`, `kd`, `time`, `matches`), the first being what it ranks by. Shown by `ListGames` (`Game.layout`). | The pages ([Pages](#pages-gravel18)) |
| `no_paid_perks` | The publisher forbids a paid benefit (a supporter perk, a crowdfunding reward) that depends on the game's data or features. Shown by `ListGames`. | The benefit logic honours it (P6) |

War Dogs' bands: `ScorePeriod` 18–30, `MinimumRequiredPlayers` at least 20, `ServerName` at most 64
characters, `ServerImageURL` on Bulkhead's image allow-list (catbox.moe, imgbb.com, ibb.co,
postimg.cc; an off-list banner otherwise fails every later edit with a 422), and
`MaxReservedSlots` 0 unless `DefaultReservedPlayerIds` lists someone. A test checks every band
against the newest recorded build's schema, so a band can't name a section or key the server
doesn't use. The Counter-Strike 2 denylist covers Valve's item-spoofing rules (weapon skins,
knives, gloves, stickers, music kits, inventory changers, fake ranks and coins), whose penalty is
every GSLT on the account. `knife` also refuses a knife-round plugin, which is deliberate:
MatchZy has knife rounds built in.

```yaml
version: 1
games:
  - id: wardogs
    bands:                       # optional: tighten a spec band, never loosen one
      - section: MatchState.Playing.KOTH
        key: ScorePeriod
        min: 21
servers:
  - id: htg-wardogs-1            # lower-case letters, digits, hyphens
    name: "HTG WARDOGS | NA WEST | #1"
    game: wardogs
    driver: wardogs
    location: qonzer-slc
    endpoint: http://203.0.113.10:7789          # the RCON origin; never shown by the API
    credential_file: /run/secrets/htg-wardogs-rcon-password
    poll_interval: 15s           # 5s–10m; default 15s (two requests a poll)
    trust: official              # or community
    seeding:                     # optional; hidden-token-gaming/htg#32 reads it through the API
      threshold: 20
      hours: "17:00-23:00"
      quiet: "23:30-09:00"
      timezone: America/Chicago
      cooldown: 1h               # default 1h
    feed:                        # optional: where the server pushes its events (ADR-0011, Ingestion)
      url: https://ingest.example.com    # an origin; the game adds /api/ingest/events
      token_file: /run/secrets/htg-wardogs-feed-token   # line 1 the token; line 2 the previous one, while rotating
```

```sh
# CI: validate the committed manifest with the pinned image (no config, database or secret)
podman run --rm -v ./hub/servers.yaml:/m.yaml:ro,Z ghcr.io/gravel-project/gravel-hub:<version> servers check /m.yaml
# On the host: the credential as a podman secret (Secret= in the hub's unit), then apply
podman exec gravel-hub /ko-app/gravel-hub servers apply /etc/gravel/servers.yaml --config /etc/gravel/hub.yaml
```

Taking a server out of the manifest marks it removed (its row stays, so what refers to it keeps a
target); putting it back revives it. `apply` refuses a credential or feed token file it cannot
read, so run it where the hub's secrets are.

**What the hub does with them.** A background job (`servers_reconcile`, every 30 s) starts one
poll job per server and stops the poll of a removed one; a changed definition gets a fresh driver,
and a changed credential file reaches the driver without a restart (a rotation). Each poll reads
status and players through the driver (the capabilities once, at the first poll) and keeps the
last observation in memory with `observed_at`; the API answers from it, so no caller ever reaches
a game server. Two failed polls in a row mark the server unreachable; the next success clears it;
a failing poll backs off up to 8× its interval (at most 2 minutes). The War Dogs driver sends one
token and, after a 401, nothing protected until the credential file changes, so a wrong password
costs one strike against the server's three-strike lockout.

The public status says what happened, never why: `state` is `unknown` (not polled yet), `ok`,
`unreachable`, `credential_refused`, `credential_missing`, `rate_limited` or `error`, and the cause
(which may name the control address) is in the hub's log, once per change of state.
`capabilities` is what the server grants now, so a page or a bot shows an absent one as not
available.

**The build watcher.** A second job per server (`server_build`, every minute) reads the server's
build from a route that needs no credential (War Dogs: `GET /v1/capabilities`). When the build
changes (a game update) it reads the capabilities again, so the status API, the pages and
moderation follow the new build at once: a route the update removed turns its capability off, and
a call for it answers `failed_precondition` instead of failing at the server. The hub logs a
warning naming the old and the new build and the capabilities gained and lost, and the driver logs
the route-level diff (War Dogs: routes added, removed or renamed), which is what re-recording the
fixtures needs (`games/wardogs/README.md`). `gravel_driver_build_info{server,game,build}` names the
build each server runs, and `gravel_driver_build_changes_total{server,game}` counts the changes;
alert on an increase of the counter (a host's Prometheus rule). 

### Configuration and the hub's ban list

`ServerConfigService` reads and writes a server's configuration through its driver, for the owner
or an app with `servers:configure` (a deployment's CI, applying the document it keeps in its own
repository). `GetServerConfig` is the current document, every secret redacted, with the server's
schema (when each section takes effect, the keys a switch locks). `PlanServerConfig` takes the
deployment's document and shows what applying it would change, key by key and redacted, with the
server's own validation; it writes nothing. `ApplyServerConfig` writes it, only if the server is
still at the revision the plan answered (`aborted` otherwise: plan again), and leaves an audit
entry (action `apply_config`, the revision, the caller's reason).

```sh
curl -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d "$(jq -n --rawfile t ServerSettings.ini '{server_id:"htg-wardogs-1",text:$t}')" \
  https://app.example.com/gravel.hub.v1.ServerConfigService/PlanServerConfig | jq '.changes, .result'
```

**What the deployment's document leaves out.** The hub owns some of it, and the driver merges it in:
for War Dogs, the RCON section (the password the hub authenticates with, and `AllowedHosts`, which
could lock the hub out) is copied from the server's own document; the feed section is written from
`servers.yaml`'s `feed` (`Url`, and `Token` from the token file's first line; copied from the server
when it has no feed); a secret the
document writes as `<redacted>` keeps the server's value; and `DefaultBannedPlayerIds` is the hub's
ban list. A document that sets any of these is refused (`invalid_argument`, problem `hub_owned`),
and so is a value outside the game's bands (`out_of_band`, `too_long`, `host_not_allowed`), before anything is sent.

**The ban list.** When the server lets the hub write its configuration, `BanPlayer` bans a player
whether or not they are on: the ban joins the hub's list (migration 8, `bans`), the hub writes the
list into the configuration, and a player who is on is also banned through the server's own route,
which kicks them. The first time the hub writes a server's list it adopts the one there (each entry
becomes a hub ban with no audit entry), so nothing a host banned before is dropped. Both directions
fail toward banned: a ban the server refuses is rolled back in the hub, and an unban is written to
the server before the hub's record is lifted. `ListServerBans` shows the server's bans with the
hub's record (`hub`, `audit_id`). A server that does not let the hub write its configuration bans
through its own route only, which needs the player on.

### Moderation and the audit log

`ModerationService` acts on a server's players through its driver: `KickPlayer`, `BanPlayer`,
`UnbanPlayer`, `MessagePlayer`, `Broadcast` and `MovePlayer` (with `respawn`, a kill after the move
so the player comes back on the new team), plus `ListServerBans` and `ListAuditLog`. The owner and
moderators ([Roles](#roles-adr-0013)) may call it, and so may an app with `servers:moderate` (a bot).
An app may name the person it acts for (`on_behalf_of`, e.g. the Discord moderator behind a
command), who must be the owner or a moderator; the hub records both. A logged-in user acts as
themselves. A player is named by their id at the game's identity
provider (`subject`: a SteamID64 for War Dogs). A kick or a ban needs a reason.

```sh
curl -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"server_id":"htg-wardogs-1","subject":"76561198000000000","reason":"team killing","on_behalf_of":{"provider":"discord","subject":"1234"}}' \
  https://app.example.com/gravel.hub.v1.ModerationService/KickPlayer
# {"auditId":"17"}
```

Every call the hub sends to a server is a row in `audit_log` (migration 7): when, who (user, or
app plus `on_behalf_of`), the server, the action, the player, the reason, the message or team, the
request id, and the outcome. The row is written **before** the call, so nothing is done that is
not recorded; if it cannot be written, nothing is sent. The outcome is filled in once, afterwards:
`ok`, `player_not_found`, `ban_not_found`, `rejected` (the server refused it as invalid; the API
error names the game's code, `message_too_long`), `not_available`, `moved_not_respawned`, or a
transport word (`unreachable`, `credential_refused`, `credential_missing`, `rate_limited`,
`error`), never error text. A row with no outcome is a call whose result the hub never learned (it
stopped mid-call). The database refuses any other change: a trigger rejects an `UPDATE` of anything
but a first outcome, every `DELETE` and a `TRUNCATE`. A request the hub refuses itself (a missing
field, an action the server's capabilities lack, a server not reached yet) sends nothing and
writes no row.

Errors: `invalid_argument` (a missing or over-long field, or a server rejection), `not_found` (an
unknown server, a player who is not on, an unban with no ban), `failed_precondition` (the server
does not offer the action now), `unavailable` (not reached yet, unreachable, credential refused, or
the server asked the hub to wait). War Dogs bans only a player who is on the server; a ban of a
player who is offline goes in the server's configuration document, which comes with configuration
apply. `gravel_moderation_actions_total{server,action,outcome}` counts the calls (the hub
dashboard's Servers row).

## Ingestion (ADR-0011)

Some servers push their events into the hub: War Dogs' `[WDServerFeed]` posts kill batches to
`<Url>/api/ingest/events` with `Authorization: Bearer <Token>`, about every two seconds while
players fight, and never retries. The hub serves that path on its public listener (put it behind
the edge, as HTG does with `ingest.hiddentoken.com` through its Tunnel).

- **Turning a feed on.** Add `feed` to the server in `servers.yaml` (above), make the token a podman
  secret (`openssl rand -hex 32`), apply. The hub reads the token file every 30 s (job
  `ingest_feeds`). Then apply the server's configuration (`ApplyServerConfig`, Configuration above):
  the driver writes `[WDServerFeed]` from the manifest. War Dogs applies that section at the next
  restart, so the feed starts after the host's next restart.
- **Rotating the token.** Put the new token on the first line of the file and the old one on the
  second, apply the configuration, and leave both. The hub accepts the old one for 48 hours from
  when it first read it, which covers a daily restart. Then remove the second line.
- **What the route does.** It finds the server by its token (compared in constant time against
  every feed). It stores the request body, as received, in `ingest_batches` and only then answers
  200: the sources do not retry, so that insert is the durability. It answers 401 with no body for
  an unknown token (and 429 to an address that keeps guessing), 413 over 64 KiB, 429 when one
  server posts more than 600 a minute, 503 when the database did not take the batch (lost: the
  metrics and the log say so). Nothing in an answer names a server.
- **What happens to a batch.** Batches are kept 30 days (job `ingest_prune`, hourly) and parsed by
  the game's adapter into stats events (#4, after the first real captures). Until then they wait
  as `pending`. `gravel-hub ingest export --server ID` prints them, so a deployment can read its
  first captures; they hold players' SteamIDs, so redact them before they leave the host.

Metrics: `gravel_ingest_batches_total{server,source,result}` (`server="unknown"` when the token
matched none) and `gravel_ingest_last_batch_timestamp_seconds{server}`, on the hub dashboard's
Ingestion row.

## Stats (ADR-0012)

The hub records stats from its own polls of each server: every good poll (ADR-0010) becomes a
**match** and each player's **totals in it** (kills, deaths, time on). A new match starts when the
rotation moves, when every player's counters drop together, or with the first player on an
empty server; an empty server ends its match. A player who leaves and rejoins keeps their totals
(the game counts from the rejoin). After a hub restart the open match is picked up again. Rows are
keyed by the identity the game reports (Steam), never a member, and carry the server's trust when
written: a server's board counts its own matches, a game's or the organization's counts Official
servers' only (ADR-0011's trust mapping). The feed's events (#4) will add who killed whom; boards
count the totals, so they are right without a feed.

**Names.** A player who has not linked the identity, or has not chosen to be shown by name
(`SetBoardName`, off by default), appears under a **pseudonym**: two words chosen by a keyed hash of
the identity, stored the first time and never changed, unique in the organization, numbered on a
collision ("Brave Falcon 2"). The key is `stats.pseudonym_key_file` (above); without it no
pseudonym is made and such players show as "Unnamed player". Keep the key once set: a pseudonym is
chosen once, and a different key only changes names not yet given.

**Boards.** `GetBoard` ranks by `kills` (default), `deaths`, `kd` (kills / max(deaths, 1), from
`stats.min_matches` matches), `time` or `matches`, over `all` (default), `week` (ISO, Monday to
Monday), `month` or a named `season`, in `stats.timezone`. Until `stats.public` is on, only the
owner and apps with `stats:read` can read boards and match lists: turn it on when the host's
privacy policy covers stats.

```sh
curl -s -H 'Content-Type: application/json' -d '{"server_id":"htg-wardogs-1","window":"week"}' \
  https://app.example.com/gravel.hub.v1.StatsService/GetBoard | jq '.entries[] | {rank, name, kills, deaths, kd}'
```

**Retention.** A player's per-match rows are kept for `stats.raw_retention_months` (13 by default, a
placeholder pending counsel), and no longer: the `stats_rollup` job (every 6 hours) moves every match
that started before the first day of the month `raw_retention_months - 1` months ago into
`stats_monthly`, one row per identity, server, trust and **period**, and deletes the raw rows in the
same transaction (13 months keeps between 12 and 13). A period is a calendar month in
`stats.timezone`, cut wherever a season starts or ends inside it, so a board over a season or all
time counts the same before and after the rollup. (Week and month boards only ever cover recent,
raw rows.) The match itself stays (map, times, no personal data) and keeps its player count and
kills for the match lists. A season added or moved after its months rolled up counts each period
by its first day, and a period cut in one timezone keeps its dates if `stats.timezone` changes.

**Erasure.** `gravel-hub stats erase --provider steam --subject <SteamID64>` replaces the identity
with `erased` and a random token in `match_stats`, `stats_monthly` and the bodies of the stored
ingest batches (their hashes recomputed), and deletes the pseudonym, in one transaction. The numbers
stay, shown as "Deleted player"; nothing maps the token back (a hash would not do: every SteamID can
be tried). A player still on a server is recorded under their identity again from the next poll.
The flow members start themselves comes with identity's erasure; this is the stats store's part.

Metrics: `gravel_stats_matches_total{server,game}`, `gravel_stats_rows_written_total{server}`,
`gravel_stats_record_errors_total{server}`, `gravel_stats_rolled_rows_total`, and the rollup job's
`gravel_hub_job_last_success_timestamp_seconds{job="stats_rollup"}` (stored by Prometheus as
`exported_job`, since the scrape's own `job` label wins), on the hub dashboard's Stats row.

## Roles (ADR-0013)

The owner grants members the **moderator** role on the Members page (`/members`, the owner only), or
with `IdentityService.SetUserRole`. A moderator kicks, bans, unbans, messages and moves players,
broadcasts, and reads the ban list and the moderation log, as the owner does; configuration stays the
owner's. Every grant and revocation is kept in `role_changes` (who, by whom, when), which can't be
changed. An app with `servers:moderate` that names `on_behalf_of` acts only for the owner or a
moderator; without `on_behalf_of` it acts for itself.

## Pages (gravel#18)

The web UI's community pages, each an API client like every page (ADR-0005): what a reader may see
is the API's to decide, and a page says so in words when it is refused.

| Page | Shows | Who sees what |
|---|---|---|
| `/servers` (also `/`) | every server: name, game, trust, state (Online, Offline, Checking, Unavailable), players and map | everyone |
| `/servers/{id}` | the status with the team scores and when it was observed, who is on (in-game name, team, kills, deaths, ping, a member badge), the last 10 matches, a link to the server's board | the status: everyone; who is on: members and the owner ("Log in to see who's on" otherwise); matches: as the boards |
| `/members/{id}` (`/profile` is yours) | a member's totals over all time, this week, this month and each season running now (Official servers, every linked account), their 20 most recent matches on any server, and on your own profile the switch that shows your name on leaderboards (`SetBoardName`) | the member, the owner and apps with `stats:read`; anyone when the member shows their name and `stats.public` is on; otherwise "no such profile", so a page never says who plays under a pseudonym. A named member on a board links here |
| `/boards?scope=&window=&metric=&page=` | a leaderboard: scope `` (everyone, Official servers), `game:<id>` or `server:<id>`; window `all`, `week`, `month` or `season:<name>`; ranked by any column of the game's `layout.board`, 50 a page | everyone when `stats.public` is on, else the owner ("Leaderboards are private…" otherwise) |

The forms and links work without JavaScript; htmx swaps only the main content. On a narrow screen a
board drops its last two columns.

**Caching.** Every board read is an aggregate, and a public board can be read by anyone as often as
they like, while the numbers change only when someone plays. The hub caches each board answer in
memory and keeps it while the stats store has not been written (the recorder and the rollup count
their writes), up to 15 minutes; once something has been written, an answer is still served for a
minute. An idle hub serves every board from memory, and a live match costs one read a minute per
board, however many people watch. A member's name choice clears the cache. The API's per-IP rate
limit applies in front of all of it.

## Login (ADR-0004)

A member logs in with a provider whose `login` is true (Discord by default) and links the others
(Steam by default) from the account page. Every attempt is a row bound to the browser by an auth
cookie and to the provider's callback by a state value, consumed once: a replayed, forged or
expired callback is refused, and a link must finish in the session that started it. A
`(provider, subject)` pair belongs to one user; linking one another member holds is refused, and
so is unlinking the last one (a provider-only account would be unreachable).

- **Discord:** register `<auth.base_url>/auth/discord/callback` as a redirect in the Developer
  Portal's OAuth2 settings; login asks for `identify` alone (no email, no connections).
- **Steam:** nothing to register; the OpenID assertion proves the SteamID64. The Web API key only
  fetches the persona name and avatar, and may be left out.
- **Sessions:** server-side, `auth.session_ttl` long, no idle timeout. The cookie is `HttpOnly`,
  `SameSite=Lax`, and `Secure` with the `__Host-` prefix when `auth.base_url` is `https://`. A
  restart keeps everyone logged in; `RevokeSessions` or the page's "log out everywhere" ends every
  session of the member.
- **Cross-site requests:** `net/http`'s cross-origin protection refuses unsafe cross-origin
  browser requests before any handler, the API included; forms also carry the session's CSRF
  token. Clients that are not browsers send neither `Sec-Fetch-Site` nor `Origin` and pass.
- **Rate limits:** anonymous requests are limited per client address, authenticated ones per
  user (`rate_limit`); pages answer 429 with `Retry-After`, procedures `resource_exhausted`;
  `/healthz` and `/readyz` are never limited. Behind a reverse proxy that is the only way in, set
  `server.client_ip_header` to the header carrying the client's address (`CF-Connecting-IP` for
  cloudflared); otherwise everyone shares the proxy's bucket. Set it only when nothing else can
  reach the listener: the header is trusted as given.
- Expired sessions and attempts are pruned every ten minutes.

## Discord Linked Roles (ADR-0008)

A guild can make a role require "Steam linked" (Server Settings, Roles, the role's Links), and
Discord asks the member to verify with the application that declares that field. The bot declares
the fields at start (`discord/rolemeta`: `steam_linked`, `xbox_linked`, `rsi_linked`,
`pubg_linked`, `supporter_tier`); the hub writes a member's values:

- In the Developer Portal, set the application's **Linked Roles Verification URL** to
  `<auth.base_url>/auth/discord/roles` (the hub logs it at start as
  `linked_roles_verification_url`). The application is the one login uses, so the redirect is the
  same callback. Discord must be a login provider (`login: true`).
- A member arriving there goes through Discord's consent for `identify role_connections.write`,
  comes back to the usual callback, and is logged in exactly as by `/auth/discord/start`
  (registered on a first visit; an existing session of the same member is kept). The hub then
  writes their role connection with the access token in hand: the platform is the organization's
  name, the username their display name, and each `<provider>_linked` is 1 for a provider they
  have linked. The token is not kept; an attempt row with intent `roles` (migration 5) is all
  that is stored, and consumed like any other.
- The values are as fresh as the member's last verification, so the account page offers
  **Update Discord linked roles** after a link or an unlink. A role the bot's role sync manages
  follows identity changes on its own within a pass; map a role to one mechanism or the other,
  not both.
- A refusal from Discord leaves the member logged in and says so; `gravel_auth_completions_total`
  counts the flow under `intent="roles"`.

## Owner claim (ADR-0002, ADR-0004)

A fresh hub is unowned. On each start while unowned it mints a one-time owner-claim token and logs
it once at WARN level:

```json
{"level":"WARN","msg":"the hub is unowned: claim it once with this token (it expires; a restart mints a new one)","token":"…","expires_at":"…"}
```

Log in, then claim it within `claim.token_ttl` on the account page (the form shows while the hub
is unowned) or over the API with the session cookie:

```sh
curl -X POST -H 'Content-Type: application/json' -b '__Host-gravel_session=<cookie>' -d '{"token":"<token>"}' https://app.example.com/gravel.hub.v1.OrganizationService/ClaimOwnership
```

The caller becomes the owner (`owner_user_id`). The claim is single-use and atomic. A call without
a session answers `unauthenticated`, a second call `failed_precondition` (the hub is owned), a
wrong or expired token `permission_denied`, an empty token `invalid_argument`. An owned hub never
mints a token again. A claim made by 0.1.0 bound no user; migration 2 reopens it, so such a hub
logs a fresh token after the upgrade.

## Backups (ADR-0006)

The hub's Postgres runs from `ghcr.io/gravel-project/gravel-postgres`, the official image plus
WAL-G, published by the release that last changed `deploy/postgres/` (`:latest`; docs/releasing.md
"The Postgres image has its own version line"): Postgres pushes every WAL segment to object storage as it is written
(`archive_command='wal-g wal-push %p'`, `archive_timeout=60`), and `gravel-backup.timer` takes a
base backup every night and keeps 30. The hub does not back up; it watches:

```yaml
backup:
  status_file: /var/lib/gravel/backup/status.json   # written by gravel-backup after every run; empty = no job
```

| Metric | Source | Alert on |
|---|---|---|
| `gravel_backup_last_success_timestamp_seconds` | the status file | `time() - … > 36h`: no base backup for a day and a half |
| `gravel_backup_last_run_ok`, `gravel_backup_last_run_timestamp_seconds` | the status file | `== 0`: the last run failed |
| `gravel_backup_status_readable` | the status file | `== 0`: the file is configured but unreadable |
| `gravel_wal_last_archived_timestamp_seconds`, `gravel_wal_archived_total` | `pg_stat_archiver` | `time() - … > 300`: the archive trails the database by more than `archive_timeout` allows |
| `gravel_wal_archive_failed_total`, `gravel_wal_last_failed_timestamp_seconds` | `pg_stat_archiver` | `increase(…[1h]) > 0`: an upload failed |
| `gravel_wal_archiver_readable` | the database | `== 0`: Postgres did not answer at scrape |

In a quadlet unit the archive command is written `wal-g wal-push %%p`: systemd expands `%`
specifiers in the generated service, and `%%` is how a literal percent reaches Postgres (the
compose file and the drill, which hand the command to podman directly, write `%p`).

Turning backups on for a quadlet deployment: edit the bucket lines in `deploy/quadlet/backup/`,
create the two secrets, `make quadlet-install-backup`, restart `gravel-postgres` and `gravel-hub`
(the drop-ins switch the image and mount the status volume), then
`systemctl --user enable --now gravel-backup.timer`; `deploy/README.md` has the steps. The
development stack archives into a local store (`make up`); `make backup` takes a base backup.

## Restore

`gravel-restore` runs in a container of the same image, with the same WAL-G settings, against an
**empty** data volume, and prepares a point-in-time recovery; the next Postgres start on that
volume replays WAL to the point and promotes:

```sh
systemctl --user stop gravel-hub gravel-postgres
podman volume create gravel-restore
podman run --rm --network gravel --secret gravel-backup-credentials \
  -e WALG_S3_PREFIX=s3://my-bucket/gravel -e AWS_ENDPOINT=https://… -e AWS_REGION=auto -e AWS_S3_FORCE_PATH_STYLE=true \
  -e AWS_SHARED_CREDENTIALS_FILE=/run/secrets/gravel-backup-credentials \
  -v gravel-restore:/var/lib/postgresql/data ghcr.io/gravel-project/gravel-postgres@sha256:… \
  gravel-restore --to 2026-10-09T02:00:00Z          # or --latest; --backup <name> picks a base backup
# point gravel-postgres.volume at the restored data (or rename the volumes), then
systemctl --user start gravel-postgres gravel-hub   # /readyz confirms the schema is current
```

Everything after the target is gone: sessions issued later, and a claim made later (the hub
mints a token again if the restored organization is unowned). The restored database carries the
password that was current when the backup was taken; rotate per the operator's runbook if it has
changed since.

**The drill.** `make backup-drill` runs `deploy/backup-drill.sh` with podman: a local S3 store, a
Postgres from the image archiving into it, rows written, a base backup, more rows, a point in time
from the database's own clock, more rows, a segment switch; the database is destroyed and
restored to the point into a fresh volume; the script checks that exactly the rows before the
point came back and that the hub's migrations are current, and prints the restore time. CI runs
it on every pull request (the `backup` job).

## Migrations

SQL migrations live in `internal/store/migrations/` and are embedded in the binary (goose). With
`database.migrate: auto` the hub applies pending ones at start; with `manual` it refuses to start
until `gravel-hub migrate` has run, and `/readyz` reports pending migrations either way. Every
migration has a `-- +goose Down` section so tests can reset a database.

## Running it

- **Development:** `make up` builds the image with ko, writes dev secrets into podman's secret
  store and starts hub + Postgres 17 with `podman compose` (`deploy/compose.yaml`). Host ports
  come from `GRAVEL_PORT` and `GRAVEL_INTERNAL_PORT` (8080 and 9090 by default). `make down` stops
  it and keeps the Postgres volume.
- **Production:** the quadlet units in `deploy/quadlet/` as rootless systemd user units, with
  the backup set from `deploy/quadlet/backup/`; see `deploy/README.md`. The config file must be
  readable by uid 65532, the container's user (mode 0644 is fine: it holds no secrets).

The image is built by ko from `cmd/gravel-hub` on `gcr.io/distroless/static-debian12:nonroot`,
runs as uid 65532, and has the binary at `/ko-app/gravel-hub` (`.ko.yaml`). Releases push it to
`ghcr.io/gravel-project/gravel-hub` as `<version>` (no `v`: `0.6.6`) and `latest`, multi-arch
(amd64, arm64), signed with cosign and with an SBOM attached; `docs/releasing.md` has the verify
command and the digest-pin procedure.

## Observability

- **Logs:** slog, JSON by default, one `request` line per HTTP request with method, path, status,
  bytes, duration, remote and `request_id`. Health probes log at debug unless they fail.
- **Request ids:** `X-Request-ID` is honoured when well-formed (up to 128 of `A-Za-z0-9._:-`),
  otherwise a UUIDv7 is generated; it is echoed on the response and attached to every log line
  and internal-error log.
- **Metrics:** on the internal listener (`server.internal_listen`), `GET /metrics`. Every series
  gravel adds is below; the Go (`go_*`) and process (`process_*`) collectors come on top. Label
  values are bounded: none comes from a request unless the hub knows it. The `gravel hub`
  dashboard shows every one of them (ADR-0009, `deploy/README.md` "Dashboards").
- **Secrets in logs:** the owner-claim token is the only secret the hub ever logs, once, at WARN.
  Provider secrets are logged by source only.
- **Panics** become a 500 and an error log line with the stack; the server stays up.

| Metric | Type | Labels | What it says | Absent when |
|---|---|---|---|---|
| `gravel_build_info` | gauge, always 1 | `version`, `go_version` | the running build | never |
| `gravel_http_requests_total` | counter | `handler` (`healthz`, `readyz`, `oauth_token`, `pages`), `method`, `code` | HTTP requests by status | until the first request |
| `gravel_http_request_duration_seconds` | histogram (default buckets) | `handler` | HTTP latency | until the first request |
| `gravel_rpc_requests_total` | counter | `procedure`, `code` (`ok` or the Connect code) | API calls, the pages' in-process calls included (ADR-0005) | until the first call |
| `gravel_rpc_request_duration_seconds` | histogram (default buckets) | `procedure` | API latency | until the first call |
| `gravel_auth_completions_total` | counter | `provider` (a configured provider, or `other`), `intent` (`login`, `link`, `roles`, `unknown` on a failure), `result` (`ok`, `denied`, `failed`, `invalid`, `mismatch`, `taken`, `error`) | how login, link and Linked Roles attempts ended | until the first callback |
| `gravel_rate_limited_total` | counter | `scope` (`ip`, `user`, `app`) | requests a rate limit refused | until the first refusal |
| `gravel_users` | gauge | | members registered | when `gravel_store_stats_readable` is 0 |
| `gravel_identities` | gauge | `provider` (each configured provider, 0 included; `other` for rows of a provider no longer configured) | linked identities | when `gravel_store_stats_readable` is 0 |
| `gravel_database_size_bytes` | gauge | | `pg_database_size` of the hub's database | when `gravel_store_stats_readable` is 0 |
| `gravel_store_stats_readable` | gauge | | 1 when the three above were read at this scrape (2 s limit), 0 when the database did not answer | never |
| `gravel_backup_*`, `gravel_wal_*` | | | the backup status file and `pg_stat_archiver` | see [Backups](#backups-adr-0006) |
| `gravel_hub_jobs_total` | counter | `job` (`prune`, `servers_reconcile`, `server_poll`; stored by Prometheus as `exported_job`, since the scrape's own `job` wins), `result` (`ok`, `error`) | background job runs; a failure is retried with backoff and never stops the hub | until a job's first run |
| `gravel_hub_job_last_success_timestamp_seconds` | gauge | `job` (stored as `exported_job`) | when each job last ran without an error | until a job's first success |
| `gravel_server_reachable` | gauge | `server`, `game` | 1 while a server answers its polls, 0 after two failed polls in a row | until the server's first poll; gone when it is removed |
| `gravel_server_players`, `gravel_server_max_players` | gauge | `server`, `game` | players on, and public slots, at the last successful poll | until the first successful poll |
| `gravel_server_last_observed_timestamp_seconds` | gauge | `server`, `game` | the last successful poll | until the first successful poll |
| `gravel_stats_matches_total` | counter | `server`, `game` | matches the stats store started (ADR-0012) | until the first match |
| `gravel_stats_rows_written_total` | counter | `server` | player rows written from polls | until someone is on |
| `gravel_stats_record_errors_total` | counter | `server` | polls the stats store could not write | until one fails |
| `gravel_stats_rolled_rows_total` | counter | | per-match rows rolled up into monthly totals past the retention window | |

- **Secrets in logs:** the owner-claim token is the only secret the hub ever logs, once, at WARN.
  Provider secrets are logged by source only.
- **Panics** become a 500 and an error log line with the stack; the server stays up.

## Tests

`make test` runs every package with the race detector. The login flows are tested against an
in-memory store and a fake provider (`internal/identity/identitytest`); the Goth adapters against
fake Discord and Steam endpoints. Packages that need Postgres (`store`, `hub`) skip unless
`GRAVEL_TEST_DATABASE_URL` points at a server the user may create databases on;
each test binary then drops and recreates its own database (`gravel_test_<package>`), so packages
run in parallel. `make test-integration` insists on the variable. Locally:

```sh
podman run -d --name gravel-test-pg -p 127.0.0.1:55432:5432 -e POSTGRES_USER=gravel -e POSTGRES_PASSWORD=test -e POSTGRES_DB=gravel public.ecr.aws/docker/library/postgres:17
GRAVEL_TEST_DATABASE_URL='postgres://gravel:test@127.0.0.1:55432/gravel?sslmode=disable' make test-integration
```

The servers' monitor runs against the War Dogs driver and a fake server serving a recorded
build (`internal/servers`), and `internal/hub`'s integration test applies a server, runs the hub
and reads its status over the API and the metrics.

The War Dogs client (`games/wardogs`) runs one contract against every recorded build in
`games/wardogs/testdata/` through a fake server (`games/wardogs/wardogstest`); the same contract
runs against a real server, reading only, with `-tags live` (`games/wardogs/README.md`).

`make a11y` runs axe (`@axe-core/cli`, pinned in the Makefile, through npx) over the login page
and fixtures of the account and error pages, driving Chrome; CI runs it too. It needs node and
a browser, so it is not part of `make check`. axe bundles a ChromeDriver that must match the
Chrome it drives: CI installs a synced pair with `browser-driver-manager` and names them in
`AXE_CHROME_PATH` and `AXE_CHROMEDRIVER_PATH`; locally, leave both unset and keep Chrome
current, or set them the same way.

## Tooling

`make tools` installs the pinned CLIs (buf, the protoc plugins, golangci-lint, govulncheck, ko,
templ) into `.bin/`; the versions live at the top of the Makefile and the CI workflow pins the
same golangci-lint. `make check` runs what CI runs, except the integration and accessibility
jobs. `make lint` also enforces the `games/` boundary: packages under `games/` must not import
gravel (depguard), because they become standalone modules at the open-source cut. `make
vendor-htmx` refreshes the vendored htmx to `HTMX_VERSION`.
