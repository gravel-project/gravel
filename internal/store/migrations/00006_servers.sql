-- +goose Up
-- Games and the servers a driver controls (ADR-0010), applied from a deployment's servers.yaml
-- by `gravel-hub servers apply`. A game row enables a Game spec gravel ships, with the manifest's
-- tightenings of its bands. A server row says where its credential is (a file), never what it
-- is. Taking a server out of the manifest marks it removed instead of deleting it, so what
-- refers to it later (the audit log, stats) keeps its target; putting it back revives it.
CREATE TABLE games (
    organization_id uuid NOT NULL REFERENCES organizations (id),
    id              text NOT NULL,
    bands           jsonb NOT NULL DEFAULT '[]',
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    removed_at      timestamptz,
    PRIMARY KEY (organization_id, id)
);

CREATE TABLE managed_servers (
    organization_id  uuid NOT NULL REFERENCES organizations (id),
    id               text NOT NULL,
    game_id          text NOT NULL,
    name             text NOT NULL,
    driver           text NOT NULL,
    location         text NOT NULL,
    endpoint         text NOT NULL,
    credential_file  text NOT NULL,
    poll_interval_ms bigint NOT NULL,
    trust            text NOT NULL,
    seeding          jsonb,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    removed_at       timestamptz,
    PRIMARY KEY (organization_id, id),
    FOREIGN KEY (organization_id, game_id) REFERENCES games (organization_id, id)
);

-- +goose Down
DROP TABLE managed_servers;
DROP TABLE games;
