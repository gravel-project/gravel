-- +goose Up
-- gravel:cost scan server_hosts
-- A reboot an operator asked for (gravel#202): RebootServerHost sets it, and the cloud-VM manager
-- clears it when it starts the reboot, so a hub restart in between still carries it out. The host
-- passes through a new state, rebooting, which the state's CHECK admits; re-adding the check reads
-- every row of server_hosts (one per cloud-VM server).
ALTER TABLE server_hosts ADD COLUMN reboot boolean NOT NULL DEFAULT false;
ALTER TABLE server_hosts DROP CONSTRAINT server_hosts_state_check,
    ADD CONSTRAINT server_hosts_state_check CHECK (state IN ('absent', 'creating', 'booting', 'starting', 'running',
        'parking', 'parked', 'resizing', 'rebooting', 'destroying', 'error'));

-- +goose Down
UPDATE server_hosts SET state = 'running', step = '' WHERE state = 'rebooting';
ALTER TABLE server_hosts DROP CONSTRAINT server_hosts_state_check,
    ADD CONSTRAINT server_hosts_state_check CHECK (state IN ('absent', 'creating', 'booting', 'starting', 'running',
        'parking', 'parked', 'resizing', 'destroying', 'error'));
ALTER TABLE server_hosts DROP COLUMN reboot;
