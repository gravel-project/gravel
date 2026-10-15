package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Player sessions (gravel#208, ADR-0035): who was on which server from when to when, stitched from
// consecutive polls, keyed by the provider identity.

// RecordSessions records that the identities were on a server at a poll: each one's open session
// on the server, the newest that a poll saw at since or later, is extended to at; an identity with
// none starts one at at. since is at less the gap that ends a session, so the stitching reads the
// store and carries on across a hub restart shorter than the gap. It reports how many sessions
// started.
func (s *Store) RecordSessions(ctx context.Context, orgID uuid.UUID, serverID, gameID string, at, since time.Time, keys []PlayerKey) (int, error) {
	if len(keys) == 0 {
		return 0, nil
	}
	providers, subjects := make([]string, len(keys)), make([]string, len(keys))
	for i, k := range keys {
		providers[i], subjects[i] = k.Provider, k.Subject
	}
	var started int
	// A data-modifying WITH sees the table as it was, so the insert's NOT EXISTS reads what the
	// update extended from its RETURNING, not the table.
	if err := s.pool.QueryRow(ctx, `
		WITH present AS (
		  SELECT DISTINCT provider, subject FROM unnest($5::text[], $6::text[]) AS p(provider, subject)
		), extended AS (
		  UPDATE player_sessions ps SET ended_at = $4
		    FROM present p
		   WHERE ps.organization_id = $1 AND ps.server_id = $2 AND ps.provider = p.provider AND ps.subject = p.subject
		     AND ps.ended_at >= $7 AND ps.ended_at <= $4
		  RETURNING ps.provider, ps.subject
		), started AS (
		  INSERT INTO player_sessions (organization_id, server_id, game_id, provider, subject, started_at, ended_at)
		  SELECT $1, $2, $3, p.provider, p.subject, $4, $4 FROM present p
		   WHERE NOT EXISTS (SELECT 1 FROM extended e WHERE e.provider = p.provider AND e.subject = p.subject)
		  RETURNING 1
		)
		SELECT count(*) FROM started`, orgID, serverID, gameID, at, providers, subjects, since).Scan(&started); err != nil {
		return 0, fmt.Errorf("store: record sessions: %w", err)
	}
	return started, nil
}

// OldestRawSession is the start of the organization's oldest session, or ErrNotFound when none.
func (s *Store) OldestRawSession(ctx context.Context, orgID uuid.UUID) (time.Time, error) {
	var at *time.Time
	if err := s.pool.QueryRow(ctx, `SELECT min(started_at) FROM player_sessions WHERE organization_id = $1`, orgID).Scan(&at); err != nil {
		return time.Time{}, fmt.Errorf("store: oldest raw session: %w", err)
	}
	if at == nil {
		return time.Time{}, ErrNotFound
	}
	return *at, nil
}

// RollUpActivity moves every session that started in one of the periods (read in timezone) into
// player_activity_monthly, adding to a period's row when one exists, and deletes the raw rows, in
// one statement. It reports how many sessions moved. Periods must not overlap.
func (s *Store) RollUpActivity(ctx context.Context, orgID uuid.UUID, timezone string, periods []Period) (int, error) {
	if len(periods) == 0 {
		return 0, nil
	}
	froms, tos := make([]time.Time, len(periods)), make([]time.Time, len(periods))
	for i, p := range periods {
		froms[i], tos[i] = dateOnly(p.From), dateOnly(p.To)
	}
	var moved int
	if err := s.pool.QueryRow(ctx, `
		WITH moved AS (
		  DELETE FROM player_sessions ps
		   USING unnest($2::date[], $3::date[]) AS p(f, t)
		   WHERE ps.organization_id = $1
		     AND (ps.started_at AT TIME ZONE $4)::date >= p.f AND (ps.started_at AT TIME ZONE $4)::date < p.t
		  RETURNING ps.server_id, ps.game_id, p.f AS period_from, p.t AS period_to, ps.provider, ps.subject,
		            extract(epoch FROM ps.ended_at - ps.started_at)::bigint AS seconds, (ps.started_at AT TIME ZONE $4)::date AS day
		), added AS (
		  INSERT INTO player_activity_monthly (organization_id, server_id, game_id, period_from, period_to, provider, subject, sessions, seconds, active_days)
		  SELECT $1, m.server_id, min(m.game_id), m.period_from, m.period_to, m.provider, m.subject, count(*), sum(m.seconds), count(DISTINCT m.day)
		    FROM moved m
		   GROUP BY m.server_id, m.period_from, m.period_to, m.provider, m.subject
		  ON CONFLICT (organization_id, server_id, period_from, provider, subject) DO UPDATE SET
		    sessions = player_activity_monthly.sessions + excluded.sessions, seconds = player_activity_monthly.seconds + excluded.seconds,
		    active_days = player_activity_monthly.active_days + excluded.active_days
		)
		SELECT count(*) FROM moved`, orgID, froms, tos, timezone).Scan(&moved); err != nil {
		return 0, fmt.Errorf("store: roll up activity: %w", err)
	}
	return moved, nil
}

// ActivityQuery is a window of player activity: From included, To excluded, read in Timezone.
// Regulars are identities active on at least RegularDays days in [RegularFrom, From).
type ActivityQuery struct {
	OrganizationID uuid.UUID
	Timezone       string
	From, To       time.Time
	RegularFrom    time.Time
	RegularDays    int
	Limit          int // regulars gone quiet listed, newest last seen first
}

// Activity is what a window held.
type Activity struct {
	// Hours are the window's hours of the week in Timezone with anyone on: the players on in that
	// hour summed over the window's weeks (an identity counts once an hour), and the most in one.
	Hours []ActivityHour
	// Sessions started in the window, the identities who started them, and the median length.
	Sessions      int
	Players       int
	MedianSeconds float64
	// Returning are the window's players active on two days or more; ActiveDays sums each
	// player's days, for the mean.
	Returning  int
	ActiveDays int
	// New are the window's players never seen before it (in a session or a rolled-up month); Lost
	// those active in the window of the same length before it and not in this one.
	New, Lost int
	Weeks     []ActivityWeek
	Quiet     []QuietRegular
}

// ActivityHour is one hour of the week: Day 1 (Monday) to 7, Hour 0 to 23.
type ActivityHour struct {
	Day, Hour   int
	PlayerHours int // players on in this hour of the week, summed over the window
	Peak        int // the most in one such hour
}

// ActivityWeek is one week (from its Monday, in Timezone) of the window: its players and the
// ones new that week.
type ActivityWeek struct {
	Start        time.Time
	Players, New int
}

// QuietRegular is a regular before the window with no session in it. An erased identity is never
// listed: it can't be reached, and listing it would say when someone asked to be forgotten.
type QuietRegular struct {
	Key        PlayerKey
	DaysBefore int // active days in the regulars' window
	LastSeen   time.Time
	LastServer string
	Seconds    int64 // seconds on in the regulars' window
}

// PlayerActivity reads a window's activity from the raw sessions; the window and the regulars'
// look-back must lie within the raw retention window (a rolled-up month only says who is not new).
func (s *Store) PlayerActivity(ctx context.Context, q ActivityQuery) (Activity, error) {
	var out Activity
	tz, org, from, to := q.Timezone, q.OrganizationID, q.From, q.To
	rows, err := s.pool.Query(ctx, `
		WITH hours AS (
		  SELECT DISTINCT ps.provider, ps.subject, h
		    FROM player_sessions ps,
		         generate_series(date_trunc('hour', greatest(ps.started_at, $2::timestamptz) AT TIME ZONE $4),
		                         least(ps.ended_at, $3::timestamptz - interval '1 microsecond') AT TIME ZONE $4, interval '1 hour') AS h
		   WHERE ps.organization_id = $1 AND ps.ended_at >= $2 AND ps.started_at < $3
		), per_hour AS (
		  SELECT h, count(*) AS players FROM hours GROUP BY h
		)
		SELECT extract(isodow FROM h)::int, extract(hour FROM h)::int, sum(players)::int, max(players)::int
		  FROM per_hour GROUP BY 1, 2 ORDER BY 1, 2`, org, from, to, tz)
	if err != nil {
		return out, fmt.Errorf("store: player activity: hours: %w", err)
	}
	for rows.Next() {
		var h ActivityHour
		if err := rows.Scan(&h.Day, &h.Hour, &h.PlayerHours, &h.Peak); err != nil {
			rows.Close()
			return out, fmt.Errorf("store: player activity: hours: %w", err)
		}
		out.Hours = append(out.Hours, h)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("store: player activity: hours: %w", err)
	}

	var median *float64
	if err := s.pool.QueryRow(ctx, `
		WITH w AS (
		  SELECT provider, subject, ended_at - started_at AS length, (started_at AT TIME ZONE $4)::date AS day
		    FROM player_sessions WHERE organization_id = $1 AND started_at >= $2 AND started_at < $3
		), per_player AS (
		  SELECT provider, subject, count(DISTINCT day) AS days FROM w GROUP BY provider, subject
		)
		SELECT (SELECT count(*) FROM w),
		       (SELECT count(*) FROM per_player),
		       (SELECT percentile_cont(0.5) WITHIN GROUP (ORDER BY extract(epoch FROM length)) FROM w),
		       (SELECT count(*) FROM per_player WHERE days >= 2),
		       (SELECT coalesce(sum(days), 0) FROM per_player)`, org, from, to, tz).
		Scan(&out.Sessions, &out.Players, &median, &out.Returning, &out.ActiveDays); err != nil {
		return out, fmt.Errorf("store: player activity: sessions: %w", err)
	}
	if median != nil {
		out.MedianSeconds = *median
	}

	// New: no session before the window and no rolled-up month before it. Lost: active in the
	// window of the same length before this one, and not in this one.
	prev := from.Add(-to.Sub(from))
	if err := s.pool.QueryRow(ctx, `
		WITH now_on AS (
		  SELECT DISTINCT provider, subject FROM player_sessions WHERE organization_id = $1 AND started_at < $3 AND ended_at >= $2
		), before_on AS (
		  SELECT DISTINCT provider, subject FROM player_sessions WHERE organization_id = $1 AND started_at < $2 AND ended_at >= $4
		)
		SELECT (SELECT count(*) FROM now_on n
		         WHERE NOT EXISTS (SELECT 1 FROM player_sessions ps WHERE ps.organization_id = $1 AND ps.provider = n.provider AND ps.subject = n.subject AND ps.ended_at < $2)
		           AND NOT EXISTS (SELECT 1 FROM player_activity_monthly m WHERE m.organization_id = $1 AND m.provider = n.provider AND m.subject = n.subject)),
		       (SELECT count(*) FROM before_on b WHERE NOT EXISTS (SELECT 1 FROM now_on n WHERE n.provider = b.provider AND n.subject = b.subject))`,
		org, from, to, prev).Scan(&out.New, &out.Lost); err != nil {
		return out, fmt.Errorf("store: player activity: growth: %w", err)
	}

	rows, err = s.pool.Query(ctx, `
		WITH firsts AS (
		  SELECT provider, subject, min(started_at) AS first FROM player_sessions WHERE organization_id = $1 GROUP BY provider, subject
		), w AS (
		  SELECT DISTINCT date_trunc('week', ps.started_at AT TIME ZONE $4) AS week, ps.provider, ps.subject,
		         (f.first >= $2 AND date_trunc('week', f.first AT TIME ZONE $4) = date_trunc('week', ps.started_at AT TIME ZONE $4)
		          AND NOT EXISTS (SELECT 1 FROM player_activity_monthly m WHERE m.organization_id = $1 AND m.provider = ps.provider AND m.subject = ps.subject)) AS is_new
		    FROM player_sessions ps JOIN firsts f ON f.provider = ps.provider AND f.subject = ps.subject
		   WHERE ps.organization_id = $1 AND ps.started_at >= $2 AND ps.started_at < $3
		)
		SELECT week, count(DISTINCT (provider, subject))::int, count(DISTINCT (provider, subject)) FILTER (WHERE is_new)::int
		  FROM w GROUP BY week ORDER BY week`, org, from, to, tz)
	if err != nil {
		return out, fmt.Errorf("store: player activity: weeks: %w", err)
	}
	for rows.Next() {
		var w ActivityWeek
		if err := rows.Scan(&w.Start, &w.Players, &w.New); err != nil {
			rows.Close()
			return out, fmt.Errorf("store: player activity: weeks: %w", err)
		}
		out.Weeks = append(out.Weeks, w)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("store: player activity: weeks: %w", err)
	}

	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}
	rows, err = s.pool.Query(ctx, `
		WITH before AS (
		  SELECT provider, subject, count(DISTINCT (started_at AT TIME ZONE $5)::date)::int AS days,
		         sum(extract(epoch FROM ended_at - started_at))::bigint AS seconds, max(ended_at) AS last_seen
		    FROM player_sessions WHERE organization_id = $1 AND started_at >= $2 AND started_at < $3
		   GROUP BY provider, subject
		)
		SELECT b.provider, b.subject, b.days, b.last_seen, b.seconds,
		       (SELECT ps.server_id FROM player_sessions ps WHERE ps.organization_id = $1 AND ps.provider = b.provider AND ps.subject = b.subject
		         ORDER BY ps.ended_at DESC LIMIT 1)
		  FROM before b
		 WHERE b.days >= $4 AND b.provider <> '`+ErasedProvider+`'
		   AND NOT EXISTS (SELECT 1 FROM player_sessions ps WHERE ps.organization_id = $1 AND ps.provider = b.provider AND ps.subject = b.subject
		                     AND ps.ended_at >= $3 AND ps.started_at < $6)
		 ORDER BY b.last_seen DESC, b.subject LIMIT $7`, org, q.RegularFrom, from, max(q.RegularDays, 1), tz, to, limit)
	if err != nil {
		return out, fmt.Errorf("store: player activity: quiet: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var r QuietRegular
		if err := rows.Scan(&r.Key.Provider, &r.Key.Subject, &r.DaysBefore, &r.LastSeen, &r.Seconds, &r.LastServer); err != nil {
			return out, fmt.Errorf("store: player activity: quiet: %w", err)
		}
		out.Quiet = append(out.Quiet, r)
	}
	return out, rows.Err()
}
