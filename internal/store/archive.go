package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
)

// ArchiveTable is one table an export carries (gravel#213): its name and its columns in order, the
// order the rows are written and read back in.
type ArchiveTable struct {
	Name    string   `json:"name"`
	Columns []string `json:"columns"`
	Rows    int64    `json:"rows"`
}

// NotArchived are the tables an export leaves out: the migration ledger (the archive says its
// schema version) and what only lasts minutes or a login (sessions and their attempts, app and
// authorization tokens, one-time password links and recovery-address links). Leaving them out
// ends every login and every open link; nothing a member or an operator made is lost.
var NotArchived = []string{"goose_db_version", "sessions", "auth_attempts", "app_tokens", "authorization_codes", "password_links", "email_verifications"}

// ErrNotEmpty is an import into a database that holds a hub's data already.
var ErrNotEmpty = errors.New("store: the database is not empty")

// archiveTables are the schema's tables an export carries, by name, with their columns.
func archiveTables(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}) ([]ArchiveTable, error) {
	rows, err := q.Query(ctx, `SELECT c.table_name, array_agg(c.column_name::text ORDER BY c.ordinal_position)
		  FROM information_schema.columns c JOIN information_schema.tables t
		    ON t.table_schema = c.table_schema AND t.table_name = c.table_name
		 WHERE c.table_schema = 'public' AND t.table_type = 'BASE TABLE' AND c.is_generated = 'NEVER'
		 GROUP BY c.table_name ORDER BY c.table_name`)
	if err != nil {
		return nil, fmt.Errorf("store: archive tables: %w", err)
	}
	all, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (ArchiveTable, error) {
		var t ArchiveTable
		err := r.Scan(&t.Name, &t.Columns)
		return t, err
	})
	if err != nil {
		return nil, fmt.Errorf("store: archive tables: %w", err)
	}
	return slices.DeleteFunc(all, func(t ArchiveTable) bool { return slices.Contains(NotArchived, t.Name) }), nil
}

// ExportTables writes every archived table's rows, each through write as CSV with a header, from
// one consistent snapshot (a read-only repeatable-read transaction), and returns the tables with
// their row counts and the schema version the rows are in.
func (s *Store) ExportTables(ctx context.Context, write func(t ArchiveTable, copyOut func(w io.Writer) (int64, error)) error) ([]ArchiveTable, int64, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, 0, fmt.Errorf("store: export: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var version int64
	if err := tx.QueryRow(ctx, `SELECT coalesce(max(version_id), 0) FROM goose_db_version WHERE is_applied`).Scan(&version); err != nil {
		return nil, 0, fmt.Errorf("store: export: schema version: %w", err)
	}
	tables, err := archiveTables(ctx, tx)
	if err != nil {
		return nil, 0, err
	}
	for i, t := range tables {
		copyOut := func(w io.Writer) (int64, error) {
			tag, err := tx.Conn().PgConn().CopyTo(ctx, w, fmt.Sprintf("COPY %s (%s) TO STDOUT WITH (FORMAT csv, HEADER true)",
				pgx.Identifier{t.Name}.Sanitize(), columnList(t.Columns)))
			if err != nil {
				return 0, fmt.Errorf("store: export %s: %w", t.Name, err)
			}
			return tag.RowsAffected(), nil
		}
		var rows int64
		if err := write(t, func(w io.Writer) (int64, error) {
			n, err := copyOut(w)
			rows = n
			return n, err
		}); err != nil {
			return nil, 0, err
		}
		tables[i].Rows = rows
	}
	return tables, version, nil
}

// ImportTables loads an export's tables into this database (gravel#213), which must be migrated to
// the archive's schema version and hold no hub data (never served: every archived table empty).
// One transaction with every foreign key deferred, so the order doesn't matter and a cycle loads;
// the keys are checked at commit. Each table's columns are the archive's, which must be the
// database's at that version. Identity and serial columns' sequences move past the loaded ids.
func (s *Store) ImportTables(ctx context.Context, tables []ArchiveTable, open func(t ArchiveTable) (io.Reader, error)) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: import: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	have, err := archiveTables(ctx, tx)
	if err != nil {
		return err
	}
	byName := map[string]ArchiveTable{}
	for _, t := range have {
		byName[t.Name] = t
		var has bool
		if err := tx.QueryRow(ctx, fmt.Sprintf("SELECT EXISTS (SELECT 1 FROM %s)", pgx.Identifier{t.Name}.Sanitize())).Scan(&has); err != nil {
			return fmt.Errorf("store: import: %w", err)
		}
		if has {
			return fmt.Errorf("%w: %s has rows (import into a database no hub has served)", ErrNotEmpty, t.Name)
		}
	}
	for _, t := range tables {
		h, ok := byName[t.Name]
		if !ok {
			return fmt.Errorf("store: import: the archive's table %s is not in this schema", t.Name)
		}
		if !slices.Equal(h.Columns, t.Columns) {
			return fmt.Errorf("store: import: %s's columns are %v in the archive and %v here", t.Name, t.Columns, h.Columns)
		}
		delete(byName, t.Name)
	}
	if len(byName) > 0 {
		missing := make([]string, 0, len(byName))
		for n := range byName {
			missing = append(missing, n)
		}
		slices.Sort(missing)
		return fmt.Errorf("store: import: the archive has no %s", strings.Join(missing, ", "))
	}
	if _, err := tx.Exec(ctx, `SET CONSTRAINTS ALL DEFERRED`); err != nil {
		return fmt.Errorf("store: import: %w", err)
	}
	for _, t := range tables {
		r, err := open(t)
		if err != nil {
			return err
		}
		tag, err := tx.Conn().PgConn().CopyFrom(ctx, r, fmt.Sprintf("COPY %s (%s) FROM STDIN WITH (FORMAT csv, HEADER true)",
			pgx.Identifier{t.Name}.Sanitize(), columnList(t.Columns)))
		if err != nil {
			return fmt.Errorf("store: import %s: %w", t.Name, err)
		}
		if tag.RowsAffected() != t.Rows {
			return fmt.Errorf("store: import %s: %d rows loaded, the archive says %d", t.Name, tag.RowsAffected(), t.Rows)
		}
	}
	// Identity and serial columns go on from the highest id loaded.
	rows, err := tx.Query(ctx, `SELECT table_name, column_name FROM information_schema.columns
		WHERE table_schema = 'public' AND (is_identity = 'YES' OR column_default LIKE 'nextval(%') AND table_name <> 'goose_db_version'`)
	if err != nil {
		return fmt.Errorf("store: import: sequences: %w", err)
	}
	type col struct{ table, column string }
	seqs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (col, error) {
		var c col
		err := r.Scan(&c.table, &c.column)
		return c, err
	})
	if err != nil {
		return fmt.Errorf("store: import: sequences: %w", err)
	}
	for _, c := range seqs {
		if _, err := tx.Exec(ctx, fmt.Sprintf(`SELECT setval(pg_get_serial_sequence($1, $2), coalesce(max(%s), 0) + 1, false) FROM %s`,
			pgx.Identifier{c.column}.Sanitize(), pgx.Identifier{c.table}.Sanitize()), c.table, c.column); err != nil {
			return fmt.Errorf("store: import: sequence of %s.%s: %w", c.table, c.column, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: import: %w", err)
	}
	return nil
}

// SchemaVersion is the newest migration applied (0 for none).
func (s *Store) SchemaVersion(ctx context.Context) (int64, error) {
	var v int64
	err := s.pool.QueryRow(ctx, `SELECT coalesce(max(version_id), 0) FROM goose_db_version WHERE is_applied`).Scan(&v)
	if err != nil {
		return 0, fmt.Errorf("store: schema version: %w", err)
	}
	return v, nil
}

func columnList(cols []string) string {
	q := make([]string, len(cols))
	for i, c := range cols {
		q[i] = pgx.Identifier{c}.Sanitize()
	}
	return strings.Join(q, ", ")
}
