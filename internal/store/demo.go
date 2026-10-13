package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// DemoVerification is the verification method on every identity `gravel-hub demo seed` writes
// (gravel#112): no provider proved anything, the account is made up. It is the marker of a demo
// member: a user whose identities all carry it. A login always records its provider's own method
// (oauth2, openid), so nobody who signed in is ever counted as one, and SeedDemo writes no other.
const DemoVerification = "demo"

// ErrNotADemoDatabase is a database the demo seed refuses: it holds a user who signed in, or
// history (matches, moderation, bans, events) already. The error is a *DemoRefusal.
var ErrNotADemoDatabase = errors.New("store: the database is not an empty demo database")

// DemoCensus is what a database holds, as the demo seed and a hub's start read it. It counts
// every organization's rows: a hub is single-tenant, and a refusal must not miss one.
type DemoCensus struct {
	// Users is every user; DemoUsers those whose identities are all DemoVerification.
	Users, DemoUsers int
	// The history a seed must never mix with.
	Matches, AuditEntries, Bans, IngestBatches, ParticipationEvents, MonthlyRows, Occupancy, Pseudonyms int
	// Apps are registered first-party apps (a bot): a hub in use has one before anyone signs in,
	// so a seed refuses them too. A demo hub registers its apps after the seed.
	Apps int
}

// RealUsers are the users who are not demo members: people who signed in.
func (c DemoCensus) RealUsers() int { return c.Users - c.DemoUsers }

// History is how many rows of history, and of apps, the database holds.
func (c DemoCensus) History() int {
	return c.Matches + c.AuditEntries + c.Bans + c.IngestBatches + c.ParticipationEvents + c.MonthlyRows + c.Occupancy + c.Pseudonyms + c.Apps
}

// DemoRefusal is why SeedDemo wrote nothing.
type DemoRefusal struct{ Census DemoCensus }

func (e *DemoRefusal) Error() string {
	c := e.Census
	switch {
	case c.RealUsers() > 0:
		return fmt.Sprintf("store: the database holds %d users who signed in for real; the demo seed writes only into an empty database", c.RealUsers())
	case c.DemoUsers > 0:
		return fmt.Sprintf("store: the database was seeded already (%d demo members); seed a fresh database", c.DemoUsers)
	}
	return fmt.Sprintf("store: the database holds history already (%d matches, %d audit entries, %d bans, %d ingest batches, %d participation events, %d monthly rows, %d occupancy minutes, %d pseudonyms, %d apps); the demo seed writes only into an empty database",
		c.Matches, c.AuditEntries, c.Bans, c.IngestBatches, c.ParticipationEvents, c.MonthlyRows, c.Occupancy, c.Pseudonyms, c.Apps)
}

func (e *DemoRefusal) Unwrap() error { return ErrNotADemoDatabase }

const demoCensusQuery = `SELECT
	(SELECT count(*) FROM users),
	(SELECT count(*) FROM users u
	  WHERE EXISTS (SELECT 1 FROM identities i WHERE i.user_id = u.id)
	    AND NOT EXISTS (SELECT 1 FROM identities i WHERE i.user_id = u.id AND i.verification_method <> $1)),
	(SELECT count(*) FROM matches),
	(SELECT count(*) FROM audit_log),
	(SELECT count(*) FROM bans),
	(SELECT count(*) FROM ingest_batches),
	(SELECT count(*) FROM participation_events),
	(SELECT count(*) FROM stats_monthly) + (SELECT count(*) FROM participation_monthly),
	(SELECT count(*) FROM server_occupancy),
	(SELECT count(*) FROM pseudonyms),
	(SELECT count(*) FROM apps)`

func scanCensus(row pgx.Row) (DemoCensus, error) {
	var c DemoCensus
	err := row.Scan(&c.Users, &c.DemoUsers, &c.Matches, &c.AuditEntries, &c.Bans, &c.IngestBatches, &c.ParticipationEvents, &c.MonthlyRows, &c.Occupancy, &c.Pseudonyms, &c.Apps)
	return c, err
}

// DemoCensus counts what the database holds.
func (s *Store) DemoCensus(ctx context.Context) (DemoCensus, error) {
	c, err := scanCensus(s.pool.QueryRow(ctx, demoCensusQuery, DemoVerification))
	if err != nil {
		return DemoCensus{}, fmt.Errorf("store: demo census: %w", err)
	}
	return c, nil
}

// HasDemoMembers reports whether any identity is a demo one: the database was seeded. A hub that
// is not a demo refuses to serve it.
func (s *Store) HasDemoMembers(ctx context.Context) (bool, error) {
	var found bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM identities WHERE verification_method = $1)`, DemoVerification).Scan(&found); err != nil {
		return false, fmt.Errorf("store: demo members: %w", err)
	}
	return found, nil
}

// DemoMember is one made-up member: the user, whether they show their name on boards, and their
// identities (each written with DemoVerification, whatever it says).
type DemoMember struct {
	User       User
	ShowName   bool
	Identities []Identity
}

// DemoMatch is one past match with its final team scores and its players' totals (MatchID is
// filled in).
type DemoMatch struct {
	Match   Match
	Teams   []TeamScore
	Players []MatchPlayer
}

// DemoOccupancy is one minute of a server's player counts.
type DemoOccupancy struct {
	ServerID     string
	Minute       time.Time
	Fewest, Most int
}

// DemoBan is a ban a moderation entry made, lifted by another or still in force. BanEntry and
// LiftEntry index DemoHistory.Moderation; LiftEntry is -1 for a ban still in force.
type DemoBan struct {
	ServerID, Provider, Subject, Reason string
	BanEntry, LiftEntry                 int
}

// DemoHistory is everything a demo seed writes.
type DemoHistory struct {
	Members []DemoMember
	Matches []DemoMatch
	// Occupancy are the minutes anyone was on.
	Occupancy []DemoOccupancy
	// Moderation are finished audit entries, oldest first; their actors are members.
	Moderation []AuditEntry
	Bans       []DemoBan
	// BanLists are the servers whose ban list the hub owns from the given time.
	BanLists map[string]time.Time
}

// demoTables are what SeedDemo locks against every other writer until it commits, so nothing can
// be recorded between its census and its writes.
const demoTables = `users, identities, identity_events, role_changes, matches, match_stats, audit_log, bans, ban_lists,
	ingest_batches, participation_events, stats_monthly, participation_monthly, server_occupancy, pseudonyms, apps`

// SeedDemo writes a demo history in one transaction, and only into an empty database: it locks
// the tables it reads and writes, counts what they hold, and refuses (a *DemoRefusal, nothing
// written) when there is any user or any history at all. The refusal lives here, beside the SQL,
// so no caller can skip it.
func (s *Store) SeedDemo(ctx context.Context, orgID uuid.UUID, h DemoHistory) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: seed demo: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `LOCK TABLE `+demoTables+` IN SHARE ROW EXCLUSIVE MODE`); err != nil {
		return fmt.Errorf("store: seed demo: lock: %w", err)
	}
	census, err := scanCensus(tx.QueryRow(ctx, demoCensusQuery, DemoVerification))
	if err != nil {
		return fmt.Errorf("store: seed demo: census: %w", err)
	}
	if census.Users > 0 || census.History() > 0 {
		return &DemoRefusal{Census: census}
	}

	if err := seedMembers(ctx, tx, orgID, h.Members); err != nil {
		return err
	}
	if err := seedMatches(ctx, tx, orgID, h.Matches); err != nil {
		return err
	}
	occupancy := make([][]any, 0, len(h.Occupancy))
	for _, o := range h.Occupancy {
		occupancy = append(occupancy, []any{orgID, o.ServerID, o.Minute.UTC().Truncate(time.Minute), o.Fewest, o.Most})
	}
	if _, err := tx.CopyFrom(ctx, pgx.Identifier{"server_occupancy"}, []string{"organization_id", "server_id", "minute", "min_players", "max_players"},
		pgx.CopyFromRows(occupancy)); err != nil {
		return fmt.Errorf("store: seed demo: occupancy: %w", err)
	}
	if err := seedModeration(ctx, tx, orgID, h); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: seed demo: %w", err)
	}
	return nil
}

func seedMembers(ctx context.Context, tx pgx.Tx, orgID uuid.UUID, members []DemoMember) error {
	for _, m := range members {
		u := m.User
		if _, err := tx.Exec(ctx, `INSERT INTO users (id, organization_id, display_name, created_at, last_login_at, show_name_on_boards)
			VALUES ($1, $2, $3, $4, $5, $6)`, u.ID, orgID, u.DisplayName, u.CreatedAt, u.LastLoginAt, m.ShowName); err != nil {
			return fmt.Errorf("store: seed demo: member: %w", err)
		}
		for _, role := range u.Roles { // instance-wide grants (ADR-0027)
			if _, err := tx.Exec(ctx, `INSERT INTO role_grants (organization_id, user_id, role, granted_at) VALUES ($1, $2, $3, $4)`,
				orgID, u.ID, role, u.CreatedAt); err != nil {
				return fmt.Errorf("store: seed demo: role: %w", err)
			}
			if _, err := tx.Exec(ctx, `INSERT INTO role_changes (user_id, role, granted, by_user_id, at) VALUES ($1, $2, true, NULL, $3)`,
				u.ID, role, u.CreatedAt); err != nil {
				return fmt.Errorf("store: seed demo: role: %w", err)
			}
		}
		for i, ident := range m.Identities {
			if _, err := tx.Exec(ctx, `INSERT INTO identities (provider, subject, user_id, display_name, avatar_url, verification_method, verified_at, linked_at, last_login_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
				ident.Provider, ident.Subject, u.ID, ident.DisplayName, ident.AvatarURL, DemoVerification, ident.VerifiedAt, ident.LinkedAt, ident.LastLoginAt); err != nil {
				if isUniqueViolation(err) {
					return ErrIdentityExists
				}
				return fmt.Errorf("store: seed demo: identity: %w", err)
			}
			event := IdentityEventLinked
			if i == 0 {
				event = IdentityEventRegistered
			}
			if err := appendIdentityEvent(ctx, tx, u.ID, ident.Provider, ident.Subject, event, ident.LinkedAt); err != nil {
				return err
			}
		}
	}
	return nil
}

func seedMatches(ctx context.Context, tx pgx.Tx, orgID uuid.UUID, matches []DemoMatch) error {
	batch := &pgx.Batch{}
	for _, m := range matches {
		teams := m.Teams
		if teams == nil {
			teams = []TeamScore{}
		}
		raw, err := json.Marshal(teams)
		if err != nil {
			return fmt.Errorf("store: seed demo: teams: %w", err)
		}
		batch.Queue(`INSERT INTO matches (organization_id, server_id, game_id, started_at, ended_at, map, rotation_index, teams)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING id`,
			orgID, m.Match.ServerID, m.Match.GameID, m.Match.StartedAt, m.Match.EndedAt, m.Match.Map, m.Match.RotationIndex, raw)
	}
	results := tx.SendBatch(ctx, batch)
	ids := make([]int64, len(matches))
	for i := range matches {
		if err := results.QueryRow().Scan(&ids[i]); err != nil {
			_ = results.Close()
			return fmt.Errorf("store: seed demo: match: %w", err)
		}
	}
	if err := results.Close(); err != nil {
		return fmt.Errorf("store: seed demo: matches: %w", err)
	}
	var rows [][]any
	for i, m := range matches {
		for _, p := range m.Players {
			rows = append(rows, []any{ids[i], p.Provider, p.Subject, p.Team, p.Kills, p.Deaths, p.SecondsOn, p.FirstSeen, p.LastSeen, p.Source, p.Trust})
		}
	}
	if _, err := tx.CopyFrom(ctx, pgx.Identifier{"match_stats"},
		[]string{"match_id", "provider", "subject", "team", "kills", "deaths", "seconds_on", "first_seen", "last_seen", "source", "trust"},
		pgx.CopyFromRows(rows)); err != nil {
		return fmt.Errorf("store: seed demo: match stats: %w", err)
	}
	return nil
}

func seedModeration(ctx context.Context, tx pgx.Tx, orgID uuid.UUID, h DemoHistory) error {
	for server, at := range h.BanLists {
		if _, err := tx.Exec(ctx, `INSERT INTO ban_lists (organization_id, server_id, adopted_at) VALUES ($1, $2, $3)`, orgID, server, at); err != nil {
			return fmt.Errorf("store: seed demo: ban list: %w", err)
		}
	}
	ids := make([]int64, len(h.Moderation))
	for i, e := range h.Moderation {
		detail := e.Detail
		if len(detail) == 0 {
			detail = json.RawMessage(`{}`)
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO audit_log (organization_id, at, actor_user_id, actor_app_id, on_behalf_provider, on_behalf_subject,
			                       server_id, action, target_provider, target_subject, reason, detail, request_id, outcome, finished_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15) RETURNING id`,
			orgID, e.At, e.ActorUserID, e.ActorAppID, e.OnBehalfProvider, e.OnBehalfSubject, e.ServerID, e.Action,
			e.TargetProvider, e.TargetSubject, e.Reason, detail, e.RequestID, e.Outcome, e.FinishedAt).Scan(&ids[i]); err != nil {
			return fmt.Errorf("store: seed demo: audit entry: %w", err)
		}
	}
	entry := func(i int) (*int64, time.Time, error) {
		if i < 0 || i >= len(ids) {
			return nil, time.Time{}, fmt.Errorf("store: seed demo: a ban names moderation entry %d of %d", i, len(ids))
		}
		return &ids[i], h.Moderation[i].At, nil
	}
	for _, b := range h.Bans {
		banID, bannedAt, err := entry(b.BanEntry)
		if err != nil {
			return err
		}
		var liftID *int64
		var liftedAt *time.Time
		if b.LiftEntry >= 0 {
			id, at, err := entry(b.LiftEntry)
			if err != nil {
				return err
			}
			liftID, liftedAt = id, &at
		}
		if _, err := tx.Exec(ctx, `INSERT INTO bans (organization_id, server_id, provider, subject, reason, banned_at, ban_audit_id, lifted_at, lift_audit_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`, orgID, b.ServerID, b.Provider, b.Subject, b.Reason, bannedAt, banID, liftedAt, liftID); err != nil {
			return fmt.Errorf("store: seed demo: ban: %w", err)
		}
	}
	return nil
}
