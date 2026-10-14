// Package servers owns the games and servers a hub controls (ADR-0010): the Game specs gravel
// ships, the servers.yaml manifest a deployment applies, and the monitor that polls each server
// through its driver and keeps the last observation for the API.
package servers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/gravel-project/gravel/cloud"
	"github.com/gravel-project/gravel/drivers/cloudvm"
	"github.com/gravel-project/gravel/internal/store"
)

// Store is what the service needs from the database.
type Store interface {
	ListGames(ctx context.Context, orgID uuid.UUID, includeRemoved bool) ([]store.Game, error)
	ListManagedServers(ctx context.Context, orgID uuid.UUID, includeRemoved bool) ([]store.ManagedServer, error)
	ListLocations(ctx context.Context, orgID uuid.UUID) ([]store.Location, error)
	ApplyServersRecorded(ctx context.Context, orgID uuid.UUID, games []store.Game, locations []store.Location, servers []store.ManagedServer, now time.Time, rec store.Applied) error
}

// Service applies manifests and reads the games and servers back.
type Service struct {
	st        Store
	orgID     uuid.UUID
	specs     map[string]Spec
	drivers   []string
	providers []string
	logger    *slog.Logger
	now       func() time.Time
}

// New builds a service for an organization; drivers are the names the hub's registry has.
func New(st Store, orgID uuid.UUID, drivers []string, logger *slog.Logger) *Service {
	return &Service{st: st, orgID: orgID, specs: Specs(), drivers: drivers, logger: logger, now: time.Now}
}

// WithProviders sets the clouds this hub makes VMs in (the cloud-VM driver's locations name one).
func (s *Service) WithProviders(names ...string) *Service {
	s.providers = slices.Clone(names)
	return s
}

// Validate checks a manifest against the shipped specs, the hub's drivers and its clouds.
func (s *Service) Validate(m Manifest) error { return m.Validate(s.specs, s.drivers, s.providers) }

// Locations are the clouds the manifest defines, by id.
func (s *Service) Locations(ctx context.Context) ([]Location, error) {
	rows, err := s.st.ListLocations(ctx, s.orgID)
	if err != nil {
		return nil, err
	}
	out := make([]Location, 0, len(rows))
	for _, r := range rows {
		out = append(out, Location{ID: r.ID, Provider: r.Provider, Region: r.Region, Project: r.Project,
			TokenFile: r.TokenFile, SSHKeyFile: r.SSHKeyFile, Image: r.Image,
			ExtraSSHKeys: r.ExtraSSHKeys, SSHFrom: r.SSHFrom})
	}
	return out, nil
}

// Location is one defined location, or store.ErrNotFound.
func (s *Service) Location(ctx context.Context, id string) (Location, error) {
	all, err := s.Locations(ctx)
	if err != nil {
		return Location{}, err
	}
	for _, l := range all {
		if l.ID == id {
			return l, nil
		}
	}
	return Location{}, store.ErrNotFound
}

// Spec is a shipped Game spec by id.
func (s *Service) Spec(id string) (Spec, bool) {
	sp, ok := s.specs[id]
	return sp, ok
}

// Game is an enabled game: its spec with the manifest's bands applied.
type Game struct {
	Spec
	// Tightened are the manifest's own tightenings (what export prints).
	Tightened []Band
}

// Games are the enabled games, by id.
func (s *Service) Games(ctx context.Context) ([]Game, error) {
	rows, err := s.st.ListGames(ctx, s.orgID, false)
	if err != nil {
		return nil, err
	}
	out := make([]Game, 0, len(rows))
	for _, r := range rows {
		spec, ok := s.specs[r.ID]
		if !ok {
			// A game this hub no longer ships: keep serving the rest.
			s.logger.Warn("an enabled game has no spec in this hub; skipped", "game", r.ID)
			continue
		}
		var over []Band
		if err := json.Unmarshal(r.Bands, &over); err != nil {
			return nil, fmt.Errorf("servers: game %s: bands: %w", r.ID, err)
		}
		g := Game{Spec: spec, Tightened: over}
		g.Bands = spec.Tighten(over)
		out = append(out, g)
	}
	return out, nil
}

// Servers are the live (not removed) servers, by id.
func (s *Service) Servers(ctx context.Context) ([]Server, error) {
	rows, err := s.st.ListManagedServers(ctx, s.orgID, false)
	if err != nil {
		return nil, err
	}
	out := make([]Server, 0, len(rows))
	for _, r := range rows {
		srv, err := fromRow(r)
		if err != nil {
			return nil, err
		}
		out = append(out, srv)
	}
	return out, nil
}

// Server is one live server, or store.ErrNotFound.
func (s *Service) Server(ctx context.Context, id string) (Server, error) {
	all, err := s.Servers(ctx)
	if err != nil {
		return Server{}, err
	}
	for _, srv := range all {
		if srv.ID == id {
			return srv, nil
		}
	}
	return Server{}, store.ErrNotFound
}

// Manifest is what is stored, as a manifest (what export prints and apply compares with).
func (s *Service) Manifest(ctx context.Context) (Manifest, error) {
	games, err := s.Games(ctx)
	if err != nil {
		return Manifest{}, err
	}
	srvs, err := s.Servers(ctx)
	if err != nil {
		return Manifest{}, err
	}
	locs, err := s.Locations(ctx)
	if err != nil {
		return Manifest{}, err
	}
	m := Manifest{Version: ManifestVersion, Servers: srvs, Locations: locs}
	for _, g := range games {
		m.Games = append(m.Games, GameRef{ID: g.ID, Bands: g.Tightened})
	}
	return m.Normalized(), nil
}

// Change is one difference apply makes.
type Change struct {
	Kind   string // "game", "location" or "server"
	ID     string
	Action string // "added", "changed" or "removed"
}

func (c Change) String() string { return c.Kind + " " + c.ID + " " + c.Action }

// ApplyOptions are apply's switches.
type ApplyOptions struct {
	// DryRun reports the changes and writes nothing.
	DryRun bool
	// CheckCredentials refuses a server whose credential file is not a readable, non-empty file
	// (apply runs where the hub's secrets are; check, in CI, does not).
	CheckCredentials bool
	// Clouds makes a location's provider from its token, so a scaling ladder is walked against
	// the cloud's sizes when it is saved (#37); nil skips that check. Only with CheckCredentials.
	Clouds func(ctx context.Context, l Location, token string) (cloud.Provider, error)
	// By is who applies, for the events log (servers.applied): the host's command line, or a
	// member or app through the API (#195).
	By store.Actor
}

// ErrCredentialFile is a credential file apply could not read.
var ErrCredentialFile = errors.New("credential file")

// Apply makes the stored games and servers match the manifest and reports what changed. An
// unchanged manifest writes nothing.
func (s *Service) Apply(ctx context.Context, m Manifest, opt ApplyOptions) ([]Change, error) {
	m = m.Normalized()
	if err := s.Validate(m); err != nil {
		return nil, err
	}
	if opt.CheckCredentials {
		var errs []error
		check := func(what, f string) {
			if b, err := os.ReadFile(f); err != nil {
				errs = append(errs, fmt.Errorf("%w: %s: %s: %w", ErrCredentialFile, what, f, err))
			} else if strings.TrimSpace(string(b)) == "" {
				errs = append(errs, fmt.Errorf("%w: %s: %s is empty", ErrCredentialFile, what, f))
			}
		}
		for _, l := range m.Locations {
			check("location "+l.ID, l.TokenFile)
			check("location "+l.ID, l.SSHKeyFile)
		}
		for _, srv := range m.Servers {
			files := []string{srv.CredentialFile}
			if srv.Feed != nil {
				files = append(files, srv.Feed.TokenFile)
			}
			if vm := srv.VM; vm != nil {
				for _, f := range []string{vm.PasswordFile, vm.PublisherTokenFile} {
					if f != "" {
						files = append(files, f)
					}
				}
			}
			for _, f := range files {
				check("server "+srv.ID, f)
			}
		}
		if len(errs) > 0 {
			return nil, errors.Join(errs...)
		}
		if opt.Clouds != nil {
			if err := s.checkClouds(ctx, m, opt.Clouds); err != nil {
				return nil, err
			}
		}
	}
	current, err := s.Manifest(ctx)
	if err != nil {
		return nil, err
	}
	changes := diff(current, m)
	if opt.DryRun || len(changes) == 0 {
		return changes, nil
	}
	games := make([]store.Game, 0, len(m.Games))
	for _, g := range m.Games {
		bands, err := json.Marshal(nonNil(g.Bands))
		if err != nil {
			return nil, err
		}
		games = append(games, store.Game{ID: g.ID, Bands: bands})
	}
	locs := make([]store.Location, 0, len(m.Locations))
	for _, l := range m.Locations {
		locs = append(locs, store.Location{ID: l.ID, Provider: l.Provider, Region: l.Region, Project: l.Project,
			TokenFile: l.TokenFile, SSHKeyFile: l.SSHKeyFile, Image: l.Image,
			ExtraSSHKeys: slices.Clone(l.ExtraSSHKeys), SSHFrom: slices.Clone(l.SSHFrom)})
	}
	rows := make([]store.ManagedServer, 0, len(m.Servers))
	for _, srv := range m.Servers {
		r, err := toRow(srv)
		if err != nil {
			return nil, err
		}
		rows = append(rows, r)
	}
	rec := store.Applied{By: opt.By}
	for _, c := range changes {
		rec.Changes = append(rec.Changes, c.String())
		if c.Kind == "server" {
			rec.Servers = append(rec.Servers, c.ID)
		}
	}
	if err := s.st.ApplyServersRecorded(ctx, s.orgID, games, locs, rows, s.now(), rec); err != nil {
		return nil, err
	}
	for _, c := range changes {
		s.logger.Info("servers manifest applied", "change", c.String())
	}
	return changes, nil
}

func diff(current, want Manifest) []Change {
	var out []Change
	curGames := map[string]GameRef{}
	for _, g := range current.Games {
		curGames[g.ID] = g
	}
	wantGames := map[string]bool{}
	for _, g := range want.Games {
		wantGames[g.ID] = true
		if c, ok := curGames[g.ID]; !ok {
			out = append(out, Change{"game", g.ID, "added"})
		} else if !reflect.DeepEqual(c, g) {
			out = append(out, Change{"game", g.ID, "changed"})
		}
	}
	for _, g := range current.Games {
		if !wantGames[g.ID] {
			out = append(out, Change{"game", g.ID, "removed"})
		}
	}
	curLoc := map[string]Location{}
	for _, l := range current.Locations {
		curLoc[l.ID] = l
	}
	wantLoc := map[string]bool{}
	for _, l := range want.Locations {
		wantLoc[l.ID] = true
		if c, ok := curLoc[l.ID]; !ok {
			out = append(out, Change{"location", l.ID, "added"})
		} else if !reflect.DeepEqual(c, l) {
			out = append(out, Change{"location", l.ID, "changed"})
		}
	}
	for _, l := range current.Locations {
		if !wantLoc[l.ID] {
			out = append(out, Change{"location", l.ID, "removed"})
		}
	}
	curSrv := map[string]Server{}
	for _, v := range current.Servers {
		curSrv[v.ID] = v
	}
	wantSrv := map[string]bool{}
	for _, v := range want.Servers {
		wantSrv[v.ID] = true
		if c, ok := curSrv[v.ID]; !ok {
			out = append(out, Change{"server", v.ID, "added"})
		} else if !reflect.DeepEqual(c, v) {
			out = append(out, Change{"server", v.ID, "changed"})
		}
	}
	for _, v := range current.Servers {
		if !wantSrv[v.ID] {
			out = append(out, Change{"server", v.ID, "removed"})
		}
	}
	return out
}

func toRow(s Server) (store.ManagedServer, error) {
	r := store.ManagedServer{
		ID: s.ID, GameID: s.Game, Name: s.Name, Driver: s.Driver, Location: s.Location, Endpoint: s.Endpoint,
		CredentialFile: s.CredentialFile, PollInterval: time.Duration(s.PollInterval), Trust: s.Trust,
	}
	if s.Feed != nil {
		b, err := json.Marshal(s.Feed)
		if err != nil {
			return store.ManagedServer{}, err
		}
		r.Feed = b
	}
	if s.VM != nil {
		b, err := json.Marshal(s.VM)
		if err != nil {
			return store.ManagedServer{}, err
		}
		r.VM = b
	}
	if s.Settings != nil {
		b, err := json.Marshal(s.Settings)
		if err != nil {
			return store.ManagedServer{}, err
		}
		r.Settings = b
	}
	return r, nil
}

func fromRow(r store.ManagedServer) (Server, error) {
	s := Server{
		ID: r.ID, Name: r.Name, Game: r.GameID, Driver: r.Driver, Location: r.Location, Endpoint: r.Endpoint,
		CredentialFile: r.CredentialFile, PollInterval: Duration(r.PollInterval), Trust: r.Trust,
	}
	if len(r.Feed) > 0 && string(r.Feed) != "null" {
		var f Feed
		if err := json.Unmarshal(r.Feed, &f); err != nil {
			return Server{}, fmt.Errorf("servers: server %s: feed: %w", r.ID, err)
		}
		s.Feed = &f
	}
	if len(r.VM) > 0 && string(r.VM) != "null" {
		var vm VMSpec
		if err := json.Unmarshal(r.VM, &vm); err != nil {
			return Server{}, fmt.Errorf("servers: server %s: vm: %w", r.ID, err)
		}
		s.VM = &vm
	}
	if len(r.Settings) > 0 && string(r.Settings) != "null" {
		if err := json.Unmarshal(r.Settings, &s.Settings); err != nil {
			return Server{}, fmt.Errorf("servers: server %s: settings: %w", r.ID, err)
		}
	}
	return s, nil
}

func nonNil(b []Band) []Band {
	if b == nil {
		return []Band{}
	}
	return b
}

// checkClouds asks each cloud-VM server's location what only the cloud knows, so a manifest it
// would refuse is refused when saved: every scaling ladder walked both ways from the root disk a
// host made at vm.size has (cloudvm.CheckLadder, #37), and a seed snapshot found in the region and
// no bigger than the volume (#166).
func (s *Service) checkClouds(ctx context.Context, m Manifest, clouds func(context.Context, Location, string) (cloud.Provider, error)) error {
	locs := map[string]Location{}
	for _, l := range m.Locations {
		locs[l.ID] = l
	}
	var errs []error
	for _, srv := range m.Servers {
		if srv.VM == nil || (srv.VM.Scaling == nil && srv.VM.SeedSnapshot == nil) {
			continue
		}
		spec, ok := s.specs[srv.Game]
		loc, lok := locs[srv.Location]
		if !ok || spec.CloudVM == nil || !lok {
			continue // Validate said so already
		}
		b, err := os.ReadFile(loc.TokenFile)
		if err != nil {
			errs = append(errs, fmt.Errorf("%w: location %s: %s: %w", ErrCredentialFile, loc.ID, loc.TokenFile, err))
			continue
		}
		token, _, _ := strings.Cut(strings.TrimSpace(string(b)), "\n")
		p, err := clouds(ctx, loc, strings.TrimSpace(token))
		if err != nil {
			errs = append(errs, fmt.Errorf("server %s: location %s: %w", srv.ID, loc.ID, err))
			continue
		}
		if seed := srv.VM.SeedSnapshot; seed != nil {
			if err := checkSeed(ctx, p, *seed, loc.Region, srv.VM.VolumeGiB); err != nil {
				errs = append(errs, fmt.Errorf("%w: servers[%s].vm.seed_snapshot: %w", ErrInvalidManifest, srv.ID, err))
			}
		}
		if srv.VM.Scaling == nil {
			continue
		}
		sizes, err := p.Sizes(ctx)
		if err != nil {
			errs = append(errs, fmt.Errorf("server %s: location %s: the cloud's sizes: %w", srv.ID, loc.ID, err))
			continue
		}
		policy := srv.VM.Scaling.Policy(spec.CloudVM.Ladder)
		if err := cloudvm.CheckLadder(sizes, p.CanResize, loc.Region, srv.VM.Size, policy.Ladder); err != nil {
			for _, e := range strings.Split(err.Error(), "\n") {
				errs = append(errs, fmt.Errorf("%w: servers[%s].vm.scaling: %s", ErrInvalidManifest, srv.ID, e))
			}
		}
	}
	return errors.Join(errs...)
}

// checkSeed finds a seed snapshot and checks a volume of volumeGiB in region can be made from it.
func checkSeed(ctx context.Context, p cloud.Provider, seed SeedSnapshot, region string, volumeGiB int) error {
	var snap cloud.Snapshot
	var err error
	if seed.ID != "" {
		snap, err = p.GetVolumeSnapshot(ctx, seed.ID)
	} else {
		snap, err = p.FindVolumeSnapshot(ctx, seed.Name, region)
	}
	if err != nil {
		return err
	}
	return snap.Fits(cloud.VolumeSpec{Region: region, SizeGiB: volumeGiB, FromSnapshot: &cloud.SnapshotRef{ID: snap.ID}})
}
