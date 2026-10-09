package store

import (
	"context"
	"fmt"
)

// Stats are the counts the hub reports as metrics: members, linked identities by provider, and
// the database's size on disk.
type Stats struct {
	Users         int64
	Identities    map[string]int64 // by provider; a provider with none is absent
	DatabaseBytes int64
}

// Stats reads the counts in one transaction, so users and identities agree with each other.
func (s *Store) Stats(ctx context.Context) (Stats, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Stats{}, fmt.Errorf("store: stats: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	st := Stats{Identities: map[string]int64{}}
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&st.Users); err != nil {
		return Stats{}, fmt.Errorf("store: stats: users: %w", err)
	}
	rows, err := tx.Query(ctx, `SELECT provider, count(*) FROM identities GROUP BY provider`)
	if err != nil {
		return Stats{}, fmt.Errorf("store: stats: identities: %w", err)
	}
	for rows.Next() {
		var provider string
		var n int64
		if err := rows.Scan(&provider, &n); err != nil {
			rows.Close()
			return Stats{}, fmt.Errorf("store: stats: identities: %w", err)
		}
		st.Identities[provider] = n
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Stats{}, fmt.Errorf("store: stats: identities: %w", err)
	}
	if err := tx.QueryRow(ctx, `SELECT pg_database_size(current_database())`).Scan(&st.DatabaseBytes); err != nil {
		return Stats{}, fmt.Errorf("store: stats: database size: %w", err)
	}
	return st, nil
}
