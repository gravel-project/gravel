-- +goose Up
-- An expected population, a scaling signal (gravel#172): the owner, a moderator or an app with
-- servers:moderate says how many players a server should expect and until when (a PUG that has
-- filled, a session about to start). Until then the scaling policy treats the larger of the live
-- and the expected players as the room, and the server isn't parked for being empty. Setting 0
-- players clears it; the hub clears it itself once it has passed. Each change is also a row of
-- server_host_events and of the audit log.
ALTER TABLE server_hosts ADD COLUMN expected_players integer NOT NULL DEFAULT 0;
ALTER TABLE server_hosts ADD COLUMN expected_until timestamptz;
ALTER TABLE server_hosts ADD COLUMN expected_reason text NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE server_hosts DROP COLUMN expected_reason;
ALTER TABLE server_hosts DROP COLUMN expected_until;
ALTER TABLE server_hosts DROP COLUMN expected_players;
