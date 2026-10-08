-- +goose Up
-- The single tenant a hub serves. `organization` is the seam on every top-level object; exactly
-- one row is the built-in organization today, and nothing else is enforced (no quotas or
-- isolation until a real second tenant exists).
CREATE TABLE organizations (
    id                     uuid PRIMARY KEY,
    name                   text NOT NULL,
    builtin                boolean NOT NULL DEFAULT false,
    created_at             timestamptz NOT NULL DEFAULT now(),
    -- Ownership. claimed_at is set by the one-time owner-claim token; owner_user_id is filled in
    -- by the identity work (gravel#13) once a logged-in user performs the claim.
    claimed_at             timestamptz,
    owner_user_id          uuid,
    claim_token_hash       bytea,
    claim_token_expires_at timestamptz,
    CONSTRAINT organizations_claim_token_pair CHECK ((claim_token_hash IS NULL) = (claim_token_expires_at IS NULL)),
    CONSTRAINT organizations_owned_has_no_token CHECK (claimed_at IS NULL OR claim_token_hash IS NULL)
);

CREATE UNIQUE INDEX organizations_one_builtin ON organizations ((builtin)) WHERE builtin;

-- +goose Down
DROP TABLE organizations;
