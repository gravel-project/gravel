-- +goose Up
-- gravel:cost scan organizations
-- Ownership transfer in two steps (gravel#197, ADR-0027 §6): the owner nominates a member, who
-- accepts within 7 days. The nomination is these three columns, set and cleared together; there
-- is at most one at a time, and a new one replaces it. Accepting moves owner_user_id in the same
-- statement that clears them, so the hub has exactly one owner at every moment.
ALTER TABLE organizations
    ADD COLUMN pending_owner_user_id      uuid REFERENCES users (id),
    ADD COLUMN pending_owner_nominated_at timestamptz,
    ADD COLUMN pending_owner_expires_at   timestamptz,
    ADD CONSTRAINT organizations_pending_owner_whole CHECK (
        (pending_owner_user_id IS NULL) = (pending_owner_nominated_at IS NULL)
        AND (pending_owner_user_id IS NULL) = (pending_owner_expires_at IS NULL)),
    ADD CONSTRAINT organizations_pending_owner_not_owner CHECK (pending_owner_user_id IS NULL OR pending_owner_user_id IS DISTINCT FROM owner_user_id);

-- +goose Down
ALTER TABLE organizations
    DROP CONSTRAINT organizations_pending_owner_not_owner,
    DROP CONSTRAINT organizations_pending_owner_whole,
    DROP COLUMN pending_owner_expires_at,
    DROP COLUMN pending_owner_nominated_at,
    DROP COLUMN pending_owner_user_id;
