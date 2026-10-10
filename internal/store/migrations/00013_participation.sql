-- +goose Up
-- Participation events from first-party apps (ADR-0014; gravel#19): a seeding session, an event a
-- member attended, posted by an app holding participation:write. They feed participation boards
-- and profiles only (ADR-0011's trust mapping for first_party), never a combat board, which counts
-- match_stats and stats_monthly: separate tables make that structural. Rows are keyed by provider
-- identity, never a user id (principle 5), and resolved to a member when read. key is the app's
-- idempotency key: a repeat from the same app is a duplicate and is not stored again.
CREATE TABLE participation_events (
    id               bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    organization_id  uuid NOT NULL REFERENCES organizations (id),
    app_id           uuid NOT NULL REFERENCES apps (id),
    key              text NOT NULL,
    kind             text NOT NULL,
    provider         text NOT NULL,
    subject          text NOT NULL,
    -- Empty when the app named no game or server.
    game_id          text NOT NULL DEFAULT '',
    server_id        text NOT NULL DEFAULT '',
    -- The app's own reference for the session or event.
    session          text NOT NULL DEFAULT '',
    occurred_at      timestamptz NOT NULL,
    duration_seconds integer NOT NULL CHECK (duration_seconds BETWEEN 0 AND 86400),
    received_at      timestamptz NOT NULL,
    -- ADR-0011's provenance: these are first-party by construction.
    source           text NOT NULL CHECK (source = 'first_party'),
    schema_version   integer NOT NULL,
    UNIQUE (organization_id, app_id, key)
);
-- A board ranks one kind over a window; a profile sums an identity's rows; the rollup reads the
-- oldest rows.
CREATE INDEX participation_events_board ON participation_events (organization_id, kind, occurred_at);
CREATE INDEX participation_events_identity ON participation_events (provider, subject);
CREATE INDEX participation_events_occurred ON participation_events (organization_id, occurred_at);

-- Retention (ADR-0012 §6): the stats_rollup job moves participation_events past
-- stats.raw_retention_months into one row per kind, game, period and identity, and deletes the
-- raw rows in the same transaction. A period is the one stats_monthly uses: a calendar month in
-- the organization's timezone, cut at every season edge inside it. events and seconds are bigint
-- so a busy identity's month can never overflow and stall the rollup.
CREATE TABLE participation_monthly (
    organization_id uuid NOT NULL REFERENCES organizations (id),
    kind            text NOT NULL,
    game_id         text NOT NULL DEFAULT '',
    -- The period, dates in the organization's timezone at the rollup: from included, to excluded.
    period_from     date NOT NULL,
    period_to       date NOT NULL CHECK (period_to > period_from),
    provider        text NOT NULL,
    subject         text NOT NULL,
    events          bigint NOT NULL CHECK (events > 0),
    seconds         bigint NOT NULL CHECK (seconds >= 0),
    UNIQUE (organization_id, kind, game_id, period_from, provider, subject)
);
CREATE INDEX participation_monthly_identity ON participation_monthly (provider, subject);

-- +goose Down
DROP TABLE participation_monthly;
DROP TABLE participation_events;
