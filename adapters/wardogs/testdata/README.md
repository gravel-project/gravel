# War Dogs feed batches

One directory per build, each file one batch body as the hub stores it.

`CL-516586/` (Season 02) holds Hidden Token Gaming's server's first four batches, exported from the
hub on 2026-10-19 with `gravel-hub ingest export --server htg-wardogs-1 --redact` and scrubbed once
more by `record.Scrubber`, which replaces the game's id for a player beside a SteamID (`playerId`)
since then:

- `server-start.json`: 2026-10-18 17:04Z, the server's start with nobody on: two `match_state`
  events (`MatchState.PreMatch`, then `MatchState.PreMatch.WaitingForWorldLoad`).
- `waiting-for-players.json`: 17:06Z, two `match_state` events waiting for players, each with the
  match's `matchId` from here on.
- `player-joined.json` and `player-left.json`: 19:18Z, one player joining (`players` 1 after) and
  leaving 22 seconds later (`players` 0).

The server id, the SteamID, the player's name and the game's id for the player are the
recorder's stand-ins; event ids, match id, map, states, times and the server name are the
capture's. The raw batches stay on the host: they hold the real ones. Kills wait for a session
with enough players to start a match.
