# ADR-0024: Match results from a server's match plugin

**Status:** proposed (2026-10-12; accepted when John merges it) · **Changes:** adds match results beside the poll's match totals (amends ADR-0012), and a second kind of inbound batch from a server's own host (amends ADR-0011) · **Tracked by:** #20, hidden-token-gaming/htg#36 (the PUG flow that reads them)

## Context

The stats store's matches come from the poll (ADR-0012): a War Dogs server says who is on and its scores, and a match is the time the server was occupied. A PUG is a different thing: two named teams, a best-of series, maps with scores, a winner, and a demo per map. On a Counter-Strike 2 server that is what MatchZy runs, and MatchZy reports it to a URL it is given (`matchzy_remote_log_url`): JSON events with one custom header, no retries, no deduplication. It uploads each map's demo to another URL, as a stream with its match id and map number in headers.

What MatchZy 0.9.1 actually sends, read from its source: `series_start` (teams, maps), `going_live` (a map starts), `round_end` (the running score and every player's running totals on the map), `map_result` (the map's score and winner, but no player totals), `series_end` (maps won, the winner). Its `demo_upload_ended` exists but is never sent.

## Decision

1. **Two routes on the hub, from the server's own host.** `POST /api/ingest/matchzy` takes an event and `POST /api/ingest/matchzy/demo` a demo, each with the server's feed token in the `Authorization` header MatchZy can send (`Bearer <token>`, which `pre.sh` writes into MatchZy's config), and only from the address of the server's host, as the log route (ADR-0011). An event is stored as received before the answer, like every batch (source `matchzy`); a demo is streamed to object storage, which is its home.
2. **A result is a series.** `match_results` is the plugin's match id on a server: its teams, maps played and won, winner, and the live lease on the server when it started (a PUG). `match_result_maps` holds each map's score, winner, times and its demo's key; `match_result_players` each player's totals on each map, keyed by the provider identity (principle 5), replaced by each `round_end`, since its totals are the map's so far. An event whose `series_start` never arrived starts the series (MatchZy doesn't retry).
3. **A series' end ends its lease's match.** `series_end` marks the server's live lease `ending`; it is given back `release_after` later (ADR-0023).
4. **Read like the boards.** `StatsService.ListMatchResults`, `GetMatchResult` and `GetMatchDemo` follow `GetBoard`'s rule (public stats, or `stats:read`), and players are shown as a board shows them: a member's name when they chose it, else a pseudonym; a SteamID never leaves the hub. The page `/results/{id}` shows a series, and a demo's link (`/results/{id}/demo/{map}`) redirects to a presigned URL good for 15 minutes; the bucket stays private.
5. **Object storage is a seam.** `internal/objectstore` is a `Store` (put a stream, a signed URL, stat, delete) with an S3-compatible implementation over minio-go (Cloudflare R2 first) and an in-memory one for tests. `hub.yaml` `demos` names the bucket and its keys; without it the hub keeps no demos and the demo route answers 404.

## Consequences

- New: migration 00022, `internal/results`, `internal/objectstore`, `games/cs2/matchzy` (MatchZy's events and match config, no gravel imports), the two routes, the three procedures, the result page, and `gravel_match_events_total` and `gravel_match_demos_total`.
- Accepted costs: MatchZy's events can be lost (no retries); the stored batches can be read again (`gravel-hub ingest export`). Many of 0.9.1's player totals are always 0 (flash assists, trade kills, KAST and others), so the hub keeps the ones it fills: kills, deaths, assists, damage, headshot kills, MVPs and score. A demo is kept until the bucket's lifecycle deletes it.
- `match_result_players` is erased and retired like every identity-linked row (#216, ADR-0012 as built): `stats erase` re-keys it and blanks the name, and past `stats.raw_retention_months` each player of an ended series gets a random token of their own in that series. The scoreboard stays; nobody is named.
