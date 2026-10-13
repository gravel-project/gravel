-- +goose Up
-- Local accounts (gravel#186, ADR-0026): a username and a password are one more provider. The
-- identity is ('local', <canonical username>) in identities, with the same uniqueness, events
-- and unlink rules as any other; its credential is a row here, removed with the identity.
CREATE TABLE local_credentials (
    provider            text NOT NULL DEFAULT 'local' CHECK (provider = 'local'),
    subject             text NOT NULL,
    -- A PHC string: $argon2id$v=19$m=<KiB>,t=<passes>,p=<lanes>$<salt>$<hash>. Never a password.
    password_hash       text NOT NULL CHECK (password_hash LIKE '$argon2id$%'),
    -- Optional, and only ever for recovery (#187): the hub never matches accounts by email.
    email               text,
    email_verified_at   timestamptz,
    password_updated_at timestamptz NOT NULL,
    -- Failed logins since the last success or lockout, and the lockouts since the last success:
    -- each lockout lasts twice the one before, up to a day.
    failed_attempts     integer NOT NULL DEFAULT 0 CHECK (failed_attempts >= 0),
    lockouts            integer NOT NULL DEFAULT 0 CHECK (lockouts >= 0),
    locked_until        timestamptz,
    PRIMARY KEY (provider, subject),
    FOREIGN KEY (provider, subject) REFERENCES identities (provider, subject) ON DELETE CASCADE
);

-- +goose Down
DROP TABLE local_credentials;
