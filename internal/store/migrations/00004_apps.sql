-- +goose Up
-- Registered first-party apps and their bearer tokens (ADR-0008, the client-credentials slice of
-- gravel#6): a service such as a host's bot authenticates with a client id and a secret, holds a
-- short-lived opaque token and acts within its scopes. The secret and the tokens are stored as
-- SHA-256 hashes, like session tokens, so a copy of the tables yields no credential.
CREATE TABLE apps (
    id              uuid PRIMARY KEY,
    organization_id uuid NOT NULL REFERENCES organizations (id),
    name            text NOT NULL,
    client_id       text NOT NULL UNIQUE,
    secret_hash     bytea NOT NULL,
    scopes          text[] NOT NULL DEFAULT '{}',
    created_at      timestamptz NOT NULL DEFAULT now(),
    revoked_at      timestamptz
);

CREATE TABLE app_tokens (
    id         uuid PRIMARY KEY,
    app_id     uuid NOT NULL REFERENCES apps (id),
    token_hash bytea NOT NULL UNIQUE,
    scopes     text[] NOT NULL DEFAULT '{}',
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL
);
CREATE INDEX app_tokens_app_id ON app_tokens (app_id);
CREATE INDEX app_tokens_expires_at ON app_tokens (expires_at);

-- +goose Down
DROP TABLE app_tokens;
DROP TABLE apps;
