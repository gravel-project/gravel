# ADR-0016: Seeding calls and seeding minutes, a stock bot module

**Status:** accepted (2026-10-10; John merged it and decided the open points) · **Changes:** how a host gets an empty server started, and how helping is counted · **Tracked by:** hidden-token-gaming/htg#32 (the first consumer); builds on ADR-0014 (participation) and ADR-0010 (the poll)

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

## Decision

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
- **Metrics:** `gravel_bot_seeding_calls_total{server,result}` (`posted`, `forbidden`, `error`), `gravel_bot_seeding_sessions_total{server,outcome}` (`started`, `reached`, `window_closed`), `gravel_bot_seeding_recorded_seconds_total{server}` and `gravel_bot_seeding_record_errors_total{server}`, on the bot dashboard's Seeding row.
- **As decided (John, 2026-10-10, hidden-token-gaming/htg#32):**
  1. HTG's War Dogs windows are Wednesday, Saturday and Sunday evenings, 18:00–23:00 America/Chicago, with a 2 h cooldown. These are HTG's settings values, not gravel defaults: gravel's default cooldown is 2 h, and a rule without windows never calls.
  2. A call goes out at each window's start while the server is under its floor. It is a rule option, `call_at_start` (default false). That call is a call like any other: it opens a session and starts the cooldown, and it waits for nothing else.
  3. HTG uses a dedicated channel (`#wd-seeding`). The channel is a settings value, so there is nothing to build.
- **As built (gravel, hidden-token-gaming/htg#32):**
  - **Settings.** `discord.seeding[]` takes `server`, `channel`, `role`, `below` (2 to 1000), `hours`, `cooldown` (a Go duration from 15m to 24h; empty is 2h), `call_at_start` and `note` (one line of at most 200 characters). There are at most 10 rules, one per server, and each needs `guild_id`. `role` may not be the guild's id (@everyone). The seeder role is the members' own choice, so role sync never manages it.
  - **Hours.** Each window is `{days: [mon … sun], from: "HH:MM", to: "HH:MM"}` in `stats.timezone`, and a rule has at most 14. A `to` before `from` is an overnight window that ends the next day (supporting it was a few lines and is tested). `from` equal to `to` is refused, and so is a window that overlaps another of the same rule, overnight ones included. Times are wall-clock in the zone, so a window keeps its local hours across a daylight-saving change. What a window means lives in one package, `discord/seedhours`, which both the hub's validation and the module use.
  - **`call_at_start`.** A window's first call ignores a cooldown left over from before the window. Without it, the cooldown runs across windows. With the default rule, a window's first pass under the floor already calls once the cooldown since the previous window's last call has run out. So the option changes behaviour only when windows sit closer together than the cooldown: two windows on one evening, or a long cooldown. The call counts toward the cooldown like any other, so it goes out once per window.
  - **When a call is due.** The server must be live (`state` ok and reachable), not merely listed. A call that fails (Discord refuses it or errs) starts no cooldown and is tried again at the next pass.
  - **The messages.** A call is `<@&role> **<server name>** is at 7/20: join to get the match going.`, then the note, then the marker `-# seeding <server> · <start unix> · open`. At the floor it is edited to `**<name>** started: 20/20 at <t:…:t>. Thanks for seeding!` (the time shows in each reader's own zone) with the marker's state `reached`. When the window ends first, the edit is `**<name>**: the seeding window closed at 12/20.` with the state `closed`. The marker's state is how a restart tells an open call from an ended one. An edit pings no one.
  - **Counting.** Each pass during a session credits every player on the server with the time since the last pass. That time is measured, capped at twice the interval (so a stall does not credit the gap) and never runs past the window's end. A pass while the server is not live credits nothing. A pass whose player list fails credits nothing and leaves the time for the next pass, which still applies the cap.
  - **Recording.** A player is recorded only with 60 seconds or more. The event's key is `seeding:<server>:<start unix>:<provider>:<subject>` and its `session` is `seeding:<server>:<start unix>`. Events go 100 to a call. The minutes are sent before the call's edit, so a crash between the two leaves the call open; the resumed session then records under the same keys, which the hub keeps once. A failure keeps the events in memory and retries them every pass, at most 5,000 per server. A batch the hub refuses as invalid is resent one event at a time, so the one event it refuses (a server since removed, an identity it does not take) is dropped alone and logged.
  - **Restart.** The newest of the bot's own posts with a marker for the server is the last call: the cooldown counts from its start. A call still `open` inside the same window is resumed, and counting restarts from the restart. The bot loses at most the time it was down, plus whatever was counted in memory for that session before it stopped. A call whose window has closed is edited to `closed` and records nothing. Events waiting for the hub when the bot stops are lost too.
  - **Rule changes.** When a rule is removed or its channel changes, its open call is closed.
  - **The servers manifest's `seeding` section is removed (John, 2026-10-10).** ADR-0010 gave the servers manifest a `seeding` section (`threshold`, `hours`, `quiet`, `timezone`, `cooldown`), shown on `Server.seeding`, before this decision. Nothing read it. In 0.8.0 it is gone: the manifest field, the API field (`Server.seeding`, number and name reserved), the `Seeding` message, and the `managed_servers.seeding` column (migration 15). A `servers.yaml` that still has the section is refused by `check` and `apply`, naming each server's section by line and pointing to `discord.seeding`. ADR-0010 carries the amendment.
