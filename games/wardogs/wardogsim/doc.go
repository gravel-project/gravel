// Package wardogsim is a simulated War Dogs dedicated server for a demo hub (gravel#112): the
// RCON API as the newest recorded build answers it (games/wardogs/testdata, embedded in the
// binary), over a world of made-up players that changes with time.
//
// The recorded files are the single source of what the server says about itself: the
// capabilities and their routes, the catalog, the configuration schema, the factions and the
// starting document. The world is the simulator's own:
//
//   - Regulars with stand-in SteamIDs (SteamID: the recorder's scrubbed range, below every real
//     account) and invented names join and leave on the profile's daily curve, in sessions of
//     the profile's length; some leave and rejoin the same match, with the game's counters
//     restarted, as a reconnect does.
//   - A match starts once the document's MinimumRequiredPlayers are on. Kills and deaths
//     accumulate by each player's skill; the faction holding the hill scores each score period.
//     After the profile's match length the rotation moves, the counters and scores reset, and the
//     next match waits for its minimum.
//   - Moderation works (kick, kill, message, broadcast, move, ban, unban), and so do the
//     configuration's get, validate and put with the revision and If-Match. A write takes effect
//     when its section's schema says: at once, at the next match, or at the daily restart
//     (Profile.RestartHour), which is also when [WDServerFeed] is read.
//   - The server's rules hold: three requests with a missing or wrong token lock the address's
//     protected routes for a minute (429 with Retry-After), 600 requests a minute per address,
//     64 KiB bodies, and the admin log is a ring of the newest 500 lines.
//   - With [WDServerFeed] Url (and Token) in force, kills go out in batches to Url + FeedPath. The
//     batch's shape is a documented guess (feed.go): no real batch is recorded yet.
//
// What the recording does not show, the simulator decides and says so: the player object follows
// the client's Player type (CL-509546 was recorded with nobody on), and the error codes of the
// writes the recording cannot hold are the client's (player_not_found, ban_not_found,
// message_too_long) or plain names of the simulator's own.
//
// The simulator never carries a real person: names, SteamIDs and the calibration's curves are
// made up or measured as distributions only (Profile).
//
// This package has no gravel imports, like the client it serves (depguard enforces it).
package wardogsim
