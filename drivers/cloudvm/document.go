package cloudvm

import (
	"bytes"
	"embed"
	"fmt"
	"regexp"
	"text/template"

	"github.com/gravel-project/gravel/internal/vmdoc"
)

//go:embed templates/*.tmpl
var templates embed.FS

var tmpl = template.Must(template.New("").ParseFS(templates, "templates/*.tmpl"))

var gameRE = regexp.MustCompile(`^[a-z][a-z0-9]{0,31}$`)

// BootMarker is the file the first-boot script leaves in the runtime user's home when the game's
// unit is installed: the hub reads it over SSH to know the host has booted.
const BootMarker = ".gravel-boot-done"

// Document is the cloud-init document a server's instance boots with: vmdoc's host, the game's
// unit staged and installed by the first-boot tail. It holds no secret. keys are the hub's public
// key and the location's extra keys.
func Document(s ServerSpec, gameName, dataDevice string, keys []string) (string, error) {
	if !gameRE.MatchString(s.Game) {
		return "", fmt.Errorf("cloudvm: game id %q cannot name a unit", s.Game)
	}
	data := struct {
		ServerID, Game, GameName, Image, InstallDir string
		RuntimeUID                                  int
	}{s.ID, s.Game, gameName, s.Profile.Image, s.Profile.InstallDir, s.Profile.RuntimeUID}
	var unit, tail bytes.Buffer
	if err := tmpl.ExecuteTemplate(&unit, "game.container.tmpl", data); err != nil {
		return "", fmt.Errorf("cloudvm: unit: %w", err)
	}
	if err := tmpl.ExecuteTemplate(&tail, "game-tail.sh.tmpl", data); err != nil {
		return "", fmt.Errorf("cloudvm: first boot: %w", err)
	}
	return vmdoc.Render(vmdoc.Params{
		Header: "# A gravel game host (the cloud-VM driver, ADR-0003) for server " + s.ID + ", made by the hub.\n" +
			"# Anything on the instance can read this document from the metadata service, so it holds no\n" +
			"# secret: the hub pushes the game's environment file over SSH once the host is up.",
		FirstBootComment: "# gravel game host for " + s.ID + ", on the first boot of every instance (cloud-init runcmd runs\n" +
			"# once per instance, so also on one that replaces an earlier instance on the same volume).",
		Hostname:      InstanceName(s.ID),
		SSHKeys:       keys,
		DataDevice:    dataDevice,
		Files:         []vmdoc.File{{Path: "/usr/local/share/gravel/" + s.Game + ".container", Mode: "0644", Content: unit.String()}},
		FirstBootTail: tail.String(),
	})
}
