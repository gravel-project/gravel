-- +goose Up
-- Users and their linked identities (gravel#13, gravel#5). A user is what (provider, subject)
-- pairs resolve to; stats are keyed by the pair, never by the user id (README principle 3).
-- Users are never deleted: erasure anonymizes (principle 8), so every reference below is a
-- plain foreign key.
CREATE TABLE users (
    id              uuid PRIMARY KEY,
    organization_id uuid NOT NULL REFERENCES organizations (id),
    display_name    text NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    last_login_at   timestamptz
);

-- One row per provider account. The pair is the key and is unique across all users; the
-- display attributes are mutable and refreshed at every login. verification_method and
-- verified_at record how control was proven, so trust can depend on it later.
CREATE TABLE identities (
    provider            text NOT NULL,
    subject             text NOT NULL,
    user_id             uuid NOT NULL REFERENCES users (id),
    display_name        text NOT NULL DEFAULT '',
    avatar_url          text NOT NULL DEFAULT '',
    verification_method text NOT NULL,
    verified_at         timestamptz NOT NULL,
    linked_at           timestamptz NOT NULL DEFAULT now(),
    last_login_at       timestamptz,
    PRIMARY KEY (provider, subject)
);
CREATE INDEX identities_user_id ON identities (user_id);

-- Append-only log of registrations, logins, links and unlinks (the design's immutable log for
-- account linking). Rows are never updated or deleted.
CREATE TABLE identity_events (
    id       bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id  uuid NOT NULL REFERENCES users (id),
    provider text NOT NULL,
    subject  text NOT NULL,
    event    text NOT NULL,
    at       timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX identity_events_user_id ON identity_events (user_id, at);

-- Server-side sessions. The cookie carries a random token; only its SHA-256 is stored, so a
-- copy of the table yields no usable cookie. csrf_token is the synchronizer token for forms.
CREATE TABLE sessions (
    id           uuid PRIMARY KEY,
    user_id      uuid NOT NULL REFERENCES users (id),
    token_hash   bytea NOT NULL UNIQUE,
    csrf_token   text NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL,
    last_seen_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX sessions_user_id ON sessions (user_id);
CREATE INDEX sessions_expires_at ON sessions (expires_at);

-- An authentication in progress: bound to the browser by a cookie (token_hash) and to the
-- provider's callback by state; consumed exactly once. user_id is set for a link attempt.
CREATE TABLE auth_attempts (
    id               uuid PRIMARY KEY,
    token_hash       bytea NOT NULL UNIQUE,
    provider         text NOT NULL,
    intent           text NOT NULL,
    user_id          uuid REFERENCES users (id),
    state            text NOT NULL,
    provider_session text NOT NULL,
    created_at       timestamptz NOT NULL DEFAULT now(),
    expires_at       timestamptz NOT NULL,
    CONSTRAINT auth_attempts_intent CHECK (intent IN ('login', 'link')),
    CONSTRAINT auth_attempts_link_has_user CHECK (intent <> 'link' OR user_id IS NOT NULL)
);
CREATE INDEX auth_attempts_expires_at ON auth_attempts (expires_at);

-- The owner claim now binds to a logged-in user (ADR-0002 left the column for this). A claim
-- made by 0.1.0 bound nobody and granted nothing, so it is reopened: the hub mints a fresh
-- token at its next start and the owner claims it while logged in.
UPDATE organizations SET claimed_at = NULL WHERE claimed_at IS NOT NULL AND owner_user_id IS NULL;
ALTER TABLE organizations
    ADD CONSTRAINT organizations_owner_user_id_fkey FOREIGN KEY (owner_user_id) REFERENCES users (id),
    ADD CONSTRAINT organizations_claimed_has_owner CHECK ((claimed_at IS NULL) = (owner_user_id IS NULL));

-- +goose Down
ALTER TABLE organizations
    DROP CONSTRAINT organizations_claimed_has_owner,
    DROP CONSTRAINT organizations_owner_user_id_fkey;
-- Back to the 0.1.0 shape: a claim without an owner (the users are gone with their table).
UPDATE organizations SET owner_user_id = NULL;
DROP TABLE auth_attempts;
DROP TABLE sessions;
DROP TABLE identity_events;
DROP TABLE identities;
DROP TABLE users;
