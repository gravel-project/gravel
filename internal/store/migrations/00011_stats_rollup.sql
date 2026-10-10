-- +goose Up
-- The stats store's retention (ADR-0012 §6): match_stats rows older than the Organization
-- settings' stats.raw_retention_months roll up into stats_monthly, one row per identity, server,
-- trust and period, and the raw rows are deleted. A period is a calendar month in the
-- organization's timezone, cut wherever a season starts or ends inside it, so a board over a
-- season or all time counts the same before and after the rollup. Rows stay keyed by provider
-- identity (principle 3); an erasure re-keys them (ADR-0012 §7). matches stays (no personal
-- data) and keeps its player count and kills once its rows are gone.
CREATE TABLE stats_monthly (
    organization_id uuid NOT NULL,
    server_id       text NOT NULL,
    game_id         text NOT NULL,
    -- The period, dates in the organization's timezone at the rollup: from included, to excluded.
    period_from     date NOT NULL,
    period_to       date NOT NULL CHECK (period_to > period_from),
    provider        text NOT NULL,
    subject         text NOT NULL,
    trust           text NOT NULL CHECK (trust IN ('official', 'community')),
    kills           integer NOT NULL CHECK (kills >= 0),
    deaths          integer NOT NULL CHECK (deaths >= 0),
    seconds_on      integer NOT NULL CHECK (seconds_on >= 0),
    matches         integer NOT NULL CHECK (matches > 0),
    PRIMARY KEY (organization_id, server_id, period_from, provider, subject, trust),
    FOREIGN KEY (organization_id, server_id) REFERENCES managed_servers (organization_id, id)
);
CREATE INDEX stats_monthly_identity ON stats_monthly (provider, subject);

-- Set when a match's rows roll up; NULL while match_stats holds them.
ALTER TABLE matches ADD COLUMN players integer, ADD COLUMN kills integer;

-- +goose Down
ALTER TABLE matches DROP COLUMN kills, DROP COLUMN players;
DROP TABLE stats_monthly;
