-- +goose Up
-- gravel:cost rewrite match_result_players, match_results
-- A game's own stats (gravel#206, ADR-0033): the numbers a player has on a map of a match result
-- beside kills and deaths are the ones the game's spec declares (stats.extensions), kept as one
-- JSON object of name to number. The five fixed columns ADR-0024 kept for CS2 move into it: every
-- row so far is MatchZy's. A key that is absent was not reported; 0 is a reported zero.
ALTER TABLE match_result_players ADD COLUMN extensions jsonb NOT NULL DEFAULT '{}'
    CHECK (jsonb_typeof(extensions) = 'object');
UPDATE match_result_players SET extensions = jsonb_build_object(
    'assists', assists, 'damage', damage, 'headshot_kills', headshot_kills, 'mvps', mvps, 'score', score);
ALTER TABLE match_result_players DROP COLUMN assists, DROP COLUMN damage, DROP COLUMN headshot_kills,
    DROP COLUMN mvps, DROP COLUMN score;

-- A result's trust is its server's when the series started (README principle 11: a promotion
-- counts from then on), as match_stats rows carry theirs, so a game's board counts the same
-- servers' results as its matches. The series so far take their server's trust now.
ALTER TABLE match_results ADD COLUMN trust text NOT NULL DEFAULT 'official' CHECK (trust IN ('official', 'community'));
UPDATE match_results r SET trust = ms.trust FROM managed_servers ms
 WHERE ms.organization_id = r.organization_id AND ms.id = r.server_id AND ms.trust IN ('official', 'community');
ALTER TABLE match_results ALTER COLUMN trust DROP DEFAULT;

-- The extensions of results past stats.raw_retention_months, rolled up per player per period
-- before the results' players are anonymised (ADR-0012 §6, ADR-0024 as amended by #216), so a
-- season's or all time's board keeps them. One row per extension: its total and the maps it
-- came from (an "avg" is total / maps) and its largest value on one map (a "max").
CREATE TABLE result_stats_monthly (
    organization_id uuid NOT NULL,
    server_id       text NOT NULL,
    game_id         text NOT NULL,
    -- The period, dates in the organization's timezone at the rollup: from included, to excluded.
    period_from     date NOT NULL,
    period_to       date NOT NULL CHECK (period_to > period_from),
    provider        text NOT NULL,
    subject         text NOT NULL,
    trust           text NOT NULL CHECK (trust IN ('official', 'community')),
    name            text NOT NULL,
    total           double precision NOT NULL,
    maps            integer NOT NULL CHECK (maps > 0),
    max             double precision NOT NULL,
    PRIMARY KEY (organization_id, server_id, period_from, provider, subject, trust, name),
    FOREIGN KEY (organization_id, server_id) REFERENCES managed_servers (organization_id, id)
);
CREATE INDEX result_stats_monthly_identity ON result_stats_monthly (provider, subject);

-- +goose Down
DROP TABLE result_stats_monthly;
ALTER TABLE match_results DROP COLUMN trust;
ALTER TABLE match_result_players ADD COLUMN assists integer NOT NULL DEFAULT 0, ADD COLUMN damage integer NOT NULL DEFAULT 0,
    ADD COLUMN headshot_kills integer NOT NULL DEFAULT 0, ADD COLUMN mvps integer NOT NULL DEFAULT 0, ADD COLUMN score integer NOT NULL DEFAULT 0;
UPDATE match_result_players SET
    assists = coalesce((extensions->>'assists')::numeric, 0)::integer,
    damage = coalesce((extensions->>'damage')::numeric, 0)::integer,
    headshot_kills = coalesce((extensions->>'headshot_kills')::numeric, 0)::integer,
    mvps = coalesce((extensions->>'mvps')::numeric, 0)::integer,
    score = coalesce((extensions->>'score')::numeric, 0)::integer;
ALTER TABLE match_result_players DROP COLUMN extensions;
