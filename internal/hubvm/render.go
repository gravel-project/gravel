// Package hubvm makes the hub's own host (ADR-0020): the cloud-init document a hub VM boots with,
// and the create, status and destroy steps the gravel CLI runs through a cloud.Provider. The hub
// is never a ManagedServer: nothing here runs inside the hub, and nothing in the hub calls it.
//
// The document has two profiles. "host" is the machine and nothing more: an admin user for the
// OS, a rootless runtime user whose home is the data volume, podman, and automatic security
// updates; an operator's own configuration management installs the hub on top (Hidden Token
// Gaming's Ansible does). "stock" is the host plus gravel's own units from deploy/quadlet, with
// the database secrets generated on the host at first boot, so one command gives a running hub.
package hubvm

import (
	"bytes"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"slices"
	"strings"
	"text/template"

	"github.com/gravel-project/gravel/deploy"
	"github.com/gravel-project/gravel/internal/vmdoc"
)

// Profiles.
const (
	ProfileHost  = "host"
	ProfileStock = "stock"
)

// The runtime user's fixed identity (vmdoc's: the hub's host and a game VM share it).
const (
	DefaultUser       = vmdoc.DefaultUser
	DefaultUID        = vmdoc.DefaultUID
	DefaultSubIDStart = vmdoc.DefaultSubIDStart
)

// BasePackages are installed on every hub host: vmdoc's, and git and jq for an operator's
// configuration management.
var BasePackages = append(slices.Clone(vmdoc.BasePackages[:1]), "git", "jq", vmdoc.BasePackages[1])

//go:embed templates/*.tmpl
var templates embed.FS

// Params are what a hub host's cloud-init document is rendered from.
type Params struct {
	Profile  string
	Hostname string
	// SSHKeys are authorized for both the admin and the runtime user.
	SSHKeys []string
	// DataDevice is the stable path the instance sees the data volume at
	// (cloud.Provider.VolumeDevice).
	DataDevice    string
	ExtraPackages []string
	// User, UID and SubIDStart default to DefaultUser, DefaultUID and DefaultSubIDStart.
	User       string
	UID        int
	SubIDStart int
	// Stock is required for the stock profile and ignored otherwise.
	Stock *StockParams
}

// StockParams configure the stock profile's hub.
type StockParams struct {
	// PublicURL is the origin members use (auth.base_url), https://… behind a tunnel.
	PublicURL string
	// OrgName is the organization's display name.
	OrgName string
	// HubImage overrides the image deploy/quadlet/gravel-hub.container pins; empty keeps the pin.
	HubImage string
	// BehindTunnel makes the hub trust CF-Connecting-IP for the client address.
	BehindTunnel bool
}

var (
	imageRE  = regexp.MustCompile(`^[a-z0-9][a-z0-9./_-]*(:[A-Za-z0-9._-]+)?(@sha256:[0-9a-f]{64})?$`)
	originRE = regexp.MustCompile(`^https?://[A-Za-z0-9.-]+(:[0-9]+)?$`)
)

// Render returns the cloud-init document for the parameters. It holds no secret: the parameters
// cannot carry one.
func Render(p Params) (string, error) {
	if err := p.normalize(); err != nil {
		return "", err
	}
	t, err := template.New("").ParseFS(templates, "templates/*.tmpl")
	if err != nil {
		return "", fmt.Errorf("hubvm: templates: %w", err)
	}
	doc := p.base()
	if p.Profile == ProfileStock {
		if doc.Files, err = stockFiles(t, p); err != nil {
			return "", err
		}
		if doc.FirstBootTail, err = execute(t, "stock-tail.sh.tmpl", p); err != nil {
			return "", err
		}
	}
	return vmdoc.Render(doc)
}

// base is the shared document's parameters for a hub host, without the stock profile's parts.
func (p Params) base() vmdoc.Params {
	return vmdoc.Params{
		Header: `# A gravel hub host (ADR-0020), rendered by ` + "`gravel hub create` or `gravel hub cloud-init`" + `.
# Profile: ` + p.Profile + `. Anything on the instance can read this document from the metadata
# service, so it holds no secret: the stock profile's secrets are generated on the host at first
# boot, and anything else (a tunnel token) is pushed over SSH afterwards.`,
		FirstBootComment: `# gravel hub host, every boot of an instance (cloud-init runcmd runs once per instance, so this
# runs on the first boot of each new instance, including one that replaces an earlier instance
# on the same volume). Profile: ` + p.Profile + `.`,
		FirstBootVars: `# Two markers on the volume: one for the home's setup, one for the stock install, so a boot that
# failed halfway through the install never relabels podman's storage when it tries again.
home_marker=$home/.gravel-home
stock_marker=$home/.gravel-stock`,
		Hostname: p.Hostname, SSHKeys: p.SSHKeys, DataDevice: p.DataDevice,
		User: p.User, UID: p.UID, SubIDStart: p.SubIDStart, Packages: packages(p.ExtraPackages),
	}
}

func (p *Params) normalize() error {
	switch p.Profile {
	case ProfileHost:
		p.Stock = nil
	case ProfileStock:
		if p.Stock == nil {
			return fmt.Errorf("hubvm: the stock profile needs its public URL")
		}
		if !originRE.MatchString(p.Stock.PublicURL) {
			return fmt.Errorf("hubvm: public URL %q must be an http(s) origin with no path, like https://hub.example.com", p.Stock.PublicURL)
		}
		if p.Stock.HubImage != "" && !imageRE.MatchString(p.Stock.HubImage) {
			return fmt.Errorf("hubvm: hub image %q is not an image reference", p.Stock.HubImage)
		}
		if p.Stock.OrgName == "" {
			p.Stock.OrgName = "gravel"
		}
		if strings.ContainsAny(p.Stock.OrgName, "\n\r") {
			return fmt.Errorf("hubvm: the organization name is one line")
		}
	default:
		return fmt.Errorf("hubvm: profile %q: want %s or %s", p.Profile, ProfileHost, ProfileStock)
	}
	// The shared checks and defaults are vmdoc's; copy back what it filled in.
	d := p.base()
	if err := d.Normalize(); err != nil {
		return err
	}
	p.SSHKeys, p.User, p.UID, p.SubIDStart = d.SSHKeys, d.User, d.UID, d.SubIDStart
	return nil
}

// packages is the base set plus the extra ones, without duplicates, in a stable order.
func packages(extra []string) []string {
	out := slices.Clone(BasePackages)
	for _, p := range extra {
		if !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out
}

// stockFiles are gravel's own units, the hub's configuration and the drop-in the stock profile
// adds, staged under /usr/local/share/gravel for first-boot to install into the user's home.
func stockFiles(t *template.Template, p Params) ([]vmdoc.File, error) {
	var files []vmdoc.File
	err := fs.WalkDir(deploy.Quadlet, "quadlet", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(deploy.Quadlet, name)
		if err != nil {
			return err
		}
		files = append(files, vmdoc.File{Path: "/usr/local/share/gravel/quadlet/" + path.Base(name), Mode: "0644", Content: string(b)})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("hubvm: stock units: %w", err)
	}
	hubYAML, err := execute(t, "hub.yaml.tmpl", p.Stock)
	if err != nil {
		return nil, err
	}
	files = append(files,
		vmdoc.File{Path: "/usr/local/share/gravel/hub.yaml", Mode: "0644", Content: hubYAML},
		vmdoc.File{Path: "/usr/local/share/gravel/gravel-hub-vm.conf", Mode: "0644", Content: dropIn(p.Stock)},
	)
	return files, nil
}

// dropIn is what the stock profile changes in gravel-hub.container without editing it: the
// pseudonym key as a secret (deploy/hub.yaml's inline key is for development only) and, when
// asked, another image.
func dropIn(s *StockParams) string {
	var b strings.Builder
	b.WriteString("# Written by gravel hub create --profile stock (ADR-0020).\n[Container]\nSecret=gravel-stats-pseudonym-key\n")
	if s.HubImage != "" {
		// GRAVEL_IMAGE follows Image=, so /healthz names what runs (gravel#214).
		b.WriteString("Image=" + s.HubImage + "\nEnvironment=GRAVEL_IMAGE=" + s.HubImage + "\n")
	}
	return b.String()
}

func execute(t *template.Template, name string, data any) (string, error) {
	var b bytes.Buffer
	if err := t.ExecuteTemplate(&b, name, data); err != nil {
		return "", fmt.Errorf("hubvm: %s: %w", name, err)
	}
	return b.String(), nil
}

// indent is vmdoc.Indent, kept for this package's tests.
func indent(n int, s string) string { return vmdoc.Indent(n, s) }
