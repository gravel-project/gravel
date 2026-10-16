-- +goose Up
-- gravel:cost instant
-- Verifying a local account's recovery address (gravel#234, ADR-0026): the account page mails a
-- one-time link to the address, and using it sets local_credentials.email_verified_at. Only the
-- token's SHA-256 is stored, with the address it was mailed to, so a link proves that address and
-- no other: an address changed since makes it worthless. One open link an account; a newer one
-- replaces it, and changing or removing the address ends it. Like password links these last
-- minutes to a day and never travel in an export (store.NotArchived).
CREATE TABLE email_verifications (
    token_hash bytea PRIMARY KEY,
    provider   text NOT NULL DEFAULT 'local' CHECK (provider = 'local'),
    subject    text NOT NULL,
    email      text NOT NULL,
    issued_at  timestamptz NOT NULL,
    expires_at timestamptz NOT NULL CHECK (expires_at > issued_at),
    FOREIGN KEY (provider, subject) REFERENCES local_credentials (provider, subject) ON DELETE CASCADE DEFERRABLE INITIALLY IMMEDIATE
);
CREATE INDEX email_verifications_subject ON email_verifications (provider, subject);
CREATE INDEX email_verifications_expires_at ON email_verifications (expires_at);

-- +goose Down
DROP TABLE email_verifications;
