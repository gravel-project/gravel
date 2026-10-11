// Package vmdoc renders the cloud-init document every VM gravel makes boots with: the hub's own
// host (internal/hubvm, ADR-0020) and the hosts the cloud-VM driver runs game servers on
// (drivers/cloudvm, ADR-0003). The document is one shape for both: an admin user for the OS, a
// rootless runtime user whose home is the data volume (formatted on its first boot only, with a
// fixed uid and subordinate ids, so a replacement instance on the same volume comes back as it
// was), podman, automatic security updates, and a first-boot script. A caller adds its own files,
// staged outside the home, and the tail of the first-boot script that installs them.
//
// The document holds no secret: anything on the instance can read it from the metadata service,
// and the cloud keeps it for the instance's life. Secrets are made on the host or pushed over SSH.
package vmdoc

import (
	"bytes"
	"embed"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"text/template"
)

// The runtime user's fixed identity. The data volume's files belong to these ids, so every
// instance that mounts the volume must give the user the same ones.
const (
	DefaultUser       = "gravel"
	DefaultUID        = 1100
	DefaultSubIDStart = 524288
)

// BasePackages are on every gravel VM: podman for the containers, dnf-automatic for the updates.
var BasePackages = []string{"podman", "dnf5-plugin-automatic"}

// DefaultFirstBootVars is the first-boot script's variables when the caller names none: the
// marker the home's one-time setup leaves on the volume.
const DefaultFirstBootVars = "home_marker=$home/.gravel-home"

// FirstBootPath is where the document writes the first-boot script.
const FirstBootPath = "/usr/local/libexec/gravel/first-boot"

//go:embed templates/*.tmpl
var templates embed.FS

// Params are what a document is rendered from.
type Params struct {
	// Header is comment lines (each starting with "#") under #cloud-config: who rendered the
	// document and what it is for.
	Header   string
	Hostname string
	// SSHKeys are authorized for both the admin and the runtime user.
	SSHKeys []string
	// DataDevice is the stable path the instance sees the data volume at
	// (cloud.Provider.VolumeDevice).
	DataDevice string
	// User, UID and SubIDStart default to DefaultUser, DefaultUID and DefaultSubIDStart.
	User       string
	UID        int
	SubIDStart int
	// Packages are installed at first boot; empty is BasePackages.
	Packages []string
	// Files are written outside the user's home (under /usr/local/share/gravel, by convention);
	// the first-boot tail installs them into it.
	Files []File
	// FirstBootComment is comment lines under the script's shebang.
	FirstBootComment string
	// FirstBootVars is shell lines after user and home are set; they must set home_marker.
	// Empty is DefaultFirstBootVars.
	FirstBootVars string
	// FirstBootTail is shell run as root after the home is set up and as_user (run a command as
	// the runtime user, in their systemd session) is defined. It runs on the first boot of every
	// instance, so it must be idempotent by markers of its own.
	FirstBootTail string
}

// File is one file the document writes.
type File struct {
	Path    string
	Mode    string
	Content string
}

var (
	hostnameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	userRE     = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
	packageRE  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]*$`)
	pathRE     = regexp.MustCompile(`^/[A-Za-z0-9._/-]+$`)
	modeRE     = regexp.MustCompile(`^0[0-7]{3}$`)
	keyRE      = regexp.MustCompile(`^(ssh-ed25519|ssh-rsa|ecdsa-sha2-nistp(256|384|521)|sk-ssh-ed25519@openssh\.com|sk-ecdsa-sha2-nistp256@openssh\.com) [A-Za-z0-9+/=]+( [^\n]*)?$`)
)

// ValidPublicKey says whether s is one OpenSSH public key line.
func ValidPublicKey(s string) bool { return keyRE.MatchString(s) }

// Render returns the document for the parameters.
func Render(p Params) (string, error) {
	if err := p.Normalize(); err != nil {
		return "", err
	}
	t, err := template.New("").Funcs(template.FuncMap{"indent": Indent}).ParseFS(templates, "templates/*.tmpl")
	if err != nil {
		return "", fmt.Errorf("vmdoc: templates: %w", err)
	}
	firstBoot, err := execute(t, "first-boot.sh.tmpl", p)
	if err != nil {
		return "", err
	}
	return execute(t, "cloud-init.yaml.tmpl", struct {
		Params
		FirstBoot string
	}{p, firstBoot})
}

// Normalize checks the parameters, trims the keys and fills the defaults. Render calls it; a
// caller that renders its own parts first calls it to see the defaults.
func (p *Params) Normalize() error {
	if !hostnameRE.MatchString(p.Hostname) {
		return fmt.Errorf("vmdoc: hostname %q: lower-case letters, digits and hyphens, at most 63", p.Hostname)
	}
	if err := comment("header", &p.Header); err != nil {
		return err
	}
	if err := comment("first-boot comment", &p.FirstBootComment); err != nil {
		return err
	}
	if len(p.SSHKeys) == 0 {
		return fmt.Errorf("vmdoc: at least one SSH public key is needed (no password logins)")
	}
	p.SSHKeys = slices.Clone(p.SSHKeys) // trimmed below; the caller's slice stays as it was
	for i, k := range p.SSHKeys {
		k = strings.TrimSpace(k)
		if !keyRE.MatchString(k) {
			return fmt.Errorf("vmdoc: SSH key %d is not an OpenSSH public key", i+1)
		}
		p.SSHKeys[i] = k
	}
	if !strings.HasPrefix(p.DataDevice, "/dev/") || strings.ContainsAny(p.DataDevice, " \t\n\"'") {
		return fmt.Errorf("vmdoc: data device %q must be a /dev path", p.DataDevice)
	}
	if len(p.Packages) == 0 {
		p.Packages = slices.Clone(BasePackages)
	}
	for _, pkg := range p.Packages {
		if !packageRE.MatchString(pkg) {
			return fmt.Errorf("vmdoc: package %q is not a package name", pkg)
		}
	}
	for _, f := range p.Files {
		if !pathRE.MatchString(f.Path) || strings.Contains(f.Path, "..") {
			return fmt.Errorf("vmdoc: file path %q must be absolute, with no spaces or quotes", f.Path)
		}
		if !modeRE.MatchString(f.Mode) {
			return fmt.Errorf("vmdoc: file %s: mode %q must be octal like 0644", f.Path, f.Mode)
		}
	}
	if p.User == "" {
		p.User = DefaultUser
	}
	if !userRE.MatchString(p.User) || p.User == "admin" || p.User == "root" {
		return fmt.Errorf("vmdoc: runtime user %q is not allowed", p.User)
	}
	if p.UID == 0 {
		p.UID = DefaultUID
	}
	if p.UID < 1000 || p.UID > 60000 {
		return fmt.Errorf("vmdoc: uid %d: want a regular user id, 1000–60000", p.UID)
	}
	if p.SubIDStart == 0 {
		p.SubIDStart = DefaultSubIDStart
	}
	if p.SubIDStart < 100000 {
		return fmt.Errorf("vmdoc: subordinate ids start at %d: want at least 100000", p.SubIDStart)
	}
	p.FirstBootVars = strings.Trim(p.FirstBootVars, "\n")
	if p.FirstBootVars == "" {
		p.FirstBootVars = DefaultFirstBootVars
	}
	if !strings.Contains(p.FirstBootVars, "home_marker=") {
		return fmt.Errorf("vmdoc: the first-boot variables must set home_marker")
	}
	if tail := strings.Trim(p.FirstBootTail, "\n"); tail != "" {
		p.FirstBootTail = "\n\n" + tail
	} else {
		p.FirstBootTail = ""
	}
	return nil
}

// comment trims a block of comment lines and checks every line is one.
func comment(what string, s *string) error {
	*s = strings.Trim(*s, "\n")
	if *s == "" {
		return fmt.Errorf("vmdoc: the %s is required", what)
	}
	for _, l := range strings.Split(*s, "\n") {
		if !strings.HasPrefix(l, "#") {
			return fmt.Errorf("vmdoc: every line of the %s is a comment (starts with #)", what)
		}
	}
	return nil
}

func execute(t *template.Template, name string, data any) (string, error) {
	var b bytes.Buffer
	if err := t.ExecuteTemplate(&b, name, data); err != nil {
		return "", fmt.Errorf("vmdoc: %s: %w", name, err)
	}
	return b.String(), nil
}

// Indent prefixes every non-empty line with n spaces, for a block scalar in YAML.
func Indent(n int, s string) string {
	pad := strings.Repeat(" ", n)
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		if l != "" {
			lines[i] = pad + l
		}
	}
	return strings.Join(lines, "\n")
}
