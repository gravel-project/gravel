// Package store is the hub's Postgres access: a pgx pool, embedded goose migrations and the
// queries each domain package needs. Nothing outside this package speaks SQL.
package store

import (
	"cmp"
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("not found")

// Store is an open connection pool.
type Store struct {
	pool    *pgxpool.Pool
	logger  *slog.Logger
	onEvent func(kind string) // OnEvent
}

// Open connects to url, waiting up to connectTimeout for the database to accept connections
// (a container stack starts Postgres and the hub together).
func Open(ctx context.Context, url string, maxConns int32, connectTimeout time.Duration, logger *slog.Logger) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("store: parse database url: %w", err)
	}
	cfg.MaxConns = maxConns
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	deadline := time.Now().Add(connectTimeout)
	wait := 500 * time.Millisecond
	for {
		pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err = pool.Ping(pingCtx)
		cancel()
		if err == nil {
			return &Store{pool: pool, logger: logger}, nil
		}
		if time.Now().Add(wait).After(deadline) {
			pool.Close()
			return nil, fmt.Errorf("store: database not reachable after %s: %w", connectTimeout, err)
		}
		logger.Info("waiting for the database", "retry_in", wait, "error", err.Error())
		select {
		case <-ctx.Done():
			pool.Close()
			return nil, fmt.Errorf("store: %w", ctx.Err())
		case <-time.After(wait):
		}
		if wait < 5*time.Second {
			wait *= 2
		}
	}
}

// Close releases the pool.
func (s *Store) Close() { s.pool.Close() }

// Ping checks that a connection can be acquired.
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

func (s *Store) sqlDB() *sql.DB { return stdlib.OpenDBFromPool(s.pool) }

func (s *Store) provider() (*goose.Provider, *sql.DB, error) {
	fsys, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		return nil, nil, fmt.Errorf("store: migrations: %w", err)
	}
	db := s.sqlDB()
	p, err := goose.NewProvider(goose.DialectPostgres, db, fsys)
	if err != nil {
		_ = db.Close()
		return nil, nil, fmt.Errorf("store: migrations: %w", err)
	}
	return p, db, nil
}

// Migrate applies every pending migration. It is safe to run on every start.
func (s *Store) Migrate(ctx context.Context) error {
	p, db, err := s.provider()
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }() // closes the sql wrapper, not the pool
	results, err := p.Up(ctx)
	if err != nil {
		return fmt.Errorf("store: migrate: %w", err)
	}
	for _, r := range results {
		s.logger.Info("applied migration", "version", r.Source.Version, "file", r.Source.Path, "took", r.Duration)
	}
	return nil
}

// MigrateUpTo applies the pending migrations up to version and no further: an import (gravel#213)
// loads an archive into the schema it was written in, then migrates the rest.
func (s *Store) MigrateUpTo(ctx context.Context, version int64) error {
	p, db, err := s.provider()
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }() // closes the sql wrapper, not the pool
	results, err := p.UpTo(ctx, version)
	if err != nil {
		return fmt.Errorf("store: migrate up to %d: %w", version, err)
	}
	for _, r := range results {
		s.logger.Info("applied migration", "version", r.Source.Version, "file", r.Source.Path, "took", r.Duration)
	}
	return nil
}

// MigrateDownTo rolls the schema back to version (0 = empty). Destructive; used by storetest.
func (s *Store) MigrateDownTo(ctx context.Context, version int64) error {
	p, db, err := s.provider()
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }() // closes the sql wrapper, not the pool
	if _, err := p.DownTo(ctx, version); err != nil {
		return fmt.Errorf("store: migrate down: %w", err)
	}
	return nil
}

// MigrationStatus is what `gravel-hub migrate --check` and /readyz report.
type MigrationStatus struct {
	Current int64
	Latest  int64
	Pending int
	// PendingMigrations are the migrations an upgrade would apply, oldest first.
	PendingMigrations []Migration
}

// Migration is one embedded migration and what applying it costs (gravel#214).
type Migration struct {
	Version int64
	// Name is the file's name without its version and extension: "ownership_transfer".
	Name string
	// Cost is the migration's `-- gravel:cost` line: "instant" (catalog changes only, milliseconds
	// whatever the data), "scan <tables>" (reads every row of those tables under a lock that blocks
	// writes to them: a constraint or foreign key validated, an index built, a backfill), or
	// "rewrite <tables>" (rewrites every row: time and disk grow with the tables, reads block too).
	Cost string
}

// costRE is a valid `-- gravel:cost` value.
var costRE = regexp.MustCompile(`^(instant|(scan|rewrite) [a-z_][a-z0-9_]*(, [a-z_][a-z0-9_]*)*)$`)

// Migrations lists the embedded migrations, oldest first, each with its cost. A migration
// without a valid `-- gravel:cost` line among its leading comments is an error: an upgrade must
// be able to say what it will do before it does it.
func Migrations() ([]Migration, error) {
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		return nil, fmt.Errorf("store: migrations: %w", err)
	}
	var out []Migration
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".sql") {
			continue
		}
		num, rest, ok := strings.Cut(strings.TrimSuffix(name, ".sql"), "_")
		v, err := strconv.ParseInt(num, 10, 64)
		if !ok || err != nil {
			return nil, fmt.Errorf("store: migration %s: not <version>_<name>.sql", name)
		}
		b, err := migrationFiles.ReadFile("migrations/" + name)
		if err != nil {
			return nil, fmt.Errorf("store: migration %s: %w", name, err)
		}
		m := Migration{Version: v, Name: rest}
		for line := range strings.SplitSeq(string(b), "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "--") {
				break
			}
			if c, ok := strings.CutPrefix(line, "-- gravel:cost "); ok {
				m.Cost = strings.TrimSpace(c)
				break
			}
		}
		if !costRE.MatchString(m.Cost) {
			return nil, fmt.Errorf("store: migration %s: its leading comments need a line \"-- gravel:cost instant\", \"-- gravel:cost scan <table>, …\" or \"-- gravel:cost rewrite <table>, …\" (docs/hub.md, Migrations); got %q", name, m.Cost)
		}
		out = append(out, m)
	}
	slices.SortFunc(out, func(a, b Migration) int { return cmp.Compare(a.Version, b.Version) })
	return out, nil
}

// Status reports the applied and latest migration versions.
func (s *Store) Status(ctx context.Context) (MigrationStatus, error) {
	p, db, err := s.provider()
	if err != nil {
		return MigrationStatus{}, err
	}
	defer func() { _ = db.Close() }() // closes the sql wrapper, not the pool
	current, err := p.GetDBVersion(ctx)
	if err != nil {
		return MigrationStatus{}, fmt.Errorf("store: migration status: %w", err)
	}
	var latest int64
	for _, src := range p.ListSources() {
		if src.Version > latest {
			latest = src.Version
		}
	}
	all, err := Migrations()
	if err != nil {
		return MigrationStatus{}, err
	}
	st := MigrationStatus{Current: current, Latest: latest}
	for _, m := range all {
		if m.Version > current {
			st.PendingMigrations = append(st.PendingMigrations, m)
		}
	}
	st.Pending = len(st.PendingMigrations)
	return st, nil
}

// Ready is the readiness check: the database answers and no migration is pending.
func (s *Store) Ready(ctx context.Context) error {
	if err := s.Ping(ctx); err != nil {
		return fmt.Errorf("database: %w", err)
	}
	st, err := s.Status(ctx)
	if err != nil {
		return err
	}
	if st.Pending > 0 {
		return fmt.Errorf("%d migration(s) pending (schema %d, latest %d): run gravel-hub migrate", st.Pending, st.Current, st.Latest)
	}
	return nil
}
