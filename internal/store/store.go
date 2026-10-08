// Package store is the hub's Postgres access: a pgx pool, embedded goose migrations and the
// queries each domain package needs. Nothing outside this package speaks SQL.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
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
	pool   *pgxpool.Pool
	logger *slog.Logger
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

// MigrationStatus is what `gravel-hub migrate --status` and /readyz report.
type MigrationStatus struct {
	Current int64
	Latest  int64
	Pending int
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
	pending := 0
	for _, src := range p.ListSources() {
		if src.Version > current {
			pending++
		}
	}
	return MigrationStatus{Current: current, Latest: latest, Pending: pending}, nil
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
