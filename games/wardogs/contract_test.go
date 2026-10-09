package wardogs_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gravel-project/gravel/games/wardogs"
	"github.com/gravel-project/gravel/games/wardogs/wardogstest"
)

// contract is what the client must read from any War Dogs server it supports: run against every
// recorded build here, and against a live server with -tags live (live_test.go). It only reads.
// A capability the server does not offer is skipped, never failed.
func contract(t *testing.T, c *wardogs.Client) {
	t.Helper()
	ctx := context.Background()
	caps, err := c.Capabilities(ctx)
	if err != nil {
		t.Fatalf("capabilities: %v", err)
	}
	if caps.Build == "" || caps.APIVersion == "" {
		t.Errorf("capabilities name no build or API version: %+v", caps)
	}
	for _, r := range caps.Routes {
		if _, err := wardogs.ParseRoute(r); err != nil {
			t.Errorf("advertised route: %v", err)
		}
	}
	if caps.Limits.MaxBodyBytes <= 0 || caps.Limits.MaxRequestsPerMinutePerIP <= 0 {
		t.Errorf("limits = %+v", caps.Limits)
	}
	if extra := caps.Unrecognised(); len(extra) > 0 {
		t.Logf("routes the client has no capability for: %v", extra)
	}

	// read runs one read when the server offers it.
	read := func(cap wardogs.Capability, f func() error) {
		t.Helper()
		if !caps.Supports(cap) {
			t.Logf("%s: not offered", cap)
			return
		}
		if err := f(); err != nil {
			t.Errorf("%s: %v", cap, err)
		}
	}

	read(wardogs.CapHealth, func() error {
		h, err := c.Health(ctx)
		if err == nil && h.Status == "" {
			err = errors.New("no status")
		}
		return err
	})
	read(wardogs.CapStatus, func() error {
		s, err := c.Status(ctx)
		if err != nil {
			return err
		}
		if s.Map == "" || s.Players.Max <= 0 || s.Players.Current < 0 || s.Players.Current > s.Players.Max {
			return fmt.Errorf("status: map %q, players %+v", s.Map, s.Players)
		}
		if st := s.ScoreTick; st.Min > st.Max || st.Current < st.Min || st.Current > st.Max {
			return fmt.Errorf("score tick %+v is outside its own band", st)
		}
		return nil
	})
	read(wardogs.CapPlayers, func() error {
		p, err := c.Players(ctx)
		if err == nil && p.Count != len(p.Players) {
			err = fmt.Errorf("players: count %d, %d listed", p.Count, len(p.Players))
		}
		for _, pl := range p.Players {
			if pl.SteamID == "" {
				return errors.New("a player without a SteamID")
			}
		}
		return err
	})
	var mapIDs []string
	read(wardogs.CapCatalogMaps, func() error {
		m, err := c.CatalogMaps(ctx)
		if err != nil {
			return err
		}
		if m.Count != len(m.Maps) || len(m.Maps) == 0 {
			return fmt.Errorf("maps: count %d, %d listed", m.Count, len(m.Maps))
		}
		for _, e := range m.Maps {
			mapIDs = append(mapIDs, e.ID)
		}
		return nil
	})
	read(wardogs.CapCatalogLightings, func() error {
		l, err := c.CatalogLightings(ctx)
		if err == nil && (l.Count != len(l.Lightings) || len(l.Lightings) == 0) {
			err = fmt.Errorf("lightings: count %d, %d listed", l.Count, len(l.Lightings))
		}
		return err
	})
	read(wardogs.CapCatalogExperiences, func() error {
		x, err := c.CatalogExperiences(ctx)
		if err == nil && (x.Count != len(x.Experiences) || len(x.Experiences) == 0) {
			err = fmt.Errorf("experiences: count %d, %d listed", x.Count, len(x.Experiences))
		}
		return err
	})
	for _, id := range mapIDs {
		read(wardogs.CapMapExperiences, func() error {
			x, err := c.MapExperiences(ctx, id)
			if err == nil && (x.Map != id || x.Count != len(x.Experiences)) {
				err = fmt.Errorf("%s experiences: %+v", id, x)
			}
			return err
		})
		read(wardogs.CapMapAlternators, func() error {
			a, err := c.MapAlternators(ctx, id)
			if err == nil && (a.Map != id || a.Count != len(a.Alternators)) {
				err = fmt.Errorf("%s alternators: %+v", id, a)
			}
			return err
		})
	}
	read(wardogs.CapRotation, func() error {
		r, err := c.Rotation(ctx)
		if err != nil {
			return err
		}
		if r.Count != len(r.Entries) {
			return fmt.Errorf("rotation: count %d, %d listed", r.Count, len(r.Entries))
		}
		for _, e := range r.Entries {
			if len(mapIDs) > 0 && !slices.Contains(mapIDs, e.Map) {
				return fmt.Errorf("rotation entry %d names map %q, not in the catalog", e.Index, e.Map)
			}
		}
		return nil
	})
	read(wardogs.CapBans, func() error {
		b, err := c.Bans(ctx)
		if err == nil && b.Count != len(b.Bans) {
			err = fmt.Errorf("bans: count %d, %d listed", b.Count, len(b.Bans))
		}
		return err
	})
	read(wardogs.CapReservedSlots, func() error {
		r, err := c.ReservedSlots(ctx)
		if err == nil && r.Count != len(r.ReservedSlots) {
			err = fmt.Errorf("reserved slots: count %d, %d listed", r.Count, len(r.ReservedSlots))
		}
		return err
	})
	read(wardogs.CapServerID, func() error { _, err := c.ServerID(ctx); return err })
	read(wardogs.CapSponsor, func() error { _, err := c.Sponsor(ctx); return err })
	read(wardogs.CapAudit, func() error {
		a, err := c.Audit(ctx, 5)
		// The answer states its own limit (a recorded fixture ignores the query).
		if err == nil && (a.Count != len(a.Entries) || a.Limit < 1 || a.Count > a.Limit) {
			err = fmt.Errorf("limit %d, count %d, %d listed", a.Limit, a.Count, len(a.Entries))
		}
		return err
	})
	read(wardogs.CapConfigRead, func() error {
		d, err := c.Config(ctx)
		if err != nil {
			return err
		}
		if d.Revision == "" || d.Text == "" || len(d.Sections) == 0 {
			return fmt.Errorf("config: revision %q, %d bytes, %d sections", d.Revision, len(d.Text), len(d.Sections))
		}
		if !strings.Contains(d.Text, "\r\n") {
			return errors.New("config: the document is not CRLF")
		}
		for _, s := range d.Sections {
			if s.Section == "" || len(s.AllowedKeys) == 0 || s.AppliesWhen == "" {
				return fmt.Errorf("config: section %+v", s)
			}
			for _, o := range s.KeyOverrides {
				if _, ok := d.Key(s.Section, o.Key); !ok {
					return fmt.Errorf("config: %s overrides %s, which it does not allow", s.Section, o.Key)
				}
			}
		}
		return nil
	})
}

// TestContractOnEveryRecordedBuild runs the contract against each build in testdata/, strictly:
// a field whose type drifted fails here even though production tolerates it.
func TestContractOnEveryRecordedBuild(t *testing.T) {
	for _, dir := range wardogstest.Builds(t, "testdata") {
		t.Run(filepath.Base(dir), func(t *testing.T) {
			srv := wardogstest.New(t, dir, wardogstest.Options{})
			contract(t, newClient(t, srv.URL, wardogstest.Token))
			for _, r := range srv.Requests() {
				if r.Method != "GET" {
					t.Errorf("the contract sent %s %s; it only reads", r.Method, r.Path)
				}
			}
		})
	}
}

// TestContractOnAChangedBuild is a build with a route gone and parameters renamed: the client
// runs with no code change and reports the capability as absent.
func TestContractOnAChangedBuild(t *testing.T) {
	srv := wardogstest.New(t, latest(t), wardogstest.Options{
		Remove: []string{"GET /v1/rotation", "GET /v1/catalog/maps/{map}/alternators"},
		Rename: map[string]string{"GET /v1/catalog/maps/{map}/experiences": "GET /v1/catalog/maps/{id}/experiences"},
	})
	c := newClient(t, srv.URL, wardogstest.Token)
	contract(t, c)
	ctx := context.Background()
	if _, err := c.Rotation(ctx); !errors.Is(err, wardogs.ErrNotSupported) {
		t.Errorf("rotation on a build without it = %v", err)
	}
	if ok, _ := c.Supports(ctx, wardogs.CapMapExperiences); !ok {
		t.Error("a renamed parameter lost the capability")
	}
	for _, r := range srv.Requests() {
		if r.Path == "/v1/rotation" || strings.HasSuffix(r.Path, "/alternators") {
			t.Errorf("sent %s %s on a build that does not offer it", r.Method, r.Path)
		}
	}
}
