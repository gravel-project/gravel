# ADR-0016: Seeding calls and seeding minutes, a stock bot module

**Status:** proposed (2026-10-10) · **Changes:** how a host gets an empty server started, and how helping is counted · **Tracked by:** hidden-token-gaming/htg#32 (the first consumer); builds on ADR-0014 (participation) and ADR-0010 (the poll)

## Context

Some games only start a match once enough players are on. War Dogs needs 20, so an empty community server stays empty unless people join it together. That is "seeding". Hidden Token Gaming's plan (§4.3) asks for two things:
- an opt-in seeder role, pinged when the server is under its minimum during set hours, rate-limited, with quiet hours;
- seeding time counted as a recognition stat.

Nothing about this is specific to HTG or to War Dogs. Any gravel host with a player floor has the same problem, so it belongs in gravel's stock modules (htg's rule: generic belongs upstream).

What exists:
- The hub polls each server every 15 s, and `ListServers` (public) gives players and max.
- `ListServerPlayers` (`servers:read`) gives who is on, by provider identity.
- `RecordParticipation` (`participation:write`, ADR-0014) records first-party participation that feeds participation boards and profiles, never combat boards.
- The server cards module (`servercards`) already places per-server messages from the Organization settings.

## Decision (proposed)

1. **A stock module, `discord/modules/seeding`,** off by default (`bot.yaml` `seeding {enabled, interval}`). It reads its rules from the Organization settings and the server state from the hub, like the server cards.
2. **Rules in the Organization settings,** a `discord.seeding` list with one entry per server (at most 10):
   - **where:** `server`, `channel` (where the call is posted) and `role` (the opt-in seeder role it pings);
   - **when:** `below` (the player count under which a call is due; the game's own floor, 20 for War Dogs), and `hours` (weekly windows in `stats.timezone`, e.g. `{days: [wed, fri, sat], from: "18:00", to: "23:00"}`);
   - **how often:** `cooldown` (the least time between calls, default 2 h).

   Outside every window nothing is posted. That is the quiet-hours rule: quiet is the default and the windows are opt-in.
3. **A call.** When a server is live, inside a window, under `below` and past its cooldown, the module posts one message pinging only that role (allowed mentions: that role, nothing else). For example: "**HTG WARDOGS | NA WEST | #1** is at 7/20: join to get the match going." with the join instructions from the host's card note. When the server reaches `below`, the same message is edited to "Started at 20". A call never repeats inside its cooldown, whatever the count does.
4. **A seeding session** runs from a call until the server reaches `below` or the window ends.
   - While it runs, every pass reads who is on (`ListServerPlayers`) and adds the pass interval to each player's time.
   - At the end, the module records one participation event per player: kind `seeding`, the player's provider identity (`steam`), `server_id`, duration = their minutes, and an idempotency key of server + session start + identity, so a restart mid-session never double counts.
   - Seeders are counted by what they did, not by holding the role: anyone who joined during the call counts, member or not, under the stats store's pseudonym rules (ADR-0012).
   - **Restarts:** the module keeps the open session in memory, and after a restart finds its place from its last call message, as server cards and the mod log do. A restart loses at most one interval of counting.
5. **The bot's app needs** `servers:read` and `participation:write`. The call itself needs only the public server list.
6. **One source of seeding minutes.** Muster's War Dogs "seeding" activity (htg ADR-0002) organises scheduled seeding nights: sign-ups, attendance and the voice channel. It does **not** record `seeding` participation, because this module counts the minutes on the server. Muster's attendance stays its own kind (`attendance`).
7. **"Who's playing now" (presence) is a separate decision.** The plan pairs it with seeding (htg#32), but it needs Discord's privileged presence intent and a privacy-policy row of its own (planned in site#40). It gets its own small module, with counts only and nothing stored, in a follow-up.

## Consequences

- **HTG:**
  - `discord.seeding` for War Dogs (#wd-chat or a seeding channel, the `@WD Seeder` role by discordctl, `below: 20`, its nights);
  - htg-bot's hub app re-created with `servers:read`, `servers:moderate`, `participation:write` and `host:muster:write`;
  - a gravel release.
- **Seeding minutes** appear on the participation board ("Seeding") and on profiles, under the same visibility as every board.
- **Metrics:** `gravel_bot_seeding_calls_total{server,result}`, `gravel_bot_seeding_sessions_total{server,outcome}` (started or timed out) and `gravel_bot_seeding_minutes_total{server}`, on the bot dashboard.
- **Open for John:**
  - the War Dogs windows and cooldown (defaults proposed: Wednesday and weekend evenings 18:00–23:00 Central, 2 h);
  - whether a call should also go out once at the start of each window while the server is empty;
  - whether the channel is `#wd-chat` or a dedicated `#wd-seeding`.
