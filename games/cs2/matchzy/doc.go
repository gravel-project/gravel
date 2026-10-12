// Package matchzy holds the wire formats of MatchZy (github.com/shobhit-pathak/MatchZy), the
// CounterStrikeSharp plugin that runs a Counter-Strike 2 match: the events it POSTs as JSON to
// matchzy_remote_log_url (Parse), the match config it loads with matchzy_loadmatch (Config), and
// the headers it sends with a demo upload. The formats follow MatchZy 0.9.1's C# source, which
// copies Get5's: Events.cs and MatchData.cs for the events, MatchManagement.cs and
// MatchConfigJson.cs for the config, Utility.cs for the upload headers.
//
// It imports nothing of gravel's: at the open-source cut it becomes a module of its own.
package matchzy
