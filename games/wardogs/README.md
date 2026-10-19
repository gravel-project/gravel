# games/wardogs

A Go client for the War Dogs dedicated server's RCON API (plain HTTP, a bearer token, JSON, and a
configuration document in INI form), with no gravel imports: it becomes a standalone module at
the open-source cut (ADR-0001). gravel's driver adapts it; ADR-0010 is the design.

War Dogs is in Early Access and its API changes every few weeks, so the client assumes no route.
It reads `GET /v1/capabilities` (with the token whenever it has one: Season 02, CL-516586, stopped
serving it without, and only `GET /v1/health` is public now), matches the advertised routes by shape (`{id}` and `{steamId}`
are the same route), and fails a call whose route is gone with `ErrNotSupported` before sending
anything. Parsing is tolerant in production (a field whose type drifted is reported through
`Options.OnMismatch` and the rest of the answer is kept) and strict in the tests.

The server's rules the client keeps:

| Rule | What the client does |
|---|---|
| Three requests with a missing or wrong token lock every protected route behind a 429 for a while | One token. After a 401 it sends nothing protected until `SetToken`; protected requests are serialised until the token has been accepted once, so concurrent callers spend one strike, not one each. No token means no protected request at all, and one read of the capabilities: a build that guards them (Season 02) answers 401, which spends one strike, and the client sends no tokenless read of them again (`ErrNoToken`). |
| A 429 carries `Retry-After` (1–60 s) | Nothing is sent until it has passed (`RateLimitedError`). |
| 64 KiB request bodies; 600 requests a minute per address | A larger body is refused before sending (`ErrBodyTooLarge`); the rate is the caller's budget. |
| `GET /v1/config` returns the RCON password in clear | `RedactConfig` blanks the password and its hash, the feed's token and URL and the join password, keeping the CRLF line endings. |
| A write replaces the whole document, `If-Match` on its revision | `PutConfig` sends CRLF and a quoted `If-Match`, and returns the server's result with `ErrRevisionMismatch` (412) or `ErrConfigRejected` (422). |

## Fixtures: record, diff, update

`testdata/<build>/` holds one recorded build: every GET route it advertises, as files that mirror
the route paths, and `recording.json` saying what was recorded and skipped, and which routes went
without the token (`tokenless`; a recording without the list, CL-509546, sent the capabilities
and the health that way). The fake server (`wardogstest`) and the simulator answer those routes
without a token and guard the rest, as that build did. The tests run the
same contract against every recorded build (`contract_test.go`), so a new build is added beside
the old ones, not over them.

When the server's build changes (the build watcher says so, or `GET /v1/capabilities` does):

```sh
gravel-hub wardogs record --base-url http://<host>:<rcon-port> --token-file <file holding the RCON password>
```

It writes `games/wardogs/testdata/<build>/` (run it from the repository root, or pass `--dir`),
prints what it skipped and how the routes differ from the newest earlier recording, and never
retries a refused token. The repository is public, so the recording replaces every SteamID, the
player names next to them, every IP address and the host's server id with stand-ins, redacts
the secrets, and writes nothing unless a final read of every file finds none of the originals.
It cannot recognise a name in free text that it never saw beside a SteamID: read the files
before committing them. Then `go test ./games/wardogs/...`, and a pull request with the new
directory.

To run the contract against a live server, reading only:

```sh
WARDOGS_BASE_URL=http://<host>:<rcon-port> WARDOGS_TOKEN_FILE=<file> go test -tags live -run Live ./games/wardogs/
```

It fails when the server runs a build with no recording here.

## The recordings in a binary, and the simulator

The recorded builds are embedded in the package (`Recordings`, `Recording`, `NewestRecording`), so
the files under `testdata/` stay the one source and a binary can still serve them.
`wardogsim` is a simulated server for a demo hub (gravel#112). It answers as the newest recording
does, over made-up players with stand-in SteamIDs who join, fight, rejoin and leave on a calibration
profile's curves. Moderation, configuration writes and the three-strike lock work as above, and it
posts its event feed in the captured shape (`wardogsim/feed.go`): from `[WDServerFeed]` on
CL-509546, which takes it over RCON and appends `/api/ingest/events` to its Url; on Season 02,
which takes the feed only from the server's own file and strips the section, from the start
options (the host's file), posting to the Url as given. `GET /v1/feed` says whether it is on. A new
recording here is what the simulator answers as next.

## The event feed

`feed.go` reads a feed batch as the first capture showed it (2026-10-18, Season 02): `{serverId,
serverName, events[]}`, each event with `eventId`, `type`, `eventTime` (the game's clock in
seconds, not a wall-clock time) and its type's own fields, kept whole (`Raw`) for a reader that
knows the type. `serverId` is the host's id for the server (the Join-by-ID); `serverName` is the
file's `EventFeedServerName`. Only `match_state` (`mapName`, `state`) is known: two at a server's
start, `MatchState.PreMatch` then `MatchState.PreMatch.WaitingForWorldLoad`. The feed posts only
when it has events: nothing for hours on an empty server. A kill's fields come from the first session with players: read
the stored batches with `gravel-hub ingest export --server ID --redact`, add them as the adapter's
fixtures (`adapters/wardogs/testdata/<build>/`) and teach `feed.go` the type.
