-- +goose Up
-- Session recaps (ADR-0018). server_occupancy is how many players a server had, by the minute:
-- the fewest and the most its polls saw in that minute, written only for a minute anyone was on.
-- A recap counts the minutes whose fewest reached the floor it is asked for, so nothing about a
-- game's floor is stored. No identity is in it, so it is kept like matches.
CREATE TABLE server_occupancy (
    organization_id uuid NOT NULL,
    server_id       text NOT NULL,
    minute          timestamptz NOT NULL CHECK (date_trunc('minute', minute) = minute),
    min_players     integer NOT NULL CHECK (min_players >= 0),
    max_players     integer NOT NULL CHECK (max_players >= min_players),
    PRIMARY KEY (organization_id, server_id, minute),
    FOREIGN KEY (organization_id, server_id) REFERENCES managed_servers (organization_id, id)
);

-- The last team (faction) scores the polls saw in the match: its final scores once it ended.
-- [{"name": "Alpha", "color": "#4CB1EF", "score": 1200}, …]; empty for a game without teams.
ALTER TABLE matches ADD COLUMN teams jsonb NOT NULL DEFAULT '[]';

-- +goose Down
ALTER TABLE matches DROP COLUMN teams;
DROP TABLE server_occupancy;
