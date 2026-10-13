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
moderation:                      # the moderation command, off by default (below)
  enabled: false
  command: mod
mod_log:                         # the mod log, off by default (below); the channel is the Organization settings' discord.mod_log
  enabled: false
  interval: 15s
server_cards:                    # the status cards; which server goes where is the Organization settings' discord section
  enabled: true
  interval: 15s                  # how often the cards are read and edited where they changed; at least 5s
seeding:                         # seeding calls and minutes, off by default (below); the rules are the Organization settings' discord.seeding
  enabled: false
  interval: 30s                  # how often the rules are checked and, during a session, who is on is read; at least 10s
presence:                        # the "who's playing now" card, off by default (below); the card is the Organization settings' discord.presence
  enabled: false                 # needs the gateway and the Presence Intent toggle
  interval: 1m                   # how often the card is brought up to date; at least 15s
recaps:                          # session recaps, off by default (below); the rules are the Organization settings' discord.recaps
  enabled: false
  interval: 1m                   # how often the rules are checked; at least 15s
streams:                         # go-live posts, off by default (below); the streamers are the Organization settings' discord.streams
  enabled: false
  interval: 1m                   # how often the platforms are asked who is live; at least 30s
  twitch:                        # the application from dev.twitch.tv; leave out for no Twitch
    client_id: "…"
    client_secret_file: /run/secrets/gravel-bot-twitch-client-secret   # or GRAVEL_BOT_TWITCH_CLIENT_SECRET
  kick:                          # the application from kick.com's developer settings; leave out for no Kick
    client_id: "…"
    client_secret_file: /run/secrets/gravel-bot-kick-client-secret     # or GRAVEL_BOT_KICK_CLIENT_SECRET
news:                            # Steam announcements, off by default (below); the feeds are the Organization settings' discord.news
  enabled: false
  interval: 15m                  # how often Steam is asked; at least 5m
match_results:                   # finished match results, off by default (below); the channel is the Organization settings' discord.match_results
  enabled: false
  interval: 1m                   # how often the hub is asked for finished results; at least 30s
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
| The Twitch application's secret | `GRAVEL_BOT_TWITCH_CLIENT_SECRET` | `streams.twitch.client_secret_file` | `streams.twitch.client_secret` |
| The Kick application's secret | `GRAVEL_BOT_KICK_CLIENT_SECRET` | `streams.kick.client_secret_file` | `streams.kick.client_secret` |

## The Discord application

In the Developer Portal: a Bot user; under Privileged Gateway Intents, **Server Members on**
(role sync lists the guild's members, and the gateway's member events start a pass), **Presence
on only for the presence card** (below), and Message Content off; the application id and public key into the configuration; the token into a secret.
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

### Server cards (`discord/modules/servercards`)

Keeps a live status card for each server the Organization settings place
(`discord.server_cards`, docs/hub.md "Organization settings"): one Components V2 message per
server in its channel, edited in place. A card shows the server's name, players / max, map and
lighting, the teams and the host's note, with a footer naming the server and when the hub
last looked at it. The teams line counts the players on each team ("Lonestar **1** · Valkyra **0**
players") and adds each team's score once a match has one ("Lonestar **8** players, **1240** pts");
a game that doesn't say who is on which team shows its scores alone ("Lonestar **1240** pts", #159).
War Dogs reports no match phase, so "a match has a score" is the signal; Counter-Strike 2's log
carries no running score, so its card counts CT and T. A server the hub cannot read shows a degraded state instead of old numbers:
"Unreachable since …" after two missed polls, "Status unavailable" for the other state words,
and "Waiting for the first look" before the first poll. The card never carries error text
(`ListServers` has none). A field the game does not report is left out.

Every `interval`, a pass reads the settings and `ServerService.ListServers` (public, so the bot's
app needs no extra scope), renders each card and edits its message only when the card changed.
The "updated" time is rounded to the minute, so a quiet server costs at most one edit a minute.
The bot keeps no message id: at start, or when an edit finds the message deleted, it reads the
channel's 100 newest messages for its own card (its author, Components V2, the server id in the
footer). It edits that card and posts a new one only when none is there. A card's message pings
no one, whatever a name or the note says.

**The map.** While the server is live, a card shows the current map's picture, full width under
the name, a "Next: <map>" line after the teams when the game reports its rotation, and how to join
when the driver knows it ("**Join by ID:** `<id>`" as code, so a phone copies it in one tap, with the
driver's hint in small text; docs/hub.md "How to join"). A server whose manifest shares its join
password (`vm.share_password`, #180) gets a "**Password:** `<password>`" line under it, as code too:
the hub gives it to the bot's app because it holds `servers:read`, only while the server is up and
not leased, so a parked card never shows it. The picture
comes from the Organization settings' `games.<id>.maps` (docs/hub.md): the image for the level in
play (War Dogs: `Ozeti`), else for the rotation's current map (the theatre, `Europe`), each tried
first in the lighting in play (`Europe/DayClear`) and then plain, else the
game's `map_fallback`, else gravel's generic placeholder, which the hub serves at
`/static/map-placeholder.png` under `hub.public_url` (only an https hub: Discord fetches the image
itself). A game with neither `maps` nor `map_fallback` gets no picture, so a host opts in. The
picture's URL is part of the card, so a map change is one edit; a degraded card shows no map at
all. The War Dogs driver reads the rotation at most once a minute: the status names only its
positions.

In each card's channel the bot needs **View Channel, Send Messages and Read Message History**.
A refusal is counted as `forbidden` and logged once. A server the hub does not list is logged
once. A Discord or hub failure is logged once while it lasts and retried at the next pass. With
no card placed the module idles and says so once. Metrics:
`gravel_bot_servercards_updates_total{result}` (`posted`, `edited`, `forbidden`, `error`) and
`gravel_bot_servercards_last_success_timestamp_seconds`.

### Mod log (`discord/modules/modlog`)

Posts every moderation action to the channel the Organization settings name (`discord.mod_log`):
the hub's audit log, whether the action came from the web or from the moderation command, one
message per entry, in order. A line says what, where, to whom, by whom (a member's name, or the
Discord account an app acted for as a mention that pings no one), why, and the result, with a small
footer `-# audit <id> · <time>`:

```text
**Kick** · War Dogs #1 · `76561190000000002` · by **Jo** · afk in spawn · ok
-# audit 12 · Oct 10, 20:00 UTC
```

Every `interval`, a pass reads the settings and the audit log back to the last entry posted. An
entry still running (no outcome yet) waits for the next pass, unless it is older than two minutes,
when it is posted as "no result recorded". The bot keeps no state in the hub: at start, or when the
channel changes, it finds its place from the footer of its own newest posts among the channel's
100 newest messages. With none there, it starts from the newest entry, so turning it on never floods
the channel with history. A Discord refusal or failure is counted, logged and retried next pass,
and nothing is skipped. The audit log stays the record; this is a copy of it in Discord.

The bot's app needs **`servers:moderate`** (reading the audit log), and in the channel **View
Channel, Send Messages and Read Message History**. Metrics: `gravel_bot_modlog_posts_total{result}`
(`posted`, `forbidden`, `error`) and `gravel_bot_modlog_last_success_timestamp_seconds`.

### Seeding (`discord/modules/seeding`)

Some games start a match only once enough players are on: War Dogs needs 20. An empty server stays
empty unless people join it together, which is called seeding. The module calls an opt-in seeder
role to a server under its floor during set hours, and records the time each player spent seeding
as participation (ADR-0016).

**Rules.** The Organization settings' `discord.seeding` (docs/hub.md, "Organization settings") has
at most one rule per server, ten in all:

- **where:** `server`, `channel` (where the call is posted) and `role` (the seeder role it pings);
- **when:** `below` (the floor, 2 to 1000) and `hours`, weekly windows in the settings'
  `stats.timezone` (UTC when unset), such as `{days: [wed, sat, sun], from: "18:00", to: "23:00"}`.
  A `to` before `from` ends the next day. Times are wall-clock times in the zone, so a window keeps
  its local hours across a daylight-saving change;
- **how often:** `cooldown` (default 2h, 15m to 24h) and `call_at_start`;
- `note`: a line appended to the call, such as how to join.

Outside every window nothing is posted, and a rule without `hours` never calls: quiet is the
default.

**Calls.** Every `interval`, a pass reads the settings and `ListServers`. A call goes out for a
server when all of these hold:

- the current time is inside one of the rule's windows;
- the server is live (`ok` and reachable);
- it has fewer than `below` players;
- no session is open;
- the cooldown has run out since the last call.

The call pings the rule's role and nothing else: allowed mentions name that role only, and a
server's name is escaped. For example:

```text
@WD Seeder **HTG WARDOGS | NA WEST | #1** is at 7/20: join to get the match going.
Join from the server browser: HTG WARDOGS.
-# seeding htg-wardogs-1 · 1760482800 · open
```

With `call_at_start: true`, the first call of each window ignores any cooldown left over from
before the window: at the window's start, while the server is under its floor. Without it, the
cooldown runs across windows, so the option matters only when windows sit closer together than the
cooldown. That call counts toward the cooldown like any other. A call Discord refuses or fails
starts no cooldown and is tried again at the next pass.

**Sessions.** A call opens a seeding session, which runs until the server reaches `below` or the
window ends.

- **Counting.** Each pass during a session reads who is on (`ListServerPlayers`). It credits every
  player there with the time since the last pass, by their identity at the game's provider (a
  SteamID64). That time is measured, at most twice the interval, so a stalled bot does not credit
  the gap, and never past the window's end. A server that is not live, or a player list that cannot
  be read, credits nothing for that pass.
- **The end.** When the server reaches its floor, the call's message is edited to
  `**…** started: 20/20 at <time>. Thanks for seeding!`. When the window ends first, it is edited to
  `**…**: the seeding window closed at 12/20.`. An edit pings no one.
- **Recording.** Each player with 60 seconds or more is recorded as one participation event
  (`RecordParticipation`, docs/hub.md "Participation"):
  - kind `seeding`;
  - their identity, the `server_id`, `occurred_at` (the call's time) and their seconds;
  - the key `seeding:<server>:<call unix time>:<provider>:<subject>`.

  The hub keeps a key once, so a retry or a restart never counts twice. Seeders are counted by what
  they did, not by holding the role. A recording that fails stays in memory and is retried every
  pass. A batch the hub refuses as invalid is resent one event at a time, so the one event it
  refuses is dropped alone and logged. The minutes appear on the participation board ("seeding")
  and on profiles.

**Restart.** The bot keeps no state in the hub. At start, and when a rule's channel changes, it
reads the channel's 100 newest messages for the newest of its own calls for the server: the
marker's server, start and state (`open`, `reached`, `closed`). The cooldown counts from that call.
A call still `open` inside the window it was made in resumes its session, counting again from the
restart. A call whose window has closed is edited to say so. So a restart loses the seeding time of
the session counted so far, plus any recording not yet sent, and never posts a second call or any
history. When a rule is removed or its channel changes, its open call is closed.

The bot's app needs **`servers:read`** (who is on) **and `participation:write`** (the minutes):
`gravel-hub apps create --name bot --scopes identity:read,servers:read,participation:write`. In the
channel the bot needs **View Channel, Send Messages, Read Message History and Mention Everyone, or a
role that members may mention**: Discord pings a role from a bot only when the role is mentionable or
the bot may mention every role. A refusal is counted as `forbidden` and logged once. A server the
hub does not list is logged once. A Discord or hub failure is logged once while it lasts and
retried at the next pass. With no rule the module idles and says so once. Metrics:

- `gravel_bot_seeding_calls_total{server,result}` (`posted`, `forbidden`, `error`);
- `gravel_bot_seeding_sessions_total{server,outcome}` (`started`, `reached`, `window_closed`);
- `gravel_bot_seeding_recorded_seconds_total{server}`;
- `gravel_bot_seeding_record_errors_total{server}`.

### Presence (`discord/modules/presence`)

A "who's playing now" card: one message with how many members are playing each game right now
(ADR-0017). Counts only. The card never names anyone, and nothing is stored.

**The card.** The Organization settings' `discord.presence` (docs/hub.md, "Organization settings")
places it:

- `channel`: where the card lives;
- `note`: an optional line under the counts;
- `games`, up to 20, in the card's order: a `game` key (in the metrics), a `label` (on the card),
  and how Discord's activity is recognized. `activities` are names as Discord shows them, compared
  without regard to case; `application_ids` are the activities' Discord application ids. At least
  one of the two is required, and a name or id counts for one game only.

```text
## Who's playing now
**War Dogs** · 3 playing
**Counter-Strike 2** · nobody right now
Counts members who share their activity on Discord.
-# `presence` · members who share their activity on Discord · changed 4 minutes ago
```

**What it sees.** The module asks the gateway for guild presences, a **privileged intent**: turn on
**Presence Intent** under the application's Bot, Privileged Gateway Intents (a toggle under 100
guilds). It keeps a live view in memory: for each online member, the names and application ids of
what they are playing or competing in, nothing else. The view starts from the guild's presences
when the gateway delivers the guild, and follows each presence update and member leave after it.
Offline members and other activities (listening, streaming, a custom status) never enter it. Only
members who share their activity on Discord are seen at all. A member counts once per game however
many of their activities match it, and may count for two games.

**Passes.** Every `interval`, a pass reads the settings and counts the view. The card is edited
only when a count, a label or the note changed, so a quiet guild costs one hub call a pass and no
Discord call. A settings change applies at the next pass, without waiting for a presence update.
Like the server cards, the module finds its own card after a restart by the marker in its footer
and posts a new one only when there is none, or when it was deleted.

**When it can't see.** If the application isn't allowed the intent, the runtime leaves it out
rather than lose the gateway (see [A host's bot](#a-hosts-bot)), and the card says the bot can't
see what members are playing: never counts of nobody. Before the gateway has delivered the guild,
the card is left as it was. `presence.enabled` needs `discord.gateway: true`, which `config check`
enforces.

The bot needs **View Channel, Send Messages and Read Message History** in the channel; the app
needs no scope beyond reading the settings. A refusal is counted as `forbidden` and logged once; a
Discord or hub failure is logged once while it lasts and retried at the next pass. With no card
placed the module idles and says so once. Metrics:

- `gravel_bot_presence_playing{game}`: each game's count at the last pass;
- `gravel_bot_presence_members_tracked`: the live view's size;
- `gravel_bot_presence_updates_total{result}` (`posted`, `edited`, `forbidden`, `error`);
- `gravel_bot_presence_last_success_timestamp_seconds`.

### Recaps (`discord/modules/recaps`)

A recap after each session: what the server's window held, posted a few minutes after it ends
(ADR-0018).

**Rules.** The Organization settings' `discord.recaps` (docs/hub.md, "Organization settings") has
at most one rule per server, ten in all:

- `server`, the `channel` the recap is posted in, and a `title` ("Wardogs Wednesday");
- `hours`: the sessions, as seeding's weekly windows in `stats.timezone`;
- `floor`: the player count "minutes at or above" counts against (0 leaves the line out);
- `note`: a line appended to the post.

**Posts.** Every `interval`, a pass reads the settings. For each rule it takes the most recent
window that ended at least three minutes ago, so the last polls are in. If that window has no recap
yet, the module reads `GetSessionRecap` (docs/hub.md "Session recaps") and posts one message:

```text
## Wardogs Wednesday recap
**HTG WARDOGS | NA WEST | #1** · October 14, 2026 6:00 PM to 10:00 PM
Peak **24** players · **1h 35m** at 20 or more · **31** players

**Matches**
- Ozeti: Lonestar **1200** · Valkyra **900**
- Europe

**Top by kills**
1. Jo: **31** kills
2. Brave Otter: **12** kills

See you next Wednesday.

[The full recap](https://app.example.com/servers/htg-wardogs-1/recap?…)
-# recap `htg-wardogs-1` · 1791673200
```

- **Times:** Discord shows them in the reader's zone.
- **Names:** players appear as the boards show them, under a pseudonym unless the member chose their
  name. A name a game or a player chose is escaped.
- **Matches:** at most eight are listed; the page has them all. Under the floor all night reads "No
  match started."
- **Pings and previews:** the post pings no one, and the link makes no preview.
- **Nobody came:** with peak 0 and no player, nothing is posted.

**Restart.** The bot keeps no state in the hub. Before it posts, it reads the channel's 100 newest
messages for its own marker with the server and the window's start, so a restart never posts twice.
After a restart it posts what is missing, for a window that ended within the last day. A recap the
hub or Discord fails is retried every pass.

The bot needs **View Channel, Send Messages and Read Message History** in the channel. Its app needs
**`stats:read`** unless `stats.public` is on. A refusal is counted as `forbidden` and logged once.
With no rule the module idles and says so once. Metrics:

- `gravel_bot_recaps_total{server,result}` (`posted`, `nobody`, `forbidden`, `error`);
- `gravel_bot_recaps_last_success_timestamp_seconds`.

### Streams (`discord/modules/streams`)

Go-live posts: when one of the community's streamers goes live on Twitch or Kick, one post in a
channel, edited when the stream ends (ADR-0019).

**Streamers.** The Organization settings' `discord.streams` (docs/hub.md, "Organization settings")
places the posts:

- `channel`: where they go;
- `role`: an opt-in role each new post pings (never the guild's `@everyone`); empty pings no one;
- `note`: an optional line under each post;
- `streamers`, up to 10: a `platform` (`twitch` or `kick`), the channel's `login` as in its URL
  (`hiddentoken` for twitch.tv/hiddentoken) and an optional `label` the post shows instead.

**The applications.** Each platform is asked with an app access token from the host's own
developer application, whose credentials are bot.yaml's `streams.twitch` and `streams.kick`. On
Twitch, register an application at dev.twitch.tv (any OAuth redirect URL; the bot never uses one)
and take its client id and a new secret. On Kick, create an app under the account's developer
settings. A platform without an application is skipped, and its streamers logged once. The token
reads public stream metadata and nothing about viewers.

**Passes.** Every `interval`, a pass reads the settings and asks each platform which of its
streamers are live: one call per platform. A streamer who went live gets a post:

```text
🔴 **Hidden Token** is live on Twitch
**Season 02 preview** · War Dogs
https://www.twitch.tv/hiddentoken
Grab the Stream role in #roles to be pinged.
@Stream
-# stream twitch `hiddentoken` · 312345
```

When the stream ends, the post is edited to "**Hidden Token** was live on Twitch · ended 2 minutes
ago". The ping goes, the link's preview is suppressed, and the edit pings nobody. A stream that
drops and comes back within ten minutes edits its post back to live instead of posting and pinging
again. Titles and names a streamer chose are escaped.

**Restart.** The bot keeps no state in the hub. The first pass reads the channel's 100 newest
messages for its own markers (the stream's platform, login and id), so a stream announced before a
restart is not announced again, and one that ended while the bot was away is marked ended. Twitch
gives each stream an id. Kick gives none, so its id is the stream's start time.

**Failures.** A platform that fails, refuses the application or rate-limits the bot leaves its
streamers' posts as they are until it answers again. A 429 waits out the platform's
`Retry-After` (Twitch's `Ratelimit-Reset`), a minute when it names none, at most 15. A refused token
is replaced once in the same pass. A post Discord fails is retried next pass. If the stream ended in
between, nothing is posted.

The bot needs **View Channel, Send Messages and Read Message History** in the channel, and **Mention
@everyone, @here and All Roles** only if the role isn't mentionable. Its app needs no scope beyond
reading the settings. With no streamer listed the module idles and says so once. Metrics:

- `gravel_bot_streams_live{platform}`: the listed streamers live at the platform's last answer;
- `gravel_bot_streams_polls_total{platform,result}` (`ok`, `throttled`, `unauthorized`, `error`);
- `gravel_bot_streams_posts_total{platform,result}` (`posted`, `resumed`, `ended`, `forbidden`, `error`);
- `gravel_bot_streams_last_success_timestamp_seconds`.

### News (`discord/modules/news`)

A game's announcements from Steam, posted in a channel: patch notes in `#cs-news`, say.

**Feeds.** The Organization settings' `discord.news.feeds` (docs/hub.md, "Organization settings")
lists them, at most 20. Each is one game into one channel:

- `app_id`: the game's Steam app id, as in its store URL (730 for Counter-Strike 2);
- `channel`: where the posts go;
- `label`: the game's name in each post's footer, one line of at most 64 characters;
- `feeds`: Steam's feed names to read, `steam_community_announcements` (the game's own
  announcements) when left out;
- `color`: the embeds' color as `0xRRGGBB`, Steam's blue (`0x66c0f4`) when left out.

```yaml
discord:
  news:
    feeds:
      - app_id: 730
        channel: "1556656220243239072"
        label: Counter-Strike 2
        color: 0xde9b35
```

A game may post to several channels, and a channel may carry several games, but each pair once.
bot.yaml's `news` has only `enabled` and `interval`. A settings change applies at the next pass: a
feed removed is forgotten, and a feed added (or added back) starts as on a first look, below.

**Passes.** Every `interval`, the module reads the settings, then asks Steam's public news API
(`ISteamNews/GetNewsForApp`, no key) for each feed's ten latest items, keeps those of the feed's app
and feeds, and posts the ones the channel lacks: oldest first, at most five a pass, the rest at the
next. Each post is one embed:

- the title, linked to the store's page for it
  (`https://store.steampowered.com/news/app/<app>/view/<gid>`);
- the notes, from Steam's BBCode into Discord Markdown: headings, bold, italics, strike-through, links,
  lists and table rows. Media is dropped, any other tag is stripped, and the text stays literal (a
  `*` or a `# ` in patch notes is escaped). Notes over 1800 characters are cut at a line and end with
  "… [Full notes on Steam](…)";
- the first image, with Steam's `{STEAM_CLAN_IMAGE}` filled in;
- the item's date, the feed's color and the footer "`<label>` · Steam announcement".

A post pings no one. This is the look of the Cloudflare Worker that fed Hidden Token Gaming's
`#cs-news` before, so a channel the module takes over stays uniform.

**What counts as posted.** The bot keeps no state in the hub. Before it posts, it reads the
channel's 100 newest messages. Every announcement linked from an embed there
(`/news/app/<app>/view/<gid>` with the feed's app) counts as posted, **whoever posted it**: the bot,
a webhook or a member whose link Discord unfurled. So a bridge the module replaces is never
repeated. The first look at a feed after the bot starts, or after the feed is added, sets where it
begins:

- with the game's announcements in the channel, only items newer than the newest of them are
  posted;
- with none, only items from the last 24 hours.

Nothing older is posted while the process runs, so turning the module on never floods a channel with
history, and an item a bridge skipped stays skipped. The bot also remembers what it posted while it
runs, so a busy channel that pushes a post past the newest 100 doesn't get it again. After a
restart only the newest 100 count.

**Failures.** A post Discord refuses or fails is not in the channel, so it is tried again at the
next pass; the feed's later items wait behind it, which keeps the channel in order. Steam failing is
retried next pass. A 429 from Steam stops every feed until its `Retry-After` (a minute when it names
none, at most 15). A hub it can't read leaves everything for the next pass. Each failure is logged
once while it lasts.

The bot needs **View Channel, Send Messages, Embed Links and Read Message History** in the channel.
Its app needs no scope beyond reading the settings. With no feed listed the module idles and says
so once. Metrics:

- `gravel_bot_news_posts_total{app,result}` (`posted`, `failed`);
- `gravel_bot_news_last_success_timestamp_seconds{app}`.

### Match results (`discord/modules/matchresults`)

Each finished match posted in a channel: the series a server's match plugin reported (MatchZy on
Counter-Strike 2, ADR-0024), a PUG's result in `#results`, say.

**Settings.** The Organization settings' `discord.match_results` (docs/hub.md, "Organization
settings"):

- `channel`: where the results go;
- `servers`: the server ids whose results are posted, at most 50; left out, every server's.

```yaml
discord:
  match_results:
    channel: "1556656220243239080"
    servers: [htg-cs2-1]
```

bot.yaml's `match_results` has only `enabled` and `interval`. A settings change applies at the next
pass.

**Passes.** Every `interval`, the module reads the settings. With a channel set, it asks the hub's
`ListMatchResults` for the series that ended in the last 24 hours (one call per listed server, or
one for every server) and, for each one the channel lacks, reads it in full with `GetMatchResult`
and posts one message: oldest first, at most five a pass, the rest at the next.

```text
## Lonestar 2–1 Valkyra
**Lonestar** wins
de_nuke 13–9
de_inferno 9–13
de_mirage 16–14
**Top by kills**
1. Jo: **35** kills
2. Brave Otter: **30** kills
3. Sam: **29** kills
[Full result](https://app.example.com/results/…)
-# result `0e4c…`
```

- **Message:** one Components V2 container. The winner line reads "A draw" when the series was
  drawn.
- **Maps:** in the order played, with each team's rounds.
- **Top by kills:** the top three across every map of the series. Players appear as the hub's result
  page shows them: under a pseudonym unless the member chose their name.
- **Names:** a team's, a map's or a player's name is escaped, so it is never read as Markdown, a
  mention or a link.
- **Link:** to the hub's result page, when `hub.public_url` is an http(s) origin.
- **Pings:** the post pings no one.

**What counts as posted.** The bot keeps no state in the hub. Before it posts, it reads the
channel's 100 newest messages for its own markers (``-# result `<id>` ``; another author's copy
doesn't count), so a restart never posts twice. The first look at a channel after the bot starts
sets where it begins:

- with results of the bot's in the channel, only series that ended after the newest of them are
  posted, which is what was missed while the bot was away;
- with none, only series that end after the module started.

Nothing older is posted while the process runs, so turning the module on never floods a channel with
history. The bot also remembers what it posted while it runs, so a pass with nothing new costs no
Discord call.

**Failures.** A post Discord refuses or fails is not in the channel, so it is tried again at the next
pass; later results wait behind it, which keeps the channel in order. The hub failing leaves
everything for the next pass. Each failure is logged once while it lasts.

The bot needs **View Channel, Send Messages and Read Message History** in the channel. Its app needs
**`stats:read`** while the settings keep stats private (`stats.public` off): the results are read
like the boards (ADR-0024 §4). A refusal is logged once, naming the scope. With no channel set the
module idles and says so once. Metrics:

- `gravel_bot_match_results_total{result}` (`posted`, `error`);
- `gravel_bot_match_results_last_success_timestamp_seconds`.

### Moderation (`discord/modules/moderation`)

The moderation slash command, `/<moderation.command>` (`/mod` by default), with a subcommand per
action: `kick player reason`, `ban player reason`, `unban player [reason]`, `message player text`
and `broadcast text`, each with an optional `server` (needed only when the hub has more than one).
A player is an in-game name of someone on the server (an exact match, else the one whose name
contains what was typed), or their id at the game's identity provider (a SteamID64), which is how
to ban someone who isn't on and the only way to unban. Reasons and messages are capped at 256
characters, the game's limit.

Nothing is sent until the moderator confirms. The command answers privately with exactly what will
be sent and Confirm and Cancel buttons; only the moderator who ran it can press them, within two
minutes, once. On Confirm the bot calls the hub's `ModerationService` with its app credential and
`on_behalf_of` the moderator's Discord account. **The hub decides:** the account must be linked to
the hub's owner or a moderator (ADR-0013), or the call is refused and the moderator is told to link
their account and ask for the role. Every confirmed action is in the hub's audit log, beside the
web's. The command is hidden from members without Moderate Members (Discord's default member
permissions), which a server can change per role. The hub's check is the one that counts.

The bot's app needs **`servers:read`** (who is on, to resolve names) **and `servers:moderate`**.
Names and reasons are escaped, so a player called `@everyone` pings no one. Metric:
`gravel_bot_moderation_actions_total{action,result}` (`ok`, `refused`, `failed`, `cancelled`,
`expired`).

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
startup error.

**Gateway intents.** The runtime asks the gateway for guilds and guild members (`Options.Intents`
replaces that base). A module that needs more implements `bot.IntentsModule`
(`Intents() gateway.Intents`), and the runtime asks for the union. For a **privileged** intent a
module adds (presences, members, message content), the runtime first reads the application's
flags, once at start. If the application isn't allowed the intent in the Developer Portal, the
intent is left out, `gravel_bot_gateway_intent_missing{intent}` is 1, and an error names the
toggle. Discord would otherwise close the whole session (4014), so one module's missing toggle
costs that module and nothing else. A module checks what it got with `Registry.HasIntent`. When
the flags can't be read, the intents are asked for as configured.

`hubclient.New` gives a module the hub's API as the app: `Identity()` and
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
| `gravel_bot_jobs_total` | counter | `job` (`reconcile`, `register-metadata`, `update`, a host's own; stored by Prometheus as `exported_job`, since the scrape's own `job` wins), `result` (`ok`, `error`) | background jobs that ended | until a job ends |
| `gravel_bot_rolesync_passes_total` | counter | `result` (`ok`, `idle` = no mapping, `error`) | role sync passes | role sync off |
| `gravel_bot_rolesync_changes_total` | counter | `action` (`add`, `remove`), `result` (`ok`, `dry_run`, `forbidden`, `gone`) | role changes made or, in a dry run, logged | role sync off, or until the first change |
| `gravel_bot_rolesync_last_success_timestamp_seconds` | gauge | | when the last pass finished without error (an `idle` pass counts) | until the first pass |
| `gravel_bot_linked_roles_schema_registered` | gauge | | 1 once the Linked Roles schema is on the application | Linked Roles off |
| `gravel_bot_servercards_updates_total` | counter | `result` (`posted`, `edited`, `forbidden`, `error`) | card messages posted or edited, and the failures | server cards off, or until the first update |
| `gravel_bot_servercards_last_success_timestamp_seconds` | gauge | | when the last pass brought every card up to date (an idle pass counts; an unknown server does not) | server cards off (0 until the first such pass) |
| `gravel_bot_seeding_calls_total` | counter | `server`, `result` (`posted`, `forbidden`, `error`) | seeding calls posted, and the refusals and failures | seeding off, or until the first call |
| `gravel_bot_seeding_sessions_total` | counter | `server`, `outcome` (`started`, `reached`, `window_closed`) | seeding sessions opened by a call and how they ended | seeding off, or until the first session |
| `gravel_bot_seeding_recorded_seconds_total` | counter | `server` | seeding time recorded as participation, every player together (a duplicate not counted) | seeding off, or until the first recording |
| `gravel_bot_seeding_record_errors_total` | counter | `server` | failed attempts to record seeding minutes (retried every pass; an invalid event is dropped) | seeding off, or until the first failure |
| `gravel_bot_gateway_intent_missing` | gauge | `intent` (`guild_presences`, `guild_members`, `message_content`) | 1 for a privileged intent a module asks for and the application isn't allowed, left out of the session; 0 when allowed | no module adds a privileged intent, or the gateway is off |
| `gravel_bot_presence_playing` | gauge | `game` | members Discord shows playing the game, at the last pass | presence off, no card placed, the bot can't see presences, or the guild not delivered yet |
| `gravel_bot_presence_members_tracked` | gauge | | members in the live view: online and playing anything | presence off |
| `gravel_bot_presence_updates_total` | counter | `result` (`posted`, `edited`, `forbidden`, `error`) | presence card posts and edits, and the failures | presence off, or until the first update |
| `gravel_bot_recaps_total` | counter | `server`, `result` (`posted`, `nobody`, `forbidden`, `error`) | session recaps posted, windows nobody came to, and the failures | recaps off, or until a window's recap |
| `gravel_bot_recaps_last_success_timestamp_seconds` | gauge | | when the last pass that left no recap failing completed (an idle pass counts) | recaps off (0 until the first such pass) |
| `gravel_bot_presence_last_success_timestamp_seconds` | gauge | | when the last pass brought the card up to date (an idle pass counts; waiting for the guild doesn't) | presence off (0 until the first such pass) |
| `gravel_bot_streams_live` | gauge | `platform` | the listed streamers live at the platform's last answer | streams off, or until the platform first answers |
| `gravel_bot_streams_polls_total` | counter | `platform`, `result` (`ok`, `throttled`, `unauthorized`, `error`) | calls asking a platform who is live | streams off, or until the first call |
| `gravel_bot_streams_posts_total` | counter | `platform`, `result` (`posted`, `resumed`, `ended`, `forbidden`, `error`) | go-live posts, their edits, and the failures | streams off, or until the first post |
| `gravel_bot_streams_last_success_timestamp_seconds` | gauge | | when the last pass in which every platform answered and every post went through completed (an idle pass counts) | streams off (0 until the first such pass) |
| `gravel_bot_news_posts_total` | counter | `app` (the Steam app id), `result` (`posted`, `failed`) | Steam announcements posted, and the posts Discord refused or failed (retried next pass) | news off (0 for each listed app from its first pass) |
| `gravel_bot_news_last_success_timestamp_seconds` | gauge | `app` | when the last pass in which Steam answered, every channel of the app was read and every post went through completed | news off, or until the app's first such pass |
| `gravel_bot_match_results_total` | counter | `result` (`posted`, `error`) | match results posted, and the failures of the hub, the channel or a post (retried next pass) | match results off (0 from the start) |
| `gravel_bot_match_results_last_success_timestamp_seconds` | gauge | | when the last pass in which the hub answered, the channel was read and every post went through completed (an idle pass counts) | match results off (0 until the first such pass) |

## Tests

`go test ./discord/...`: the configuration (defaults, strict keys, secret precedence, validation),
the runtime over a signed PING and a slash command delivered over HTTP against a fake Discord API
and a fake hub, the hub client's token fetch, refresh and retry, and the core module's replies.
Role sync is tested as a plan (who gets what) and as passes against a fake Discord REST API and a
fake hub: changes applied and settled, a refused role, a member who left, a 429 waited out, a
refused member list, a dry run, paging past a thousand members, and the loop following the
identity log; Linked Roles against a fake metadata endpoint, idempotent and retried. The server
cards are tested as rendering (live, degraded, unobserved, escaped names) and as passes against a
fake channel API and a fake hub: posted once, then edited in place only on a change, found again
after a restart without a duplicate, posted again after a deletion, a forbidden channel and an
unknown server counted, and no mention allowed. Seeding is tested as its windows (weekdays in a
zone, both daylight-saving changes, overnight, overlaps) and as passes on a fake clock against a
fake channel API and a fake hub: calls only inside a window, the cooldown, `call_at_start`, the
floor and the window's end editing the same message, per-player seconds, recording once with retries
and an invalid event dropped alone, restarts that resume or close a call, and a call that pings only
its role. The intents are tested against a fake application endpoint: allowed, allowed under 100
guilds, refused (left out, the bot still builds), unreadable, the gateway off, and the base never
checked. Presence is tested as rendering (counts, nobody, blind, escaped text) and as passes against
a fake channel API with the runtime's own intent check: offline members, non-game activities,
streaming and duplicates not counted, matching by name and by application id, an unchanged count
costing no Discord call, one edit in place on a change, a settings change applied without an event,
waiting for the guild, the card saying it can't see without the intent and showing counts after the
toggle and a restart, finding its card again, and every gateway event the view follows. Recaps are
tested as the last ended window (an overnight window, the daylight-saving change) and as passes on a
fake clock against a fake hub and a fake channel API: nothing before the window ends plus the grace,
one post with the right window, floor, numbers, matches, top and link, no second post on the next
pass or after a restart, another author's marker ignored, no post when nobody came, under the floor
all night, retries after a hub failure, a forbidden channel, the catch-up limit, and escaping. News is
tested as rendering, with every case of the Worker it replaces (the BBCode mapping, literal text, the
cut with its link, the first image and the clan placeholder) against Steam's real answers, and as
passes on a fake clock against a fake hub, a fake Steam and a fake channel API: no backlog on a
fresh channel, only the last day after a restart, nothing older than the channel's newest when
taking over from a webhook, oldest first, five a pass, a failed post retried in order, a Steam error or a cut answer
posting nothing, another app's or feed's items ignored, a member's unfurled link counted and another
app's not, a 429 waited out for every feed, a forbidden channel logged once, two feeds in their own
channels, a feed removed and added back starting afresh with the settings' label and color, idle with
no feed, the hub down, and the metrics. Match results are tested as rendering (the title, the
winner or a draw, the maps in order, the top three across maps with ties, the link, escaped team,
map and player names) and as passes on a fake clock against a fake hub and a fake channel API: a
finished result posted once and a running one not, no Discord call when nothing is new, no repost
after a restart and the missed ones posted, another author's marker ignored, no history on a first
look, the servers filter, five a pass oldest first, a forbidden channel logged once and retried, the
hub refusing the bot, idle with no channel, and the metrics. The gateway itself is disgo's and is not driven in tests.
