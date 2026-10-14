-- +goose Up
-- gravel:cost instant
-- Notifiers (gravel#196, ADR-0028 §5): each notifier the Organization settings name follows the
-- events log with a cursor of its own, so one that fails never holds up another. A notifier's row
-- is made at the log's head the first time the dispatcher sees it (a new notifier sends nothing
-- from before it existed) and is kept when the notifier leaves the settings, so one removed and
-- added again under the same name resumes where it stopped. The cursor moves only when a batch
-- was delivered; a failure records itself and when to try again.
CREATE TABLE notifier_cursors (
    organization_id uuid NOT NULL REFERENCES organizations (id),
    name            text NOT NULL,
    -- The last event delivered (or passed over by the notifier's filter).
    after_id        bigint NOT NULL,
    last_sent_at    timestamptz,
    -- The last failure's message, without secrets; '' after a delivery.
    last_error      text NOT NULL DEFAULT '',
    -- When the current run of failures began; NULL while the notifier delivers.
    failing_since   timestamptz,
    failures        integer NOT NULL DEFAULT 0,
    -- No attempt before this (the backoff after a failure).
    next_attempt_at timestamptz,
    created_at      timestamptz NOT NULL,
    PRIMARY KEY (organization_id, name)
);

-- +goose Down
DROP TABLE notifier_cursors;
