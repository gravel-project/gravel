# MatchZy fixtures

`events/` is hand-written from MatchZy 0.9.1's C# source (Events.cs and MatchData.cs for the
property names, the code that fills each event for the values it can hold), to be replaced by a
live capture from a server running it. It mirrors System.Text.Json's default output: every
property written, `null` included, and non-ASCII characters and `'` escaped as `\uXXXX`. The
server sends each event on one line; the files are indented to be read, and property order is
not significant. `player_death.json` is an event the package has no type for.

`config/listed.json` and `config/unlisted.json` are golden files of `Config.MarshalJSON`
(`go test -update` rewrites them). `config/get5_style.json` is a config written the other ways
MatchZy accepts (MatchManagement.cs, MatchConfigJson.cs), for `Config.UnmarshalJSON`.
