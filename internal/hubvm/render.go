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
)

// Profiles.
const (
	ProfileHost  = "host"
	ProfileStock = "stock"
)

// The runtime user's fixed identity. The data volume's files belong to these ids, so every
// instance that mounts the volume must give the user the same ones.
const (
	DefaultUser       = "gravel"
	DefaultUID        = 1100
	DefaultSubIDStart = 524288
)

// BasePackages are installed on every hub host.
var BasePackages = []string{"podman", "git", "jq", "dnf5-plugin-automatic"}

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
	hostnameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	userRE     = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
	packageRE  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]*$`)
	imageRE    = regexp.MustCompile(`^[a-z0-9][a-z0-9./_-]*(:[A-Za-z0-9._-]+)?(@sha256:[0-9a-f]{64})?$`)
	originRE   = regexp.MustCompile(`^https?://[A-Za-z0-9.-]+(:[0-9]+)?$`)
	keyRE      = regexp.MustCompile(`^(ssh-ed25519|ssh-rsa|ecdsa-sha2-nistp(256|384|521)|sk-ssh-ed25519@openssh\.com|sk-ecdsa-sha2-nistp256@openssh\.com) [A-Za-z0-9+/=]+( [^\n]*)?$`)
)

// file is one file the document writes outside the user's home.
type file struct {
	Path    string
	Mode    string
	Content string
}

// Render returns the cloud-init document for the parameters. It holds no secret: the parameters
// cannot carry one.
func Render(p Params) (string, error) {
	if err := p.normalize(); err != nil {
		return "", err
	}
	t, err := template.New("").Funcs(template.FuncMap{"indent": indent}).ParseFS(templates, "templates/*.tmpl")
	if err != nil {
		return "", fmt.Errorf("hubvm: templates: %w", err)
	}
	firstBoot, err := execute(t, "first-boot.sh.tmpl", p)
	if err != nil {
		return "", err
	}
	var files []file
	if p.Profile == ProfileStock {
		files, err = stockFiles(t, p)
		if err != nil {
			return "", err
		}
	}
	return execute(t, "cloud-init.yaml.tmpl", struct {
		Params
		Packages  []string
		FirstBoot string
		Files     []file
	}{p, packages(p.ExtraPackages), firstBoot, files})
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
	if !hostnameRE.MatchString(p.Hostname) {
		return fmt.Errorf("hubvm: hostname %q: lower-case letters, digits and hyphens, at most 63", p.Hostname)
	}
	if len(p.SSHKeys) == 0 {
		return fmt.Errorf("hubvm: at least one SSH public key is needed (no password logins)")
	}
	p.SSHKeys = slices.Clone(p.SSHKeys) // trimmed below; the caller's slice stays as it was
	for i, k := range p.SSHKeys {
		k = strings.TrimSpace(k)
		if !keyRE.MatchString(k) {
			return fmt.Errorf("hubvm: SSH key %d is not an OpenSSH public key", i+1)
		}
		p.SSHKeys[i] = k
	}
	if !strings.HasPrefix(p.DataDevice, "/dev/") || strings.ContainsAny(p.DataDevice, " \t\n\"'") {
		return fmt.Errorf("hubvm: data device %q must be a /dev path", p.DataDevice)
	}
	for _, pkg := range p.ExtraPackages {
		if !packageRE.MatchString(pkg) {
			return fmt.Errorf("hubvm: package %q is not a package name", pkg)
		}
	}
	if p.User == "" {
		p.User = DefaultUser
	}
	if !userRE.MatchString(p.User) || p.User == "admin" || p.User == "root" {
		return fmt.Errorf("hubvm: runtime user %q is not allowed", p.User)
	}
	if p.UID == 0 {
		p.UID = DefaultUID
	}
	if p.UID < 1000 || p.UID > 60000 {
		return fmt.Errorf("hubvm: uid %d: want a regular user id, 1000–60000", p.UID)
	}
	if p.SubIDStart == 0 {
		p.SubIDStart = DefaultSubIDStart
	}
	if p.SubIDStart < 100000 {
		return fmt.Errorf("hubvm: subordinate ids start at %d: want at least 100000", p.SubIDStart)
	}
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
func stockFiles(t *template.Template, p Params) ([]file, error) {
	var files []file
	err := fs.WalkDir(deploy.Quadlet, "quadlet", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(deploy.Quadlet, name)
		if err != nil {
			return err
		}
		files = append(files, file{Path: "/usr/local/share/gravel/quadlet/" + path.Base(name), Mode: "0644", Content: string(b)})
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
		file{Path: "/usr/local/share/gravel/hub.yaml", Mode: "0644", Content: hubYAML},
		file{Path: "/usr/local/share/gravel/gravel-hub-vm.conf", Mode: "0644", Content: dropIn(p.Stock)},
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
		b.WriteString("Image=" + s.HubImage + "\n")
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

// indent prefixes every non-empty line with n spaces, for a block scalar in YAML.
func indent(n int, s string) string {
	pad := strings.Repeat(" ", n)
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		if l != "" {
			lines[i] = pad + l
		}
	}
	return strings.Join(lines, "\n")
}
