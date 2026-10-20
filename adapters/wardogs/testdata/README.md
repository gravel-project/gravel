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

Two more, exported on 2026-10-20 the same way and scrubbed by `record.Scrubber` from main (the
hub then ran 0.10.1, whose export left `playerId` in place):

- `match-ended.json`: 2026-10-19 17:04Z, the empty match running out after a day: `match_ended`
  with every faction on 0 and the winner drawn (`randomWinner`), then `MatchState.PostMatch`.
- `sink-down-flush.json`: 2026-10-20 12:34:52Z, the batch the server posted 19 s after nine minutes
  of 503s from the hub's route (deploy#49): the join and the leave it had queued, together, each
  with the game time it happened at.

The server id, the SteamIDs, the players' names and the game's ids for them are the recorder's
stand-ins; event ids, match ids, map, states, scores, times and the server name are the capture's.
The raw batches stay on the host: they hold the real ones.

**`killed-synthetic.json` is not a capture.** No `killed` event has reached Hidden Token Gaming's
server yet (a kill needs a match, a match needs players; an RCON kill of a lone player waiting for
one posts nothing, 2026-10-20). Its fields are the community's capture of CL-501228: Warcon's
`docs/wardogs-api.md` and its parser `src/lib/server/feed-core.ts` (warcon-app/warcon at 8a79a7b,
2026-10-19; the Season 02 update of 2026-10-15 left the kill unchanged). Every value is ours: the
stand-ins, the ids, the times, the items and the distance. Its four events are a headshot, a fall
(no killer, item or distance), a grenade suicide (no distance) and a vehicle's explosion (no
distance). Replace it with a real batch when one comes.
