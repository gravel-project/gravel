# The bot

`gravel-bot` is gravel's Discord bot: one Discord application, the stock modules, and the hub's
API called as a registered app (ADR-0008). A host builds its own bot from the same packages
(`discord/bot`, `discord/hubclient`, `discord/modules/...`) and registers its modules beside the
stock ones, so there is one bot user in the guild. This page is the operator reference.

## Commands

| Command | Does |
|---|---|
| `gravel-bot serve` (default) | Register the commands, open the gateway when it is on, serve the listeners, run the jobs. Stops cleanly on SIGINT/SIGTERM. |
| `gravel-bot config check` | Load and validate the configuration, print a summary (never a secret). |
| `gravel-bot healthcheck [--url …]` | GET `/healthz` and exit 0 when it answers 200. The container healthcheck. |
| `gravel-bot version` | Print the build version. |

The config path is `--config`, else `$GRAVEL_BOT_CONFIG`, else `/etc/gravel/bot.yaml`.

## Configuration

One YAML file, strict (an unknown key is an error) and versioned.

```yaml
version: 1
discord:
  application_id: "1234567890"   # the Developer Portal's application id
  public_key: "…"                # its 64-hex public key; verifies HTTP interactions
  token_file: /run/secrets/gravel-bot-discord-token   # or GRAVEL_BOT_DISCORD_TOKEN
  guild_ids: ["…"]               # commands registered per guild (visible at once); empty = global (within the hour)
  gateway: true                  # member events, and interactions when the application has no endpoint URL
hub:
  url: http://gravel-hub:8080    # the hub's API on the stack's network
  public_url: https://app.example.com   # the origin members open; the links in replies
  client_id: gravel_…            # from `gravel-hub apps create --name bot --scopes identity:read`
  client_secret_file: /run/secrets/gravel-bot-hub-client-secret   # or GRAVEL_BOT_HUB_CLIENT_SECRET
role_sync:                       # the reconciler; what it maps is the Organization settings' discord section
  enabled: true
  interval: 10m                  # the full pass; at least 1m
  poll_interval: 30s             # the hub's identity log; at least 5s and at most interval
  dry_run: false                 # log the changes, make none
linked_roles:
  enabled: true                  # register the Linked Roles metadata schema at start
server:
  listen: 127.0.0.1:8081         # /interactions, /healthz, /readyz; put TLS termination in front
  internal_listen: 127.0.0.1:9091   # /metrics; keep it off the public network
  shutdown_timeout: 15s
log:
  level: info                    # debug · info · warn · error
  format: json                   # json · text
```

**Secrets**, each with the same precedence as the hub's: the environment variable, then the
`*_file` key (a podman secret mount), then the inline key for development.

| Secret | Environment | File key | Inline key |
|---|---|---|---|
| The bot token | `GRAVEL_BOT_DISCORD_TOKEN` | `discord.token_file` | `discord.token` |
| The hub client secret | `GRAVEL_BOT_HUB_CLIENT_SECRET` | `hub.client_secret_file` | `hub.client_secret` |

## The Discord application

In the Developer Portal: a Bot user; under Privileged Gateway Intents, **Server Members on**
(role sync lists the guild's members, and the gateway's member events start a pass) and Message
Content off; the application id and public key into the configuration; the token into a secret.
Invite the bot with the `bot` and `applications.commands` scopes and the Manage Roles permission,
and keep its role **above every role the mapping names** and below the staff roles: Discord lets a
bot manage only the roles under its own.

Use **the application the hub logs members in with** (`auth.discord.client_id`): Linked Roles
metadata belongs to an application, the bot declares it and the hub writes each member's values
for its own client id, so the two must be one application. Set its **Linked Roles Verification
URL** to the hub's `<auth.base_url>/auth/discord/roles` (docs/hub.md, "Discord Linked Roles").

Interactions arrive one of two ways, and the application decides: with an **Interactions Endpoint
URL** set to the bot's public `/interactions` (behind TLS termination), Discord POSTs them there,
signature-verified with the public key and acknowledged within three seconds; with the field
empty, they arrive over the gateway, so a staging bot needs no public URL. Both go through the same
router.

## Endpoints

| Listener | Path | What |
|---|---|---|
| `server.listen` | `POST /interactions` | Discord's outgoing webhook; a request with a bad signature is 401 |
| | `GET /healthz` | 200 `{"status":"ok","version":"…"}` while the process runs |
| | `GET /readyz` | 200 once the commands are registered and, with the gateway on, while the session is ready (read live, so a dropped session is 503 at once); else 503 with a reason |
| `server.internal_listen` | `GET /metrics` | Prometheus; every series is in [Observability](#observability) |

## The stock modules

| Command | Reply (ephemeral) |
|---|---|
| `/whoami` | the member's display name, each linked account (Discord, Steam, …) with its link date, and the account page; or that the hub does not know this Discord account yet |
| `/link` | where to link game accounts: the account page |

### Role sync (`discord/modules/rolesync`)

Keeps the roles the Organization settings map (docs/hub.md, "Organization settings") in step with
the hub's members:

- a member whose identities include any provider besides Discord gets `roles.linked`;
- a member who linked a provider gets `roles.providers.<provider>` where one is mapped;
- the first `count` members by registration get a `recognition` role, which is **never removed**
  once given (registration order does not change, and a role given by hand before role sync is
  kept).

A pass reads the mapping, every hub member (`ListUsers`) and every guild member, and adds or
removes **only the roles the mapping names**: other roles, staff roles and bots are never touched,
and a guild member the hub does not know loses the linked and provider roles. It runs at start,
every `interval`, as soon as the identity log (read every `poll_interval`) shows a registration,
a link or an unlink, and when a member joins the guild. Passes are idempotent. Discord's rate
limits are the REST client's business (it waits and retries); a change Discord refuses because
the role sits above the bot's is counted and logged once per role per pass ("move the bot's role
above it"); a member who left before their change is skipped. Any other failure ends the pass,
which is logged and counted, and the next one tries again: the bot keeps running. With no mapping
the module idles and says so once.

Run `dry_run: true` first on a guild whose roles were given by hand: every change is logged
("role sync would add a role") and none is made. Metrics:
`gravel_bot_rolesync_passes_total{result}` (`ok`, `idle`, `error`),
`gravel_bot_rolesync_changes_total{action,result}` (`add`/`remove`; `ok`, `dry_run`, `forbidden`,
`gone`), `gravel_bot_rolesync_last_success_timestamp_seconds`.

### Linked Roles (`discord/modules/linkedroles`)

Puts the metadata schema (`discord/rolemeta`: `steam_linked`, `xbox_linked`, `rsi_linked`,
`pubg_linked` as booleans, `supporter_tier` as an integer) on the application at start, when what
Discord holds differs, retrying with backoff while Discord is away. A provider the hub does not
offer yet reads 0 until it does. The member's values come from the hub's verification flow.
Metric: `gravel_bot_linked_roles_schema_registered`.

## A host's bot

A host's `main` is a `cli.Program` (`discord/bot/cli`): the same commands, config handling and
stock modules as `gravel-bot`, with the host's name, paths and own modules.

```go
var version = "dev" // set by the linker

func main() {
    os.Exit(cli.Program{
        Name: "my-bot", Version: version,
        DefaultConfig: "/etc/my/bot.yaml", ConfigEnv: "MY_BOT_CONFIG",
        Modules: func(cfg bot.Config, hub *hubclient.Client) []bot.Module {
            return []bot.Module{mybot.NewGoLive(hub)} // registered after the stock set
        },
    }.Main())
}
```

The stock set comes from `stock.Modules` (`discord/bot/stock`): `core` always, `rolesync` and
`linkedroles` as `bot.yaml` enables them, so a module gravel adds later reaches the host's bot at
its next gravel upgrade without a code change. `config check` lists the modules it would run. A
host that needs a different command line can call `stock.Modules` and `bot.New` itself.

A module implements `Name()` and `Register(r *bot.Registry) error`; the registry takes slash
commands with their handlers (disgo's `handler.CommandEvent`: reply with `CreateMessage`, or
`DeferCreateMessage` then `UpdateInteractionResponse` for anything slower than three seconds),
component handlers, gateway listeners and background jobs. A command name registered twice is a
startup error. `hubclient.New` gives a module the hub's API as the app: `Identity()` and
`Organization()` are Connect clients that carry the bearer token and refresh it.

## Running it

The compose stack has a `bot` profile (`deploy/compose.yaml`, config `deploy/bot.yaml`): fill in
the application, create the two secrets, register the bot with the hub, then
`podman compose --profile bot up`. `deploy/quadlet/gravel-bot.container` is the quadlet unit,
pinned by digest to the latest release; `make bot-image` builds `localhost/gravel-bot:dev` for a
local one. The release ships `gravel-bot` binaries and
`ghcr.io/gravel-project/gravel-bot` beside the hub's (docs/releasing.md).

```sh
gravel-hub apps create --name bot --scopes identity:read --config /etc/gravel/hub.yaml   # the client id and secret, once
printf '%s' "$SECRET" | podman secret create gravel-bot-hub-client-secret -
printf '%s' "$TOKEN"  | podman secret create gravel-bot-discord-token -
gravel-bot config check --config bot.yaml
```

## Observability

Logs are slog, JSON by default. `GET /metrics` on the internal listener carries the series below,
plus the Go (`go_*`) and process (`process_*`) collectors. The gateway series are read from the
session at scrape time, never from a remembered event. The `gravel bot` dashboard shows every one
of them (ADR-0009, `deploy/README.md` "Dashboards").

| Metric | Type | Labels | What it says | Absent when |
|---|---|---|---|---|
| `gravel_bot_build_info` | gauge, always 1 | `version`, `go_version` | the running build | never |
| `gravel_bot_gateway_connected` | gauge | | 1 while the gateway session is ready; 0 while it connects, resumes or is down, and when the gateway is off | never |
| `gravel_bot_gateway_latency_seconds` | gauge | | the last heartbeat's round trip | the session is not ready, or a heartbeat is in flight before the first answer |
| `gravel_bot_interactions_total` | counter | `route` (`/<command>`, or `other`), `result` (`ok`, `error`) | interactions handled | until the first interaction |
| `gravel_bot_interaction_duration_seconds` | histogram (.025 to 10 s, dense around Discord's 3 s) | `route` | from arrival to the handler's return, which includes the first answer | until the first interaction |
| `gravel_bot_jobs_total` | counter | `job` (`reconcile`, `register-metadata`, a host's own), `result` (`ok`, `error`) | background jobs that ended | until a job ends |
| `gravel_bot_rolesync_passes_total` | counter | `result` (`ok`, `idle` = no mapping, `error`) | role sync passes | role sync off |
| `gravel_bot_rolesync_changes_total` | counter | `action` (`add`, `remove`), `result` (`ok`, `dry_run`, `forbidden`, `gone`) | role changes made or, in a dry run, logged | role sync off, or until the first change |
| `gravel_bot_rolesync_last_success_timestamp_seconds` | gauge | | when the last pass finished without error (an `idle` pass counts) | until the first pass |
| `gravel_bot_linked_roles_schema_registered` | gauge | | 1 once the Linked Roles schema is on the application | Linked Roles off |

## Tests

`go test ./discord/...`: the configuration (defaults, strict keys, secret precedence, validation),
the runtime over a signed PING and a slash command delivered over HTTP against a fake Discord API
and a fake hub, the hub client's token fetch, refresh and retry, and the core module's replies.
Role sync is tested as a plan (who gets what) and as passes against a fake Discord REST API and a
fake hub: changes applied and settled, a refused role, a member who left, a 429 waited out, a
refused member list, a dry run, paging past a thousand members, and the loop following the
identity log; Linked Roles against a fake metadata endpoint, idempotent and retried. The gateway itself is disgo's and is not
driven in tests.
