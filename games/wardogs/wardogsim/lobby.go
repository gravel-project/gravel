package wardogsim

import (
	"encoding/json"
	"fmt"
	"io/fs"

	"github.com/gravel-project/gravel/games/wardogs"
)

// Lobby is what a recorded build says a match looks like, for a history that should read like
// the simulator's: the rotation's maps as the status names them in play, and the factions.
type Lobby struct {
	// Maps are the rotation's entries in order, by the name the status reports ("Ozeti").
	Maps     []string
	Factions []wardogs.FactionScore
	// MinPlayers is the document's MinimumRequiredPlayers.
	MinPlayers int
}

// RecordedLobby reads a recorded build's lobby; "" is the newest build.
func RecordedLobby(build string) (Lobby, error) {
	var files fs.FS
	var err error
	if build == "" {
		_, files, err = wardogs.NewestRecording()
	} else {
		files, err = wardogs.Recording(build)
	}
	if err != nil {
		return Lobby{}, fmt.Errorf("wardogsim: %w", err)
	}
	var cfg wardogs.ConfigDocument
	var status wardogs.Status
	for name, out := range map[string]any{"v1/config.json": &cfg, "v1/status.json": &status} {
		b, err := fs.ReadFile(files, name)
		if err != nil {
			return Lobby{}, fmt.Errorf("wardogsim: %w", err)
		}
		if err := json.Unmarshal(b, out); err != nil {
			return Lobby{}, fmt.Errorf("wardogsim: %s: %w", name, err)
		}
	}
	set := read(wardogs.ParseDoc(cfg.Text))
	l := Lobby{Factions: status.Factions, MinPlayers: set.minPlayers}
	for _, e := range set.rotation {
		l.Maps = append(l.Maps, e.level())
	}
	if len(l.Maps) == 0 {
		l.Maps = []string{status.Map}
	}
	return l, nil
}
