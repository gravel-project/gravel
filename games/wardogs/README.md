# games/wardogs

A Go client for the War Dogs dedicated server's RCON API (plain HTTP, a bearer token, JSON, and a
configuration document in INI form), with no gravel imports: it becomes a standalone module at
the open-source cut (ADR-0001). gravel's driver adapts it; ADR-0010 is the design.

War Dogs is in Early Access and its API changes every few weeks, so the client assumes no route.
It reads `GET /v1/capabilities`, matches the advertised routes by shape (`{id}` and `{steamId}`
are the same route), and fails a call whose route is gone with `ErrNotSupported` before sending
anything. Parsing is tolerant in production (a field whose type drifted is reported through
`Options.OnMismatch` and the rest of the answer is kept) and strict in the tests.

The server's rules the client keeps:

| Rule | What the client does |
|---|---|
| Three requests with a missing or wrong token lock every protected route behind a 429 for a while | One token. After a 401 it sends nothing protected until `SetToken`; protected requests are serialised until the token has been accepted once, so concurrent callers spend one strike, not one each. No token means no protected request at all. |
| A 429 carries `Retry-After` (1–60 s) | Nothing is sent until it has passed (`RateLimitedError`). |
| 64 KiB request bodies; 600 requests a minute per address | A larger body is refused before sending (`ErrBodyTooLarge`); the rate is the caller's budget. |
| `GET /v1/config` returns the RCON password in clear | `RedactConfig` blanks the password and its hash, the feed's token and URL and the join password, keeping the CRLF line endings. |
| A write replaces the whole document, `If-Match` on its revision | `PutConfig` sends CRLF and a quoted `If-Match`, and returns the server's result with `ErrRevisionMismatch` (412) or `ErrConfigRejected` (422). |

## Fixtures: record, diff, update

`testdata/<build>/` holds one recorded build: every GET route it advertises, as files that mirror
the route paths, and `recording.json` saying what was recorded and skipped. The tests run the
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
posts kill batches to `[WDServerFeed]` in a shape that is a guess until one is captured
(`wardogsim/feed.go`). A new recording here is what the simulator answers as next.
