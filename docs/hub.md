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
| `gravel-hub apps create --name NAME --scopes SCOPES [--redirect-uri URL]...` | Register a first-party app (ADR-0008) and print its client id and secret once; the hub keeps the secret's hash. `--redirect-uri` (repeatable) is where its web pages receive a member who signed in ([Members sign in to apps](#members-sign-in-to-apps-adr-0015)): https, or http on 127.0.0.1, `[::1]` or localhost, compared as an exact string. |
| `gravel-hub apps list` | The registered apps: client id, name, scopes, redirect URIs, created, revoked. |
| `gravel-hub apps revoke --client-id ID` | Revoke an app: its tokens (its own and its members'), the grants members gave it and its unexchanged codes are deleted, and no new token is issued. |
| `gravel-hub users create-local --username NAME [--email ADDRESS]` | Make a member with a local account and print the one-time link that sets its password ([Local accounts](#local-accounts-adr-0026)), with no API credential: how a hub with no provider gets its first member. Needs `auth.local.enabled`. |
| `gravel-hub users reset-password --username NAME` | Print a one-time link that resets a local account's password; its earlier links stop working. |
| `gravel-hub servers export` | Print the games and servers as a `servers.yaml` manifest ([Servers](#servers-adr-0010)). |
| `gravel-hub servers apply <servers.yaml> [--dry-run]` | Validate a manifest, check each credential file is readable where it runs, and store it; prints each change. With `--dry-run`, exit 3 when something would change. A running hub picks it up within 30 s. |
| `gravel-hub servers check <servers.yaml>` | Validate a manifest with no configuration, no database and no secret: exit 0 with its summary, 1 with every problem named, 2 on a usage error. |
| `gravel-hub ingest export --server ID [--after ID] [--limit N]` | Print a server's stored ingest batches, oldest first, one JSON object per line: a JSON body as JSON, anything else as base64 ([Ingestion](#ingestion-adr-0011)). They hold players' SteamIDs: redact before they leave the host. |
| `gravel-hub stats erase --provider P --subject S` | Erase a player from the stats store ([Stats](#stats-adr-0012), Erasure): their identity becomes a random token in every stats table (participation included) and the stored ingest batches, their pseudonym is deleted, the numbers stay. For an operator acting on a request they have verified. Prints the counts, never the identity. |
| `gravel-hub wardogs record --base-url URL [--token-file FILE] [--dir DIR]` | Record a War Dogs server's read-only answers as test fixtures in `<dir>/<build>/` (default `games/wardogs/testdata`), with people, addresses and secrets replaced (ADR-0010; `games/wardogs/README.md`). One token, never retried; without one, only the public routes. |
| `gravel-hub demo wardogs-server --listen ADDR --token-file FILE [--profile FILE] [--seed N]` | Serve a simulated War Dogs server for a demo hub to poll ([Demo hub](#demo-hub-gravel112)). |
| `gravel-hub demo seed [--days 90] [--members 40] [--seed N] [--profile FILE]` | Write made-up members, matches and moderation into a demo hub's database: only with `demo: true`, only into a database nobody signed in to and nothing was recorded in ([Demo hub](#demo-hub-gravel112)). |
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
  local:
    enabled: false               # usernames and passwords, no provider needed ([Local accounts](#local-accounts-adr-0026))
rate_limit:
  per_ip:   {requests_per_minute: 120, burst: 40}    # anonymous requests, per client address
  per_user: {requests_per_minute: 600, burst: 100}   # authenticated requests, per user
  per_app:  {requests_per_minute: 600, burst: 100}   # requests carrying an app's bearer token, per app
apps:
  token_ttl: 1h                  # how long a bearer token from /oauth/token lives
stats:
  pseudonym_key_file: /run/secrets/gravel-stats-pseudonym-key   # the key pseudonyms are chosen with ([Stats](#stats-adr-0012))
demo: false                      # true marks a demo hub: every page says its data is made up ([Demo hub](#demo-hub-gravel112))
cloud:
  egress_cidrs: []               # the hub's own address, which cloud-VM servers' firewalls admit RCON and SSH from; empty asks a public service ([Cloud-VM servers](#cloud-vm-servers-adr-0003))
leases:
  key_file: /run/secrets/gravel-lease-key   # the key a lease's join password is derived from; without it the hub leases nothing ([Server leases](#server-leases-adr-0023))
demos:                           # where match demos are kept: an S3-compatible bucket; without one, none are ([Match results](#match-results-adr-0024))
  endpoint: <account>.r2.cloudflarestorage.com
  region: auto
  bucket: htg-match-demos
  path_style: true
  max_mib: 512
  access_key_file: /run/secrets/gravel-demos-access-key
  secret_key_file: /run/secrets/gravel-demos-secret-key
```

**Secrets.** Seven, each with the same precedence: the environment variable, then the `*_file`
key (a podman secret mount), then the inline key, which is for development only.

| Secret | Environment | File key | Inline key |
|---|---|---|---|
| Database URL | `GRAVEL_DATABASE_URL` | `database.url_file` | `database.url` |
| Discord OAuth2 client secret | `GRAVEL_DISCORD_CLIENT_SECRET` | `auth.discord.client_secret_file` | `auth.discord.client_secret` |
| Steam Web API key (optional) | `GRAVEL_STEAM_API_KEY` | `auth.steam.api_key_file` | `auth.steam.api_key` |
| Stats pseudonym key (optional; at least 32 characters, `openssl rand -hex 32`) | `GRAVEL_STATS_PSEUDONYM_KEY` | `stats.pseudonym_key_file` | `stats.pseudonym_key` |
| Lease key (optional; at least 32 characters, `openssl rand -hex 32`) | `GRAVEL_LEASE_KEY` | `leases.key_file` | `leases.key` |
| Demo bucket's access key and secret (with `demos.bucket`) | `GRAVEL_DEMOS_ACCESS_KEY`, `GRAVEL_DEMOS_SECRET_KEY` | `demos.access_key_file`, `demos.secret_key_file` | `demos.access_key`, `demos.secret_key` |

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
| `/gravel.hub.v1.OrganizationService/GetOrganizationSettings` | the Organization settings (theme, navigation, the Discord mapping, stats, the games' overlays); public |
| `/gravel.hub.v1.OrganizationService/UpdateOrganizationSettings` | replace them; the owner only; `invalid_argument` names every invalid field |
| `/gravel.hub.v1.IdentityService/GetMe` | the caller and their linked identities; needs a session, or a member's token from an app (no scope needed: how an app learns who signed in, ADR-0015) |
| `/gravel.hub.v1.IdentityService/UnlinkIdentity` | `{"provider": "steam", "subject": "…"}`; the last identity is refused (`failed_precondition`) |
| `/gravel.hub.v1.IdentityService/StartProof` | `{"server_id": "…", "subject": "…"}` → a one-time code to type in that server's chat, with its id, expiry and instructions ([Proof in game](#proof-in-game-adr-0022)); `subject` (optional) is an account of the caller's to prove again; a session only, five an hour |
| `/gravel.hub.v1.IdentityService/GetProof` | `{"proof_id": "…"}` → `open`, `proven`, `taken`, `replaced` or `expired`, and the account once proven; a session only, the caller's own |
| `/gravel.hub.v1.ServerLeaseService/RequestServerLease` | `{"pool": "pug", "holder_ref": "…", "expected_players": 10, "settings": {"start_map": "de_nuke"}, "plugins": ["matchzy"], "match_config": "<base64 JSON>"}` → the lease and how to join, password included ([Server leases](#server-leases-adr-0023)); an app with `servers:lease`, the owner or a moderator |
| `/gravel.hub.v1.ServerLeaseService/WatchServerLease` | `{"lease_id": "…"}` → the lease at once and at every change, a heartbeat every 30 s, until it is over (server streaming); the lease's app, the owner or a moderator |
| `/gravel.hub.v1.StatsService/GetMatchResult` | `{"id": "…"}` → a match plugin's series with its maps and each player's totals, named as on a board ([Match results](#match-results-adr-0024)); the same access as `GetBoard` |
| `/gravel.hub.v1.IdentityService/Logout` | end the calling session and clear its cookie |
| `/gravel.hub.v1.IdentityService/RevokeSessions` | log out everywhere; answers how many sessions ended and clears the cookie |
| `/gravel.hub.v1.IdentityService/LookupUser` | `{"provider": "discord", "subject": "…"}` → the user and their identities, for role sync; an app with `identity:read`, or the owner |
| `/gravel.hub.v1.IdentityService/ListUsers` | `{"page_size": 100, "page_token": "…"}` → members with their identities, oldest first, and the next page's token; role sync's full pass; `identity:read` or the owner |
| `/gravel.hub.v1.IdentityService/SetUserRole` | `{"user_id": "…", "role": "operator", "game_id": "cs2", "granted": true}` → the member with their roles, grants and capabilities ([Roles](#roles)); `roles:grant` for the grant's scope, admin grants by the owner only, an app moderator only, never one's own; every change recorded |
| `/gravel.hub.v1.OrganizationService/ListEvents` | `{"after_id": 0, "kinds": ["host"], "min_severity": "warning"}` → the events log by cursor ([Events](#events-adr-0028)); `events:read`, a game-scoped holder that game's events |
| `/gravel.hub.v1.IdentityService/ListRoleGrants` | `{"user_id": "…"}` (or `{}` for everyone) → who holds which role where; `identity:read` |
| `/gravel.hub.v1.IdentityService/ListIdentityEvents` | `{"after_id": 0, "limit": 100}` → the identity log after a position, oldest first, the id to continue from, and the log's newest id (`head_id`, where a reader that just made a full pass starts); role sync's incremental pass; `identity:read` or the owner |
| `/gravel.hub.v1.ServerService/ListGames` | the enabled games, their identity provider, bands, layout and `no_paid_perks`; public |
| `/gravel.hub.v1.ServerService/ListServers` | every server with its last observation (state, reachable, players and max, map, teams with their scores and, when the hub can count them, their players, capabilities, build, `observed_at`); public, no personal data, never a control address |
| `/gravel.hub.v1.ServerService/GetServerStatus` | `{"server_id": "…"}` → one server; public; `not_found` for an unknown id |
| `/gravel.hub.v1.StatsService/GetMemberProfile` | `{"user_id": "…"}` (empty: the caller) → a member's totals by window, their participation per kind over the same windows, and recent matches ([Pages](#pages-gravel18)); the member, the owner, `stats:read`, or anyone when they show their name and stats are public; else `not_found` |
| `/gravel.hub.v1.StatsService/GetBoard` | `{"server_id"/"game_id", "window": "week", "metric": "kd"}` → a ranked page of players under pseudonyms or opted-in names ([Stats](#stats-adr-0012)); public when the settings make stats public, else the owner or `stats:read` |
| `/gravel.hub.v1.StatsService/ListMatches` | `{"server_id": "…"}` → a server's matches, newest first; the same access |
| `/gravel.hub.v1.StatsService/GetBoardName`, `SetBoardName` | a logged-in member's choice to be shown by name on boards (off by default) |
| `/gravel.hub.v1.StatsService/RecordParticipation` | `{"events": [{"key", "kind", "identity": {"provider", "subject"}, "occurred_at", "duration_seconds", …}]}` (1 to 100) → a `recorded` or `duplicate` result per event, in order ([Participation](#participation-adr-0014)); an app with `participation:write` only, never a member or the owner; `invalid_argument` names every invalid field by index and stores nothing |
| `/gravel.hub.v1.StatsService/GetSessionRecap` | `{"server_id": "…", "from": "…", "to": "…", "floor": 20}` → what the server's window held: peak players, minutes with anyone on and at or above `floor`, distinct players, the matches that overlap it with their team scores, and the top five by kills under pseudonyms or opted-in names ([Session recaps](#session-recaps-adr-0018)); the window at most 24 h and ended; the same access as `GetBoard` |
| `/gravel.hub.v1.StatsService/GetParticipationBoard` | `{"kind": "seeding", "game_id", "window": "week"}` → a page of players ranked by time taken part, under pseudonyms or opted-in names, and every kind recorded (`kinds`); the same access as `GetBoard` |
| `/gravel.hub.v1.ServerService/ListServerPlayers` | `{"server_id": "…"}` → who was on at the last poll (name, provider identity, team, kills, deaths, ping, and the member's user id when that identity is linked); a logged-in member, the owner, or an app with `servers:read` |
| `/grpc.health.v1.Health/Check` | gRPC health |
| `/grpc.reflection.v1.ServerReflection/…` (and v1alpha) | gRPC reflection, so `grpcurl` and `buf curl` discover the API |
| `GET /healthz` | 200 `{"status":"ok","version":"…"}` while the process runs |
| `GET /readyz` | 200 `{"status":"ready"}` when Postgres answers and no migration is pending, else 503 with a reason |
| `/gravel.hub.v1.AppService/PrepareAuthorization`, `Authorize` | the authorization endpoint's checks and consent, which the `/oauth/authorize` page calls ([Members sign in to apps](#members-sign-in-to-apps-adr-0015)); a session only, never a bearer token |
| `/gravel.hub.v1.AppService/ListAppGrants`, `RevokeAppGrant` | `{}` → the apps the member allowed (name, scopes, when); `{"client_id": "…"}` → revoke one with the member's tokens for it; a session only |
| `POST /oauth/token` | the token endpoint for first-party apps: client credentials (ADR-0008), or an authorization code with its PKCE verifier (ADR-0015), in; a Bearer token out; see [Service credentials](#service-credentials-adr-0008) |
| `POST /oauth/introspect` | RFC 7662 for apps: with the app's client credentials and a form `token`, whether that token is active, and whose ([Members sign in to apps](#members-sign-in-to-apps-adr-0015)) |

Pages and flows, on the same listener (ADR-0005: the pages are templ components that call the
procedures above in process; htmx, served from the binary, swaps the main content on forms, and
every page works without it):

| Path | What |
|---|---|
| `GET /` | to `/account`, or `/login` when anonymous |
| `GET /login` | the login providers; `?return_to=/path` comes back to that path on this hub after the login (anything but a same-origin path is ignored) |
| `GET /account` | the member's identities, link buttons, unlink forms, "Apps you allowed" with a Revoke for each (`POST /account/apps/revoke`), "log out everywhere", and the claim form while the hub is unowned |
| `GET /oauth/authorize` | the authorization endpoint (ADR-0015): checks the app's request, sends a visitor through the login and back, shows the consent page unless a remembered grant covers the request, and redirects to the app with a code |
| `POST /oauth/authorize` | the consent page's form: Allow or Cancel, with the CSRF token |
| `GET /auth/{provider}/start` | begin a login (providers with `login: true`) |
| `GET /auth/{provider}/link` | begin a link, logged in |
| `GET /auth/{provider}/roles` | begin Discord's Linked Roles verification: a login that also publishes the member's linked accounts to Discord (below) |
| `GET /auth/{provider}/callback` | the provider's return |
| `POST /auth/local/login` | a username and password ([Local accounts](#local-accounts-adr-0026)); 401 with one answer for every refusal, 429 past the per-address limit |
| `GET`, `POST /auth/local/register` | make a local account and log in, when `local_accounts.registration` is `open`; 404 otherwise |
| `POST /account/local` | give the logged-in member a username and password, with the CSRF token |
| `GET`, `POST /auth/local/reset/{token}` | a password link: choose the password, once, within the hour; every other session of the member ends and this browser is logged in. 404 for an unknown, used or expired link. The access log shows `/auth/local/reset/<redacted>`, and the page sends no referrer |
| `POST /auth/logout` | this session (`Logout`), or every session with `everywhere=1` (`RevokeSessions`) |
| `POST /account/unlink`, `POST /account/claim`, `POST /account/apps/revoke` | forms; every POST carries the session's CSRF token in `_csrf`; with `HX-Request: true` the answer is the page's content, else a redirect |
| `GET /theme.css` | the theme's tokens as custom properties, with an ETag |
| `GET /static/map-placeholder.png` | gravel's generic map picture (1280×720, no text), for a server card's map without an image of its own |
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
light and dark schemes, the font, a logo, a favicon), the navigation links, the Discord role
mapping, the stats boards and each game's overlay now; token lifetimes and layout overrides later. Zero values mean the default. The pages
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
    ok: "#1e3a2a"            # the tints behind ok and error rows; omitted, gravel's own
    err: "#3a1e1e"
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
  mod_log: "…"               # optional: the channel every moderation action is posted to (docs/bot.md "Mod log")
  seeding:                   # seeding calls and minutes (docs/bot.md "Seeding"); at most 10, one per server
    - server: htg-wardogs-1
      channel: "…"           # where the call is posted, such as #wd-seeding
      role: "…"              # the opt-in seeder role the call pings, and nothing else; never guild_id
      below: 20              # the floor: a call is due under it (2 to 1000)
      hours:                 # weekly windows in stats.timezone; at most 14; none means no call ever
        - {days: [wed, sat, sun], from: "18:00", to: "23:00"}   # a to before from ends the next day
      cooldown: 2h           # the least time between calls, 15m to 24h (default 2h)
      call_at_start: true    # a window's first call ignores a cooldown left from before it (default false)
      note: Join from the server browser.   # optional, one line of at most 200 characters
  recaps:                    # session recaps (docs/bot.md "Recaps"); at most 10, one per server
    - server: htg-wardogs-1
      channel: "…"           # where the recap is posted, such as #wd-chat
      title: Wardogs Wednesday   # the session's name in the post, at most 60 characters
      hours:                 # the sessions, as seeding's windows (stats.timezone); none means no recap ever
        - {days: [wed], from: "18:00", to: "22:00"}
      floor: 20              # "minutes at 20 or more" (0 to 1000; 0 leaves the line out)
      note: See you next Wednesday.   # optional, one line of at most 200 characters
  streams:                   # go-live posts (docs/bot.md "Streams"); stream metadata only
    channel: "…"             # where the posts go; the bot needs View Channel, Send Messages, Read Message History
    role: "…"                # optional: an opt-in role each new post pings; never guild_id
    note: Grab the Stream role in #roles to be pinged.   # optional, one line of at most 200 characters
    streamers:               # at least 1, at most 10
      - platform: twitch     # twitch or kick
        login: hiddentoken   # as in the channel's URL, lower case
        label: Hidden Token  # optional, what the post shows; the login when empty (at most 40 characters)
  news:                      # Steam announcements (docs/bot.md "News")
    feeds:                   # at most 20; a game once per channel
      - app_id: 730          # the game's Steam app id, as in its store URL
        channel: "…"         # the bot needs View Channel, Send Messages, Embed Links, Read Message History
        label: Counter-Strike 2   # the posts' footer, one line of at most 64 characters
        feeds: [steam_community_announcements]   # optional: Steam's feed names; this one when empty
        color: 0xde9b35      # optional: the embeds' color, 0x000000 to 0xffffff; Steam's blue when 0
  match_results:             # each finished match result, posted once (docs/bot.md "Match results", ADR-0024)
    channel: "…"             # the bot needs View Channel, Send Messages, Read Message History
    servers: [htg-cs2-1]     # optional: the servers whose results are posted, at most 50; every server's when empty
  presence:                  # the "who's playing now" card (docs/bot.md "Presence"); counts only, nothing stored
    channel: "…"             # where the card lives; the bot needs View Channel, Send Messages, Read Message History
    note: Counts members who share their activity on Discord.   # optional, one line of at most 200 characters
    games:                   # at least 1, at most 20, in the card's order
      - game: wardogs        # the line's key, in the metrics (lower-case letters, digits, hyphens)
        label: War Dogs      # what the card shows, at most 40 characters
        activities: [WARDOGS]   # activity names as Discord shows them, any case; at most 10
      - game: cs2
        label: Counter-Strike 2
        application_ids: ["…"]  # or the activity's Discord application id; at most 10
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
events:                      # the events log ([Events](#events-adr-0028))
  retention_days: 400        # how long an event is kept (default 400; 30 to 3650)
local_accounts:              # with auth.local.enabled ([Local accounts](#local-accounts-adr-0026))
  registration: closed       # closed (the default: staff make accounts) or open (anyone may register)
  min_password_length: 12    # the shortest password, in characters (default 12; 8 to 128)
games:                       # a game's overlay on its pages, by game id; at most 20 ([Game overlays](#game-overlays-gravel66))
  wardogs:
    theme:
      light: {accent: "#8a5a00"}   # only the accent; omit a scheme to keep the organization's
      dark: {accent: "#f0b429"}
      banner_url: https://example.com/brand/wardogs-banner.png   # https or site-relative
      banner_alt: War Dogs   # one line, at most 120; omit when the banner is decorative
    nav:                     # the game's own links on its pages; at most 6; no placement
      - label: Playbook
        url: https://example.com/wardogs/
      - label: Server config
        url: /servers/htg-wardogs-1
        role: owner
    maps:                    # the server card's picture by map name as the game spells it; https only; at most 50
      Ozeti: https://example.com/games/wardogs/maps/ozeti.jpg       # a level in play
      Europe: https://example.com/games/wardogs/maps/europe.jpg     # a theatre, for a level without its own
      Europe/DayClear: https://example.com/games/wardogs/maps/europe-dayclear.webp   # that theatre in one lighting, tried first
    map_fallback: https://example.com/games/wardogs/maps/default.jpg  # any other map; omit for gravel's placeholder
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
```

The default tokens meet WCAG AA contrast in both schemes; a host's tokens are
the host's responsibility (`make a11y` checks the defaults only).

The `discord` section holds ids, never a credential, and is as public as the rest. Ids are Discord
snowflakes as quoted strings (YAML would read a bare one as a number); a role may be mapped once,
never to the guild's own id (`@everyone`), and roles need a guild. The bot reads the section at
every role-sync pass, so a mapping change is a manifest change and lands within one pass.

A `seeding` rule (ADR-0016) names a server, the channel its calls go to and the seeder role they
ping. That role is the members' own choice, so role sync never manages it, and it may not be
`@everyone`. The rule's `hours` are weekly windows: `days` from `mon` to `sun`, and `from` and `to`
as 24-hour `HH:MM` in `stats.timezone`. A `to` before `from` is an overnight window that ends the
next day, `from` equal to `to` is refused, and a rule's windows may not overlap. Times are
wall-clock in the zone, so HTG's `18:00`–`23:00` America/Chicago stays the evening across a
daylight-saving change. A rule without windows never calls, and neither does a module that is off
(`seeding.enabled` in bot.yaml). The bot's app needs `servers:read` and `participation:write`.

A `recaps` rule (ADR-0018) names a server, the channel its recaps go to, a title and the sessions
as weekly `hours`, in the same form and with the same rules as seeding's. A few minutes after each
window ends, the bot posts the recap, unless nobody was on. Keep the hours in step with the event
members see: HTG's Wardogs Wednesdays is both a Discord Scheduled Event and this rule. The bot's
app needs `stats:read` unless `stats.public` is on.

The `streams` posts (ADR-0019) need a channel and at least one streamer. A login is the channel's
name as its URL spells it: Twitch's are 4 to 25 letters, digits and underscores; Kick's also take
hyphens. A streamer is listed once per platform. The platforms' applications are the bot's
(bot.yaml `streams`), never the settings', and the module is off until bot.yaml turns it on.

The `news` feeds post a Steam game's announcements into a channel. Each feed needs a positive
`app_id`, a channel and a `label`, and a game posts to a channel once (it may post to several).
`feeds` are Steam's feed names (`steam_community_announcements`, the game's own announcements, when
empty). Steam's news API needs no key, and the module is off until bot.yaml's `news.enabled` turns
it on.

The `match_results` posts (ADR-0024) need a channel: each series a server's match plugin reported
is posted there once it ends, with its maps, its winner and the top three by kills. `servers` lists
the server ids whose results are posted, each once; without it every server's are. The players
appear as on the result page, under a pseudonym unless the member chose their name. The bot's app
needs `stats:read` unless `stats.public` is on, and the module is off until bot.yaml's
`match_results.enabled` turns it on.

The `presence` card (ADR-0017) needs a channel and at least one game. Each game needs an activity
name or an application id to recognize it by, and no name (compared without regard to case) or id
may count for two games. The card shows counts only, and only of members who share their activity
on Discord: the bot keeps what they are playing in memory and nothing in the hub. It is drawn only
by a bot with `presence.enabled` and the application's Presence Intent (docs/bot.md "Presence").

### Game overlays (gravel#66)

A game's pages carry its overlay: a server of the game (`/servers/{id}`), that server's moderation
confirmation and log, and a board scoped to the game or one of its servers. The overlay wraps the
page's content, under the site's header: the game's banner first, then its links in a navigation
of their own (labelled with the game's name; owner-only links for the owner), then the page, with
the game's accent on its links, buttons and badges. The header and footer keep the organization's
theme, and every other page is untouched. The overlay is in the content, not the layout, so an
htmx swap carries it in and out as a board's scope changes.

Only the accent is overlaid. The overlay colours a region of the page, so a background or text
colour there would paint a patch on the organization's background; the accent stays legible on
it. `/theme.css` gives each game a rule for both schemes, the organization's accent standing in
for a scheme the game leaves out, so a light-only overlay never shows its light accent on the dark
background. As with the base theme, the accent's contrast is the host's to check: `make a11y`
checks gravel's defaults and its fixtures only.

A key under `games`, and a season's `game`, must be a game this hub ships (`wardogs`, `cs2`,
`seaofthieves`, `starcitizen`, `pubg`): `apply`, `check` and `UpdateOrganizationSettings` refuse
any other, so a typo can't theme nothing or count a season over no matches. A game that sets
nothing is dropped from the document.

`maps` and `map_fallback` are the Discord server card's picture of the map in play (docs/bot.md
"Server cards"): keys as the game names a map (War Dogs: a level such as `Ozeti`, or a rotation
theatre such as `Europe`), optionally with the lighting the status reports (`Europe/DayClear`,
tried before the plain name), values https URLs, since Discord fetches them itself. A game with either
set gets a picture for every map: its own, else the fallback, else gravel's generic placeholder
(`/static/map-placeholder.png` on the hub). The War Dogs driver adds the rotation's current and next
map to the status (`ServerStatus.rotation_map`, `next_rotation_map`), read at most once a minute;
the server page shows the next one.

**How to join.** A driver that knows how players reach its server puts it on the status as
`ServerStatus.join {method, value, hint}`. `method` is `id` (paste `value` into the game's own
server browser), `address` (`value` is host:port) or `uri` (a link that launches the game onto the
server). `hint` is one line in the game's terms. It is public, since it is what a community
shares to bring players in. The War Dogs driver reads the host's server ID (`GET /v1/server-id`) at
most once every 10 minutes and offers it as `id` with "Paste it into Join by ID at the bottom left
of the Community browser". That is the only way in that works (hidden-token-gaming/wardogs-server#8,
2026-10-11): no launch URI and no `steam://connect` joins a War Dogs server. The server page and the
Discord card show it while the server is live.

**A shared join password (#180).** A cloud-VM server whose manifest entry sets
`vm.share_password: true` (it needs `vm.password_file`) gives its password as `join.password` to an
**app whose token carries `servers:read`**, and to no one else: an anonymous caller, a member, a
member's token an app holds and the web pages always get it empty, so public reads still carry no
credential (ADR-0010 §1). It is set only while the server is up (`ok` and reachable), and never
while a lease is live: a lease's own password goes only to its holder (ADR-0023 §3). The hub reads
the file each time and keeps nothing. The stock bot's server card shows it under the connect line,
for a community that posts its casual password where members find the server anyway.

## Service credentials (ADR-0008)

A first-party service (a host's Discord bot, later Muster) calls the API as an **app**, not as a
member: a registered client id and secret, traded at the token endpoint for a short-lived opaque
bearer token that carries the app's scopes. The secret and every token are stored as SHA-256
hashes, like session tokens. An app's web pages can also sign a member in and act as them:
[Members sign in to apps](#members-sign-in-to-apps-adr-0015).

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
| `servers:read` | `ListServerPlayers`: who is on a server, for a live card (the rest of `ServerService` is public), and a server's shared join password in `ServerStatus.join` (`vm.share_password`, #180) |
| `servers:moderate` | `ModerationService`: kick, ban, unban, message, broadcast and move players, list a server's bans, read the audit log |
| `servers:operate` | `SetServerHostDesired` and `SetServerHostExpectation`: park and unpark a cloud-VM server, and say how many players to expect |
| `roles:grant` | `SetUserRole`, for moderator grants only: Discord role sync's ceiling ([Roles](#roles)) |
| `events:read` | `ListEvents`: the events log, for a notifier or a dashboard of its own ([Events](#events-adr-0028)) |
| `servers:lease` | `ServerLeaseService`: take a leasable server for a match and give it back ([Server leases](#server-leases-adr-0023)) |
| `servers:configure` | `ServerConfigService`: read, plan and apply a server's configuration |
| `stats:read` | `StatsService`'s boards, match lists and match results while the settings keep stats private |
| `participation:write` | `StatsService` `RecordParticipation`: participation events from a first-party app ([Participation](#participation-adr-0014)); the only way to record them, the owner's session included |
| `host:<name>[:<more>]` | nothing in gravel: a host's own scope, for a service beside the hub that checks it (below) |

Scopes are capabilities ([Capabilities](#capabilities-adr-0027)): `apps create` accepts the ones an
app may hold, listed above. The rest (`settings:write`, `servers:provision` and the like) are a
member's in person until a procedure lets an app use them.

**A host's own scopes.** A host's service beside the hub (Hidden Token Gaming's Muster, say) can
give its callers scopes of its own: `host:` followed by one to four lower-case segments joined by
colons (`host:muster:read`, `host:muster:write`), at most 64 characters. The operator grants one
with `apps create --scopes host:muster:write`; the hub stores it on the app, issues it in
client-credentials tokens like any other, and returns it from introspection, where the service
checks it. Gravel itself admits nothing by a host scope, so an app holding only host scopes may
call only what is public. A member never grants one: the authorization endpoint refuses it like
an app's own scope.

Rules: a token lives `apps.token_ttl` (an hour by default) and is pruned after; `scope` at the
token endpoint may narrow a token to a subset of the app's scopes, never widen it; an unknown,
expired or revoked token is answered 401 with `WWW-Authenticate: Bearer error="invalid_token"`
before any procedure runs; a procedure an app may not call answers `permission_denied`, and one a
member may not call still does. The owner's session is admitted wherever a scope is, so the owner
can do by hand what the app does. Requests carrying a token are rate-limited per app
(`rate_limit.per_app`), the token endpoint per client address. To rotate a secret, create a new
app and revoke the old one once the service has moved; `apps revoke` deletes the tokens at once.

## Members sign in to apps (ADR-0015)

A first-party app's web pages (Muster, first) sign a member in with their hub account through the
OAuth 2.0 authorization-code flow with PKCE (RFC 6749 §4.1, RFC 7636 `S256` only, RFC 9700), and
the app then calls the API **as that member**. It is not OpenID Connect: there is no ID token and
the hub keeps no signing key. The app learns who signed in from `IdentityService.GetMe` with the
member's token, and a service checks a token it is handed by introspection.

```sh
# Register with the URI the app's callback lives at (exact; https, or http on a loopback host)
gravel-hub apps create --name Muster --scopes stats:read --redirect-uri https://muster.example.com/auth/callback
# The app sends the member's browser to (golang.org/x/oauth2: AuthCodeURL with S256ChallengeOption):
#   https://app.example.com/oauth/authorize?response_type=code&client_id=$CLIENT_ID
#     &redirect_uri=https%3A%2F%2Fmuster.example.com%2Fauth%2Fcallback&scope=stats%3Aread
#     &state=$STATE&code_challenge=$CHALLENGE&code_challenge_method=S256
# and receives https://muster.example.com/auth/callback?code=…&state=$STATE. It trades the code:
curl -u "$CLIENT_ID:$CLIENT_SECRET" -d grant_type=authorization_code -d code="$CODE" \
  -d redirect_uri=https://muster.example.com/auth/callback -d code_verifier="$VERIFIER" https://app.example.com/oauth/token
# {"access_token":"…","token_type":"Bearer","expires_in":3600,"scope":"stats:read"}
# Who signed in (no scope needed)
curl -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' -d '{}' \
  https://app.example.com/gravel.hub.v1.IdentityService/GetMe
# Is a token live, and whose? (any app of the hub, with its own credentials)
curl -u "$CLIENT_ID:$CLIENT_SECRET" -d token="$TOKEN" https://app.example.com/oauth/introspect
# {"active":true,"sub":"<member's user id>","client_id":"gravel_…","scope":"stats:read","exp":1791700000,"token_type":"Bearer"}
# {"active":false} for an expired, revoked, unknown or another organization's token: nothing else
```

- **The authorization endpoint** is a page of the hub, `GET /oauth/authorize`. `redirect_uri` is
  required and must be one the app registered, as an exact string: a trailing slash, another port,
  http for https or an extra query is a different URI. An unknown or revoked client, or a redirect
  URI the app did not register, shows an error page and never redirects. Anything else wrong goes
  back to the app with `error` (`invalid_request`, for example missing PKCE or `plain`;
  `unsupported_response_type`; `invalid_scope`) and its `state`. A parameter sent twice is
  `invalid_request`. A visitor without a session goes through the login and comes back.
- **Consent.** The first time, the member sees the app's name and what each scope lets it do, with
  Allow and Cancel (Cancel answers `access_denied`). The grant is remembered per member and app; the
  next sign-in is silent while the member's hub session lasts, and the member is asked again only
  when the app asks for more. An empty `scope` asks for sign-in only. A scope that is only an
  app's own (`participation:write`, or a host's `host:…`) cannot be asked for.
- **Codes and tokens.** A code is hashed, single use and lives 60 seconds, bound to the app, the
  redirect URI, the PKCE challenge and the member. Its first presentation spends it, whatever the
  outcome; a second one answers `invalid_grant` and revokes every token issued from it. A member's
  token lives one hour, carries scopes no wider than the app's, and has no refresh token: the app
  sends the member through the flow again.
- **What a member's token may do.** What the member may do, narrowed to the token's scopes: `GetMe`
  needs no scope; every other procedure needs the scope in [Service credentials](#service-credentials-adr-0008)
  as well as the member's own right (an owner's token with `servers:configure` configures, a
  member's with the same scope is refused). The public reads that do not look at the caller need
  none. Unlinking, logging out, roles, settings, the owner claim, the leaderboard name and the apps
  a member allowed are refused to any token. A member's token never acts `on_behalf_of` anyone and
  is never the app on an app-only procedure (`RecordParticipation`). Moderation through it is
  recorded with the member and the app ("Jo via Muster"). Requests with it are rate-limited per
  member (`rate_limit.per_user`).
- **Introspection** (`POST /oauth/introspect`, RFC 7662) is for apps: without valid client
  credentials it answers 401 `invalid_client`. An app may cache an answer until `exp` or for 60
  seconds, whichever is sooner; revoking an app, a grant or a member's sessions takes effect within
  that window. The token and introspection endpoints are rate-limited per client address.
- **Revocation.** The member revokes an app on the account page ("Apps you allowed"): the grant,
  the member's tokens for it and its unexchanged codes go at once. "Log out everywhere" ends the
  member's tokens in every app too. `gravel-hub apps revoke` ends everything the app holds.
- **Privacy.** A remembered grant (member, app, scopes, when) is data about the member: the account
  page shows it, revoking deletes it, and it goes with the account. Nothing else of the flow is
  kept past its use: codes once no token issued from them can be alive, tokens at expiry.

## Servers (ADR-0010)

The hub knows the game servers it controls as resources: a **game** is a Game spec gravel ships
(`games/<game>/game.yaml`: who its players are keyed by, which drivers control it, the
publisher's rules), and a **server** is one a driver controls. A deployment declares both
in `servers.yaml` and applies it like the Organization settings; the hub stores them (migration 6)
and never stores a credential, only the file to read it from.

**Game specs and publisher rules (gravel#8).** gravel ships five specs. War Dogs has a driver;
Counter-Strike 2 has the cloud-VM driver ([Cloud-VM servers](#cloud-vm-servers-adr-0003)). Sea of
Thieves, Star Citizen and PUBG are **catalog entries** (`drivers: []`): their rules are known
before gravel can run a server for them. A manifest may enable a catalog
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
target); putting it back revives it. A server has no `seeding` section any more (removed in 0.8.0):
seeding rules are the Organization settings' `discord.seeding` (ADR-0016, [Organization
settings](#organization-settings-adr-0007)), and `check` and `apply` refuse a manifest that still
has one, naming each server's section and its line. `apply` refuses a credential or feed token file it cannot
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
`unreachable`, `credential_refused`, `credential_missing`, `rate_limited` or `error` (and, for a
cloud-VM server whose machine is not running, `parked` or `provisioning`), and the cause
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

### Cloud-VM servers (ADR-0003)

A cloud-VM server runs on a VM the hub makes, sizes, parks and destroys itself, through
`cloud.Provider` (DigitalOcean today). The hub boots the VM from a cloud-init document (vmdoc: an
`admin` user for the OS, the runtime user `gravel` whose home is the data volume, podman, security
updates), installs the game's container as a rootless quadlet, pushes the game's environment file
over SSH (never in the document: the cloud keeps it, and anything on the instance can read it), and
controls the game over its own channels: RCON, and the log the game pushes to the hub's [log
route](#ingestion-adr-0011). What a game needs on the VM is its Game spec's `cloudvm` section
(image by digest, ports, install path, environment, the settings a manifest may set); the driver is
the same for every game. Counter-Strike 2 is the first (the `joedwards32/cs2` image, which updates
the game at every start, so a restart is the patch-day step).

```yaml
locations:                                   # the clouds the hub makes VMs in
  - id: do-sfo3
    provider: digitalocean
    region: sfo3
    project: 7fd3841c-8bd5-49b1-bf2b-8956a8e252e0   # optional; an id survives a rename
    token_file: /run/secrets/gravel-digitalocean-token   # the API token, a podman secret
    ssh_key_file: /run/secrets/gravel-cloudvm-ssh-key    # the hub's ed25519 private key (ssh-keygen -t ed25519)
    image: fedora-44-x64                     # optional
    extra_ssh_keys: ["ssh-ed25519 AAAA… ops"]   # optional: operators' break-glass keys on the VMs
    ssh_from: ["198.51.100.7/32"]            # optional: addresses SSH is admitted from besides the hub's
servers:
  - id: cs2-1                                # a letter first, at most 40: it names the volume and firewall
    name: "Our CS2 | 10-man"
    game: cs2
    driver: cloudvm
    location: do-sfo3                        # one of locations; no endpoint: the hub knows the address
    credential_file: /run/secrets/cs2-rcon-password
    poll_interval: 15s
    trust: official
    feed:                                    # required: the game's log is how its players are known
      url: https://ingest.example.com        # an origin; the hub adds /api/ingest/log/<token>
      token_file: /run/secrets/cs2-feed-token
    vm:
      size: c-2                              # the provider's size the VM is made at
      volume_gib: 100                        # the install's disk (at least the spec's minimum, 100 for CS2)
      address: reserved                      # reserved (kept across parks and resizes; default) or ephemeral
      park_after: 30m                        # optional: park after this long empty (5m–24h); absent never
      password_file: /run/secrets/cs2-join-password   # optional: the password players join with
      share_password: true     # optional: give it to apps with servers:read while up, for a card (#180)
      publisher_token_file: /run/secrets/cs2-gslt     # optional: a Game Server Login Token
      scaling: {}                            # optional: walk the game's ladder as the room fills (below)
      seed_snapshot: {name: cs2-install}     # optional: make a NEW volume from this snapshot (or {id: …})
    settings:                                # what the spec's cloudvm.settings declares, checked by type
      game_alias: competitive                # competitive · wingman · casual · deathmatch · custom
      map_group: mg_active
      start_map: de_mirage                   # max_players is the ladder's with scaling, else set it here
      tv: false
```

`hub.yaml` names the hub's own address for the VMs' firewalls (`cloud.egress_cidrs`); without it
the hub asks a public service every ten minutes. Set it on a hub with a fixed address.

**What the hub makes, and finds again.** Everything carries the tags `gravel-server` and
`gravel-server:<id>`; the instance is named `<id>`, the volume `<id>-data`, the firewall
`gravel-server-<id>`, and the hub's key is registered as `gravel-hub-<location>`. The firewall
admits the game's public ports (CS2: UDP 27015) from everyone, RCON (TCP 27015) and SSH only from
the hub (and `ssh_from`), and re-pins them when the hub's address changes. On DigitalOcean a
reserved IP reaches the instance on its anchor address and carries outbound traffic only through
the anchor gateway, so for a server with a reserved address the document installs the provider's
egress script as a boot service (`gravel-reserved-egress`, with a NetworkManager hook): once the
address is assigned, outbound traffic leaves from it, and the game answers players from the address
they dialled and registers with its publisher under it. The hub still reaches the instance's own
address for SSH and RCON, and takes the game's log from either address. A `server_hosts` row
records what the hub wants and where it got to; the cloud is the truth, so a hub that stops in the
middle of anything finds what exists by its tags and names and carries on without making it
twice.

| State | What the hub is doing | The status says |
|---|---|---|
| `creating` | firewall, volume, reserved address, instance, the address assigned | `provisioning` |
| `booting` | waiting for the first boot (cloud-init, the game's unit installed); SSH host key trusted on first use and pinned | `provisioning` |
| `starting` | the environment file pushed over SSH, then waiting for the game to answer RCON (a first start downloads the install: CS2 was 73.4 GB in 2026-10) | `provisioning` |
| `running` | polled like any server; settings or secrets changed in the manifest reach the game when it is empty | `ok` (or a failure) |
| `parking` | players warned, the game stopped, the instance stopped and, where a stopped instance bills (DigitalOcean), destroyed | `provisioning` |
| `parked` | nothing: the volume and the reserved address stay; unparking makes a new instance on them | `parked` |
| `resizing` | the game stopped, the instance stopped, resized (CPU and memory only: the root disk never changes, so the way back is open) and started | `provisioning` |
| `destroying` | the instance, then the volume and the address only when asked, then the firewall | `provisioning` |

A step that fails keeps its state, shows a word (`error_code`: `cloud_failed`,
`cloud_rate_limited`, `boot_timeout`, `install_timeout`, `ssh_host_key_changed`,
`duplicate_instances`, `volume_not_ours`, …) and is retried with a backoff up to ten minutes. A
host key that changes, or two instances with the server's tag, stop the host until a human looks.
Removing a server from the manifest parks its host; destroying it is a separate request.

**The game's hooks.** A Game spec's `cloudvm.hooks` are files the hub keeps in the game's install
on the volume (`~/<game>/<path>` on the host). They are written with the environment file, in the
same push, and checked with it, so a hook edited or deleted on the host is put back like the
environment file (below). A hook holds no secret: it travels in the command the hub runs over SSH,
and all of them together are at most 32 KiB.

**The VM's journal doesn't hold the secrets** (#170). The `joedwards32/cs2` image puts the Game
Server Login Token, the RCON password and the join password on the server's command line
(`+sv_setsteamaccount`, `+rcon_password`, `+sv_password`), and the server prints its command line
when it starts. The image's `entry.sh` sources `pre.sh` from the install just before it starts the
server, so Counter-Strike 2's Game spec makes `pre.sh` a hook that sends everything the container
prints from then on through a filter that writes `[redacted]` for the three values. The filter is a
process substitution, not a wrapper: `entry.sh` stays the container's first process, and stop
signals, the RCON `quit` and the unit's restarts are unchanged. A server that started before its
hook was written keeps the values in its journal until the journal rotates. The hub's copy of the
game's log never had them (CS2's log push doesn't carry the command line, and the log route
redacts it anyway).

**The environment file.** The hub pushes the game's environment file, and its hooks, when what it
renders changes (a setting, a rotated secret, a new hook), and it reads the files' SHA-256 on the
host before trusting its own record: at every start (a create, an unpark, a recreated instance) and on the five-minute drift
pass while the server runs (#169). The file lives in the runtime user's home, which is the data
volume, so a hand edit on the host outlives a park; a file that differs, or is missing, is pushed
again and the game restarts on it, at once while starting and when the server is empty while it
runs. Each one is an `env_drift` event on the host and `gravel_cloudvm_env_drift_total`; only the
hash leaves the host, never the file. A host that can't be read at start fails the step (the hub
never starts a game on a file it couldn't check); while running it is logged and left alone.

**Parking and resizing.** An empty server past `park_after` is parked by the hub (its event says
`idle`). The owner and moderators park and start a server (`SetServerHostDesired`, or the server
page's "Server machine" section); the owner resizes it to a size the provider allows from its
current one (`ResizeServerHost`; refused with players on unless `force`) and destroys a parked one
(`DestroyServerHost`; the volume and the address stay unless asked). A resize disconnects everyone
for a few minutes (DigitalOcean's `c-2` to `c-4` and back: 2½ minutes each way, measured on HTG's
server), and an owner's resize lasts until the host is parked: an unpark makes the instance at
`vm.size` again.

**Snapshots (#166).** A server's install lives on its data volume, so the full download comes
back only for a new volume: a second server in a location, or a rebuild after a volume is lost
(Counter-Strike 2: 73.4 GB, about half an hour inside DigitalOcean). The owner copies a parked
host's volume into a snapshot (`SnapshotServerHostVolume`, or "Snapshot the disk" on the server
page; parked only, so the volume is detached and the copy consistent), named
`<server>-data-<game>-<build>-<date>` unless given a name and tagged with the server's tags; an
event on the host records it. A server whose manifest has `vm.seed_snapshot` (by `name` in the
location's region, or by `id`) has its volume made from it **when the volume is first made**,
never for a volume that exists, so an unpark or a recreate always uses the server's own volume;
then the game needs only the update since the snapshot. `servers apply` asks the cloud and refuses
a seed it doesn't find, one in another region, and a `volume_gib` smaller than the snapshot's
source. A snapshot holds whatever the volume does, the server's environment file with its secrets
included: keep it like the secrets, and delete it in the cloud when it is no longer wanted (it
bills by the gigabyte every month). A seeded server's start finds that file is not its own and
pushes its own (the environment check above). The provider's token needs DigitalOcean's
`block_storage_snapshot` scope (create, read, delete).

**Scaling (#37).** With `vm.scaling` the hub sizes the host to its room, walking a **ladder**
one rung at a time: the Game spec's `cloudvm.ladder` (CS2: `c-2` for 10 players, `c-4` for 20),
or the manifest's own `scaling.ladder`. `vm.size` is the rung the host is made at, and each rung's
players become the game's players setting (`players_setting`, CS2's `max_players`), so the
manifest leaves that setting out. The status polls keep since when the room has been full or
empty: a rung at least `up_at` full (0.8) for `up_for` (5m), or holding as many players as it is
sized for, decides a move up; one at most `down_at` full (0.25) for `down_for` (30m) decides a move
down; no decision comes within `cooldown` (30m) of the last resize. A decision is an event on the
host (step `scale`, with its inputs: "up: 9 of 10 players for 5m0s, to c-4 (20 players) at the
next boundary"), players are warned once in chat, and it waits for a **boundary**: the game's log
announcing a match's end or a map loading (CS2's `Game Over` and `Loading map`), or an empty
server. Then the host resizes as an owner's resize does, and the game restarts with the new rung's
players. A decision the room stops supporting (an up with the room back to `down_at`, a down with
it back to `up_at`) is dropped (step `scale_cancelled`); parking beats a move down. The owner can
**pin** a size (`ResizeServerHost` with `pin`, or "Keep it at" on the page): the policy stands
aside and an unparked host is resized back to the pin as soon as it runs empty, until
`UnpinServerHostSize`. A host is always made at `vm.size`, never at a pin, because the root disk a
host is made with never changes and never shrinks: DigitalOcean's `c-4` made as `c-4` has a 50 GB
disk and could never come back to `c-2`'s 25. `gravel-hub servers apply` asks the cloud and
refuses a ladder a host made at `vm.size` can't walk both ways; `servers check` (no cloud) checks
the rest. The server page shows the rung, the ladder with its prices, a decision waiting and why,
and the month's cost with each run at its own size's price.

**Expected population (#172).** The live players are what the policy sees, so a PUG that has just
filled would get its bigger machine at the first boundary after it arrives. The owner, a moderator
or an app with `servers:moderate` (a PUG bot, Muster) can say how many players to expect until when:
`SetServerHostExpectation` (`players`, `until` at most 24 hours ahead, a one-line `reason`;
`players` 0 clears it), or "Expect this many players" on the page. Until then the room is the
larger of the live and the expected players. An expected crowd the rung can't hold is decided at
once, not after `up_for`, and not held by the cooldown: it is an announcement, not noise. It is
carried out at once while the server is empty, else at the next boundary, and the decision says
where it came from ("up: expected 10 until 2026-10-14 23:00 UTC: PUG 3 filled, 0 of 10 players on,
to c-4 (20 players) at the next boundary"). Meanwhile the server isn't parked for being empty (the
crowd hasn't come yet), and it isn't moved down. `players` is clamped to the ladder's top. A pin
beats it. An expectation doesn't unpark a server: start it as well. An unparked host is made at
`vm.size` and moved up as soon as it runs, since it is empty. Once `until` passes the hub clears it
(an `expect` event, as setting and clearing are); each change is also an audit row
(`host_expect`).

```sh
curl -s -X POST -H 'Content-Type: application/json' -b "gravel_session=…" \
  -d '{"server_id":"cs2-1","desired":"running"}' https://app.example.com/gravel.hub.v1.ServerHostService/SetServerHostDesired
```

**Moderation.** A CS2 server offers status, players, kick, broadcast (`say`) and bans; kill,
message and moving a player are not offered. Players are known from the log the game pushes (CS2's
`status` lists no SteamIDs), so a player is kickable once their connect line arrived. The hub's ban
list is enforced by the driver itself: a banned SteamID is kicked as it connects.

**Rehearse it locally.** `deploy/game-vm.sh drill` boots the CS2 document in a local VM (qemu/KVM,
as `make hub-vm` does for the hub's host), pushes the environment over SSH and checks the game's
container runs as the runtime user; `drill-full` also lets the game download, answers RCON and
pushes its log to a local listener, then proves a new instance on the same disk plays again with no
push and no download.

### Server leases (ADR-0023)

A server the manifest marks leasable is taken for a match on request (#20): an app with
`servers:lease` (Muster's `/pug`), or the owner or a moderator. The lease gives it its own
password, settings, plugin set and match config. The hub starts it, says when it is ready, and
parks it and gives it back after.

```yaml
servers:
  - id: htg-cs2-1
    vm:
      lease:
        pool: pug              # leases may ask for the pool instead of the server
        release_after: 5m      # after the match's series ends
        idle_after: 15m        # empty this long once ready
        max: 4h
        ready_within: 15m      # not ready by then fails the lease
        settings: [start_map, tv]   # what a lease may set
```

- **Requesting:** `RequestServerLease` names the server or the pool, the holder's reference (one
  live lease per reference; asking again answers it), the players to expect (the server is sized
  for them, #172), settings, at most one plugin set and a match config. The hub takes a running
  empty server first, then a parked one, never one with players on (`failed_precondition`). The
  answer holds the join password and the connect line, the only place they appear; the password is
  derived from `leases.key_file` and the lease's id and never stored.
- **Ready:** once the host runs the lease's environment, the game answers, the plugin set answers
  its check, and the match config is loaded. `WatchServerLease` streams each change (a heartbeat
  every 30 s) until the lease is over; `GetServerLease` and `ListServerLeases` read them. An app
  sees its own leases only.
- **Plugins (#176):** a Game spec's `cloudvm.lease.plugins` are pinned sets: Counter-Strike 2's
  `matchzy` is Metamod:Source and MatchZy with CounterStrikeSharp, by URL and SHA-256. The game's
  `pre.sh` fetches them into a cache on the volume, checks and unpacks them, patches
  `gameinfo.gi` at every start, and writes MatchZy's config (the hub's event and demo routes, the
  feed token as its header); with no set asked for, it strips Metamod and the server is vanilla. It
  records the outcome in `~/cs2/.gravel/plugins.status` on the VM. A set that doesn't answer
  within three minutes is dropped: the server restarts vanilla and the lease says why
  (`plugin_state` `vanilla`).
- **Ending:** `release_after` after the match's series ends, `idle_after` empty, `max`, a park,
  or `ReleaseServerLease` (the holder, the owner or a moderator; also "Give back the lease" on the
  server page). The expectation is cleared and the server parks, warning anyone on; its own
  password and settings come back at its next start. Every request and release is in the audit
  log.

### Match results (ADR-0024)

A server's match plugin reports each series it runs: MatchZy, which a lease's `matchzy` plugin set
installs, posts its events to `POST /api/ingest/matchzy` and uploads each map's demo to
`POST /api/ingest/matchzy/demo`, with the server's feed token as a `Bearer` header and only from
the server's own host (`pre.sh` writes both into MatchZy's config). An event is stored as received
(source `matchzy`), then recorded:

- `series_start` makes the result (teams, maps), linked to the server's live lease;
- `going_live` starts a map (its name from the hub's last look at the server);
- each `round_end` writes the map's running score and every player's running totals;
- `map_result` the map's score and winner;
- `series_end` the maps won and the winner, and starts the lease's last `release_after`.

An event for a series whose start never came starts it. A demo goes to the `demos` bucket under
`matches/<result>/<map>-<file>`, and the map records it; without a bucket the demo route answers
404. Read them with `StatsService.ListMatchResults` (by server or lease, ended or not),
`GetMatchResult` (players shown as on a board: an opted-in name, else a pseudonym) and
`GetMatchDemo` (a 15-minute link), or on the page `/results/{id}`, under the same access as the
boards. `gravel_match_events_total{event, result}` and `gravel_match_demos_total{result}` are on
the Server leases row.

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
moderators ([Capabilities](#capabilities-adr-0027)) may call it, and so may an app with `servers:moderate` (a bot).
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

**The log route.** A game that cannot send an Authorization header pushes to
`POST /api/ingest/log/<token>` instead (CS2's `logaddress_add_http`; the cloud-VM driver writes the
URL into the game's environment). The token in the path needs three guards the bearer route does
not: the access log writes the path as `/api/ingest/log/<redacted>`; a batch is accepted only from
the address of the server's own host (anything else, or a server whose host is not running, is 401
and counted as `address_mismatch`); and the batch is kept redacted (CS2: secret cvars in its startup
dump, and the token wherever the log echoes it). The batch is stored with the headers the source
defines (CS2: instance token, byte offsets, ticks, timestamp, game state), at most 256 KiB, then
handed to the driver, whose roster is how a CS2 server's players are known. A log source's token
is refused on the bearer route.

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
with `erased` and a random token in `match_stats`, `stats_monthly`, `participation_events`,
`participation_monthly` and the bodies of the stored ingest batches (their hashes recomputed), and
deletes the pseudonym, in one transaction. The numbers
stay, shown as "Deleted player"; nothing maps the token back (a hash would not do: every SteamID can
be tried). A player still on a server is recorded under their identity again from the next poll.
The flow members start themselves comes with identity's erasure; this is the stats store's part.

Metrics: `gravel_stats_matches_total{server,game}`, `gravel_stats_rows_written_total{server}`,
`gravel_stats_record_errors_total{server}`, `gravel_stats_rolled_rows_total`, and the rollup job's
`gravel_hub_job_last_success_timestamp_seconds{job="stats_rollup"}` (stored by Prometheus as
`exported_job`, since the scrape's own `job` label wins), on the hub dashboard's Stats row.

### Session recaps (ADR-0018)

A recap is what one server's window held: an evening's anchor, read after it ends. Nothing is
stored for a recap. `GetSessionRecap` reads it from what the recorder keeps, each time it isn't
cached:

- **Occupancy, by the minute** (`server_occupancy`). For each minute anyone was on, it holds the
  fewest and the most players the polls saw: the server's own count, or the identities on, whichever
  is larger. An empty poll counts, so a minute the server emptied in has a fewest of 0.
  - **Peak** is the largest most.
  - **Minutes at or above the floor** counts the minutes whose fewest reached it, so a minute that
    touched 20 once doesn't count. The floor is asked for when the recap is read.
  - Nothing in it names anyone, so it is kept like the matches, with no retention.
- **Team scores** on each match (`matches.teams`): the last scores the polls saw, written when they
  change, so a match that ended holds its final ones.
- **The matches** are those that overlap the window. A player counts when they were seen inside it.
  Their kills are their match totals, so a match that straddles an edge brings its kills from outside
  the window too.
- **The top five** by kills, each with at least one kill, are shown as the boards show players.
  After the raw retention window, a recap keeps its numbers but loses its top five.

The window is at most 24 hours and must have ended. The recap has the boards' access and cache, and
the hub's page `/servers/{id}/recap?from=…&to=…&floor=…` renders it. The bot's recaps module
(docs/bot.md "Recaps") posts one after each window of the settings' `discord.recaps`.

```sh
curl -s -H 'Content-Type: application/json' \
  -d '{"server_id":"htg-wardogs-1","from":"2026-10-14T23:00:00Z","to":"2026-10-15T03:00:00Z","floor":20}' \
  https://app.example.com/gravel.hub.v1.StatsService/GetSessionRecap | jq '{peakPlayers, minutesAtFloor, players}'
```

### Participation (ADR-0014)

Not everything a community counts is a kill. A first-party app (a seeding tracker, an events app)
records that a player **took part** in something: a seeding session, an event they attended. These
**participation events** are `first_party` (ADR-0011) and feed participation boards and profiles
only, never a leaderboard: they live in tables of their own that no combat board reads.

- **Recording.** Register the app with `participation:write` (`gravel-hub apps create --name muster
  --scopes participation:write`), trade its credentials for a token ([Service
  credentials](#service-credentials-adr-0008)) and call `RecordParticipation` with 1 to 100 events. A
  member's session is refused, the owner's too: an event is what an app saw. Requests are limited
  per app (`rate_limit.per_app`, 600 a minute and a burst of 100 by default).
- **An event.** `key` is the app's idempotency key (1 to 128 printable ASCII characters): sending a
  key again answers `duplicate` and stores nothing, even with other fields, so a retry is always
  safe; another app's key is its own. `kind` is the app's name for the activity (a lower-case letter,
  then up to 31 lower-case letters, digits or underscores). `identity` is a `discord` or `steam`
  identity (a subject of at most 64 characters): events are keyed by identity, so a player who links
  it later inherits them. `game_id` (a game the hub ships) and `server_id` (a server it knows; its
  game is stored, and must match `game_id` when both are sent) are optional, as is `session`, the
  app's own reference (at most 128 characters). `occurred_at` is required, at most 5 minutes ahead of
  the hub's clock and not older than the stats store keeps raw rows (`stats.raw_retention_months`).
  `duration_seconds` is 0 to 86400. One invalid event refuses the whole request, with every problem
  named (`events[3].kind: …`).
- **Boards.** `GetParticipationBoard` ranks the players of one kind (empty: the first recorded) by
  time taken part, then times, over `all`, `week`, `month` or a named `season`, optionally for one
  game; it answers every kind recorded for a picker. It has `GetBoard`'s access, names and cache.
  Profiles (`GetMemberProfile`) carry each kind's totals over their windows.
- **Retention and erasure** are the stats store's: the `stats_rollup` job rolls events past the
  window into `participation_monthly` (one row per kind, game, period and identity, the same
  periods), and `stats erase` re-keys both tables.

```sh
curl -s -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"events":[{"key":"muster-42/1234","kind":"attendance","identity":{"provider":"discord","subject":"1234"},
       "session":"muster-42","occurredAt":"2026-10-10T20:00:00Z","durationSeconds":5400}]}' \
  https://app.example.com/gravel.hub.v1.StatsService/RecordParticipation
# {"results":[{"key":"muster-42/1234","outcome":"recorded"}]}
```

Metric: `gravel_participation_events_total{kind,outcome}` (`recorded`, `duplicate`, `invalid`; every
event of a refused request is `invalid`, under kind `invalid` when its kind is malformed), on the hub
dashboard's Stats row.

## Demo hub (gravel#112)

A demo hub shows what a hub looks like with a community on it, before there is one: full boards,
match lists, members' profiles, moderation history, and a server card that changes. It is a
**separate hub with its own database**. Production never gets fake rows: `audit_log` is append-only,
and pseudonyms and matches are permanent.

Two halves:

- **History:** `gravel-hub demo seed` writes made-up members, past matches and moderation through
  the store, once, into an empty database.
- **Live:** `gravel-hub demo wardogs-server` is a simulated War Dogs server
  (`games/wardogs/wardogsim`). The hub polls it like any server in `servers.yaml`. Its matches are
  recorded beside the seeded ones, and its card and status move.

**`demo: true` in hub.yaml** marks the hub. Every page then shows "Demo data: these members, matches
and stats are made up" above the header, in both themes and under the CSP. `GetOrganization`
returns `demo: true` so other clients can say it too, and `config check` prints that the hub is a
demo. The flag works both ways: a hub **without** it refuses to start on a database that holds demo
members, so made-up data is never served as real.

**The seed's refusals.** Nothing is written in any of these cases:

1. hub.yaml lacks `demo: true`. Checked before the database is opened.
2. The hub has no War Dogs server. Apply `servers.yaml` first: a seed runs once.
3. The database is not empty. In one transaction, the store locks the users, identities, history,
   ban and pseudonym tables against every other writer. It counts them and refuses on any of:
   - a user who is not a demo member;
   - a demo member, meaning it was seeded already;
   - a match, an audit entry, a ban, an ingest batch, a participation event, a monthly rollup row, a
     minute of occupancy or a pseudonym;
   - a registered app. A hub in use registers its bot before anyone signs in, so a demo hub
     registers its apps after the seed.

   The refusal sits beside the SQL (`store.SeedDemo`), so no caller can skip it. A production hub
   with even one sign-in, one recorded poll or a registered bot is refused, whatever its hub.yaml
   says.

**The demo marker.** Every identity the seed writes has verification method `demo`
(`store.DemoVerification`), and a demo member is a user all of whose identities carry it. A login
always records its provider's method (`oauth2`, `openid`). So a person who signed in to the demo
hub, to look around or to claim it, is a real user, and a later seed is refused. The made-up
subjects can't be anyone's:

- Discord ids are 1000001 and up, below the first id Discord's clock could issue.
- SteamIDs are in the recorder's stand-in range `76561190000000001`–`…999`, below the first
  individual account. The simulator's players use the same ids, so a seeded member who holds one
  plays on the simulated server too.

**What the seed writes.** `--members` (default 40) members with display names and linked Discord
and Steam identities. They joined across the first half of the window, the first three are
moderators, and about 60% show their names on boards. For each War Dogs server in `servers.yaml`
there are `--days` (default 90) of matches: one whenever the profile's population for the hour
reaches 10, with 10 to 40 players. The players come from the profile's regulars (default 400):
mostly passers-by, shown under pseudonyms, with the members playing about twice as often. Each
match has kills, deaths, time on, final faction scores, and every minute's occupancy for the session
recaps. A second server fills less than the first. There are also about one and a half moderation
entries a day per server (kicks, whispers, broadcasts, moves, bans, a third of the bans lifted on
appeal). A banned player plays nowhere on that server while the ban stands. With a pseudonym key
configured, the seed also stores every player's pseudonym; without one, the boards show "Unnamed
player". The same `--seed` gives the same members and the same history, ending when the command
runs.

**The simulator.** `gravel-hub demo wardogs-server --listen ADDR --token-file FILE` answers the RCON
API as the newest recorded build (`games/wardogs/testdata/`, embedded in the binary). The token file
holds the RCON password it accepts; put the same one in `servers.yaml`'s `credential_file`.

- **Players:** stand-in SteamIDs and invented names join and leave on the profile's daily curve.
  Some leave and rejoin the same match with their counters restarted. A match starts once the
  document's `MinimumRequiredPlayers` are on.
- **Matches:** kills and deaths accumulate by skill, and the faction holding the hill scores each
  score period. After the profile's match length the rotation moves and the counters reset.
- **Moderation and configuration** work as on the live server: kick, kill, message, broadcast,
  move, ban, unban, and get, validate and put with the revision and `If-Match`. A write takes effect
  when its section says: at once, at the next match, or at the daily restart (`restart_hour`).
- **The server's rules:** three requests with a missing or wrong token lock an address's protected
  routes for a minute (429 with Retry-After); 600 requests a minute; 64 KiB bodies.
- **Optional flags:** `--name` (default "Demo War Dogs server (made-up players)"; the recording's
  name is a real community's), `--build` (an older recording), and `--feed-url` with
  `--feed-token-file` (start with `[WDServerFeed]` set).
- **State:** it keeps everything in memory, so a restart starts from the recording again.
- **The feed:** with `[WDServerFeed] Url` and `Token` in force, it posts kill batches to
  `<Url>/api/ingest/events` every 2 s. The batch's shape is a guess until a real one is captured
  (hidden-token-gaming/deploy#49). It is a JSON array of `{eventId, timestampUtc, killerSteamId,
  victimSteamId, distance}` and lives in `wardogsim/feed.go` alone. The hub stores batches without
  parsing them, so the guess can't reach the stats.

**The calibration profile** (`--profile`, both commands) holds distributions only, never a name or a
SteamID. A fresh measurement changes the demo without code. Sources, best first: HTG's own
`match_stats`, the CL-509546 lobby, and other servers' public counts. It is decoded strictly, and
the built-in profile applies without the flag:

```yaml
version: 1
timezone: America/Chicago           # the hours below are local
population: [18, 12, 8, 5, 4, 3, 3, 4, 6, 8, 10, 12, 14, 16, 18, 22, 26, 32, 40, 48, 52, 46, 36, 26]  # players on, by hour, midnight first
regulars: 400                       # distinct players (1–999)
session_minutes: {min: 20, max: 120}
match_minutes: {min: 20, max: 35}
kills_per_minute: 0.6               # a typical player's, while a match is on
skill_spread: 0.55                  # log-normal sigma of players' kill rates
rejoin_rate: 0.08                   # share who leave and come back to the same match
restart_hour: 9                     # the host's daily restart, local; leave it out for none
```

**Bringing one up**, in this order, because the seed runs once and only on an empty database:

```sh
gravel-hub demo wardogs-server --listen 127.0.0.1:7789 --token-file /run/secrets/demo-rcon &
gravel-hub servers apply servers.yaml --config demo-hub.yaml   # a server with endpoint http://127.0.0.1:7789
gravel-hub demo seed --config demo-hub.yaml                    # migrates and creates the organization if needed
gravel-hub settings apply organization.yaml --config demo-hub.yaml   # stats: {public: true}, so visitors see the boards
gravel-hub serve --config demo-hub.yaml
```

To seed again, drop the database and start over.

## Capabilities (ADR-0027)

Every procedure checks one **capability**, an `area:verb` name, against whoever is calling. App
scopes are the same names, so one word says what a role, a member's token and an app may do.

| Caller | Holds |
|---|---|
| Nobody | Nothing: a procedure that needs a capability answers `unauthenticated` |
| A member in person | `servers:read`, plus their grants' roles (below) |
| The owner in person | Every capability a person can hold (not `participation:write`, which is an app's own) |
| A member's token | What the member holds, narrowed to the token's scopes ([Members sign in to apps](#members-sign-in-to-apps-adr-0015)) |
| An app | Its scopes, of the capabilities an app may hold ([Service credentials](#service-credentials-adr-0008)) |

A caller who lacks the capability gets `permission_denied`. An app that names `on_behalf_of` acts
only for a member who holds the capability themselves; without it, it acts for itself. A lease's
`on_behalf_of` names the member the lease is for, who must be a member of the hub. `GetMe` and
`ListUsers` return each member's `roles` (instance-wide grants), `grants` (with their game) and
`capabilities` (what they hold instance-wide).

### Roles

Five roles, fixed bundles of capabilities (ADR-0027 §3):

| Role | Holds | How someone gets it |
|---|---|---|
| Owner | Every capability a person can hold, `org:transfer` included | The owner claim; exactly one, never a grant |
| Admin | Everything but `org:transfer`, including what costs money (`servers:provision`), settings, roles and apps | A grant, by the owner only |
| Operator | `servers:read`, `servers:operate` (park, unpark, the expectation), `servers:configure`, `servers:lease`, `events:read`, `stats:read`, `metrics:read` | A grant |
| Moderator | `servers:read`, `servers:moderate`, `servers:lease`, `identity:read`, `events:read`, `stats:read` | A grant |
| Member | `servers:read` | Being a member |

**A grant can be for one game** (`game_id`): it gives the role's **scopable** capabilities
(`servers:*`, `roles:grant`, `events:read`) for that game's servers, those added later included,
and the rest of the bundle not at all. A game-scoped admin can't change the settings or the servers
manifest; a game-scoped moderator sees the instance-wide audit log and lease list filtered to their
games. Leasing in person needs `servers:lease` everywhere; leases are mostly an app's (Muster).

**Who may grant what** (ADR-0027 §5): a grant needs `roles:grant` for its scope (everywhere for an
instance-wide grant, or for the game); only the owner changes an admin grant; an app holding
`roles:grant` changes moderator grants only (so Discord role sync can never grant more); nobody
changes their own. Grants are made on the Members page (`/members`, for anyone holding
`roles:grant`) or with `IdentityService.SetUserRole`; `ListRoleGrants` lists them
(`identity:read`). Every grant and revocation is kept in `role_changes` (who or which app, the game,
when), which can't be changed.

**From ADR-0013's roles** (migration 00025): every member who held `moderator` holds a moderator
grant and an operator grant, so park and unpark, which moved to operator, still work for them.
Prune the operator grants on the Members page where they don't belong. `users.roles` is no longer
read and goes in #199.

| Capability | Admits | Held by |
|---|---|---|
| `identity:read` | `LookupUser`, `ListUsers`, `ListIdentityEvents`, `ListRoleGrants` | admins, moderators; apps |
| `servers:read` | `ListServerPlayers`, `GetServerHost`, `ListServerHostEvents` | every member; apps |
| `servers:moderate` | `ModerationService` | admins, moderators; apps |
| `servers:operate` | `SetServerHostDesired`, `SetServerHostExpectation` (park, unpark, the expectation) | admins, operators; apps |
| `servers:lease` | `ServerLeaseService` | admins, operators, moderators; apps |
| `servers:configure` | `ServerConfigService` | admins, operators; apps |
| `servers:provision` | `ResizeServerHost`, `UnpinServerHostSize`, `SnapshotServerHostVolume`, `DestroyServerHost` | admins |
| `stats:read` | the boards, match lists and match results while stats are private | admins, operators, moderators; apps |
| `participation:write` | `RecordParticipation` | apps only |
| `roles:grant` | `SetUserRole` | admins; apps (moderator only) |
| `members:manage` | `CreateLocalAccount`, `IssuePasswordReset` | admins |
| `settings:write` | `UpdateOrganizationSettings` | admins |
| `events:read` | `ListEvents` | admins, operators, moderators (a game-scoped grant: that game's events); apps |
| `servers:manage`, `apps:manage`, `metrics:read`, `stats:erase` | named now; their procedures arrive with H1 | per the roles above |
| `org:transfer` | ownership transfer (#197) | the owner alone |

**The access matrix.** `internal/api/testdata/access_matrix.txt` is every procedure's answer to every
kind of caller, and `TestAccessMatrix` fails when it changes. A change to who may call what is a
diff of that file, rewritten with `go test ./internal/api -run TestAccessMatrix -update`; a new
procedure fails the test until it is listed.

## Events (ADR-0028)

The **events log** is one append-only record of what the hub did: who logged in for the first
time, who was granted what, what a moderator did and how it went, what a cloud-VM server's machine
went through, leases, finished series, backups. Every source writes its event in its own
transaction, so the log never says something happened that didn't, or misses something that did.
Notifiers (#196), the admin dashboard (#193) and the CLI (#194) read it; so can an app with
`events:read`.

- **Reading:** `OrganizationService.ListEvents` with `after_id` follows the log, oldest first
  (pass `next_after_id` back to keep following); without it, the newest come first, and
  `next_before_id` pages back. `head_id` is the newest id at the time of the read. Filter by kinds
  or groups (`"host"`), `min_severity` and `server_id`. A caller holding `events:read` for some
  games sees those games' events only.
- **Kinds and severities** are fixed (`internal/events`): `info` (it happened), `notice` (a person
  did something worth seeing), `warning` (something may need a person soon), `critical`
  (something needs a person now). Logins are not events: they are every visit.
- **Backfill:** migration 00026 turns the history the hub already kept (`identity_events`,
  `role_changes`, finished `audit_log` rows, `server_host_events`) into events, oldest first.
- **Backups:** with `backup.status_file` set, the hub reads it every five minutes and writes
  `backup.failed`, `backup.stale` (no good base backup for 26 hours) or `backup.recovered` when the
  state changes; each is keyed by the run it is about, so a restart writes nothing twice.
- **Retention:** `events.retention_days` (400 by default) in the Organization settings; the prune
  is the only delete the log allows (a trigger refuses every other UPDATE, DELETE and TRUNCATE).
- `gravel_events_total{kind}` counts events appended, on the hub dashboard's Events row.

| Kind | Severity | What |
|---|---|---|
| `identity.registered` | info | A new member registered with a provider or a username |
| `identity.linked` | info | A member linked another account |
| `identity.unlinked` | notice | A member unlinked an account |
| `identity.proven` | info | A member proved an account again in game |
| `identity.password_set` | notice | A local account's first password was set from its link |
| `identity.password_reset` | notice | A local account's password was reset from its link |
| `identity.password_reset_issued` | notice | Staff or the host issued a password link |
| `role.granted` | notice | A member was granted a role, everywhere or for a game |
| `role.revoked` | notice | A member lost a role |
| `moderation.kick` | notice | A player was kicked |
| `moderation.ban` | notice | A player was banned |
| `moderation.unban` | notice | A player was unbanned |
| `moderation.message` | info | A player was sent a message |
| `moderation.broadcast` | info | A server was sent a broadcast |
| `moderation.move_player` | info | A player was moved to a team |
| `action.apply_config` | notice | A server's configuration was applied |
| `action.host_park` | notice | Someone asked a cloud-VM server to park |
| `action.host_unpark` | notice | Someone asked a cloud-VM server to start |
| `action.host_resize` | notice | Someone resized a cloud-VM server's machine |
| `action.host_unpin` | info | Someone let the scaling policy size a server again |
| `action.host_snapshot` | notice | Someone snapshotted a parked server's volume |
| `action.host_expect` | info | Someone said how many players to expect |
| `action.host_destroy` | warning | Someone destroyed a parked server's remains |
| `action.lease_request` | info | An app or a member asked for a lease |
| `action.lease_release` | info | An app or a member gave a lease back |
| `host.absent` | info | The host is gone |
| `host.creating` | info | The host is being made |
| `host.booting` | info | The host is booting |
| `host.starting` | info | The game is starting on the host |
| `host.running` | info | The server is up |
| `host.parking` | info | The host is parking |
| `host.parked` | info | The host is parked |
| `host.resizing` | info | The host is being resized |
| `host.resized` | notice | The host was resized and is running again |
| `host.destroying` | notice | The host is being destroyed |
| `host.failing` | warning | A step on the host keeps failing; the hub retries |
| `host.recovered` | info | The host's failing step went through |
| `host.expect` | info | The host was told how many players to expect |
| `host.snapshot` | notice | A volume snapshot was taken |
| `host.scale` | info | The scaling policy moved the host to another size |
| `host.scale_cancelled` | info | A planned resize was called off |
| `host.env_drift` | notice | The host's game files had drifted and were put back |
| `host.step` | info | Another step on the host |
| `lease.starting` | info | A lease was taken; its server is getting ready |
| `lease.ready` | info | A lease's server is ready to join |
| `lease.ending` | info | A lease's match ended; the server is given back shortly |
| `lease.released` | info | A lease ended and its server was given back |
| `lease.failed` | warning | A lease's server did not get ready |
| `match.ended` | info | A series ended on a server |
| `backup.failed` | critical | The last base backup failed |
| `backup.stale` | warning | No base backup has succeeded for over 26 hours |
| `backup.recovered` | info | A base backup succeeded again |

Not yet events: the owner claim, settings updates and app registrations (#189); a server becoming
unreachable or recovering (#190).

## Pages (gravel#18)

The web UI's community pages, each an API client like every page (ADR-0005): what a reader may see
is the API's to decide, and a page says so in words when it is refused.

| Page | Shows | Who sees what |
|---|---|---|
| `/servers` (also `/`) | every server: name, game, trust, state (Online, Offline, Checking, Unavailable), players and map | everyone |
| `/servers/{id}` | the status with the team scores, the next map in the rotation, how to join (while live) and when it was observed, who is on (in-game name, team, kills, deaths, ping, a member badge), the last 10 matches, a link to the server's board | the status: everyone; who is on: members and the owner ("Log in to see who's on" otherwise); matches: as the boards |
| `/members/{id}` (`/profile` is yours) | a member's totals over all time, this week, this month and each season running now (Official servers, every linked account), their participation over the same windows (a table per kind, [Participation](#participation-adr-0014)), their 20 most recent matches on any server, and on your own profile the switch that shows your name on leaderboards (`SetBoardName`) | the member, the owner and apps with `stats:read`; anyone when the member shows their name and `stats.public` is on; otherwise "no such profile", so a page never says who plays under a pseudonym. A named member on a board links here |
| `/servers/{id}`, for the owner and moderators | per player in "Who's on": kick, ban or message, with a reason or the message; a Moderation section with a broadcast, a ban by SteamID64 for a player who isn't on, the ban list with unban, and the log link. A control shows only when the server's driver offers it (its capabilities). Every action goes through a confirmation page (`POST /servers/{id}/moderate`) that says exactly what will be sent, and is recorded in the audit log with the moderator's name | the owner and moderators ([Capabilities](#capabilities-adr-0027)) |
| `/servers/{id}/log` | the server's moderation log, newest first: when, who (a member's name, or an app and who it acted for), the action, the player, the reason or message, the result | the owner and moderators |
| `/boards?scope=&window=&metric=&page=` | a leaderboard: scope `` (everyone, Official servers), `game:<id>` or `server:<id>`; window `all`, `week`, `month` or `season:<name>`; ranked by any column of the game's `layout.board`, 50 a page | everyone when `stats.public` is on, else the owner ("Leaderboards are private…" otherwise) |
| `/servers/{id}/recap?from=&to=&floor=` | a session recap ([Session recaps](#session-recaps-adr-0018)): the window in `stats.timezone`, peak players, minutes with anyone on and at or above the floor, distinct players, the matches with their times, map and team scores, and the top five by kills as the leaderboard shows players; "Nobody was on during this window." when nobody came. A link that isn't a recap window (not RFC 3339, longer than a day, not ended) is a 400 | as `/boards` ("Recaps are private…" otherwise) |
| `/participation?kind=&window=&page=` | a participation board: the kinds recorded (a picker; "Nothing has been recorded yet" before the first), window `all`, `week`, `month` or `season:<name>`, players ranked by time taken part, 50 a page; linked from `/boards` | as `/boards` ("Participation boards are private…" otherwise) |

The forms and links work without JavaScript; htmx swaps only the main content. On a narrow screen a
board drops its last two columns. A game's pages carry its overlay from the Organization settings
([Game overlays](#game-overlays-gravel66)).

**Caching.** Every board read is an aggregate, and a public board can be read by anyone as often as
they like, while the numbers change only when someone plays. The hub caches each board answer in
memory and keeps it while the stats store has not been written (the recorder and the rollup count
their writes), up to 15 minutes; once something has been written, an answer is still served for a
minute. An idle hub serves every board from memory, and a live match costs one read a minute per
board, however many people watch. A member's name choice clears the cache. The API's per-IP rate
limit applies in front of all of it.

## Login (ADR-0004)

A member logs in with a provider whose `login` is true (Discord by default), or with a username
and password ([Local accounts](#local-accounts-adr-0026)), and links the others (Steam by default)
from the account page. Every attempt is a row bound to the browser by an auth
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

### Local accounts (ADR-0026)

With `auth.local.enabled: true` a username and a password are one more way in, with no provider
at all: the hub needs no Discord application to have members or an owner. A local account is the
identity `("local", <username>)`, unique across the hub, linked and unlinked like any other; its
password is stored beside it as an argon2id hash and goes with it when it is unlinked.

- **Logging in:** the login page shows a username and password form (first, when this browser
  last logged in with it), which posts to `/auth/local/login`. A wrong password, an unknown
  username and a locked account get one answer and cost one hash each, so the page says nothing
  about which.
- **Registration** is the host's choice, `local_accounts.registration` in the Organization
  settings: `closed` by default, when staff make accounts (#187), or `open`, when
  `/auth/local/register` takes anyone. Closed, that page is 404.
- **A member who signed in with Discord** sets a username and password on the account page, then
  logs in with either. One local account a member; a username another member holds is refused.
- **Usernames** are 3 to 32 of `a-z`, `0-9`, `.`, `_` and `-`, starting with a letter or a digit,
  compared after NFKC and lower case (`Neckbeard` and `ｎｅｃｋｂｅａｒｄ` are one name). `admin`,
  `administrator`, `gravel`, `moderator`, `operator`, `owner`, `root`, `staff`, `support` and
  `system` are reserved.
- **Passwords** are at least `local_accounts.min_password_length` characters (12 by default),
  at most 1024 bytes, and never the username; compared after NFKC. No other rule: a long sentence
  is the advice.
- **Hashing:** argon2id at OWASP's minimum (19 MiB, 2 passes, 1 lane), as a PHC string, no pepper.
  A hash made with older parameters is replaced at the next good login.
- **Lockout:** five failures in a row lock an account for 15 minutes; each lockout since the last
  good login doubles it, up to a day. A good login clears it. Failures while locked don't count.
- **Limits:** local logins and registrations are also limited to 10 a minute per client address
  (5 at once), on top of the pages' own limit, because each hashes a password.
- **Password links** (#187): staff with `members:manage` make an account with
  `IdentityService.CreateLocalAccount` (or `gravel-hub users create-local` on the host) and hand its
  owner the one-time link that sets the password; a forgotten password gets a new link from
  `IssuePasswordReset` (or `gravel-hub users reset-password`). A link works once, for an hour, and
  a newer one for the account ends the older ones. Using it clears a lockout, ends every session
  the member had, and logs the browser in. Only the link's SHA-256 is stored; the link is never
  logged. Links are kept 30 days after they expire, then pruned.
- **Email** is optional and only ever for recovery; the hub never matches or merges accounts by
  email. Mailing a reset link to it waits for the email notifier: #234, after #196.
- `gravel_local_logins_total{result}` counts `ok`, `refused`, `locked`, `invalid` (a refused
  registration), `rate_limited` and `error`, and `gravel_password_resets_total{result}` counts links
  `issued`, `used`, `invalid` and `refused`, both on the hub dashboard's Login row;
  `gravel_identities{provider="local"}` counts the local accounts. Identity events
  `password_reset_issued`, `password_set` and `password_reset` go on the identity log.

### Proof in game (ADR-0022)

Where a provider has no sign-in the hub can use, or a member wants to prove an account again, a
one-time code typed in a game server's chat proves it (#5). The account page lists the running
servers whose chat the hub reads (a status with the `chat` capability: Counter-Strike 2 on the
cloud-VM driver), and "Get a code" shows a code like `7K3M-9QXA` once, on its own page, with the
server's address. The member joins and types `!link 7K3M-9QXA` (or `.link`, in any case, with or
without the hyphen) in all chat or team chat, from any team or none. The server's log carries the
line with the player's SteamID, from the server's own address. The hub links that Steam account to
the member, or proves it again when it is theirs already (`verification_method` becomes
`game-chat`), and says so in chat. The page notices within five seconds.

- **One code, ten minutes, once,** bound to the member, the server and (when proving an account
  again) that account; a new code replaces the open one. Only its SHA-256 is stored.
- **Another member's account stays theirs:** the code is spent, nothing moves, and the chat says
  the account belongs to someone else. The member asks the hub's owner.
- **Limits:** five codes a member an hour; ten codes presented per account and server, and sixty
  per server, every ten minutes. A wrong code gets one answer: unknown or expired. Failures are
  answered in chat at most once a minute per server.
- **What is kept:** proof rows for 30 days after they expire (pruned with the sessions); the log
  batch as the ingest route keeps it, with the code redacted in the copy a person reads.
- `gravel_identity_proofs_total{channel, result}` counts codes started, linked, proven, taken,
  bad and limited, on the hub dashboard's Login row; many bad codes on one server is someone
  guessing.

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

**With no Discord or Steam at all** (ADR-0025's single machine): turn local accounts on
(`auth.local.enabled: true`), start the hub, and from the host

```sh
gravel-hub users create-local --username neckbeard --config /etc/gravel/hub.yaml
# local account created: neckbeard
# password link (shown once, works once, until 2026-10-13 14:05 UTC):
# https://app.example.com/auth/local/reset/…
```

Open the link, choose the password (you are logged in), and paste the claim token on the account
page.

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

One hub, four tiers ([tiers.md](tiers.md), [ADR-0025](adr/0025-hub-as-a-product-the-unplug-test-tiers-and-personas.md)); the single-machine tier always stays viable.

| Tier | Run it with | Status |
|---|---|---|
| **Development** | `make up` builds the image with ko, writes dev secrets into podman's secret store and starts hub + Postgres 17 with `podman compose` (`deploy/compose.yaml`). Host ports come from `GRAVEL_PORT` and `GRAVEL_INTERNAL_PORT` (8080 and 9090 by default). `make down` stops it and keeps the Postgres volume | In |
| **1 · A single machine** | The quadlet units in `deploy/quadlet/` as rootless systemd user units, with the backup set from `deploy/quadlet/backup/`, the bot and the observability set as opt-ins; see `deploy/README.md`. The config file must be readable by uid 65532, the container's user (mode 0644 is fine: it holds no secrets) | In |
| **2 · Compose on a cloud box** | A production Compose file with the quadlets' digest pins and secrets from files, for a box you install on yourself; `docker compose up` on a fresh box gives a working hub with a local owner | #210 (H1) |
| **3 · A cloud VM the CLI makes** | `gravel hub create` makes the host (a firewall, a data volume that is the runtime user's home, a Fedora instance from a cloud-init document) through a cloud provider, and `--profile stock` installs tier 1's units on it with secrets generated on the host; `destroy` then `create` replaces the host with its data intact ([docs/cli.md](cli.md), [ADR-0020](adr/0020-the-hubs-own-host-made-by-the-cli-through-the-cloud-provider.md)) | In |
| **4 · A Kubernetes cluster** | The operator projects hub resources into the cluster and Agones runs the managed servers; Helm and OLM packaging | K1, after P5 (#21) |

The image is built by ko from `cmd/gravel-hub` on `gcr.io/distroless/static-debian12:nonroot`,
runs as uid 65532, and has the binary at `/ko-app/gravel-hub` (`.ko.yaml`). Releases push it to
`ghcr.io/gravel-project/gravel-hub` as `<version>` (no `v`: `0.8.0`) and `latest`, multi-arch
(amd64, arm64), signed with cosign and with an SBOM attached. Every green push to `main` publishes
it the same way as `main-<commit>` and `main`. `docs/releasing.md` has the verify commands for both
and the digest-pin procedure.

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
| `gravel_http_requests_total` | counter | `handler` (`healthz`, `readyz`, `oauth_token`, `oauth_introspect`, `pages`), `method`, `code` | HTTP requests by status | until the first request |
| `gravel_http_request_duration_seconds` | histogram (default buckets) | `handler` | HTTP latency | until the first request |
| `gravel_rpc_requests_total` | counter | `procedure`, `code` (`ok` or the Connect code) | API calls, the pages' in-process calls included (ADR-0005) | until the first call |
| `gravel_rpc_request_duration_seconds` | histogram (default buckets) | `procedure` | API latency | until the first call |
| `gravel_auth_completions_total` | counter | `provider` (a configured provider, or `other`), `intent` (`login`, `link`, `roles`, `unknown` on a failure), `result` (`ok`, `denied`, `failed`, `invalid`, `mismatch`, `taken`, `error`) | how login, link and Linked Roles attempts ended | until the first callback |
| `gravel_rate_limited_total` | counter | `scope` (`ip`, `user`, `app`) | requests a rate limit refused | until the first refusal |
| `gravel_oauth_authorizations_total` | counter | `result` (`issued`, `denied`, `rejected`, `error`) | how requests at the authorization endpoint ended (ADR-0015): a code went back, the member pressed Cancel, an unknown app or redirect URI or a request the app got an error for | until the first request there |
| `gravel_oauth_token_grants_total` | counter | `grant_type` (`client_credentials`, `authorization_code`, `other`), `result` (`issued`, `invalid_client`, `invalid_grant`, `replayed`, `invalid_scope`, `invalid_request`, `unsupported_grant_type`, `error`) | token endpoint requests; `replayed` is a code presented twice, whose tokens were revoked | until the first request |
| `gravel_oauth_introspections_total` | counter | `result` (`active`, `inactive`, `invalid_client`, `invalid_request`, `error`) | introspection requests | until the first request |
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
| `gravel_cloudvm_host_state` | gauge | `server`, `state` (every state, 0 or 1) | the state of each cloud-VM server's host (ADR-0003) | until the hub first sees the host |
| `gravel_cloudvm_state_since_timestamp_seconds` | gauge | `server` | when the host entered its state; long in a transition is a stuck step | until the hub first sees the host |
| `gravel_cloudvm_host_failing` | gauge | `server`, `code` | 1 while a host's step keeps failing, by its error word | while nothing fails |
| `gravel_cloudvm_operations_total` | counter | `server`, `op` (`create`, `unpark`, `park`, `resize`, `destroy`, `recreate`, `restart`), `result` (`ok`, `error`: one failed attempt) | host operations | until the first |
| `gravel_cloudvm_operation_duration_seconds` | histogram (5 s–1 h) | `op` | from leaving a resting state to reaching the next | until the first |
| `gravel_cloudvm_running_seconds_total` | counter | `server` | how long the host ran: what the cloud bills | until it first runs |
| `gravel_cloudvm_hourly_price_usd` | gauge | `server`, `size` | a running host's price | while it is not running |
| `gravel_cloudvm_log_batches_total` | counter | `server` | log batches that reached the game's roster | until the first batch |
| `gravel_cloudvm_bans_enforced_total` | counter | `server` | banned players kicked as they connected | until the first |
| `gravel_cloudvm_env_drift_total` | counter | `server` | times the game's environment file on the host was not the one the hub pushed, and was pushed again (#169) | until the first |
| `gravel_cloudvm_tier_players` | gauge | `server` | the players the rung of its scaling ladder a running host is on is sized for (#37) | without a policy, or off the ladder |
| `gravel_cloudvm_scale_pending` | gauge | `server`, `direction` (`up`, `down`) | 1 while a scaling decision waits for a boundary | until the first decision |
| `gravel_cloudvm_expected_players` | gauge | `server` | the players a host expects until the time it was given (#172); 0 when none | until the first expectation |
| `gravel_leases_total` | counter | `pool`, `result` (`requested`, `released`, `failed`, `never_ready`) | server leases and what became of them (#20) | until the first lease |
| `gravel_lease_plugins_total` | counter | `set`, `result` (`loaded`, `vanilla`) | plugin sets leases asked for, loaded or fallen back to vanilla (#176) | until a lease asks for one |
| `gravel_lease_ready_seconds` | histogram | | a lease's request to ready | until a lease is ready |
| `gravel_leases_live` | gauge | | leases holding a server now | always |
| `gravel_match_events_total` | counter | `event`, `result` (`recorded`, `ignored`, `error`) | match plugin events (MatchZy) and what became of them (ADR-0024) | until the first event |
| `gravel_match_demos_total` | counter | `result` (`kept`, `error`) | demos a server's match plugin uploaded | until the first demo |
| `gravel_cloudvm_scale_decisions_total` | counter | `server`, `direction`, `outcome` (`decided`, `executed`, `cancelled`) | the scaling policy's decisions and what became of them | until the first |
| `gravel_cloudvm_resizes_total` | counter | `server`, `direction`, `result` (`ok`, `refused`, `failed`) | resizes, the policy's and the owner's; a failed one started again at the old size | until the first |
| `gravel_cloudvm_volume_snapshots_total` | counter | `server`, `result` (`ok`, `error`) | snapshots of a parked host's volume the owner asked for (#166) | until the first |
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
and reads its status over the API and the metrics. Until a stats conformance suite exists, the
`first_party` source's conformance case (ADR-0014) is two integration tests: a participation event
reaches no combat board, of any scope, metric or window (`internal/store`,
`TestParticipationBoardAndTotals`), and an app's event reaches the member's profile and the
participation board but not `GetBoard` (`internal/hub`, `TestParticipationFromAnApp`).

The War Dogs client (`games/wardogs`) runs one contract against every recorded build in
`games/wardogs/testdata/` through a fake server (`games/wardogs/wardogstest`); the same contract
runs against a real server, reading only, with `-tags live` (`games/wardogs/README.md`). The demo
simulator (`games/wardogs/wardogsim`) is held to the recordings over the same client: every route a
recorded build advertises has a handler and answers in the recorded shapes. Its counters move,
matches end, players rejoin, and its moderation, configuration writes, three-strike lock and feed
work. The seed's refusals are tested against Postgres (`internal/store`, `cmd/gravel-hub`), and
`internal/hub`'s `TestDemoHub` runs the done-when: a seeded database, a simulator in `servers.yaml`,
banners on the pages, full boards, live status that changes, new matches from the polls, and a hub
without `demo: true` refusing the database.

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
