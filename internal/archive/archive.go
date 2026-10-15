// Package archive is a hub's export and import (gravel#213): the exit promise, a host leaves with
// everything the hub holds and loads it into a fresh hub. An archive is a gzipped tar that gravel
// writes itself: every table's rows as CSV from one consistent snapshot (Postgres COPY), the
// Organization settings and the servers manifest as YAML a person can read and re-apply, a README,
// and manifest.json last (the format, gravel's version, the schema version, the tables and their
// row counts). It holds personal data and password hashes: it is written 0600 and is to be kept
// like a backup. Logins don't travel (sessions, tokens, one-time links: store.NotArchived).
package archive

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/gravel-project/gravel/internal/org"
	"github.com/gravel-project/gravel/internal/servers"
	"github.com/gravel-project/gravel/internal/store"
)

// Format is the archive layout's version: an importer refuses one it doesn't know.
const Format = 1

// Manifest is manifest.json.
type Manifest struct {
	Format       int                  `json:"format"`
	Gravel       string               `json:"gravel"`
	Schema       int64                `json:"schema"`
	ExportedAt   time.Time            `json:"exported_at"`
	Organization string               `json:"organization"`
	Tables       []store.ArchiveTable `json:"tables"`
}

// File is an extra file an archive carries beside the tables (settings.yaml, servers.yaml).
type File struct {
	Name string
	Data []byte
}

// Exporter is what Write reads the tables from (store.Store).
type Exporter interface {
	ExportTables(ctx context.Context, write func(t store.ArchiveTable, copyOut func(w io.Writer) (int64, error)) error) ([]store.ArchiveTable, int64, error)
}

const readme = `This is a gravel hub's export (gravel#213): everything the hub holds, to load into a fresh hub
with gravel-hub import. It holds personal data (members, their linked accounts, their stats) and
password hashes: keep it as you keep your backups, and delete it when it has served.

manifest.json  the format, gravel's version, the schema version, each table's rows
settings.yaml  the Organization settings (gravel-hub settings apply takes it)
servers.yaml   the servers, locations and games (gravel-hub servers apply takes it)
tables/        every table as CSV with a header (Postgres COPY), in the schema manifest.json names

Logins don't travel: members sign in again on the new hub, with the same passwords.
`

// Write writes an archive of the hub to w: the tables from ex, the extra files, and the manifest.
// Each table is spooled to a private temporary file first (a tar entry needs its size), never kept
// in memory whole.
func Write(ctx context.Context, ex Exporter, w io.Writer, gravelVersion, organization string, extras []File, now time.Time) (Manifest, error) {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	put := func(name string, size int64, r io.Reader) error {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: size, ModTime: now, Typeflag: tar.TypeReg}); err != nil {
			return fmt.Errorf("archive: %s: %w", name, err)
		}
		if _, err := io.Copy(tw, r); err != nil {
			return fmt.Errorf("archive: %s: %w", name, err)
		}
		return nil
	}
	if err := put("README.txt", int64(len(readme)), strings.NewReader(readme)); err != nil {
		return Manifest{}, err
	}
	for _, f := range extras {
		if err := put(f.Name, int64(len(f.Data)), strings.NewReader(string(f.Data))); err != nil {
			return Manifest{}, err
		}
	}
	spool, err := os.CreateTemp("", "gravel-export-*.csv")
	if err != nil {
		return Manifest{}, fmt.Errorf("archive: %w", err)
	}
	defer func() { _ = os.Remove(spool.Name()); _ = spool.Close() }()
	tables, schema, err := ex.ExportTables(ctx, func(t store.ArchiveTable, copyOut func(io.Writer) (int64, error)) error {
		if err := spool.Truncate(0); err != nil {
			return err
		}
		if _, err := spool.Seek(0, io.SeekStart); err != nil {
			return err
		}
		if _, err := copyOut(spool); err != nil {
			return err
		}
		size, err := spool.Seek(0, io.SeekCurrent)
		if err != nil {
			return err
		}
		if _, err := spool.Seek(0, io.SeekStart); err != nil {
			return err
		}
		return put(tablePath(t.Name), size, io.LimitReader(spool, size))
	})
	if err != nil {
		return Manifest{}, err
	}
	m := Manifest{Format: Format, Gravel: gravelVersion, Schema: schema, ExportedAt: now.UTC(), Organization: organization, Tables: tables}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return Manifest{}, err
	}
	b = append(b, '\n')
	if err := put("manifest.json", int64(len(b)), strings.NewReader(string(b))); err != nil {
		return Manifest{}, err
	}
	if err := tw.Close(); err != nil {
		return Manifest{}, fmt.Errorf("archive: %w", err)
	}
	if err := gz.Close(); err != nil {
		return Manifest{}, fmt.Errorf("archive: %w", err)
	}
	return m, nil
}

func tablePath(table string) string { return "tables/" + table + ".csv" }

// Importer is what Load writes into (store.Store).
type Importer interface {
	SchemaVersion(ctx context.Context) (int64, error)
	MigrateUpTo(ctx context.Context, version int64) error
	Migrate(ctx context.Context) error
	ImportTables(ctx context.Context, tables []store.ArchiveTable, open func(t store.ArchiveTable) (io.Reader, error)) error
}

// ErrUnsupported is an archive this gravel can't load: an unknown format, or a schema newer than
// its migrations (export from a hub no newer than the one importing).
var ErrUnsupported = errors.New("archive: not one this gravel can load")

// Load reads an archive and loads it into an empty database (store.ImportTables's rules): the
// database is migrated to the archive's schema version, the tables load in one transaction, then
// the rest of the migrations run, so an archive from an older gravel arrives in this one's schema.
// latest is the newest migration this gravel has. The archive is unpacked into a private
// temporary directory, removed after.
func Load(ctx context.Context, im Importer, r io.Reader, latest int64) (Manifest, error) {
	dir, err := os.MkdirTemp("", "gravel-import-*")
	if err != nil {
		return Manifest{}, fmt.Errorf("archive: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := unpack(r, dir); err != nil {
		return Manifest{}, err
	}
	b, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return Manifest{}, fmt.Errorf("%w: no manifest.json (%w)", ErrUnsupported, err)
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return Manifest{}, fmt.Errorf("%w: manifest.json: %w", ErrUnsupported, err)
	}
	switch {
	case m.Format != Format:
		return m, fmt.Errorf("%w: format %d (this gravel reads %d)", ErrUnsupported, m.Format, Format)
	case m.Schema < 1 || m.Schema > latest:
		return m, fmt.Errorf("%w: schema %d from gravel %s, and this gravel's newest is %d: import with a gravel at least as new as the one that exported", ErrUnsupported, m.Schema, m.Gravel, latest)
	}
	current, err := im.SchemaVersion(ctx)
	if err != nil && !isNoLedger(err) {
		return m, err
	}
	if current > m.Schema {
		return m, fmt.Errorf("%w: the database is at schema %d, past the archive's %d: import into an empty database", store.ErrNotEmpty, current, m.Schema)
	}
	if err := im.MigrateUpTo(ctx, m.Schema); err != nil {
		return m, err
	}
	var open []*os.File
	defer func() {
		for _, f := range open {
			_ = f.Close()
		}
	}()
	if err := im.ImportTables(ctx, m.Tables, func(t store.ArchiveTable) (io.Reader, error) {
		f, err := os.Open(filepath.Join(dir, filepath.FromSlash(tablePath(t.Name))))
		if err != nil {
			return nil, fmt.Errorf("archive: %s: %w", t.Name, err)
		}
		open = append(open, f)
		return f, nil
	}); err != nil {
		return m, err
	}
	return m, im.Migrate(ctx)
}

// isNoLedger is a database migrations never ran on (no goose_db_version yet): schema 0.
func isNoLedger(err error) bool { return strings.Contains(err.Error(), "goose_db_version") }

// unpack extracts a gzipped tar into dir: regular files only, at clean relative paths.
func unpack(r io.Reader, dir string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("%w: not a gzipped archive (%w)", ErrUnsupported, err)
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("%w: %w", ErrUnsupported, err)
		}
		name := path.Clean(h.Name)
		if h.Typeflag != tar.TypeReg || path.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") {
			return fmt.Errorf("%w: entry %q", ErrUnsupported, h.Name)
		}
		dst := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return err
		}
		f, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return fmt.Errorf("archive: %s: %w", name, err)
		}
		if _, err := io.Copy(f, tr); err != nil { //nolint:gosec // the importer's own archive; its size is bounded by the disk
			_ = f.Close()
			return fmt.Errorf("archive: %s: %w", name, err)
		}
		if err := f.Close(); err != nil {
			return err
		}
	}
}

// Readable are an archive's readable files, read through the hub's own services: the
// Organization settings and the servers manifest as YAML, as `settings export` and
// `servers export` print them. organization is the hub's name, for the manifest.
func Readable(ctx context.Context, o *org.Service, sv *servers.Service) (files []File, organization string, err error) {
	got, err := o.Get(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("archive: the hub's organization: %w", err)
	}
	set, _, err := o.Settings(ctx)
	if err != nil {
		return nil, "", err
	}
	settings, err := set.Manifest()
	if err != nil {
		return nil, "", err
	}
	m, err := sv.Manifest(ctx)
	if err != nil {
		return nil, "", err
	}
	srv, err := m.Marshal()
	if err != nil {
		return nil, "", err
	}
	return []File{{Name: "settings.yaml", Data: settings}, {Name: "servers.yaml", Data: srv}}, got.Name, nil
}
