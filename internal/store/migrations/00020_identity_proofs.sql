-- +goose Up
-- Proof of control (gravel#5): a member proves they control a provider account by putting a
-- one-time code where only that account can, such as typing it in a game server's chat, where
-- the server's log carries the account's id. A proof is bound to the member who asked, the channel
-- and its scope (the server), and optionally the identity it must come from (re-proving a link).
-- Only the code's SHA-256 is kept. It is used once: used_at stays, so a replay finds a used row
-- rather than nothing. result says what became of it. The rows go 30 days after they expire.
CREATE TABLE identity_proofs (
    id              uuid PRIMARY KEY,
    user_id         uuid NOT NULL REFERENCES users (id),
    channel         text NOT NULL,
    scope           text NOT NULL DEFAULT '',
    provider        text NOT NULL,
    expect_subject  text NOT NULL DEFAULT '',
    code_hash       bytea NOT NULL UNIQUE,
    created_at      timestamptz NOT NULL,
    expires_at      timestamptz NOT NULL,
    used_at         timestamptz,
    result          text NOT NULL DEFAULT '' CHECK (result IN ('', 'proven', 'taken', 'replaced')),
    proven_subject  text NOT NULL DEFAULT '',
    CHECK ((used_at IS NULL) = (result = ''))
);
CREATE INDEX identity_proofs_user ON identity_proofs (user_id, created_at);
CREATE INDEX identity_proofs_expires ON identity_proofs (expires_at);

-- +goose Down
DROP TABLE identity_proofs;
