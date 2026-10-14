-- +goose Up
-- gravel:cost instant
-- Server leases (gravel#20, ADR-0023): an app (a PUG bot, Muster) or a moderator takes a server
-- for a match with its own password, settings, plugins and match config, and it is parked and given
-- back after. A lease is a request against a server or a pool; the hub picks the server, starts it,
-- and says when it is ready. Its join password is derived from the hub's lease key and the lease's
-- id, never stored. At most one live lease per server, and per holder's reference (one per
-- session: a retry gets the same lease).
CREATE TABLE server_leases (
    id               uuid PRIMARY KEY,
    organization_id  uuid NOT NULL,
    server_id        text NOT NULL,
    pool             text NOT NULL DEFAULT '',
    -- Who holds it: an app's client id, or a user's id for a moderator in person.
    holder_kind      text NOT NULL CHECK (holder_kind IN ('app', 'user')),
    holder           text NOT NULL,
    holder_ref       text NOT NULL,
    state            text NOT NULL CHECK (state IN ('starting', 'ready', 'ending', 'released', 'failed')),
    reason           text NOT NULL DEFAULT '',
    settings         jsonb NOT NULL DEFAULT '{}',
    plugins          jsonb NOT NULL DEFAULT '[]',
    match_config     bytea,
    expected_players integer NOT NULL DEFAULT 0,
    -- The plugin set's fate: '' while unknown or none asked, 'loaded', or 'vanilla' after a fallback.
    plugin_state     text NOT NULL DEFAULT '' CHECK (plugin_state IN ('', 'loaded', 'vanilla')),
    plugin_reason    text NOT NULL DEFAULT '',
    match_loaded     boolean NOT NULL DEFAULT false,
    audit_id         bigint,
    created_at       timestamptz NOT NULL,
    ready_at         timestamptz,
    ended_at         timestamptz,
    released_at      timestamptz,
    max_until        timestamptz NOT NULL,
    FOREIGN KEY (organization_id, server_id) REFERENCES managed_servers (organization_id, id)
);
CREATE UNIQUE INDEX server_leases_live_server ON server_leases (organization_id, server_id)
    WHERE state IN ('starting', 'ready', 'ending');
CREATE UNIQUE INDEX server_leases_live_ref ON server_leases (organization_id, holder_kind, holder, holder_ref)
    WHERE state IN ('starting', 'ready', 'ending');
CREATE INDEX server_leases_server ON server_leases (organization_id, server_id, created_at DESC);

-- +goose Down
DROP TABLE server_leases;
