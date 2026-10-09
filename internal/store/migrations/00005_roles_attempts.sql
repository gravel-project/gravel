-- +goose Up
-- A third kind of authentication attempt: "roles", Discord's Linked Roles verification (ADR-0008
-- §4). It logs the member in like "login" and also publishes their linked accounts to Discord with
-- the grant in hand; nothing of the grant is stored, so only the intent's allowed values change.
ALTER TABLE auth_attempts DROP CONSTRAINT auth_attempts_intent;
ALTER TABLE auth_attempts ADD CONSTRAINT auth_attempts_intent CHECK (intent IN ('login', 'link', 'roles'));

-- +goose Down
DELETE FROM auth_attempts WHERE intent = 'roles';
ALTER TABLE auth_attempts DROP CONSTRAINT auth_attempts_intent;
ALTER TABLE auth_attempts ADD CONSTRAINT auth_attempts_intent CHECK (intent IN ('login', 'link'));
