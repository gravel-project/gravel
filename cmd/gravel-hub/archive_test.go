package main

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/gravel-project/gravel/internal/store/storetest"
)

// export and import (gravel#213): a hub with its settings exported to a 0600 file and loaded into
// an empty database; a never-served database has nothing to export, an existing file isn't
// overwritten, and a second import is refused. (internal/store's round trip compares every row.)
func TestExportImport(t *testing.T) {
	_, cfgPath, dir, run := freshHub(t)
	archive := filepath.Join(dir, "hub.tar.gz")
	if code, _, errOut := run("export", "-o", archive, "--config", cfgPath); code != 1 || !strings.Contains(errOut, "never served") {
		t.Fatalf("export of a never-served database: %d %s", code, errOut)
	}
	manifest := filepath.Join(dir, "org.yaml")
	writeFile(t, manifest, "version: 1\nstats:\n  public: true\n")
	if code, _, errOut := run("settings", "apply", manifest, "--config", cfgPath); code != 0 {
		t.Fatalf("settings apply: %d %s", code, errOut)
	}
	code, _, errOut := run("export", "-o", archive, "--config", cfgPath)
	if code != 0 || !strings.Contains(errOut, "exported Test:") || !strings.Contains(errOut, "personal data") {
		t.Fatalf("export: %d %s", code, errOut)
	}
	if fi, err := os.Stat(archive); err != nil || fi.Mode().Perm() != 0o600 || fi.Size() == 0 {
		t.Fatalf("the archive: %v %v", fi, err)
	}
	if code, _, errOut := run("export", "-o", archive, "--config", cfgPath); code != 1 || !strings.Contains(errOut, "exists") {
		t.Errorf("an export over an existing file: %d %s", code, errOut)
	}

	// An empty database, as a new hub's.
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, storetest.DatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = admin.Close(ctx) }()
	for _, q := range []string{"DROP DATABASE IF EXISTS gravel_test_cli_import WITH (FORCE)", "CREATE DATABASE gravel_test_cli_import"} {
		if _, err := admin.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	u, _ := url.Parse(storetest.DatabaseURL(t))
	u.Path = "/gravel_test_cli_import"
	newCfg := filepath.Join(dir, "new-hub.yaml")
	writeFile(t, newCfg, "version: 1\norganization:\n  name: Test\ndatabase:\n  url: "+u.String()+"\n")
	code, out, errOut := run("import", archive, "--config", newCfg)
	if code != 0 || !strings.Contains(out, "imported Test:") {
		t.Fatalf("import: %d %s %s", code, out, errOut)
	}
	if code, out, _ := run("settings", "export", "--config", newCfg); code != 0 || !strings.Contains(out, "public: true") {
		t.Errorf("the imported hub's settings: %d %s", code, out)
	}
	if code, _, errOut := run("import", archive, "--config", newCfg); code != 1 || !strings.Contains(errOut, "not empty") {
		t.Errorf("a second import: %d %s", code, errOut)
	}
	if code, _, _ := run("import", "--config", newCfg); code != 2 {
		t.Errorf("import without an archive: %d", code)
	}
}
