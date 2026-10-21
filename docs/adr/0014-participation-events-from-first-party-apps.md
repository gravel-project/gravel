# ADR-0014: Participation events come from first-party apps and feed participation boards only

**Status:** accepted (2026-10-10) · **Changes:** how a first-party app records that a player took part in something, where those events live, who reads them, and how retention and erasure reach them · **Tracked by:** #19; hidden-token-gaming/htg#32 (the seeding job, the first producer) and htg#34 (Muster's attendance, the second)

## Context

Some of what a community values is not a kill count. Hidden Token Gaming wants to credit the members who seed a server (minutes spent on it while it is under its player floor, during the hours it wants players) and the members who turn up to its events (Muster sign-ups marked attended). Neither comes from a game server's poll or feed. A first-party app sees them: the seeding job (htg#32) watches the server cards' player counts, and Muster knows who attended.

The pieces are in place. Apps authenticate with client credentials and scoped bearer tokens (ADR-0008), and requests carrying a token are rate-limited per app. ADR-0011 gave every stats event a `source`, with `first_party` among the four, and a trust mapping in which `first_party` events feed participation boards. ADR-0012 gave the stats store its windows, seasons, pseudonyms, retention rollup and erasure. What was missing is the endpoint, the storage, and the boards that read it.

## Decision

1. **One procedure, apps only.** `StatsService.RecordParticipation` takes 1 to 100 `ParticipationEvent`s and answers a `ParticipationResult` per event, in request order: `recorded`, or `duplicate`. Only an app whose token carries the new scope `participation:write` may call it. A member's session is refused with `permission_denied`, the owner's included: an event says what an app observed, and the owner has no observation to add by hand. An unauthenticated call is `unauthenticated`. The request is checked whole before anything is stored: any invalid event refuses the request with `invalid_argument`, naming every problem by index and field (`events[3].kind: …`), and nothing is stored.

2. **The event.**
   - `key`: the app's idempotency key, 1 to 128 printable ASCII characters, unique per app. A key the app used before (earlier in the same request too) is a `duplicate` and is not stored again, even when the other fields differ: retries are safe, corrections are new events. Two apps may use the same key.
   - `kind`: `^[a-z][a-z0-9_]{0,31}$`, the app's own name for the activity (`seeding`, `attendance`). Kinds are not registered anywhere: a kind exists once an event of it is recorded.
   - `identity`: provider and subject. The provider is one a member can link (`discord`, `steam`), so an event can reach a profile; the subject is 1 to 64 characters with no control characters. Events are keyed by identity, never a member (principle 5), so an unlinked player accumulates participation that a later link inherits.
   - `game_id` (optional): a game this hub ships. `server_id` (optional): a server the hub knows. With both, the server's game must be `game_id`; with only `server_id`, the server's game is stored.
   - `session` (optional): the app's own reference, at most 128 characters with no control characters.
   - `occurred_at`: required; at most 5 minutes ahead of the hub's clock; not older than the stats store keeps raw rows (the first day the retention rollup keeps, from `stats.raw_retention_months`), so an event is never stored only to be rolled up at once.
   - `duration_seconds`: 0 to 86400. `schema_version`: 0 (the current one) or 1, stored as 1 (the stats schema is versioned from day one).

3. **Storage of its own.** Migration 13 adds `participation_events` (the event, the app, `received_at`, `source` constrained to `first_party`, `schema_version`; unique on organization, app and key) and `participation_monthly` (organization, kind, game, period, identity, events, seconds). The insert is `ON CONFLICT DO NOTHING` on the unique key, so a duplicate is found atomically, also between two concurrent requests. Every write bumps the stats store's change counter, so the board cache sees it as it sees the recorder's.

4. **The trust mapping is structural.** Participation events feed participation boards and profiles only (ADR-0011 §4). The combat boards (`GetBoard`) read `match_stats` and `stats_monthly`, and participation lives in its own two tables, which no combat query reads. That is the conformance case for the `first_party` source: there is no stats conformance suite in the repository yet, so the case is a test in the store's integration tests (`TestParticipationBoardAndTotals`) and one end to end through the hub (`TestParticipationFromAnApp`): a recorded participation event changes no combat board of any scope, metric or window, nor a profile's combat totals. When a conformance suite exists, the case moves into it.

5. **Retention and erasure follow ADR-0012.** The `stats_rollup` job also rolls participation events that happened before the same cutoff into `participation_monthly`, one row per kind, game, period and identity, with the same periods (calendar months in `stats.timezone`, cut at season edges), and deletes the raw rows in the same statement. A board over all time or a season reads both tables and counts the same before and after. `gravel-hub stats erase` (`EraseIdentity`) re-keys both tables with the rest, in the same transaction, and reports their counts.

6. **The boards.** `StatsService.GetParticipationBoard` takes a kind (empty: the first recorded, alphabetically), an optional game, a window (`all`, `week`, `month`, `season`) and a page. It ranks by seconds, then events, then identity, and answers the kinds the organization has recorded, for a picker. Its access is `GetBoard`'s: everyone when `stats.public` is on, else the owner and apps with `stats:read`. Names come from the same code as `GetBoard`'s: a member who chose to be shown by name, else a pseudonym. Answers are cached as `GetBoard`'s are. A well-formed kind nobody recorded is an empty board, a malformed one `invalid_argument`.

7. **Profiles.** `GetMemberProfile` gains `participation`: the member's totals per kind over the windows it already reports (all time, this week, this month, each season running now), across every linked identity. A game's season counts that game's events only.

8. **Rate limits.** Requests carrying an app token are already limited per app, by its client id (`rate_limit.per_app`, 600 a minute with a burst of 100 by default; `resource_exhausted` beyond), before the per-user and per-address limits. `RecordParticipation` needs nothing more: one app cannot spend another's budget, and a hundred events a call is 60,000 a minute at most.

9. **Metrics.** `gravel_participation_events_total{kind,outcome}`, outcome `recorded`, `duplicate` or `invalid`, on the hub dashboard's Stats row. Every event of a refused request counts as `invalid`, under kind `invalid` when the kind itself is malformed, so a bad sender cannot grow the label set.

10. **Pages.** `/participation?kind=&window=&page=` renders a participation board the way `/boards` renders a leaderboard (the same access, refusals in words, htmx swapping the content), with a kind picker and an empty state before anything is recorded; `/boards` links to it. The member profile gains a Participation section, a table per kind with a row per window. A kind is shown with underscores as spaces and its first letter upper-case ("event_attendance" is "Event attendance").

## Consequences

- **Additive only.** A new scope, two new procedures, new messages and a new profile field; migration 13; one new metric. A patch release.
- **Providers.** Only `discord` and `steam` are accepted, the providers a member can link today. Sea of Thieves (`xbox`), Star Citizen (`rsi`) and PUBG (`pubg`) identities become acceptable when identity can link them (P5), by widening the list.
- **Kinds are the app's vocabulary.** gravel gives them no meaning beyond the label. A host that wants a board per game uses `game_id`; one that wants a new activity sends a new kind. Retiring a kind means not sending it any more; its history stays, as every stats row does.
- **No member input.** A person cannot record their own participation through this procedure. Self-reported activity is ADR-0011's `self_report` source, which feeds nothing until a reviewer approves it, and is not built here.
- **Producers.** HTG's seeding job (htg#32) and Muster (htg#34) each register an app with `participation:write`, and keep `stats:read` on a separate app (or the same one) if they read boards while stats are private.

## Amendments

- **2026-10-21, a game's own provider (gravel#2).** The widening the Providers consequence foresaw,
  per game: an event may name `discord`, `steam`, or the identity provider of its game (the
  `game_id` it names, or its server's game). A Sea of Thieves event may name an `xbox` identity; an
  event of no game, or of War Dogs, still may not. A game whose spec's `sources` lack `first_party`
  refuses participation. Kinds stay unregistered: gravel's Game specs name none, since they are a
  host's app's vocabulary, and a game's page (`/games/{id}`) shows every kind recorded for it.
  `GetParticipationBoard`'s `kinds` are the game's own when `game_id` is set.
