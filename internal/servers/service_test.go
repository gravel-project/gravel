package servers_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/gravel-project/gravel/internal/servers"
	"github.com/gravel-project/gravel/internal/servers/serverstest"
	"github.com/gravel-project/gravel/internal/store"
)

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func manifest(t *testing.T, credFile string, extra ...servers.Server) servers.Manifest {
	t.Helper()
	m := servers.Manifest{
		Games: []servers.GameRef{{ID: "wardogs"}},
		Servers: append([]servers.Server{{
			ID: "wd-1", Name: "War Dogs #1", Game: "wardogs", Driver: "wardogs", Location: "slc",
			Endpoint: "http://203.0.113.10:7789", CredentialFile: credFile, Trust: servers.TrustOfficial,
		}}, extra...),
	}
	return m.Normalized()
}

func credential(t *testing.T, value string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "rcon")
	if err := os.WriteFile(p, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func changes(cs []servers.Change) []string {
	var out []string
	for _, c := range cs {
		out = append(out, c.String())
	}
	return out
}

func TestApplyReportsWhatChanges(t *testing.T) {
	st := serverstest.NewFakeStore()
	svc := servers.New(st, uuid.New(), []string{"wardogs"}, discard())
	ctx := context.Background()
	cred := credential(t, "secret\n")

	got, err := svc.Apply(ctx, manifest(t, cred), servers.ApplyOptions{CheckCredentials: true})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(changes(got), []string{"game wardogs added", "server wd-1 added"}) {
		t.Errorf("first apply = %v", changes(got))
	}
	if got, _ := svc.Apply(ctx, manifest(t, cred), servers.ApplyOptions{}); len(got) != 0 || st.Applies != 1 {
		t.Errorf("unchanged apply = %v, %d writes", changes(got), st.Applies)
	}

	// A changed server, a new one, a dry run: reported, nothing written.
	m := manifest(t, cred, servers.Server{ID: "wd-2", Name: "War Dogs #2", Game: "wardogs", Driver: "wardogs", Location: "slc",
		Endpoint: "http://203.0.113.11:7789", CredentialFile: cred, Trust: servers.TrustCommunity})
	m.Servers[0].Name = "Renamed"
	got, err = svc.Apply(ctx, m, servers.ApplyOptions{DryRun: true})
	if err != nil || !slices.Equal(changes(got), []string{"server wd-1 changed", "server wd-2 added"}) || st.Applies != 1 {
		t.Errorf("dry run = %v, %v, %d writes", changes(got), err, st.Applies)
	}
	if _, err := svc.Apply(ctx, m, servers.ApplyOptions{}); err != nil {
		t.Fatal(err)
	}

	// Export is the applied manifest.
	exported, err := svc.Manifest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := exported.Marshal()
	b, _ := m.Marshal()
	if string(a) != string(b) {
		t.Errorf("export =\n%s\nwant\n%s", a, b)
	}

	// Taking a server out removes it; the game stays while listed.
	got, err = svc.Apply(ctx, manifest(t, cred), servers.ApplyOptions{})
	if err != nil || !slices.Equal(changes(got), []string{"server wd-1 changed", "server wd-2 removed"}) {
		t.Errorf("removal = %v, %v", changes(got), err)
	}
	all, _ := svc.Servers(ctx)
	if len(all) != 1 || all[0].ID != "wd-1" {
		t.Errorf("servers = %+v", all)
	}
	if _, err := svc.Server(ctx, "wd-2"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a removed server = %v", err)
	}
}

func TestApplyRefusesBadManifestsAndUnreadableCredentials(t *testing.T) {
	st := serverstest.NewFakeStore()
	svc := servers.New(st, uuid.New(), []string{"wardogs"}, discard())
	ctx := context.Background()
	m := manifest(t, "/nonexistent/rcon")
	if _, err := svc.Apply(ctx, m, servers.ApplyOptions{CheckCredentials: true}); !errors.Is(err, servers.ErrCredentialFile) || strings.Contains(err.Error(), "secret") {
		t.Errorf("missing credential = %v", err)
	}
	if _, err := svc.Apply(ctx, manifest(t, credential(t, "  \n")), servers.ApplyOptions{CheckCredentials: true}); err == nil || !strings.Contains(err.Error(), "is empty") {
		t.Errorf("empty credential = %v", err)
	}
	m.Servers[0].Trust = ""
	if _, err := svc.Apply(ctx, m, servers.ApplyOptions{}); !errors.Is(err, servers.ErrInvalidManifest) {
		t.Errorf("invalid = %v", err)
	}
	if st.Applies != 0 {
		t.Errorf("%d writes", st.Applies)
	}
}

func TestGamesCarryTheTightenedBands(t *testing.T) {
	svc := servers.New(serverstest.NewFakeStore(), uuid.New(), []string{"wardogs"}, discard())
	ctx := context.Background()
	m := manifest(t, credential(t, "x"))
	v := int64(24)
	m.Games[0].Bands = []servers.Band{{Section: "MatchState.Playing.KOTH", Key: "ScorePeriod", Max: &v}}
	if _, err := svc.Apply(ctx, m, servers.ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	games, err := svc.Games(ctx)
	if err != nil || len(games) != 1 {
		t.Fatal(games, err)
	}
	b, _ := games[0].Band("MatchState.Playing.KOTH", "ScorePeriod")
	if *b.Min != 18 || *b.Max != 24 || len(games[0].Tightened) != 1 {
		t.Errorf("band = %+v, tightened %+v", b, games[0].Tightened)
	}
}
