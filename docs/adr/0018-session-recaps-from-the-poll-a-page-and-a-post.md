# ADR-0018: Session recaps from the poll, a page and a post

**Status:** accepted (2026-10-21, John; built in #140) · **Changes:** what the stats store keeps beside matches, a new read of it, and a stock bot module · **Tracked by:** hidden-token-gaming/htg#56 (the first consumer); builds on ADR-0012 (the stats store) and ADR-0016 (weekly windows)

## Context

Hidden Token Gaming's anchor, Wardogs Wednesdays (18:00–22:00 America/Chicago), leaves no record. htg#56 asks for a recap after each one, posted in Discord and kept on a web page:
- peak players;
- minutes at or above the match floor (20 for War Dogs);
- the matches played (maps, and the faction scores);
- the top five by kills, by name only for members who chose it, pseudonyms otherwise;
- no post when nobody came.

What the hub has: the recorder (ADR-0012) turns each good poll (every 15 s) into matches and per-player rows. A board can be read for any window. What it lacks: the player count over time (a match has its players, not how many were on at once) and the team scores, which the poll carries but nothing keeps.

Nothing here is HTG's alone. Any host with a regular night wants its recap, so it belongs in gravel.

## Decision

1. **Occupancy, by the minute.** The recorder writes `server_occupancy`: for each minute a server had anyone on, the fewest and the most players its polls saw that minute (the server's own count, or the identities on, whichever is larger). It has no identity in it, so it is kept like `matches`, with no retention.
   - "Minutes at or above N" counts the minutes whose **fewest** reached N. A minute that touched 20 once doesn't count.
   - Peak is the largest most.
   - Any floor can be asked for when the recap is read, so nothing about a game's floor is stored.
2. **Team scores on the match.** `matches.teams` (JSON: name, colour, score) is the last scores the polls saw, written when they change. At the match's end it holds the final scores.
3. **One read: `StatsService.GetSessionRecap {server_id, from, to, floor}`.** The window is at most 24 hours, and its end can't be in the future. The answer covers every match that overlaps the window:
   - peak players, minutes with anyone on, and minutes at or above `floor`;
   - distinct players;
   - each match's map, times and teams;
   - the top five by kills from those matches, shown as the boards show players.

   Access is the boards': public when `stats.public`, else the owner and `stats:read`. Like a server's board, it counts the server's own rows whatever its trust. Nothing is stored for a recap: it is computed from the data each time it is read.
4. **A page:** `/servers/{id}/recap?from=…&to=…` on the hub renders the same answer, so a recap has a link that stays.
5. **A stock bot module, `discord/modules/recaps`, off by default.** The Organization settings' `discord.recaps[]` name a `server`, a `channel`, a `title` ("Wardogs Wednesday"), weekly `hours` (ADR-0016's windows, in `stats.timezone`), a `floor` and an optional `note`.
   - **When it posts:** a few minutes after a window ends, it reads the recap and posts one message with the numbers and the page's link.
   - **When it doesn't:** if nobody came (peak 0), nothing is posted.
   - **Exactly once:** the post carries a marker (`-# recap <server> · <window start>`). After a restart, the module looks in its own posts and posts only what is missing, for a window that ended within the last day.
   - **Pings:** the post allows no mention.
6. **The window is the settings' hours, not a Discord Scheduled Event.** A recurring event's next occurrence replaces the last one as soon as it ends, so following events would mean keeping state across restarts. The hours are deterministic, restart-safe, and use the same format the host already writes for seeding. A host keeps its event and its recap hours in step (HTG: deploy's `events:` and `discord.recaps`). Muster's sessions may drive recaps later.

## Consequences

- The recorder makes one more write per poll while anyone is on: an upsert of the current minute. It writes the scores only when they change.
- A recap reads the occupancy rows within the window. A five-hour window is at most 300 rows.
- "No match started" (under the floor all night) still gets a recap if anyone came: peak and minutes on, zero minutes at the floor, and whatever matches the recorder opened. A server restart mid-session shows as two matches and a gap in occupancy.
- The top five comes from the raw rows. A recap read after the raw retention window (13 months by default) shows the numbers without the top five.
- Not done: a recap from the feed's per-round events (gravel#4), per-member recaps, editing a recap after it is posted.
