# ADR-0035: Player sessions from the poll

**Status:** proposed (2026-10-15; accepted when John merges #208's pull request) · **Changes:** what the stats store keeps beside matches; amends ADR-0012 §2 (the poll also records sessions) and §6 and §7 (sessions are rolled up and erased like match rows) · **Decided in:** John's design session *gravel — Design Session: Hub, Spines & Dashboard* (2026-10-10 to 12, the admin dashboard's player activity view) and Hidden Token Gaming's roadmap re-cut (handbook D35–D36) · **Built in:** #208 · **Part of:** H1 (#182)

## Context

An operator wants to know when their community plays and who stopped coming: which evenings to schedule events on, whether the server is growing, which regulars to check in on. The hub already sees every player at every good poll (ADR-0010), but keeps only what a match needs: per-match totals with a first and last sighting (ADR-0012), and each minute's player count (ADR-0018). Neither says when a person was on: match rows end when the match does and roll up into months, and occupancy counts nobody in particular.

## Decision

1. **A session is one identity on one server, from the poll that first saw them to the poll that last did.** The recorder (`internal/stats`) stitches them on every good poll: a poll that sees an identity extends its newest session on that server when that session's last sighting is within `stats.SessionGap` (three poll intervals and a half: up to two missed polls and jitter), and starts one otherwise. The stitching reads the store, not memory, so a hub restart shorter than the gap carries a session on and a longer one splits it, which is the truth: nobody saw them meanwhile.
2. **Keyed by the provider identity** (`player_sessions`: organization, server, game, provider, subject, started, ended), never a member (README principle 5). Who it is is resolved when it's read, as a board resolves players.
3. **Personal data, kept like match rows.** `stats erase` re-keys a player's sessions and monthly rows (ADR-0012 §7). Past `stats.raw_retention_months` the retention job rolls a period's sessions into `player_activity_monthly` (sessions, seconds and active days per identity, server and period) and deletes them (§6); a rolled-up month still says a player isn't new.
4. **`StatsService.GetActivity{window}`** reads a window ending now: 7, 30 or 90 days. It answers the week's 168 hours in `stats.timezone` (the players on in each, averaged over the window's weeks, and the most in one); the sessions, the players and the median session; how many came back on another day and the mean days played; new players (never seen before the window) and lost ones (on in the window of the same length before it, not in this one); each week's players and new players; and the **regulars gone quiet**: identities that played on 4 days or more in the 8 weeks before the window and not once in it, the most recently seen first, at most 50. An erased identity is never listed.
5. **Staff's, in person.** `GetActivity` needs `stats:read` held everywhere (a game's moderator sees their game's boards, not the hub's community) and a session: no app or member's token reads it, because it lists people who went quiet. Names follow the boards' rule: a member's name when they chose to be shown, else a pseudonym. `/admin/activity` shows it.

## Consequences

- One more write per good poll with anyone on: a single statement over the poll's identities (extend, else insert), on an index that finds the open session.
- New: migration 00031, `store.RecordSessions`/`RollUpActivity`/`PlayerActivity`, `stats.Boards.Activity`, `gravel_stats_sessions_started_total{server}` on the Stats row, `/admin/activity`.
- A session only knows what polls see. A server that names no identities (a game without player identities in its status) records none; a player on for less than one poll interval may be missed; the edges are a poll interval wide.
- Windows reach back at most 90 days plus the regulars' 8 weeks, inside the default 13-month retention; a host that keeps less is told when a window reaches past it (`partial`).
- Not here: the bot reaching out to regulars gone quiet (a module over this procedure, later, with an app capability then), per-game activity, and Discord presence (ADR-0017 stays memory-only).
