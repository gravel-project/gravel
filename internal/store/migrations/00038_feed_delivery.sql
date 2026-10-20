-- +goose Up
-- gravel:cost instant
-- What a pushing server last said of its own delivery to the hub (ADR-0011 §5, amended
-- 2026-10-20): War Dogs queues what it could not post and retries, and counts what it gave up on
-- (GET /v1/feed's droppedTotal). The dropped gap compares each poll's count with this row, so a
-- drop while the hub was away is still seen at its next start. The counts are the server's, since
-- it started: a count below the row's is a restart.
CREATE TABLE feed_delivery (
    organization_id uuid NOT NULL REFERENCES organizations (id) DEFERRABLE INITIALLY IMMEDIATE,
    server_id       text NOT NULL,
    sent_total      bigint NOT NULL CHECK (sent_total >= 0),
    dropped_total   bigint NOT NULL CHECK (dropped_total >= 0),
    observed_at     timestamptz NOT NULL,
    PRIMARY KEY (organization_id, server_id)
);

-- +goose Down
DROP TABLE feed_delivery;
