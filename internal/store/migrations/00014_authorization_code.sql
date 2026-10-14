-- +goose Up
-- gravel:cost scan app_tokens
-- Members sign in to first-party apps with the authorization-code flow (ADR-0015, gravel#6).
--
-- An app registration gains its redirect URIs: exact strings, compared as strings, never as
-- prefixes. A redirect is sent only to one of them.
ALTER TABLE apps ADD COLUMN redirect_uris text[] NOT NULL DEFAULT '{}';

-- An authorization code: hashed like every credential, single use, 60 seconds, bound to the app,
-- the redirect URI, the PKCE challenge (S256) and the member who allowed it. A used code is kept
-- (used_at) until the tokens issued from it have expired, so a replay is recognised and revokes
-- them (RFC 6749 §4.1.2).
CREATE TABLE authorization_codes (
    id             uuid PRIMARY KEY,
    code_hash      bytea NOT NULL UNIQUE,
    app_id         uuid NOT NULL REFERENCES apps (id),
    user_id        uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    redirect_uri   text NOT NULL,
    code_challenge text NOT NULL,
    scopes         text[] NOT NULL DEFAULT '{}',
    created_at     timestamptz NOT NULL,
    expires_at     timestamptz NOT NULL,
    used_at        timestamptz
);
CREATE INDEX authorization_codes_expires_at ON authorization_codes (expires_at);
CREATE INDEX authorization_codes_user_app ON authorization_codes (user_id, app_id);

-- A remembered grant: the member allowed the app these scopes, at this time. The consent page is
-- skipped while a grant covers what the app asks for. It is data about the member: the account
-- page lists it and the member can revoke it.
CREATE TABLE app_grants (
    user_id    uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    app_id     uuid NOT NULL REFERENCES apps (id),
    scopes     text[] NOT NULL DEFAULT '{}',
    granted_at timestamptz NOT NULL,
    PRIMARY KEY (user_id, app_id)
);
CREATE INDEX app_grants_app ON app_grants (app_id);

-- A member's token is an app token with the member's user id (NULL: the app acting for itself)
-- and the code it was issued from, so a replay of that code can revoke it.
ALTER TABLE app_tokens ADD COLUMN user_id uuid REFERENCES users (id) ON DELETE CASCADE;
ALTER TABLE app_tokens ADD COLUMN code_id uuid REFERENCES authorization_codes (id) ON DELETE SET NULL;
CREATE INDEX app_tokens_user_app ON app_tokens (user_id, app_id) WHERE user_id IS NOT NULL;
CREATE INDEX app_tokens_code ON app_tokens (code_id) WHERE code_id IS NOT NULL;

-- A member acting through an app's token is recorded with both: the member who acted and the app
-- they acted through (ADR-0015 §4). Before, exactly one of the two was set.
ALTER TABLE audit_log DROP CONSTRAINT audit_log_one_actor;
ALTER TABLE audit_log ADD CONSTRAINT audit_log_actor CHECK (num_nonnulls(actor_user_id, actor_app_id) >= 1);

-- +goose Down
-- Rows naming both a member and an app stay; the old constraint returns unvalidated over them.
ALTER TABLE audit_log DROP CONSTRAINT audit_log_actor;
ALTER TABLE audit_log ADD CONSTRAINT audit_log_one_actor CHECK (num_nonnulls(actor_user_id, actor_app_id) = 1) NOT VALID;
DELETE FROM app_tokens WHERE user_id IS NOT NULL;
DROP INDEX app_tokens_code;
DROP INDEX app_tokens_user_app;
ALTER TABLE app_tokens DROP COLUMN code_id;
ALTER TABLE app_tokens DROP COLUMN user_id;
DROP TABLE app_grants;
DROP TABLE authorization_codes;
ALTER TABLE apps DROP COLUMN redirect_uris;
