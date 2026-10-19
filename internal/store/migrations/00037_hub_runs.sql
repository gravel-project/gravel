-- +goose Up
-- gravel:cost scan stats_events
-- When the hub was away (ADR-0011 §5, amended 2026-10-19): one row per start of the hub, and the
-- last minute it was known to run. alive_at moves every minute while it runs, so the run before a
-- start says when the hub stopped, to the minute, whatever stopped it (an upgrade, a crash, a host
-- that went away). The hub_down gap runs from there to the start, when the poll saw players then.
CREATE TABLE hub_runs (
    id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    organization_id uuid NOT NULL REFERENCES organizations (id) DEFERRABLE INITIALLY IMMEDIATE,
    started_at      timestamptz NOT NULL,
    alive_at        timestamptz NOT NULL CHECK (alive_at >= started_at)
);
CREATE INDEX hub_runs_newest ON hub_runs (organization_id, id DESC);

-- 0.10.1's hub_down gaps ran from a server's last batch, and asked whether players were on at that
-- batch. A feed that posts only when it has events (War Dogs, Season 02) can be silent for hours
-- before the hub stops, so such a gap claimed hours the hub was up and nothing was lost (Hidden
-- Token Gaming's first, 1029 minutes, with nobody on). None of them can be told true from false
-- now, and a gap names no player: they go.
DELETE FROM stats_events WHERE kind = 'gap' AND extensions->>'reason' = 'hub_down';

-- +goose Down
DROP TABLE hub_runs;
