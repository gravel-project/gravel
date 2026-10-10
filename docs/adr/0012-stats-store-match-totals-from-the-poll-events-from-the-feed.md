# ADR-0012: The stats store keeps match totals from the poll and events from the feed, and boards are queries

**Status:** proposed (2026-10-10) · **Changes:** where stats live, what a board counts, how an unlinked player is shown, how a season works, how an erasure reaches stats · **Tracked by:** #17 (the store and the boards); #3 (provenance, which lands in its first migration); #4 (the feed's parser, which writes into it); #18 (the pages); hidden-token-gaming/site#40 (the privacy policy goes live before the first board is public)

## Context

P2's boards (#17) need a store. The README's design is a Go stats-store interface in domain terms, with Postgres first (ADR-0001). Events are keyed by provider identity and resolved to a user at read time (principle 3). Unlinked players appear under stable pseudonyms (Player privacy). Seasons are windows, never deletions. Raw identity-linked events roll off at 13 months while aggregates stay. ADR-0011 added the provenance every event carries and gaps as events.

War Dogs gives two sources:

- **The poll.** The hub already reads `/v1/players` every 15 s: each player's SteamID, team, and kills and deaths for the current match (ADR-0010). It is complete for counts (nothing is lost while the hub is up), coarse in time, and says nothing about victims or weapons.
- **The feed.** It pushes each kill with killer, victim and details (ADR-0011). It is precise, but it never retries, its format is still unconfirmed (S1, hidden-token-gaming/deploy#49), and it is lost while the hub is down.

ADR-0011 already decided how they meet: the feed is reconciled against the poll, and at a match end the poll's totals are the record.

## Decision

1. **Two tables, one record.**
   - **`match_stats`: the counts.** One row per player per match (server, match, identity, kills, deaths, time on, team, the trust at receipt, the source), written from the poll as the match goes and closed at its end.
   - **`stats_events`: the detail** (ADR-0011's provenance columns): a kill with its killer, victim and extensions, a gap, presence.
   - **Boards count `match_stats`.** Events add what counts cannot (who killed whom, weapons, distances), and when the feed's count for a player in a match exceeds the poll's (a poll missed while the hub was down), the row takes the higher number and a gap event says so. So a board is right without the feed, and the feed makes it richer, never different.

2. **A match is found from the poll.** A new match starts when the counts of the players present all drop, when the rotation index moves (`status.rotation`), or when the server restarts (the public health route's `uptimeSeconds` starts over; the poll reads it with the status). A player who leaves and comes back in the same match keeps their row: the game's counters for them are cumulative per match (an assumption to check in the first live session; if they reset on rejoin, the row adds the segments). Matches are rows too (`matches`: server, started, ended, map, the final score), which the recaps (htg#56) and the pages (#18) read.

3. **The interface is in domain terms.** It lives in `stats.Store`:
   - `RecordObservation(server, observation)`, from the monitor after each good poll;
   - `RecordEvents(batch, events)`, from the feed's parser, deduplicated on `(server, source, source_key)`;
   - `Board(query)`, where a query has scope (server, game, organization), window (a week, a month, a named season, all time), metric (kills, deaths, K/D, time on, matches) and a page;
   - `PlayerStats(identity, window)`, `Matches(server, page)`, `Gaps(server, window)`.

   The Postgres implementation is the default and the only SQL is in `internal/store`. Nothing above the interface knows tables.

4. **Trust and windows are filters, not copies.**
   - **Trust:** ADR-0011's trust mapping decides which rows a board may count. Official rows feed global boards; Community rows feed only their server's.
   - **Windows:** a week is ISO (Monday to Sunday) in the organization's timezone. A season is a named window (`seasons`: game, name, from, to), set in the Organization settings' `stats` section (ADR-0007). A season ending deletes nothing, and a new one is a new row.
   - **K/D:** kills / max(deaths, 1), shown only from a minimum of matches (a setting, 3 by default), so one lucky match does not top the board.

5. **Pseudonyms are persisted, players opt in to their names.** An unlinked identity is shown as its pseudonym: two words from a curated list, chosen by a keyed hash of `(provider, subject)` and persisted at first sight in `pseudonyms`, unique by construction (a suffix on a collision), never reversible to the SteamID. It is the `stats.Pseudonyms` interface, with the word list as the default. A member who links the identity and turns on "show my name on boards" (a user preference, off by default) is shown by display name, at read time; turning it off goes back to the pseudonym. No event moves either way. The key for the hash is a hub secret file (`stats.pseudonym_key_file` in `hub.yaml`), so pseudonyms are stable across restores and not guessable from the list.

6. **Retention keeps aggregates.** A nightly job rolls `match_stats` older than the retention window into `stats_monthly` (identity, server, month, the counts), then deletes those raw rows and their events. The window is the Organization settings' `stats.raw_retention_months`, 13 by default, the placeholder pending counsel. Boards read both tables, so a board over a past season is the same before and after the rollup. Gap events and `matches` (no personal data) are kept.

7. **Erasure re-keys.** A player's erasure (principle 8) replaces their `(provider, subject)` with an irreversible token in `match_stats`, `stats_events`, `stats_monthly` and the 30-day `ingest_batches` (ADR-0011 §7), in one transaction, and deletes their pseudonym row. It is the one update the stats tables allow besides closing a match. Aggregates keep their numbers ("a deleted player"). The erasure flow itself (who asks, how it is verified) is identity's, and comes with it; this ADR gives stats the operation it calls.

8. **What the API shows.** `StatsService` is public for boards and match lists: pseudonyms or opted-in names, never a SteamID. `PlayerStats` by identity is the member's own, the owner's, or an app's with a new `stats:read` scope (htg-bot's `/stats`). The privacy policy's stats rows go live before the first board is public (hidden-token-gaming/site#40); until then the boards are owner-only, by a setting (`stats.public`, off).

## Consequences

- **Migration 10:** `matches`, `match_stats`, `stats_events`, `pseudonyms`, `seasons`, `stats_monthly`. `hub.yaml` gains `stats.pseudonym_key_file`; the Organization settings gain a `stats` section (seasons, retention, public, minimum matches); users gain one preference. All additive: patch releases.
- **Order of work:**
  1. The store with the poll's match totals, pseudonyms, the boards API, owner-only. This is buildable now and needs no feed.
  2. HTG's first live session checks the match and rejoin assumptions (§2).
  3. The feed's parser writes `stats_events` from S1's captures (#4).
  4. Retention and the rollup.
  5. Erasure's operation.
  6. Public boards once site#40 is live.
- **Pages:** #18 renders boards, match lists and profiles from `StatsService`. The live card (htg#31) is unchanged.
- **Metrics,** on the hub dashboard: `gravel_stats_matches_total{server,game}`, `gravel_stats_rows_written_total{server}`, `gravel_stats_feed_shortfall_total{server}` (the feed below the poll), `gravel_stats_rollup_last_success_timestamp_seconds`.
- **Sanity bounds** (README, Trust) apply at `RecordObservation`: a count that rises faster than the Game spec allows is refused and flagged. The bounds are #8's, written into the spec; until they exist nothing is refused, and the decision is recorded so the hook is in the interface from the start.
