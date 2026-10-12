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

// BootMarker is the file the first-boot script leaves when the game's unit is installed: the hub
// reads it over SSH to know the host has booted. It is on the root disk, which is new with every
// instance. In the runtime user's home it was on the data volume, so a replacement instance
// showed the previous one's marker and was taken as booted before its own first boot had installed
// the unit (HTG's first warm start, 2026-10-12: "booting" in under a second).
const BootMarker = "/var/lib/gravel/boot-done"

// Document is the cloud-init document a server's instance boots with: vmdoc's host, the game's
// unit staged and installed by the first-boot tail. It holds no secret. keys are the hub's public
// key and the location's extra keys. reservedEgress is the provider's script that makes outbound
// traffic leave from a reserved address (cloud.Provider.ReservedEgressScript); for a server with a
// reserved address it is installed as a boot service and a NetworkManager hook.
func Document(s ServerSpec, gameName, dataDevice string, keys []string, reservedEgress string) (string, error) {
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
	files := []vmdoc.File{{Path: "/usr/local/share/gravel/" + s.Game + ".container", Mode: "0644", Content: unit.String()}}
	if s.AddressReserved && reservedEgress != "" {
		var svc, hook bytes.Buffer
		if err := tmpl.ExecuteTemplate(&svc, "reserved-egress.service.tmpl", data); err != nil {
			return "", fmt.Errorf("cloudvm: egress unit: %w", err)
		}
		if err := tmpl.ExecuteTemplate(&hook, "reserved-egress-dispatcher.sh.tmpl", data); err != nil {
			return "", fmt.Errorf("cloudvm: egress hook: %w", err)
		}
		files = append(files,
			vmdoc.File{Path: "/usr/local/libexec/gravel-reserved-egress", Mode: "0755", Content: reservedEgress},
			vmdoc.File{Path: "/etc/systemd/system/gravel-reserved-egress.service", Mode: "0644", Content: svc.String()},
			vmdoc.File{Path: "/etc/NetworkManager/dispatcher.d/90-gravel-reserved-egress", Mode: "0755", Content: hook.String()})
		if err := tmpl.ExecuteTemplate(&tail, "reserved-egress-tail.sh.tmpl", data); err != nil {
			return "", fmt.Errorf("cloudvm: egress first boot: %w", err)
		}
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
		Files:         files,
		FirstBootTail: tail.String(),
	})
}
