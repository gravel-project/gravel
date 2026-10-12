-- +goose Up
-- A cloud-VM server's vertical scaling policy (ADR-0003 §4, gravel#37). The policy itself is the
-- manifest's (managed_servers.vm); the host row keeps what it decided and what the owner fixed:
-- a size the owner pinned (the policy stands aside, an unparked host comes back to it), a decision
-- waiting for a boundary (a map change, a match's end, an empty server) with its reason and since
-- when, and when the last resize began (the cooldown counts from it). Each decision, cancellation
-- and resize is also a row of server_host_events.
ALTER TABLE server_hosts ADD COLUMN pinned_size text NOT NULL DEFAULT '';
ALTER TABLE server_hosts ADD COLUMN pending_size text NOT NULL DEFAULT '';
ALTER TABLE server_hosts ADD COLUMN pending_reason text NOT NULL DEFAULT '';
ALTER TABLE server_hosts ADD COLUMN pending_since timestamptz;
ALTER TABLE server_hosts ADD COLUMN last_resize_at timestamptz;

-- +goose Down
ALTER TABLE server_hosts DROP COLUMN last_resize_at;
ALTER TABLE server_hosts DROP COLUMN pending_since;
ALTER TABLE server_hosts DROP COLUMN pending_reason;
ALTER TABLE server_hosts DROP COLUMN pending_size;
ALTER TABLE server_hosts DROP COLUMN pinned_size;
