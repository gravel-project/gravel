package matchzy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// The names of the events this package reads into a type of its own (Events.cs). MatchZy 0.9.1
// sends more (round_start, player_death, bomb_planted, bomb_defused, backup_loaded, game_paused,
// game_unpaused); Parse returns those as Unknown until the hub has a use for them.
const (
	EventSeriesStart      = "series_start"
	EventGoingLive        = "going_live"
	EventRoundEnd         = "round_end"
	EventMapResult        = "map_result"
	EventSeriesEnd        = "series_end"
	EventDemoUploadEnded  = "demo_upload_ended"
	EventMapPicked        = "map_picked"
	EventMapVetoed        = "map_vetoed"
	EventSidePicked       = "side_picked"
	EventPlayerDisconnect = "player_disconnect"
)

// The headers MatchZy sets on a demo upload (UploadFileAsync in Utility.cs), a POST of the .dem
// file as application/octet-stream to matchzy_demo_upload_url. It sends each again under a
// Get5- prefix for Get5's panel; these are the ones a reader should prefer.
const (
	HeaderFileName    = "MatchZy-FileName"
	HeaderMatchID     = "MatchZy-MatchId"
	HeaderMapNumber   = "MatchZy-MapNumber"
	HeaderRoundNumber = "MatchZy-RoundNumber"
)

// The values of a Winner's Team, and of the team fields of the veto events.
const (
	Team1    = "team1"
	Team2    = "team2"
	TeamNone = "none"
)

// The values of a Winner's Side: CS2's team numbers as strings, not the "ct" and "t" a Player's
// side and side_picked use. A drawn map or series has SideNone.
const (
	SideT    = "2"
	SideCT   = "3"
	SideNone = "0"
)

// Event is what every event carries: its name, and the match it belongs to. MatchID is the
// config's matchid for a loaded match; with no match loaded it is the id MatchZy's own database
// gave the pug, and -1 before it has one.
type Event struct {
	Name    string `json:"event"`
	MatchID int64  `json:"matchid"`
}

// Team names a team in series_start: the config's team id (empty when it gave none) and its name
// as MatchZy kept it (see ConfigTeam.Name).
type Team struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Winner is who won a round, a map or a series. Side is SideT, SideCT or SideNone; Team is Team1,
// Team2 or TeamNone (a draw, or a series an admin ended with no winner).
type Winner struct {
	Side string `json:"side"`
	Team string `json:"team"`
}

// StatsTeam is a team with its scores and players (MatchZyStatsTeam). In round_end, SeriesScore,
// ScoreCT and ScoreT are always 0 and Players holds every player on the team's side with their
// totals for the map so far. In map_result, Players is always empty and ScoreCT and ScoreT are 0:
// a map's player stats are the last round_end's.
type StatsTeam struct {
	ID          string        `json:"id"`
	Name        string        `json:"name"`
	SeriesScore int           `json:"series_score"`
	Score       int           `json:"score"`
	ScoreCT     int           `json:"score_ct"`
	ScoreT      int           `json:"score_t"`
	Players     []StatsPlayer `json:"players"`
}

// StatsPlayer is one player's totals for the map (StatsPlayer in MatchData.cs).
type StatsPlayer struct {
	// SteamID is the player's SteamID64 as a string.
	SteamID string      `json:"steamid"`
	Name    string      `json:"name"`
	Stats   PlayerStats `json:"stats"`
}

// PlayerStats is every field of MatchZy's PlayerStats. MatchZy 0.9.1 fills only some of them
// (GetPlayerStatsDict in Utility.cs): kills, deaths, assists, damage, utility_damage,
// enemies_flashed, headshot_kills, rounds_played, bomb_defuses, bomb_plants, 2k to 5k, 1v1, 1v2,
// score and mvp. The rest are always 0 and must not be read as a count of nothing.
type PlayerStats struct {
	Kills             int `json:"kills"`
	Deaths            int `json:"deaths"`
	Assists           int `json:"assists"`
	FlashAssists      int `json:"flash_assists"`
	TeamKills         int `json:"team_kills"`
	Suicides          int `json:"suicides"`
	Damage            int `json:"damage"`
	UtilityDamage     int `json:"utility_damage"`
	EnemiesFlashed    int `json:"enemies_flashed"`
	FriendliesFlashed int `json:"friendlies_flashed"`
	KnifeKills        int `json:"knife_kills"`
	HeadshotKills     int `json:"headshot_kills"`
	RoundsPlayed      int `json:"rounds_played"`
	BombDefuses       int `json:"bomb_defuses"`
	BombPlants        int `json:"bomb_plants"`
	Kills1            int `json:"1k"`
	Kills2            int `json:"2k"`
	Kills3            int `json:"3k"`
	Kills4            int `json:"4k"`
	Kills5            int `json:"5k"`
	OneV1             int `json:"1v1"`
	OneV2             int `json:"1v2"`
	OneV3             int `json:"1v3"`
	OneV4             int `json:"1v4"`
	OneV5             int `json:"1v5"`
	FirstKillsT       int `json:"first_kills_t"`
	FirstKillsCT      int `json:"first_kills_ct"`
	FirstDeathsT      int `json:"first_deaths_t"`
	FirstDeathsCT     int `json:"first_deaths_ct"`
	TradeKills        int `json:"trade_kills"`
	KAST              int `json:"kast"`
	Score             int `json:"score"`
	MVPs              int `json:"mvp"`
}

// Player is a player in the live events (MatchZyPlayer, Get5's Get5Player).
type Player struct {
	// SteamID is the SteamID64 as a string, or "BOT-<user id>" for a bot.
	SteamID string `json:"steamid"`
	Name    string `json:"name"`
	UserID  int    `json:"user_id"`
	// Side is "ct", "t" or "spec"; MatchZy sends null for a player on no team, read as "".
	Side  string `json:"side"`
	IsBot bool   `json:"is_bot"`
}

// SeriesStart is sent when a match config is loaded.
type SeriesStart struct {
	Event
	Team1   Team `json:"team1"`
	Team2   Team `json:"team2"`
	NumMaps int  `json:"num_maps"`
}

// GoingLive is sent when a map goes live. MapNumber counts from 0, as in every event.
type GoingLive struct {
	Event
	MapNumber int `json:"map_number"`
}

// RoundEnd is sent after each live round.
type RoundEnd struct {
	Event
	MapNumber int `json:"map_number"`
	// RoundNumber is the rounds played when the round started: 0 for the first (since 0.9.1).
	RoundNumber int `json:"round_number"`
	// RoundTime is the milliseconds from the end of freeze time to the end of the round.
	RoundTime int `json:"round_time"`
	// Reason is CS2's round end reason (EventRoundEnd.Reason), as a number.
	Reason int       `json:"reason"`
	Winner Winner    `json:"winner"`
	Team1  StatsTeam `json:"team1"`
	Team2  StatsTeam `json:"team2"`
}

// MapResult is sent when a map ends. Team1 and Team2 carry the map score and the series score
// with this map counted; their Players are empty (see StatsTeam).
type MapResult struct {
	Event
	MapNumber int       `json:"map_number"`
	Winner    Winner    `json:"winner"`
	Team1     StatsTeam `json:"team1"`
	Team2     StatsTeam `json:"team2"`
}

// SeriesEnd is sent about two seconds after the last map's MapResult, or when an admin ends the
// series.
type SeriesEnd struct {
	Event
	Winner           Winner `json:"winner"`
	Team1SeriesScore int    `json:"team1_series_score"`
	Team2SeriesScore int    `json:"team2_series_score"`
	// TimeUntilRestore is in seconds; MatchZy always sends 10.
	TimeUntilRestore int `json:"time_until_restore"`
}

// DemoUploadEnded is declared by MatchZy 0.9.1 but never sent by it: a demo upload's outcome is
// only in the server's log. It is read here so a later MatchZy that sends it needs no change.
type DemoUploadEnded struct {
	Event
	MapNumber int    `json:"map_number"`
	Filename  string `json:"filename"`
	Success   bool   `json:"success"`
}

// MapPicked is sent when a team picks a map in the veto; MapNumber is the map's place in the
// series, from 0.
type MapPicked struct {
	Event
	Team      string `json:"team"`
	MapName   string `json:"map_name"`
	MapNumber int    `json:"map_number"`
}

// MapVetoed is sent when a team bans a map in the veto.
type MapVetoed struct {
	Event
	Team    string `json:"team"`
	MapName string `json:"map_name"`
}

// SidePicked is sent when a team picks its starting side on a map in the veto. Side is "ct" or
// "t".
type SidePicked struct {
	Event
	Team      string `json:"team"`
	MapName   string `json:"map_name"`
	MapNumber int    `json:"map_number"`
	Side      string `json:"side"`
}

// PlayerDisconnect is sent when a player (never a bot or GOTV) leaves while a match is loaded.
type PlayerDisconnect struct {
	Event
	Player Player `json:"player"`
}

// Unknown is an event this package has no type for. Body is the event as received, so a caller
// can still store it.
type Unknown struct {
	Event
	Body json.RawMessage `json:"-"`
}

var decoders = map[string]func([]byte) (any, error){
	EventSeriesStart:      decode[SeriesStart],
	EventGoingLive:        decode[GoingLive],
	EventRoundEnd:         decode[RoundEnd],
	EventMapResult:        decode[MapResult],
	EventSeriesEnd:        decode[SeriesEnd],
	EventDemoUploadEnded:  decode[DemoUploadEnded],
	EventMapPicked:        decode[MapPicked],
	EventMapVetoed:        decode[MapVetoed],
	EventSidePicked:       decode[SidePicked],
	EventPlayerDisconnect: decode[PlayerDisconnect],
}

func decode[T any](body []byte) (any, error) {
	var v T
	if err := json.Unmarshal(body, &v); err != nil {
		return nil, err
	}
	return v, nil
}

// ErrNoEventName is returned by Parse for a JSON object with no "event".
var ErrNoEventName = errors.New("matchzy: event has no name")

// Parse reads one event as MatchZy POSTs it. It returns the envelope and the event as the
// package's type for its name (a SeriesStart, a RoundEnd and so on, as values), or as an Unknown
// for a name it has no type for, which is not an error: MatchZy adds events between releases.
// Fields the package does not know are ignored. It fails on a body that is not a JSON object, on
// an object without an event name, and on a known event whose fields have the wrong types.
func Parse(body []byte) (Event, any, error) {
	var env Event
	if err := json.Unmarshal(body, &env); err != nil {
		return Event{}, nil, fmt.Errorf("matchzy: event: %w", err)
	}
	if env.Name == "" {
		return Event{}, nil, ErrNoEventName
	}
	dec, ok := decoders[env.Name]
	if !ok {
		return env, Unknown{Event: env, Body: bytes.Clone(body)}, nil
	}
	v, err := dec(body)
	if err != nil {
		return env, nil, fmt.Errorf("matchzy: %s: %w", env.Name, err)
	}
	return env, v, nil
}
