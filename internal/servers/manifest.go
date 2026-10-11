package servers

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/gravel-project/gravel/cloud"
	"github.com/gravel-project/gravel/drivers/cloudvm"
	"github.com/gravel-project/gravel/internal/vmdoc"
)

// ManifestVersion is the servers.yaml format this hub reads.
const ManifestVersion = 1

// Manifest is servers.yaml: the games a deployment enables and the servers it registers
// (ADR-0010). `gravel-hub servers apply` makes the hub match it; `export` prints it back.
type Manifest struct {
	Version int       `yaml:"version"`
	Games   []GameRef `yaml:"games"`
	// Locations are the clouds the hub makes servers' VMs in (the cloud-VM driver, ADR-0003); a
	// server elsewhere names a free location slug instead.
	Locations []Location `yaml:"locations,omitempty"`
	Servers   []Server   `yaml:"servers"`
}

// Location is a cloud account and region the hub makes VMs in: the provider, its token and the
// SSH key the hub reaches the VMs with (both files, read and never stored, like a credential).
type Location struct {
	ID       string `yaml:"id" json:"id"`
	Provider string `yaml:"provider" json:"provider"`
	Region   string `yaml:"region" json:"region"`
	// Project is the provider's grouping of resources (a DigitalOcean project, by id or name).
	Project    string `yaml:"project,omitempty" json:"project,omitempty"`
	TokenFile  string `yaml:"token_file" json:"token_file"`
	SSHKeyFile string `yaml:"ssh_key_file" json:"ssh_key_file"`
	// Image is the VM's OS image; empty is the provider's default.
	Image string `yaml:"image,omitempty" json:"image,omitempty"`
	// ExtraSSHKeys are operators' public keys authorized on the VMs (break-glass SSH), beside
	// the hub's own.
	ExtraSSHKeys []string `yaml:"extra_ssh_keys,omitempty" json:"extra_ssh_keys,omitempty"`
	// SSHFrom are addresses the VMs' firewalls admit SSH from besides the hub's own.
	SSHFrom []string `yaml:"ssh_from,omitempty" json:"ssh_from,omitempty"`
}

// VMSpec is a cloud-VM server's machine (ADR-0003): its size, its data volume, its address and
// when it is parked. Its secrets are files, read and never stored.
type VMSpec struct {
	// Size is the provider's size slug (DigitalOcean: c-2).
	Size string `yaml:"size" json:"size"`
	// VolumeGiB is the data volume's size; the game's install lives there.
	VolumeGiB int `yaml:"volume_gib" json:"volume_gib"`
	// Address is "reserved" (a reserved address that survives a park or a resize; the default)
	// or "ephemeral" (the instance's own, which changes with it).
	Address string `yaml:"address,omitempty" json:"address,omitempty"`
	// ParkAfter parks the host after the server has been empty this long; zero never parks it.
	ParkAfter Duration `yaml:"park_after,omitempty" json:"park_after,omitempty"`
	// PasswordFile is the password players join with; empty is no password.
	PasswordFile string `yaml:"password_file,omitempty" json:"password_file,omitempty"`
	// PublisherTokenFile is a token the game's publisher issues a server (CS2's Game Server Login
	// Token); empty is none.
	PublisherTokenFile string `yaml:"publisher_token_file,omitempty" json:"publisher_token_file,omitempty"`
}

// Address kinds.
const (
	AddressReserved  = "reserved"
	AddressEphemeral = "ephemeral"
)

// GameRef enables a shipped Game spec, with optional tightenings of its bands.
type GameRef struct {
	ID    string `yaml:"id"`
	Bands []Band `yaml:"bands,omitempty"`
}

// Server is one ManagedServer: a server that exists somewhere and a driver controls. It holds no
// credential: CredentialFile names a file the hub reads at start and on change, and never stores.
type Server struct {
	ID       string `yaml:"id"`
	Name     string `yaml:"name"`
	Game     string `yaml:"game"`
	Driver   string `yaml:"driver"`
	Location string `yaml:"location"`
	// Endpoint is the server's control address; empty for a cloud-VM server, whose hub knows it.
	Endpoint       string   `yaml:"endpoint,omitempty"`
	CredentialFile string   `yaml:"credential_file"`
	PollInterval   Duration `yaml:"poll_interval,omitempty"`
	Trust          string   `yaml:"trust"`
	Feed           *Feed    `yaml:"feed,omitempty"`
	// VM is the machine a cloud-VM server runs on; nil for any other driver.
	VM *VMSpec `yaml:"vm,omitempty"`
	// Settings are the game's settings for this server, as the Game spec's cloudvm section
	// declares them; only for a cloud-VM server.
	Settings map[string]string `yaml:"settings,omitempty"`
}

// Feed is where a server that pushes events posts them, and the token it posts with (ADR-0011).
// The driver writes both into the server's configuration (War Dogs: [WDServerFeed]); the hub's
// ingest route takes the token as the server's. Like the credential, the token is a file the hub
// reads and never stores: its first line is the token, an optional second line the previous one,
// accepted for RotationGrace while the server still has it.
type Feed struct {
	// URL is the origin the server posts to; the game appends its own path (/api/ingest/events).
	URL       string `yaml:"url" json:"url"`
	TokenFile string `yaml:"token_file" json:"token_file"`
}

// Trust levels.
const (
	TrustOfficial  = "official"
	TrustCommunity = "community"
)

// Defaults and limits.
const (
	DefaultPollInterval = 15 * time.Second
	MinPollInterval     = 5 * time.Second
	MaxPollInterval     = 10 * time.Minute
	MaxNameLength       = 64
	MaxServers          = 100
	MaxLocations        = 20
	MaxVolumeGiB        = 16384
	MinParkAfter        = 5 * time.Minute
	MaxParkAfter        = 24 * time.Hour
)

// ErrInvalidManifest wraps every validation failure; the message says which field and why.
var ErrInvalidManifest = errors.New("invalid servers manifest")

var (
	idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	// vmIDPattern is a cloud-VM server's id: it names the server's volume, firewall and instance,
	// and providers want a leading letter and a short name (DigitalOcean volumes: 64 characters).
	vmIDPattern   = regexp.MustCompile(`^[a-z][a-z0-9-]{0,38}[a-z0-9]$`)
	regionPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
	slugPattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
)

// ValidID reports whether id is a server id: lower-case letters, digits and hyphens, at most 63,
// not starting with a hyphen.
func ValidID(id string) bool { return idPattern.MatchString(id) }

// ParseManifest reads a manifest strictly (an unknown key is an error) and fills the defaults. A
// key gravel removed is refused with where it went (removedKeys).
func ParseManifest(b []byte) (Manifest, error) {
	if err := removedKeys(b); err != nil {
		return Manifest{}, fmt.Errorf("servers: manifest: %w", err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return Manifest{}, fmt.Errorf("servers: manifest: %w", err)
	}
	return m.Normalized(), nil
}

// removed are the server keys gravel took out of the manifest, and what replaces each: a manifest
// that still has one is refused with the way to move, not the decoder's bare "field not found".
var removed = map[string]string{
	"seeding": "removed in gravel 0.8.0: seeding rules are the Organization settings' discord.seeding now " +
		"(ADR-0016; docs/hub.md \"Organization settings\", docs/bot.md \"Seeding\"); delete it here and set the rule there",
}

// removedKeys reports each server that still has a removed key, by index and line.
func removedKeys(b []byte) error {
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil || len(doc.Content) == 0 {
		return nil // the strict decode reports it
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil
	}
	var errs []error
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value != "servers" || root.Content[i+1].Kind != yaml.SequenceNode {
			continue
		}
		for n, srv := range root.Content[i+1].Content {
			if srv.Kind != yaml.MappingNode {
				continue
			}
			for k := 0; k+1 < len(srv.Content); k += 2 {
				key := srv.Content[k]
				if why, ok := removed[key.Value]; ok {
					errs = append(errs, fmt.Errorf("%w: servers[%d].%s (line %d): %s", ErrInvalidManifest, n, key.Value, key.Line, why))
				}
			}
		}
	}
	return errors.Join(errs...)
}

// Normalized fills the defaults and sorts games and servers by id, so two manifests that mean
// the same thing compare equal.
func (m Manifest) Normalized() Manifest {
	if m.Version == 0 {
		m.Version = ManifestVersion
	}
	m.Games = slices.Clone(m.Games)
	m.Servers = slices.Clone(m.Servers)
	m.Locations = slices.Clone(m.Locations)
	if len(m.Locations) == 0 {
		m.Locations = nil
	}
	for i := range m.Games {
		m.Games[i].Bands = slices.Clone(m.Games[i].Bands)
		slices.SortFunc(m.Games[i].Bands, func(a, b Band) int {
			return strings.Compare(a.Section+"\x00"+a.Key, b.Section+"\x00"+b.Key)
		})
		if len(m.Games[i].Bands) == 0 {
			m.Games[i].Bands = nil
		}
	}
	for i := range m.Servers {
		s := &m.Servers[i]
		if s.PollInterval == 0 {
			s.PollInterval = Duration(DefaultPollInterval)
		}
		if s.VM != nil {
			vm := *s.VM
			if vm.Address == "" {
				vm.Address = AddressReserved
			}
			s.VM = &vm
		}
		if len(s.Settings) == 0 {
			s.Settings = nil
		}
	}
	slices.SortFunc(m.Locations, func(a, b Location) int { return strings.Compare(a.ID, b.ID) })
	slices.SortFunc(m.Games, func(a, b GameRef) int { return strings.Compare(a.ID, b.ID) })
	slices.SortFunc(m.Servers, func(a, b Server) int { return strings.Compare(a.ID, b.ID) })
	return m
}

// Marshal writes the manifest as YAML, normalized.
func (m Manifest) Marshal() ([]byte, error) { return yaml.Marshal(m.Normalized()) }

// Validate reports every problem at once, each wrapping ErrInvalidManifest. specs are the
// shipped Game specs, drivers the drivers this hub has and providers the clouds it can make VMs in.
func (m Manifest) Validate(specs map[string]Spec, drivers, providers []string) error {
	var errs []error
	bad := func(format string, a ...any) {
		errs = append(errs, fmt.Errorf("%w: "+format, append([]any{ErrInvalidManifest}, a...)...))
	}
	if m.Version != ManifestVersion {
		bad("version: got %d, this hub reads version %d", m.Version, ManifestVersion)
	}
	games := map[string]bool{}
	for i, g := range m.Games {
		spec, ok := specs[g.ID]
		switch {
		case !ok:
			bad("games[%d].id: %q is not a game this hub ships (%s)", i, g.ID, strings.Join(sortedKeys(specs), ", "))
		case games[g.ID]:
			bad("games[%d].id: %q is listed twice", i, g.ID)
		}
		games[g.ID] = true
		if ok {
			validateBands(fmt.Sprintf("games[%d]", i), spec, g.Bands, bad)
		}
	}
	locations := map[string]Location{}
	if len(m.Locations) > MaxLocations {
		bad("locations: %d locations, at most %d", len(m.Locations), MaxLocations)
	}
	for i, l := range m.Locations {
		at := fmt.Sprintf("locations[%d]", i)
		if !idPattern.MatchString(l.ID) {
			bad("%s.id: %q must be lower-case letters, digits and hyphens", at, l.ID)
		} else if _, dup := locations[l.ID]; dup {
			bad("%s.id: %q is listed twice", at, l.ID)
		}
		locations[l.ID] = l
		if !slices.Contains(providers, l.Provider) {
			bad("%s.provider: %q is not a cloud this hub has (%s)", at, l.Provider, strings.Join(providers, ", "))
		}
		if !regionPattern.MatchString(l.Region) {
			bad("%s.region: %q is not a region slug", at, l.Region)
		}
		if l.Project != "" && (len(l.Project) > 128 || strings.ContainsAny(l.Project, "\n\r\t")) {
			bad("%s.project: one line, at most 128 characters", at)
		}
		if l.Image != "" && !slugPattern.MatchString(l.Image) {
			bad("%s.image: %q is not an image slug", at, l.Image)
		}
		checkFile(at+".token_file", l.TokenFile, bad)
		checkFile(at+".ssh_key_file", l.SSHKeyFile, bad)
		if l.TokenFile == l.SSHKeyFile && l.TokenFile != "" {
			bad("%s: the token and the SSH key must be different files", at)
		}
		for j, k := range l.ExtraSSHKeys {
			if !vmdoc.ValidPublicKey(strings.TrimSpace(k)) {
				bad("%s.extra_ssh_keys[%d]: not an OpenSSH public key line", at, j)
			}
		}
		if _, err := cloud.NormalizeCIDRs(l.SSHFrom); err != nil {
			bad("%s.ssh_from: %v", at, err)
		}
	}
	if len(m.Servers) > MaxServers {
		bad("servers: %d servers, at most %d", len(m.Servers), MaxServers)
	}
	seen := map[string]bool{}
	for i, s := range m.Servers {
		at := fmt.Sprintf("servers[%d]", i)
		if !idPattern.MatchString(s.ID) {
			bad("%s.id: %q must be lower-case letters, digits and hyphens, starting with a letter or digit, at most 63", at, s.ID)
		} else if seen[s.ID] {
			bad("%s.id: %q is listed twice", at, s.ID)
		}
		seen[s.ID] = true
		if strings.TrimSpace(s.Name) == "" || len(s.Name) > MaxNameLength {
			bad("%s.name: required, at most %d characters", at, MaxNameLength)
		}
		spec, gameOK := specs[s.Game]
		if !games[s.Game] {
			bad("%s.game: %q is not in games", at, s.Game)
		}
		switch {
		case !slices.Contains(drivers, s.Driver):
			bad("%s.driver: %q is not a driver this hub has (%s)", at, s.Driver, strings.Join(drivers, ", "))
		case gameOK && len(spec.Drivers) == 0:
			bad("%s.game: %s is a catalog entry; gravel has no driver for its servers yet", at, spec.Name)
		case gameOK && !slices.Contains(spec.Drivers, s.Driver):
			bad("%s.driver: %q does not control %s (%s)", at, s.Driver, spec.Name, strings.Join(spec.Drivers, ", "))
		}
		if !idPattern.MatchString(s.Location) {
			bad("%s.location: %q must be lower-case letters, digits and hyphens", at, s.Location)
		}
		if s.Driver == cloudvm.Name {
			validateVM(at, s, spec, gameOK, locations, bad)
		} else {
			if s.VM != nil || s.Settings != nil {
				bad("%s: vm and settings are a cloud-VM server's (driver %s)", at, cloudvm.Name)
			}
			if u, err := url.Parse(s.Endpoint); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.User != nil {
				bad("%s.endpoint: %q must be an http(s) origin with no path, query or credentials", at, s.Endpoint)
			}
		}
		if !filepath.IsAbs(s.CredentialFile) || filepath.Clean(s.CredentialFile) != s.CredentialFile {
			bad("%s.credential_file: %q must be a clean absolute path (a podman secret: /run/secrets/<name>)", at, s.CredentialFile)
		}
		if d := time.Duration(s.PollInterval); d < MinPollInterval || d > MaxPollInterval {
			bad("%s.poll_interval: %s is outside %s–%s", at, d, MinPollInterval, MaxPollInterval)
		}
		if s.Trust != TrustOfficial && s.Trust != TrustCommunity {
			bad("%s.trust: %q is not %q or %q", at, s.Trust, TrustOfficial, TrustCommunity)
		}
		if f := s.Feed; f != nil {
			if u, err := url.Parse(f.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.User != nil {
				bad("%s.feed.url: %q must be an http(s) origin with no path, query or credentials (the game adds its path)", at, f.URL)
			}
			if !filepath.IsAbs(f.TokenFile) || filepath.Clean(f.TokenFile) != f.TokenFile {
				bad("%s.feed.token_file: %q must be a clean absolute path (a podman secret: /run/secrets/<name>)", at, f.TokenFile)
			} else if f.TokenFile == s.CredentialFile {
				bad("%s.feed.token_file: the feed token must not be the RCON credential", at)
			}
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return errors.Join(errs...)
}

// validateVM checks a cloud-VM server: a location the manifest defines, no endpoint (the hub
// knows the address), a feed (the log push is where its players' identities come from), the
// machine, its secret files, and its settings against the game's cloudvm section.
func validateVM(at string, s Server, spec Spec, gameOK bool, locations map[string]Location, bad func(string, ...any)) {
	if !vmIDPattern.MatchString(s.ID) {
		bad("%s.id: a cloud-VM server's id names its volume and firewall: a letter first, then letters, digits and hyphens, at most 40", at)
	}
	if _, ok := locations[s.Location]; !ok {
		bad("%s.location: %q is not in locations (a cloud-VM server runs in one the manifest defines)", at, s.Location)
	}
	if s.Endpoint != "" {
		bad("%s.endpoint: a cloud-VM server has none; the hub knows the address of the VM it made", at)
	}
	if s.Feed == nil {
		bad("%s.feed: required for a cloud-VM server: the log the game pushes is where its players are known from", at)
	}
	if gameOK && spec.CloudVM == nil {
		bad("%s.game: %s has no cloudvm section; the cloud-VM driver cannot run it", at, spec.Name)
	}
	vm := s.VM
	if vm == nil {
		bad("%s.vm: required for a cloud-VM server", at)
		return
	}
	if !slugPattern.MatchString(vm.Size) {
		bad("%s.vm.size: %q is not a size slug", at, vm.Size)
	}
	min := 1
	if gameOK && spec.CloudVM != nil {
		min = spec.CloudVM.VolumeMinGiB
	}
	if vm.VolumeGiB < min || vm.VolumeGiB > MaxVolumeGiB {
		bad("%s.vm.volume_gib: %d is outside %d–%d (the game's install needs %d)", at, vm.VolumeGiB, min, MaxVolumeGiB, min)
	}
	if vm.Address != "" && vm.Address != AddressReserved && vm.Address != AddressEphemeral {
		bad("%s.vm.address: %q is not %s or %s", at, vm.Address, AddressReserved, AddressEphemeral)
	}
	if d := time.Duration(vm.ParkAfter); d != 0 && (d < MinParkAfter || d > MaxParkAfter) {
		bad("%s.vm.park_after: %s is outside %s–%s (leave it out to never park)", at, d, MinParkAfter, MaxParkAfter)
	}
	files := map[string]string{"credential_file": s.CredentialFile}
	if s.Feed != nil {
		files["feed.token_file"] = s.Feed.TokenFile
	}
	if vm.PasswordFile != "" {
		checkFile(at+".vm.password_file", vm.PasswordFile, bad)
		files["vm.password_file"] = vm.PasswordFile
	}
	if vm.PublisherTokenFile != "" {
		checkFile(at+".vm.publisher_token_file", vm.PublisherTokenFile, bad)
		files["vm.publisher_token_file"] = vm.PublisherTokenFile
	}
	byPath := map[string]string{}
	for _, k := range slices.Sorted(maps.Keys(files)) {
		if other, dup := byPath[files[k]]; dup && files[k] != "" {
			bad("%s: %s and %s are the same file; each secret is its own", at, other, k)
		}
		byPath[files[k]] = k
	}
	if gameOK && spec.CloudVM != nil {
		if err := spec.CloudVM.ValidateSettings(s.Settings, s.Name); err != nil {
			for _, e := range strings.Split(err.Error(), "\n") {
				bad("%s.settings: %s", at, e)
			}
		}
	}
}

// checkFile is a secret file's path: clean and absolute (a podman secret: /run/secrets/<name>).
func checkFile(at, path string, bad func(string, ...any)) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		bad("%s: %q must be a clean absolute path (a podman secret: /run/secrets/<name>)", at, path)
	}
}

func validateBands(at string, spec Spec, over []Band, bad func(string, ...any)) {
	seen := map[string]bool{}
	for i, o := range over {
		where := fmt.Sprintf("%s.bands[%d]", at, i)
		base, ok := spec.Band(o.Section, o.Key)
		if !ok {
			bad("%s: %s has no band for %s %s; a manifest may only tighten the spec's bands", where, spec.Name, o.Section, o.Key)
			continue
		}
		if seen[o.Section+"\x00"+o.Key] {
			bad("%s: %s %s is listed twice", where, o.Section, o.Key)
		}
		seen[o.Section+"\x00"+o.Key] = true
		if o.UnlessSet != "" {
			bad("%s.unless_set: the spec's condition can't be changed by a manifest", where)
		}
		kind := o.Kind()
		switch {
		case kind == "" && o.Min == nil && o.Max == nil && o.MaxLength == nil && o.Hosts == nil:
			bad("%s: neither min nor max is set", where)
			continue
		case kind == "":
			bad("%s: set only one of min/max, max_length or hosts", where)
			continue
		case kind != base.Kind():
			bad("%s: the spec's band for %s is a %s band, not a %s band", where, o.Key, base.Kind(), kind)
			continue
		}
		switch kind {
		case BandRange:
			if o.Min != nil && base.Min != nil && *o.Min < *base.Min {
				bad("%s.min: %d loosens the spec's %d", where, *o.Min, *base.Min)
			}
			if o.Max != nil && base.Max != nil && *o.Max > *base.Max {
				bad("%s.max: %d loosens the spec's %d", where, *o.Max, *base.Max)
			}
			lo, hi := o.Min, o.Max
			if lo == nil {
				lo = base.Min
			}
			if hi == nil {
				hi = base.Max
			}
			if lo != nil && hi != nil && *lo > *hi {
				bad("%s: min %d is above max %d", where, *lo, *hi)
			}
		case BandLength:
			if *o.MaxLength < 1 || *o.MaxLength > *base.MaxLength {
				bad("%s.max_length: %d must be from 1 to the spec's %d", where, *o.MaxLength, *base.MaxLength)
			}
		case BandHosts:
			if len(o.Hosts) == 0 {
				bad("%s.hosts: an empty list allows no URL; leave the value empty in the configuration instead", where)
			}
			for _, h := range o.Hosts {
				if !slices.ContainsFunc(base.Hosts, func(b string) bool { return h == b || strings.HasSuffix(h, "."+b) }) {
					bad("%s.hosts: %q loosens the spec's hosts (%s)", where, h, strings.Join(base.Hosts, ", "))
				}
			}
		}
	}
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// Duration is a time.Duration written as "15s" in YAML and JSON.
type Duration time.Duration

// String is the duration as Go writes it.
func (d Duration) String() string { return time.Duration(d).String() }

// MarshalYAML writes "15s".
func (d Duration) MarshalYAML() (any, error) { return d.String(), nil }

// UnmarshalYAML reads "15s".
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("line %d: %q is not a duration (15s, 2m, 1h)", n.Line, s)
	}
	*d = Duration(v)
	return nil
}

// MarshalJSON writes "15s".
func (d Duration) MarshalJSON() ([]byte, error) { return []byte(`"` + d.String() + `"`), nil }

// UnmarshalJSON reads "15s".
func (d *Duration) UnmarshalJSON(b []byte) error {
	v, err := time.ParseDuration(strings.Trim(string(b), `"`))
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}
