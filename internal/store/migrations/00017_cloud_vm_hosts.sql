-- +goose Up
-- gravel:cost instant
-- Cloud-VM servers (ADR-0003, gravel#36). A location is a cloud account and region a deployment
-- declares in servers.yaml (its token and SSH key are files the hub reads, never stored here). A
-- cloud-VM server's machine and settings are the manifest's vm and settings sections. A
-- server_hosts row is the hub's record of the VM it runs one server on: the cloud is the truth
-- (everything the hub makes carries the server's tags and a predictable name, so a restart finds
-- it), and the row is the intent (desired) and where the hub got to (state, step, the operation in
-- flight). server_host_events is the append-only history of a host's transitions; an owner's or
-- moderator's request behind one links to its audit_log row.
CREATE TABLE locations (
    organization_id uuid NOT NULL REFERENCES organizations (id),
    id              text NOT NULL,
    provider        text NOT NULL,
    region          text NOT NULL,
    project         text NOT NULL DEFAULT '',
    token_file      text NOT NULL,
    ssh_key_file    text NOT NULL,
    image           text NOT NULL DEFAULT '',
    extra_ssh_keys  jsonb NOT NULL DEFAULT '[]',
    ssh_from        jsonb NOT NULL DEFAULT '[]',
    created_at      timestamptz NOT NULL,
    updated_at      timestamptz NOT NULL,
    removed_at      timestamptz,
    PRIMARY KEY (organization_id, id)
);

ALTER TABLE managed_servers ADD COLUMN vm jsonb;
ALTER TABLE managed_servers ADD COLUMN settings jsonb;

CREATE TABLE server_hosts (
    organization_id     uuid NOT NULL,
    server_id           text NOT NULL,
    location_id         text NOT NULL,
    desired             text NOT NULL CHECK (desired IN ('running', 'parked', 'absent')),
    state               text NOT NULL CHECK (state IN ('absent', 'creating', 'booting', 'starting', 'running',
                                                       'parking', 'parked', 'resizing', 'destroying', 'error')),
    -- The sub-step within a transition ("volume", "instance", "env"); '' when settled.
    step                text NOT NULL DEFAULT '',
    size                text NOT NULL DEFAULT '',
    -- A resize asked for and not yet done; '' for none.
    target_size         text NOT NULL DEFAULT '',
    -- What a destroy also deletes, as asked.
    destroy_volume      boolean NOT NULL DEFAULT false,
    release_address     boolean NOT NULL DEFAULT false,
    -- The root disk the instance was made with (a resize never changes it).
    disk_gib            integer NOT NULL DEFAULT 0,
    instance_id         text NOT NULL DEFAULT '',
    -- The instance's own IPv4: where the hub reaches it over SSH, and where its log push comes from.
    instance_address    text NOT NULL DEFAULT '',
    volume_id           text NOT NULL DEFAULT '',
    -- The IPv4 players join: the reserved address, or the instance's.
    address             text NOT NULL DEFAULT '',
    address_reserved    boolean NOT NULL DEFAULT false,
    firewall_id         text NOT NULL DEFAULT '',
    -- The addresses the firewall admits RCON and SSH from, as last pinned.
    egress              jsonb NOT NULL DEFAULT '[]',
    -- The cloud operation in flight ({"id","kind","resource"}); NULL for none.
    operation           jsonb,
    -- The instance's SSH host key, trusted on first use; cleared when a new instance is made.
    host_key            text NOT NULL DEFAULT '',
    -- The environment file last pushed over SSH, as a hash: the file itself holds secrets.
    env_sha256          bytea,
    attempts            integer NOT NULL DEFAULT 0,
    next_attempt_at     timestamptz,
    -- A word for the API while a step fails ("cloud_failed", "boot_timeout"); the cause, which can
    -- name an address, is last_error and goes to the log only.
    error_code          text NOT NULL DEFAULT '',
    last_error          text NOT NULL DEFAULT '',
    -- The audit row of the request the next transition carries out; NULL for none.
    request_audit_id    bigint REFERENCES audit_log (id),
    state_since         timestamptz NOT NULL,
    instance_created_at timestamptz,
    -- When the server was last seen empty while running; NULL while players are on.
    empty_since         timestamptz,
    boot_seconds        integer,
    ready_seconds       integer,
    created_at          timestamptz NOT NULL,
    updated_at          timestamptz NOT NULL,
    PRIMARY KEY (organization_id, server_id),
    FOREIGN KEY (organization_id, server_id) REFERENCES managed_servers (organization_id, id)
);

CREATE TABLE server_host_events (
    id               bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    organization_id  uuid NOT NULL,
    server_id        text NOT NULL,
    at               timestamptz NOT NULL,
    from_state       text NOT NULL,
    to_state         text NOT NULL,
    step             text NOT NULL DEFAULT '',
    -- A word, a size or a duration; never a secret or an address.
    detail           text NOT NULL DEFAULT '',
    size             text NOT NULL DEFAULT '',
    price_hourly_usd numeric(10, 5),
    audit_id         bigint REFERENCES audit_log (id),
    FOREIGN KEY (organization_id, server_id) REFERENCES managed_servers (organization_id, id)
);
CREATE INDEX server_host_events_by_server ON server_host_events (organization_id, server_id, id DESC);

-- +goose StatementBegin
CREATE FUNCTION server_host_events_append_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'server_host_events is append-only: % refused', TG_OP USING ERRCODE = 'insufficient_privilege';
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER server_host_events_append_only BEFORE UPDATE OR DELETE ON server_host_events
    FOR EACH ROW EXECUTE FUNCTION server_host_events_append_only();
CREATE TRIGGER server_host_events_no_truncate BEFORE TRUNCATE ON server_host_events
    FOR EACH STATEMENT EXECUTE FUNCTION server_host_events_append_only();

-- +goose Down
DROP TABLE server_host_events;
DROP FUNCTION server_host_events_append_only();
DROP TABLE server_hosts;
ALTER TABLE managed_servers DROP COLUMN settings;
ALTER TABLE managed_servers DROP COLUMN vm;
DROP TABLE locations;
