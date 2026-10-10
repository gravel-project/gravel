# ADR-0011: Inbound ingestion stores every batch raw before it parses, and every event carries its provenance

**Status:** proposed (2026-10-10) · **Changes:** how a source that pushes into the hub is received, authenticated and kept; what a stats event records about where it came from; how missing data is recorded · **Tracked by:** #4 (ingestion) and #3 (provenance); #17 (the store and boards it feeds); hidden-token-gaming/deploy#49 (S1, the first feed), wardogs-server#12 (the config that turns it on)

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
   - **`hub_down`:** at start, the hub writes a gap for every server with a feed from the last batch it stored to now, if the server had players then.
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

