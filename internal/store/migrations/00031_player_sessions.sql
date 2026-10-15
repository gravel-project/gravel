-- +goose Up
-- gravel:cost instant
-- Player sessions from the poll (gravel#208, ADR-0035): who was on which server from when to when,
-- stitched from consecutive good polls. A session is one identity on one server; a poll that sees
-- the identity extends its open session (ended_at is the last poll that saw them), and a gap of more
-- than two poll intervals starts a new one, so a hub restart shorter than that carries a session
-- on. Keyed by the provider identity, never a member (README principle 5): who it is is resolved
-- at read time. Personal data: erased with the stats (ADR-0012 §7), rolled into
-- player_activity_monthly past stats.raw_retention_months (§6).
CREATE TABLE player_sessions (
    id              bigserial PRIMARY KEY,
    organization_id uuid NOT NULL,
    server_id       text NOT NULL,
    game_id         text NOT NULL,
    provider        text NOT NULL,
    subject         text NOT NULL,
    started_at      timestamptz NOT NULL,
    ended_at        timestamptz NOT NULL CHECK (ended_at >= started_at),
    FOREIGN KEY (organization_id, server_id) REFERENCES managed_servers (organization_id, id)
);
-- The open session a poll extends: one identity's newest on one server.
CREATE INDEX player_sessions_open ON player_sessions (organization_id, server_id, provider, subject, ended_at DESC);
-- Windows of activity, and the rollup's oldest row.
CREATE INDEX player_sessions_started ON player_sessions (organization_id, started_at);
CREATE INDEX player_sessions_identity ON player_sessions (provider, subject);

-- A period's sessions per identity and server once they pass the raw retention window, as
-- stats_monthly holds matches (ADR-0012 §6): how many, how long, on how many days.
CREATE TABLE player_activity_monthly (
    organization_id uuid NOT NULL,
    server_id       text NOT NULL,
    game_id         text NOT NULL,
    -- The period, dates in the organization's timezone at the rollup: from included, to excluded.
    period_from     date NOT NULL,
    period_to       date NOT NULL CHECK (period_to > period_from),
    provider        text NOT NULL,
    subject         text NOT NULL,
    sessions        integer NOT NULL CHECK (sessions > 0),
    seconds         bigint NOT NULL CHECK (seconds >= 0),
    active_days     integer NOT NULL CHECK (active_days > 0),
    PRIMARY KEY (organization_id, server_id, period_from, provider, subject),
    FOREIGN KEY (organization_id, server_id) REFERENCES managed_servers (organization_id, id)
);
CREATE INDEX player_activity_monthly_identity ON player_activity_monthly (provider, subject);

-- +goose Down
DROP TABLE player_activity_monthly;
DROP TABLE player_sessions;
