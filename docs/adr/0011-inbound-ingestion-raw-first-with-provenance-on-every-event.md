# ADR-0011: Inbound ingestion stores every batch raw before it parses, and every event carries its provenance

**Status:** accepted (2026-10-21, John; built in #99) · **Changes:** how a source that pushes into the hub is received, authenticated and kept; what a stats event records about where it came from; how missing data is recorded · **Tracked by:** #4 (ingestion) and #3 (provenance); #17 (the store and boards it feeds); hidden-token-gaming/deploy#49 (S1, the first feed), wardogs-server#12 (the config that turns it on)

## Context

The README's stats design is outbound: an adapter dials the hub over SPIFFE mTLS, signs its batches and keeps a durable local queue, so the hub can always re-ask (README, "Stats adapters"). Three of Hidden Token Gaming's sources do the opposite. They push into the hub over HTTP, from machines gravel does not run, and two of them never retry (#4):

- War Dogs' `[WDServerFeed]` posts kill batches of 1 to 10 events about every 2 s to `<Url>/api/ingest/events` with `Authorization: Bearer <Token>`. It does not retry. The per-event fields (`eventId`, an optional `killerSteamId` and `distance`) come from community captures, never from a primary source. The section applies at the server's next restart.
- Counter-Strike 2's `logaddress_add_http` sends `text/plain` log lines with `X-Server-Instance-Token` and `X-LogBytes-BeginOffset`/`EndOffset`. MatchZy's remote log sends Get5-style JSON and doesn't retry either. Both arrive with P4.

Principle 4 ("game-server logs are authoritative") holds for one of HTG's five games (#3). War Dogs has no log access, PUBG's stats come from the publisher's API, Muster's participation is first-party, and Sea of Thieves and Star Citizen can only be self-reported. A board that mixes those without saying which is which can't be trusted, and a pipeline that loses a batch without saying so under-counts silently.

Two more constraints. The feed's format is unverified: S1 (deploy#49) is meant to capture real batches, and it needs a sink to capture into. And a parser written today will be wrong for some build, because War Dogs changes every few weeks (ADR-0010).

## Decision

1. **Raw first.** The ingest route commits each authenticated request to an append-only table, `ingest_batches`, before it answers 2xx. The row holds the server, the source kind, `received_at`, the body bytes (capped at the source's own limit, 64 KiB for War Dogs), the headers the source defines (CS2's offsets and instance token, nothing else), and a content hash. Parsing happens after the answer, in a hub job (`ingest_parse`, ADR-0010's job runner) that drains unparsed batches. A parser bug or a new game build therefore loses nothing: the batch is kept, marked `failed`, counted, and parsed again by the fixed parser. The same table is S1's capture store. The first real batches are read from it, redacted, and become #4's fixtures before the War Dogs parser is final. A source that doesn't retry is only as durable as this one insert, so the route does nothing else before it answers.

2. **One shared route; the adapter only parses.** `internal/ingest` serves `POST /api/ingest/events`, the exact path the War Dogs game appends, on the hub's public listener (HTG routes `ingest.hiddentoken.com` to it through the Tunnel). It handles authentication, the body cap, a per-server rate limit, the raw insert and the metrics. An adapter (`adapters/wardogs`, using `games/wardogs`) implements `ingest.Parser`: it turns a batch into events, each with its **source key**, the identity the source gives it (War Dogs `eventId`; CS2 instance token plus byte range; MatchZy match, round and event). CS2's and MatchZy's routes and parsers come with P4; their cases are written now as design tests of the interface (overlapping and missing byte ranges, a new instance token after a restart), so the interface fits them before they exist.

3. **A per-server feed token, from a file, like the RCON credential.** `servers.yaml` gains an optional `feed` section per server: `url` (the origin the server posts to, `https://ingest.hiddentoken.com`) and `token_file` (a podman secret the hub reads at start and on change, never stores, ADR-0010 §1). The route finds the server by comparing the bearer in constant time against each feed token. An unknown token answers 401 with no body and is counted. Nothing in the answer says which servers exist. The War Dogs driver now *writes* `[WDServerFeed]` (`Url`, `Token`) from the manifest on `PlanServerConfig`/`ApplyServerConfig`, where today it copies the server's section. The hub owns that section the way it owns the RCON block. The section applies at next restart, so a rotated token reaches the server only at the host's daily restart. Until then the hub accepts the previous token too: `token_file` may hold two lines, new then old, and the old line is removed once the build watcher sees the restart (health `uptimeSeconds` resets) or after 48 hours, whichever comes first.

4. **Provenance on every event (#3).** The stats event #17 stores carries, as columns and not in its extensions map:
   - `source`: one of `server_adapter`, `platform_api`, `first_party`, `self_report`.
   - `server_id` and `game`.
   - `source_key`, unique per server and source; a second copy is dropped and counted, never stored twice.
   - `observed_at` (the game's time when it has one) and `received_at`.
   - `trust`: the server's trust **when the event was received**, copied in, never joined. Trust is not retroactive (principle 9): promoting a server to Official changes what its next events feed, not its history.
   - `batch_id`: the raw row it came from.
   - `schema_version`.

   A **trust mapping**, one function in the stats package, decides which boards an event may feed:
   - `server_adapter` and `platform_api` events from Official servers feed the combat boards, global ones included.
   - The same events from Community servers feed that server's boards only.
   - `first_party` events feed participation boards.
   - `self_report` events feed nothing until a reviewer approves them in the unified queue (README, "Trust, leaderboards & moderation"), and then only profile badges.

   Board queries call the mapping and never filter on `source` themselves.

5. **Missing data is a row, not a shortfall.** A gap is a stats event of kind `gap`, with a reason, a window (`from`, `to`) and, where it is known, a count. Boards keep their numbers and the server and game pages show the gaps beside them (#3, #18). Three detectors for War Dogs:
   - **`reconcile`:** the poll (ADR-0010 §3) already reads each player's cumulative kills and deaths for the match. When the counts the feed delivered for a player fall behind the poll's, a gap records the difference, and on a match end the poll's totals are the record for that match. That is the "reconcile against polled state" half of the restated principle.
   - **`hub_down`:** at start, the hub writes a gap for every server with a feed from when it stopped to now, if the server had players as it stopped (amended 2026-10-19: from the last batch it stored, until a feed with no heartbeat showed why not).
   - **`silence`:** no batch for 10 minutes while the poll reports at least 10 players. This is weaker evidence, because a quiet match is possible, so it is a gap with no count.

   CS2's byte-offset holes are the P4 detector, written now as a design test.

6. **Principle 4 is restated** in the README: game-server logs are authoritative **where logs exist**. Otherwise the hub reconciles against polled state and records the gaps explicitly. The at-source signing and the durable spoke queues of the README stay the design for adapters gravel runs (agents, the operator). A pushed feed from a third-party server can't sign, so its provenance is the per-server token, the source kind and the trust the server had when the event was received.

7. **Raw batches are a buffer, not the record.** They hold SteamIDs, so they are identity-linked data. They are kept 30 days, long enough to re-parse after a parser fix or a game update, and the `ingest_prune` job deletes older ones. Events are the record and follow #17's retention (the 13-month placeholder). An erasure (principle 8) re-keys events as #17 defines it. Raw batches inside the 30 days are rewritten with the same re-key. The 30 days and the erasure path go to counsel with the rest of the retention questions.

## Consequences

- **Migrations.** Migration 9 adds `ingest_batches`. The events table with its provenance columns is #17's first migration, and #3 lands in it, not separately.
- **Configuration.** `hub.yaml` gains nothing: the feed is per server in `servers.yaml`. The route is on the public listener the hub already serves.
- **Metrics,** each on the hub dashboard's Servers row in the same PR (ADR-0009):
  - `gravel_ingest_batches_total{server,source,result}` (`stored`, `unauthorized`, `too_large`, `rate_limited`)
  - `gravel_ingest_parse_total{server,source,result}` (`parsed`, `failed`)
  - `gravel_ingest_events_total{server,kind}`
  - `gravel_ingest_duplicates_total{server}`
  - `gravel_ingest_gaps_total{server,reason}`
  - `gravel_ingest_last_batch_timestamp_seconds{server}`
- **Order.**
  1. The raw route, the token, the driver writing `[WDServerFeed]`, the parse job as a no-op, and pruning. One PR.
  2. HTG turns the feed on (a new secret, `servers.yaml`'s `feed`, `ApplyServerConfig`, then the host's daily restart) and captures real batches. That is S1, deploy#49, including its 10-minute sink-down test.
  3. The War Dogs parser from those captures, with the events table, provenance, dedup and the gap detectors (#4, #3, #17's first migration).
  4. The boards (#17).
- **Releases.** The first PR is a patch release. HTG needs it deployed before S1 can capture anything, so it is released on its own when S1 is scheduled, not per PR.
- **Applying the config.** Turning the feed on is a config apply. Until wardogs-server#12 keeps the document in git, the owner calls `ApplyServerConfig` with the live document. The plan shows the feed section as the only change.
- **Exposure.** The ingest route is reachable from the internet. Its answers carry no server list and no error text. A wrong token costs a constant-time compare per configured server, and the per-IP rate limit (`internal/ratelimit`) bounds it.

## Amendments

- **2026-10-10, decisions.** John kept both open values for now: raw batches are kept 30 days, and a
  `silence` gap is 10 minutes with no batch while the poll reports 10 or more players.
- **2026-10-10, the raw route as built (step 1).**
  - **Middleware.** The route is mounted ahead of the session and app middlewares. The app
    middleware answers 401 to any bearer that is not an app token, so it would refuse every feed
    batch. The route keeps the request id, client address, recovery and access log.
  - **Rate limits.** The per-address limit charges only requests with no valid token (10 a minute,
    then 429). A host that runs many servers posts from one address, and their good batches are
    limited per server instead (600 a minute).
  - **Rotation grace.** It runs 48 hours from when the hub first reads a second line in the token
    file, not from a restart the watcher detects: the poll does not read the server's uptime.
  - **Feed tokens** are read every 30 s (job `ingest_feeds`), as the monitor reads credentials.
  - **The parse job comes with the parser** (step 3). Batches wait as `pending` until then, which
    is how S1 reads them.
  - **`gravel-hub ingest export --server ID`** prints them as JSON lines for that.
  - **Migration 9** adds `ingest_batches` and the `feed` column on `managed_servers`.
- **2026-10-11, the log route as built (#36).** A source that cannot send an `Authorization`
  header (CS2's `logaddress_add_http`) posts to `POST /api/ingest/log/<token>`.
  - **The token in the path** needs guards the bearer route does not: the access log writes the
    path as `/api/ingest/log/<redacted>` (`httpx.SetLogPath`), and a log source's token is refused
    on the bearer route.
  - **Pinned to the host.** A batch is accepted only from the IPv4 of the server's own running
    instance (not its reserved address, which is inbound only); anything else is 401, counted as
    `address_mismatch`.
  - **Redacted before it is stored.** CS2's startup cvar dump can carry `sv_password` and the
    RCON password, and the log can echo the token; the driver's redaction runs before the insert,
    so the raw-first rule keeps a redacted batch.
  - **Provenance:** the headers the source defines are stored with the batch (CS2: instance token,
    byte offsets, ticks, timestamp, map and score), with the body at most 256 KiB.
  - **The tap.** After the insert, the batch is handed to the server's driver, whose roster is how
    a CS2 server's players are known; the parser for stats events (#4) is still to come.
- **2026-10-18, the first real feed (Season 02, CL-516586) and the parser as built (step 3, #4).**
  - **The feed moved.** Season 02 removed `[WDServerFeed]`: the feed is the RCON section's
    `bEventFeedEnabled`, `EventFeedEndpoints` and `EventFeedServerName`, set in the server's own
    file only, so the driver writes no feed on such a build and its plan says what Url to set.
    The server **posts to an endpoint's Url as given**; CL-509546 appended `/api/ingest/events`.
    HTG's first try (an origin) posted to `/` and was refused 401; the Url now carries the path.
  - **The shape** (`games/wardogs/feed.go`): a batch is `{serverId, serverName, events[]}`,
    `serverId` the host's id for the server and `serverName` the file's `EventFeedServerName`. An
    event has `eventId`, `type` and `eventTime`, the game's clock in seconds (0.132 at the start),
    not a wall-clock time, so an event's `observed_at` is its batch's `received_at`. The first
    batch carried two `match_state` events (`mapName`, `state`: `MatchState.PreMatch`, then
    `MatchState.PreMatch.WaitingForWorldLoad`) at the server's start with nobody on. **The feed
    posts only when it has events**: after that start the hub received nothing for hours while
    the server stood empty. A kill's fields are not known yet.
  - **`silence` stays 10 minutes while the poll sees 10 or more players** (John's 2026-10-10 value,
    kept 2026-10-19). On 2026-10-18 it was briefly one player or more, on the belief that the feed
    posted every two minutes regardless; the hub's own counters showed it does not (4 batches in
    the 6 hours after the feed went live, all at the server's starts). With no heartbeat, a few
    players idling below the match's minimum post nothing and would raise false gaps; with ten
    or more a match runs and kills come every few seconds. The gap runs from the last batch to
    the next one and is written when that one arrives; a silence still open when the hub stops is
    left to `hub_down`.
  - **`hub_down` uses the poll's occupancy.** "The server had players then" is a
    `server_occupancy` row (ADR-0018) for the minute of the last batch before the start or the
    one before it. The hub-down window is that batch to the start.
  - **The parse job** (`ingest_parse`, every 10 s) reads one source's `pending` batches with that
    source's `ingest.Parser` (a batch's source is its server's game; `adapters/wardogs` for War
    Dogs). Sources with no parser, such as CS2's log, which its driver reads as it arrives, stay
    `pending` and are out of its way (the pending index is per source). A batch, its events and
    its new state are saved in one transaction. A batch the parser can't read is `failed` and
    counted.
  - **Unknown kinds stay in the batch.** An event of a type the parser does not know is counted
    (`gravel_ingest_events_total{kind="unknown"}`) and logged once per type, and the batch is
    still `parsed`. The batch records the parser version that read it (`parser_version`). When a
    parser's version rises, every stored batch of its source, parsed or failed, goes back to
    `pending` at the next start, and the source keys make reading it again safe. A duplicate found
    on a re-read is not counted as one the source sent. This is how the kills will be read: the
    first session with players is captured with `gravel-hub ingest export --redact` (the
    recorder's stand-ins, across the export), its batches become the adapter's fixtures, and the
    next parser version reads every batch still inside the 30 days.
  - **`stats_events`** (migration 36) carries §4's columns and up to two players, each a provider
    identity: the one the event is about (a kill's killer, `provider`/`subject`) and the other
    (its victim, `target_*`). Either is absent for an event of nobody (a match state, a gap). A
    gap is `kind = 'gap'`, its window `observed_at` to `extensions.to`, its `extensions.reason`.
  - **Erasure and retention.** `stats erase` re-keys either player of an event in one statement.
    Past `stats.raw_retention_months` the rollup job anonymises each identity on each event with
    a token of its own, as #216 does for match results (principle 10), rather than deleting the
    events; events of nobody are kept as they are.
  - **Not yet:** the `reconcile` detector (built 2026-10-20, below) and the trust mapping's first
    event-fed board need the kills; gaps on the server and game pages wait for #18's pages (shown since, below); CS2's and MatchZy's parsers
    come when their events are wanted as stats.
- **2026-10-19, after Hidden Token Gaming's first start on 0.10.1.**
  - **A false `hub_down` gap.** The upgrade's restart wrote a gap of 1029 minutes, from the last
    batch (the day before, 19:18Z, one player on) to the start. The poll had seen nobody on for
    the 16 hours before the restart, and nothing was lost. A window from the last batch, and
    players asked for at that batch, are only right for a feed that posts all the time; this one
    posts only when it has events.
  - **The hub keeps its runs** (`hub_runs`, migration 37): a row per start and the last minute it
    was known to run, moved every minute (job `hub_alive`), so the run before a start says when
    the hub stopped, crash or not. `hub_down` now runs from that stop to the start, when the poll
    saw anyone on as it stopped (`server_occupancy` for that minute or the one before), for a
    server whose feed had posted before. A hub with no previous run recorded writes no `hub_down`
    gap: it cannot say when it was away. Migration 37 deletes the `hub_down` gaps 0.10.1 wrote;
    none can be told true from false now, and a gap names no player.
  - **A batch read again refreshes its own events.** The source keys kept a re-read safe, but
    they also kept an event's old fields: a parser that reads a new field of a known kind would
    never see it stored. Now an event that an earlier reading of the same batch stored takes the
    newer reading's kind, time, players and extensions (never its trust, nor when it was received),
    and is not counted again. The same key from another batch, or twice in one batch, is still a
    duplicate and changes nothing.
  - **Joins and leaves** (feed version 2): the first session with a player on carried
    `player_joined` and `player_left` (`playerSteamId`, `playerName`, `playerId`, `players` after
    the change, `matchId`, `mapName`), and `match_state` carries `matchId` from the wait for
    players on. The stats events are the player by SteamID with the map, the match and the count;
    the name and the game's own id are not kept. `--redact` now replaces the game's ids for a
    player beside a SteamID too (`playerId` and its kin), since the fixtures are public.
- **2026-10-20, the feed retries, and the kills (Hidden Token Gaming's sink-down test, deploy#49).**
  - **War Dogs queues and retries.** With HTG's tunnel answering 503 on the ingest route for nine
    minutes (12:25:33–12:34:33Z) and the hub up, a player joined and left. The server held both
    events (`GET /v1/feed`: `queued` 2, `healthy` false), retried a few times quickly and then about
    once a minute (12 failures by the end), and dropped nothing (`droppedTotal` 0). It posted both
    in one batch 19 s after the route came back, each with the `eventTime` it happened at (2 min
    29 s apart, which the poll's minutes agree with). So the Context's "It does not retry" was
    wrong for this build, and an outage of the hub costs War Dogs a delay, not data, up to however
    much the server holds (not measured: two events were all the test had). The 0.10.1 export did
    not scrub `playerId`; the fixtures were scrubbed again by main's `record.Scrubber`.
  - **A game's spec says what its feed does** (`feed.retries`, `feed.kills` in `game.yaml`; War Dogs
    both). `hub_down` is recorded only for a feed that does not retry.
  - **`dropped`** replaces it as the loss War Dogs can have: the server's own count of events it
    gave up on (`droppedTotal`), read by the driver with the status at most once a minute
    (`drivers.Status.Feeds`, the endpoint whose host is the hub's feed URL's). The gap job compares
    each reading with the last, kept in `feed_delivery` (migration 38) so a drop while the hub was
    away is seen at its next start; a rise is a gap from the last reading to this one with how
    many (`extensions.events`, and `gravel_ingest_dropped_events_total`). A count below the kept
    one is the server's restart, and all it counts since is lost.
  - **`reconcile`, as built.** For a game whose feed posts every kill, five minutes after a poll
    match ends (ADR-0012's `matches`; the game's own match id is not linked to it), each player's
    kills and deaths from the feed's `kill` events between the match's start and five minutes past
    its end against `match_stats`. A kill counts for its player unless that is its victim (a
    suicide is a death only), a death for its victim, the world's kill too. Where the feed falls
    short, one gap for the match, its window the match, with the poll's match id and the missing
    kills and deaths. The five minutes past the end can count a kill of the next match, so it can
    hide a shortfall but never invent one. A match that ended more than six hours before is not
    reconciled (a hub away longer). On a match's end the poll's totals stay the record (§5), so
    nothing else changes.
  - **The kills (feed version 3).** No `killed` event has reached HTG's server: a match needs the
    spec's `MinimumRequiredPlayers` (at least 20), and an RCON kill of a lone player waiting for
    one posts nothing. John chose to read them now from the War Dogs community's capture of
    CL-501228 (Warcon's `docs/wardogs-api.md` and parser, whose Season 02 update left the kill
    unchanged) and check against HTG's first match: `type` `killed`; `killerName`, `killerId`,
    `killerSteamId` (absent when the world killed); `victimName`, `victimId`, `victimSteamId`;
    `cause` (an item id, absent on a fall); `distance` (centimetres, absent on a vehicle's
    explosion and most suicides); `contextTags` (`…KillContext.Headshot` and others,
    `…PlayerKillFlag.Player.Suicide`). No faction on either side, so a team kill is the poll's to
    tell at read time; no board reads one yet. The stats event is kind `kill`, the killer as the
    player and the victim as the target, with the map, the match, `cause`, `distance_cm`,
    `headshot` and `suicide`; the names and the game's ids are not kept. Its fixture is synthetic
    and says so (`adapters/wardogs/testdata/README.md`).
  - **`match_ended`** (captured on HTG's server, 2026-10-19): `winningFaction`, `factionScores`
    (`faction`, `score`), `players`, and `randomWinner` (true when nobody scored and the server drew
    the winner), then `MatchState.PostMatch`. The stats event is kind `match_ended` with the map,
    the match, `winner`, `random_winner`, `scores` and `players`.
- **2026-10-20, where gaps are shown (#3).** `StatsService.ListGaps` reads a server's gaps with the
  boards' access, counts only, never a player. A server's page lists the last 30 days' under
  "Missing data", and the admin dashboard's server page lists them too. §5's "beside" the boards
  is narrowed. The boards count from the poll, not the feed (ADR-0012), so a gap leaves every board
  as it was, and a gap beside a board would say it is short when it isn't. Gaps reach the boards
  and the game pages when the feed's events do (#17). The sources' conformance is
  `internal/hub`'s `TestStatsSourcesConformance`, for the sources with a producer (`server_adapter`
  from the poll and the feed, `first_party`); `platform_api` and `self_report` come with theirs (#2).
- **2026-10-21, sources per game (gravel#2).** Each Game spec now says which sources its stats may
  come from (`sources`), and the hub refuses an event from any other: a participation event for a
  game without `first_party` is invalid. A spec is checked when gravel builds, with
  `server_adapter` exactly when it has drivers, so a server-less game can never have a
  server-reported event. The trust mapping by source is now one function, `stats.Feeds`
  (`server_adapter` and `platform_api` feed the combat boards, `first_party` the participation
  boards, `self_report` and anything unknown nothing until reviewed), and `stats.CombatBoards` says
  whether a game can have combat boards at all (`ListGames`' `combat_boards`). The mapping by trust
  stays the boards' own. `platform_api` and `self_report` still have no producer: PUBG's API stats
  and the self-report review flow bring theirs, with their conformance cases.
