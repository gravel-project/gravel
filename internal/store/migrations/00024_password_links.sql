-- +goose Up
-- One-time links that set or reset a local account's password (gravel#187, ADR-0026): issued by
-- staff with members:manage, or from the host (`gravel-hub users`, issued_by NULL). Only the
-- token's SHA-256 is stored. A link is used once, within its hour; issuing a new one for an
-- account ends the open ones. Rows go with the account's identity.
CREATE TABLE password_links (
    id         uuid PRIMARY KEY,
    token_hash bytea NOT NULL UNIQUE,
    provider   text NOT NULL DEFAULT 'local' CHECK (provider = 'local'),
    subject    text NOT NULL,
    purpose    text NOT NULL CHECK (purpose IN ('set', 'reset')),
    issued_by  uuid REFERENCES users (id),
    issued_at  timestamptz NOT NULL,
    expires_at timestamptz NOT NULL CHECK (expires_at > issued_at),
    used_at    timestamptz,
    FOREIGN KEY (provider, subject) REFERENCES identities (provider, subject) ON DELETE CASCADE
);
CREATE INDEX password_links_subject ON password_links (provider, subject);
CREATE INDEX password_links_expires_at ON password_links (expires_at);

-- +goose Down
DROP TABLE password_links;
