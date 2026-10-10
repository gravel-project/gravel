package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ErrPseudonymTaken is a pseudonym another identity in the organization already has.
var ErrPseudonymTaken = errors.New("store: the pseudonym is taken")

// Match is a row of matches: one match on a server, open until EndedAt (ADR-0012).
type Match struct {
	ID             int64
	OrganizationID uuid.UUID
	ServerID       string
	GameID         string
	StartedAt      time.Time
	EndedAt        *time.Time
	Map            string
	RotationIndex  int
}

// MatchPlayer is a row of match_stats: one player's totals in one match, keyed by the identity the
// game reports, with where the counts came from and the server's trust when written.
type MatchPlayer struct {
	MatchID   int64
	Provider  string
	Subject   string
	Team      string
	Kills     int
	Deaths    int
	SecondsOn int
	FirstSeen time.Time
	LastSeen  time.Time
	Source    string
	Trust     string
}

const (
	matchColumns       = `id, organization_id, server_id, game_id, started_at, ended_at, map, rotation_index`
	matchPlayerColumns = `match_id, provider, subject, team, kills, deaths, seconds_on, first_seen, last_seen, source, trust`
)

func scanMatch(row pgx.Row) (Match, error) {
	var m Match
	err := row.Scan(&m.ID, &m.OrganizationID, &m.ServerID, &m.GameID, &m.StartedAt, &m.EndedAt, &m.Map, &m.RotationIndex)
	if errors.Is(err, pgx.ErrNoRows) {
		return Match{}, ErrNotFound
	}
	return m, err
}

func scanMatchPlayer(row pgx.Row) (MatchPlayer, error) {
	var p MatchPlayer
	err := row.Scan(&p.MatchID, &p.Provider, &p.Subject, &p.Team, &p.Kills, &p.Deaths, &p.SecondsOn, &p.FirstSeen, &p.LastSeen, &p.Source, &p.Trust)
	return p, err
}

// OpenMatch is a server's match in progress, or ErrNotFound.
func (s *Store) OpenMatch(ctx context.Context, orgID uuid.UUID, serverID string) (Match, error) {
	m, err := scanMatch(s.pool.QueryRow(ctx, `SELECT `+matchColumns+` FROM matches
		WHERE organization_id = $1 AND server_id = $2 AND ended_at IS NULL`, orgID, serverID))
	if err != nil && !errors.Is(err, ErrNotFound) {
		return Match{}, fmt.Errorf("store: open match: %w", err)
	}
	return m, err
}

// StartMatch ends the server's open match, if any, at m.StartedAt and starts m, in one
// transaction; it returns m with its id.
func (s *Store) StartMatch(ctx context.Context, m Match) (Match, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Match{}, fmt.Errorf("store: start match: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `UPDATE matches SET ended_at = $3 WHERE organization_id = $1 AND server_id = $2 AND ended_at IS NULL`,
		m.OrganizationID, m.ServerID, m.StartedAt); err != nil {
		return Match{}, fmt.Errorf("store: start match: end the open one: %w", err)
	}
	if err := tx.QueryRow(ctx, `INSERT INTO matches (organization_id, server_id, game_id, started_at, map, rotation_index)
		VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`,
		m.OrganizationID, m.ServerID, m.GameID, m.StartedAt, m.Map, m.RotationIndex).Scan(&m.ID); err != nil {
		return Match{}, fmt.Errorf("store: start match: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Match{}, fmt.Errorf("store: start match: %w", err)
	}
	m.EndedAt = nil
	return m, nil
}

// EndMatch ends a match at the given time; an ended one is left as it is.
func (s *Store) EndMatch(ctx context.Context, matchID int64, at time.Time) error {
	if _, err := s.pool.Exec(ctx, `UPDATE matches SET ended_at = $2 WHERE id = $1 AND ended_at IS NULL`, matchID, at); err != nil {
		return fmt.Errorf("store: end match: %w", err)
	}
	return nil
}

// MatchPlayers are a match's rows.
func (s *Store) MatchPlayers(ctx context.Context, matchID int64) ([]MatchPlayer, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+matchPlayerColumns+` FROM match_stats WHERE match_id = $1 ORDER BY provider, subject`, matchID)
	if err != nil {
		return nil, fmt.Errorf("store: match players: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (MatchPlayer, error) { return scanMatchPlayer(r) })
	if err != nil {
		return nil, fmt.Errorf("store: match players: %w", err)
	}
	return out, nil
}

// SaveMatchPlayers writes rows, in one transaction: a new player is inserted, a known one takes
// the row's team, counts, time on and last_seen (first_seen, source and trust stay as first
// written).
func (s *Store) SaveMatchPlayers(ctx context.Context, players []MatchPlayer) error {
	if len(players) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for _, p := range players {
		batch.Queue(`INSERT INTO match_stats (`+matchPlayerColumns+`) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
			ON CONFLICT (match_id, provider, subject) DO UPDATE SET
			  team = excluded.team, kills = excluded.kills, deaths = excluded.deaths,
			  seconds_on = excluded.seconds_on, last_seen = excluded.last_seen`,
			p.MatchID, p.Provider, p.Subject, p.Team, p.Kills, p.Deaths, p.SecondsOn, p.FirstSeen, p.LastSeen, p.Source, p.Trust)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: save match players: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := tx.SendBatch(ctx, batch).Close(); err != nil {
		return fmt.Errorf("store: save match players: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: save match players: %w", err)
	}
	return nil
}

// Board metrics: what a board ranks by.
const (
	BoardKills   = "kills"
	BoardDeaths  = "deaths"
	BoardKD      = "kd"
	BoardTime    = "time"
	BoardMatches = "matches"
)

// BoardQuery selects and ranks players. An empty ServerID or GameID is every one; a zero From or
// To is unbounded (matches by their start). Only rows written under one of Trusts count. Rolled-up
// periods (stats_monthly) count by their first day, read in Timezone (the organization's; UTC when
// empty), which is how a window's instants were made.
type BoardQuery struct {
	OrganizationID uuid.UUID
	ServerID       string
	GameID         string
	From, To       time.Time
	Timezone       string
	Trusts         []string
	Metric         string
	MinMatches     int
	Limit, Offset  int
}

// BoardRow is one player's totals over the query's matches.
type BoardRow struct {
	Provider  string
	Subject   string
	Kills     int
	Deaths    int
	SecondsOn int
	Matches   int
}

// Board ranks the players by the query's metric, then kills, then identity, so the order is stable.
func (s *Store) Board(ctx context.Context, q BoardQuery) ([]BoardRow, error) {
	order := map[string]string{
		BoardKills:   "sum(x.kills)",
		BoardDeaths:  "sum(x.deaths)",
		BoardKD:      "sum(x.kills)::float8 / greatest(sum(x.deaths), 1)",
		BoardTime:    "sum(x.seconds_on)",
		BoardMatches: "sum(x.matches)",
	}[q.Metric]
	if order == "" {
		return nil, fmt.Errorf("store: board: %q is not a metric", q.Metric)
	}
	var from, to *time.Time
	if !q.From.IsZero() {
		from = &q.From
	}
	if !q.To.IsZero() {
		to = &q.To
	}
	tz := q.Timezone
	if tz == "" {
		tz = "UTC"
	}
	// The raw rows by their match's start, and the rolled-up periods by their first day.
	rows, err := s.pool.Query(ctx, `
		SELECT x.provider, x.subject, sum(x.kills)::int, sum(x.deaths)::int, sum(x.seconds_on)::int, sum(x.matches)::int
		  FROM (
		    SELECT ms.provider, ms.subject, ms.kills, ms.deaths, ms.seconds_on, 1 AS matches
		      FROM match_stats ms JOIN matches m ON m.id = ms.match_id
		     WHERE m.organization_id = $1
		       AND ($2 = '' OR m.server_id = $2) AND ($3 = '' OR m.game_id = $3)
		       AND ($4::timestamptz IS NULL OR m.started_at >= $4) AND ($5::timestamptz IS NULL OR m.started_at < $5)
		       AND ms.trust = ANY($6)
		    UNION ALL
		    SELECT sm.provider, sm.subject, sm.kills, sm.deaths, sm.seconds_on, sm.matches
		      FROM stats_monthly sm
		     WHERE sm.organization_id = $1
		       AND ($2 = '' OR sm.server_id = $2) AND ($3 = '' OR sm.game_id = $3)
		       AND ($4::timestamptz IS NULL OR sm.period_from >= ($4::timestamptz AT TIME ZONE $10)::date)
		       AND ($5::timestamptz IS NULL OR sm.period_from < ($5::timestamptz AT TIME ZONE $10)::date)
		       AND sm.trust = ANY($6)
		  ) x
		 GROUP BY x.provider, x.subject
		HAVING sum(x.matches) >= $7
		 ORDER BY `+order+` DESC, sum(x.kills) DESC, x.provider, x.subject
		 LIMIT $8 OFFSET $9`,
		q.OrganizationID, q.ServerID, q.GameID, from, to, q.Trusts, q.MinMatches, q.Limit, q.Offset, tz)
	if err != nil {
		return nil, fmt.Errorf("store: board: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (BoardRow, error) {
		var b BoardRow
		err := r.Scan(&b.Provider, &b.Subject, &b.Kills, &b.Deaths, &b.SecondsOn, &b.Matches)
		return b, err
	})
	if err != nil {
		return nil, fmt.Errorf("store: board: %w", err)
	}
	return out, nil
}

// MatchSummary is a match with how many players it had and their kills.
type MatchSummary struct {
	Match
	Players int
	Kills   int
}

// ListMatches returns a server's matches newest first, with an id below beforeID (0 for the first
// page).
func (s *Store) ListMatches(ctx context.Context, orgID uuid.UUID, serverID string, beforeID int64, limit int) ([]MatchSummary, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT m.id, m.organization_id, m.server_id, m.game_id, m.started_at, m.ended_at, m.map, m.rotation_index,
		       coalesce(m.players, count(ms.subject))::int, coalesce(m.kills, sum(ms.kills), 0)::int
		  FROM matches m LEFT JOIN match_stats ms ON ms.match_id = m.id
		 WHERE m.organization_id = $1 AND m.server_id = $2 AND ($3 = 0 OR m.id < $3)
		 GROUP BY m.id ORDER BY m.id DESC LIMIT $4`, orgID, serverID, beforeID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list matches: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (MatchSummary, error) {
		var m MatchSummary
		err := r.Scan(&m.ID, &m.OrganizationID, &m.ServerID, &m.GameID, &m.StartedAt, &m.EndedAt, &m.Map, &m.RotationIndex, &m.Players, &m.Kills)
		return m, err
	})
	if err != nil {
		return nil, fmt.Errorf("store: list matches: %w", err)
	}
	return out, nil
}

// PlayerKey is a provider identity, for the lookups below.
type PlayerKey struct{ Provider, Subject string }

// Pseudonyms returns the stored pseudonyms of the given identities; one with none is absent.
func (s *Store) Pseudonyms(ctx context.Context, orgID uuid.UUID, keys []PlayerKey) (map[PlayerKey]string, error) {
	out := map[PlayerKey]string{}
	if len(keys) == 0 {
		return out, nil
	}
	providers, subjects := splitKeys(keys)
	rows, err := s.pool.Query(ctx, `SELECT p.provider, p.subject, p.name FROM pseudonyms p
		JOIN unnest($2::text[], $3::text[]) AS k(provider, subject) ON k.provider = p.provider AND k.subject = p.subject
		WHERE p.organization_id = $1`, orgID, providers, subjects)
	if err != nil {
		return nil, fmt.Errorf("store: pseudonyms: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var k PlayerKey
		var name string
		if err := rows.Scan(&k.Provider, &k.Subject, &name); err != nil {
			return nil, fmt.Errorf("store: pseudonyms: %w", err)
		}
		out[k] = name
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: pseudonyms: %w", err)
	}
	return out, nil
}

// AddPseudonym stores an identity's pseudonym. An identity that already has one keeps it (and
// AddPseudonym returns it); a name another identity has is ErrPseudonymTaken.
func (s *Store) AddPseudonym(ctx context.Context, orgID uuid.UUID, key PlayerKey, name string, at time.Time) (string, error) {
	tag, err := s.pool.Exec(ctx, `INSERT INTO pseudonyms (organization_id, provider, subject, name, created_at) VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (organization_id, provider, subject) DO NOTHING`, orgID, key.Provider, key.Subject, name, at)
	if err != nil {
		if isUniqueViolation(err) {
			return "", ErrPseudonymTaken
		}
		return "", fmt.Errorf("store: add pseudonym: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return name, nil
	}
	var kept string
	if err := s.pool.QueryRow(ctx, `SELECT name FROM pseudonyms WHERE organization_id = $1 AND provider = $2 AND subject = $3`,
		orgID, key.Provider, key.Subject).Scan(&kept); err != nil {
		return "", fmt.Errorf("store: add pseudonym: %w", err)
	}
	return kept, nil
}

// BoardMember is a linked identity's member, as a board shows them.
type BoardMember struct {
	UserID      uuid.UUID
	DisplayName string
	ShowName    bool
}

// BoardMembers resolves identities to the members who linked them; an unlinked one is absent.
func (s *Store) BoardMembers(ctx context.Context, keys []PlayerKey) (map[PlayerKey]BoardMember, error) {
	out := map[PlayerKey]BoardMember{}
	if len(keys) == 0 {
		return out, nil
	}
	providers, subjects := splitKeys(keys)
	rows, err := s.pool.Query(ctx, `SELECT i.provider, i.subject, u.id, u.display_name, u.show_name_on_boards
		  FROM identities i JOIN users u ON u.id = i.user_id
		  JOIN unnest($1::text[], $2::text[]) AS k(provider, subject) ON k.provider = i.provider AND k.subject = i.subject`,
		providers, subjects)
	if err != nil {
		return nil, fmt.Errorf("store: board members: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var k PlayerKey
		var m BoardMember
		if err := rows.Scan(&k.Provider, &k.Subject, &m.UserID, &m.DisplayName, &m.ShowName); err != nil {
			return nil, fmt.Errorf("store: board members: %w", err)
		}
		out[k] = m
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: board members: %w", err)
	}
	return out, nil
}

// SetShowNameOnBoards records a member's choice to be shown by name on boards.
func (s *Store) SetShowNameOnBoards(ctx context.Context, userID uuid.UUID, show bool) error {
	tag, err := s.pool.Exec(ctx, `UPDATE users SET show_name_on_boards = $2 WHERE id = $1`, userID, show)
	if err != nil {
		return fmt.Errorf("store: show name on boards: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ShowNameOnBoards is a member's choice.
func (s *Store) ShowNameOnBoards(ctx context.Context, userID uuid.UUID) (bool, error) {
	var show bool
	err := s.pool.QueryRow(ctx, `SELECT show_name_on_boards FROM users WHERE id = $1`, userID).Scan(&show)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, fmt.Errorf("store: show name on boards: %w", err)
	}
	return show, nil
}

func splitKeys(keys []PlayerKey) (providers, subjects []string) {
	for _, k := range keys {
		providers = append(providers, k.Provider)
		subjects = append(subjects, k.Subject)
	}
	return providers, subjects
}
