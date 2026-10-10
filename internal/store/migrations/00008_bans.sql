-- +goose Up
-- The hub's ban list per server (ADR-0010 §5–6; John, 2026-10-10: the hub owns War Dogs'
-- DefaultBannedPlayerIds). A ban is keyed by the player's provider identity (principle 5). The
-- hub writes its active bans into the server's configuration; a deployment's own document leaves
-- the list out. The first time the hub touches a server's list it adopts the server's current
-- one (ban_lists records when), importing each entry as a ban with no audit entry, so nothing a
-- host banned before is dropped. Lifting a ban marks it lifted (principle 10: the record stays);
-- an identity has at most one active ban per server.
CREATE TABLE ban_lists (
    organization_id uuid NOT NULL,
    server_id       text NOT NULL,
    adopted_at      timestamptz NOT NULL,
    PRIMARY KEY (organization_id, server_id),
    FOREIGN KEY (organization_id, server_id) REFERENCES managed_servers (organization_id, id)
);

CREATE TABLE bans (
    id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    organization_id uuid NOT NULL,
    server_id       text NOT NULL,
    provider        text NOT NULL,
    subject         text NOT NULL,
    reason          text NOT NULL DEFAULT '',
    banned_at       timestamptz NOT NULL,
    -- The audit entry of the BanPlayer call; NULL for a ban imported when the list was adopted.
    ban_audit_id    bigint REFERENCES audit_log (id),
    lifted_at       timestamptz,
    lift_audit_id   bigint REFERENCES audit_log (id),
    FOREIGN KEY (organization_id, server_id) REFERENCES managed_servers (organization_id, id),
    CONSTRAINT bans_lift CHECK (lift_audit_id IS NULL OR lifted_at IS NOT NULL)
);

CREATE UNIQUE INDEX bans_one_active ON bans (organization_id, server_id, provider, subject) WHERE lifted_at IS NULL;

-- +goose Down
DROP TABLE bans;
DROP TABLE ban_lists;
