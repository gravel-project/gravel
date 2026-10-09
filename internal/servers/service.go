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
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/gravel-project/gravel/internal/store"
)

// Store is what the service needs from the database.
type Store interface {
	ListGames(ctx context.Context, orgID uuid.UUID, includeRemoved bool) ([]store.Game, error)
	ListManagedServers(ctx context.Context, orgID uuid.UUID, includeRemoved bool) ([]store.ManagedServer, error)
	ApplyServers(ctx context.Context, orgID uuid.UUID, games []store.Game, servers []store.ManagedServer, now time.Time) error
}

// Service applies manifests and reads the games and servers back.
type Service struct {
	st      Store
	orgID   uuid.UUID
	specs   map[string]Spec
	drivers []string
	logger  *slog.Logger
	now     func() time.Time
}

// New builds a service for an organization; drivers are the names the hub's registry has.
func New(st Store, orgID uuid.UUID, drivers []string, logger *slog.Logger) *Service {
	return &Service{st: st, orgID: orgID, specs: Specs(), drivers: drivers, logger: logger, now: time.Now}
}

// Validate checks a manifest against the shipped specs and the hub's drivers.
func (s *Service) Validate(m Manifest) error { return m.Validate(s.specs, s.drivers) }

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
	m := Manifest{Version: ManifestVersion, Servers: srvs}
	for _, g := range games {
		m.Games = append(m.Games, GameRef{ID: g.ID, Bands: g.Tightened})
	}
	return m.Normalized(), nil
}

// Change is one difference apply makes.
type Change struct {
	Kind   string // "game" or "server"
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
		for _, srv := range m.Servers {
			if b, err := os.ReadFile(srv.CredentialFile); err != nil {
				errs = append(errs, fmt.Errorf("%w: server %s: %s: %w", ErrCredentialFile, srv.ID, srv.CredentialFile, err))
			} else if strings.TrimSpace(string(b)) == "" {
				errs = append(errs, fmt.Errorf("%w: server %s: %s is empty", ErrCredentialFile, srv.ID, srv.CredentialFile))
			}
		}
		if len(errs) > 0 {
			return nil, errors.Join(errs...)
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
	rows := make([]store.ManagedServer, 0, len(m.Servers))
	for _, srv := range m.Servers {
		r, err := toRow(srv)
		if err != nil {
			return nil, err
		}
		rows = append(rows, r)
	}
	if err := s.st.ApplyServers(ctx, s.orgID, games, rows, s.now()); err != nil {
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
	if s.Seeding != nil {
		b, err := json.Marshal(s.Seeding)
		if err != nil {
			return store.ManagedServer{}, err
		}
		r.Seeding = b
	}
	return r, nil
}

func fromRow(r store.ManagedServer) (Server, error) {
	s := Server{
		ID: r.ID, Name: r.Name, Game: r.GameID, Driver: r.Driver, Location: r.Location, Endpoint: r.Endpoint,
		CredentialFile: r.CredentialFile, PollInterval: Duration(r.PollInterval), Trust: r.Trust,
	}
	if len(r.Seeding) > 0 && string(r.Seeding) != "null" {
		var sd Seeding
		if err := json.Unmarshal(r.Seeding, &sd); err != nil {
			return Server{}, fmt.Errorf("servers: server %s: seeding: %w", r.ID, err)
		}
		s.Seeding = &sd
	}
	return s, nil
}

func nonNil(b []Band) []Band {
	if b == nil {
		return []Band{}
	}
	return b
}
