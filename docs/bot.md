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
(the gateway subscribes to guilds and guild members, which role sync follows) and Message Content
off; the application id and public key into the configuration; the token into a secret. Invite
the bot with the `bot` and `applications.commands` scopes.

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
| | `GET /readyz` | 200 once the commands are registered and, with the gateway on, the session is up; else 503 with a reason |
| `server.internal_listen` | `GET /metrics` | Prometheus: `gravel_bot_build_info`, `gravel_bot_interactions_total{route,result}`, `gravel_bot_gateway_connected`, `gravel_bot_jobs_total{job,result}` |

## The stock modules

| Command | Reply (ephemeral) |
|---|---|
| `/whoami` | the member's display name, each linked account (Discord, Steam, …) with its link date, and the account page; or that the hub does not know this Discord account yet |
| `/link` | where to link game accounts: the account page |

Role sync and Linked Roles are the next modules (ADR-0008 §4).

## A host's bot

```go
rt, err := bot.New(bot.Options{Config: cfg, Logger: logger, Version: version},
    core.New(hub, cfg.Hub.PublicURL),   // gravel's modules
    mybot.NewGoLive(hub, ...),          // the host's, same interface
)
```

A module implements `Name()` and `Register(r *bot.Registry) error`; the registry takes slash
commands with their handlers (disgo's `handler.CommandEvent`: reply with `CreateMessage`, or
`DeferCreateMessage` then `UpdateInteractionResponse` for anything slower than three seconds),
component handlers, gateway listeners and background jobs. A command name registered twice is a
startup error. `hubclient.New` gives a module the hub's API as the app: `Identity()` and
`Organization()` are Connect clients that carry the bearer token and refresh it.

## Running it

The compose stack has a `bot` profile (`deploy/compose.yaml`, config `deploy/bot.yaml`): fill in
the application, create the two secrets, register the bot with the hub, then
`podman compose --profile bot up`. `deploy/quadlet/gravel-bot.container` is the quadlet unit;
`make bot-image` builds `localhost/gravel-bot:dev`. The release ships `gravel-bot` binaries and
`ghcr.io/gravel-project/gravel-bot` beside the hub's (docs/releasing.md).

```sh
gravel-hub apps create --name bot --scopes identity:read --config /etc/gravel/hub.yaml   # the client id and secret, once
printf '%s' "$SECRET" | podman secret create gravel-bot-hub-client-secret -
printf '%s' "$TOKEN"  | podman secret create gravel-bot-discord-token -
gravel-bot config check --config bot.yaml
```

## Tests

`go test ./discord/...`: the configuration (defaults, strict keys, secret precedence, validation),
the runtime over a signed PING and a slash command delivered over HTTP against a fake Discord API
and a fake hub, the hub client's token fetch, refresh and retry, and the core module's replies.
The gateway itself is disgo's and is not driven in tests.
