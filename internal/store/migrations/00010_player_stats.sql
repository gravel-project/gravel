-- +goose Up
-- The stats store (ADR-0012), its first step: matches and each player's totals in them, recorded
-- from the hub's polls of a server. A board counts match_stats, so it is right without a feed;
-- the feed's events (#4) add detail later. Rows are keyed by provider identity, never a user id
-- (principle 3): a player is resolved to a member when a board is read. trust is the server's
-- when the row was written, copied in, so a promotion never rewrites history (principle 9).
-- A pseudonym is how an unlinked player is shown: chosen once from a keyed hash and kept, unique
-- in the organization. users.show_name_on_boards is a member's own choice, off by default.
CREATE TABLE matches (
    id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    organization_id uuid NOT NULL,
    server_id       text NOT NULL,
    game_id         text NOT NULL,
    started_at      timestamptz NOT NULL,
    ended_at        timestamptz,
    -- The map as the game names it in play, and the rotation's position, when known.
    map             text NOT NULL DEFAULT '',
    rotation_index  integer NOT NULL DEFAULT -1,
    FOREIGN KEY (organization_id, server_id) REFERENCES managed_servers (organization_id, id)
);
CREATE INDEX matches_server ON matches (organization_id, server_id, started_at DESC);
-- One open match per server.
CREATE UNIQUE INDEX matches_open ON matches (organization_id, server_id) WHERE ended_at IS NULL;

CREATE TABLE match_stats (
    match_id    bigint NOT NULL REFERENCES matches (id),
    provider    text NOT NULL,
    subject     text NOT NULL,
    team        text NOT NULL DEFAULT '',
    kills       integer NOT NULL DEFAULT 0 CHECK (kills >= 0),
    deaths      integer NOT NULL DEFAULT 0 CHECK (deaths >= 0),
    seconds_on  integer NOT NULL DEFAULT 0 CHECK (seconds_on >= 0),
    first_seen  timestamptz NOT NULL,
    last_seen   timestamptz NOT NULL,
    -- ADR-0011's provenance: where the counts came from, and the server's trust when written.
    source      text NOT NULL CHECK (source IN ('server_adapter', 'platform_api', 'first_party', 'self_report')),
    trust       text NOT NULL CHECK (trust IN ('official', 'community')),
    PRIMARY KEY (match_id, provider, subject)
);
CREATE INDEX match_stats_identity ON match_stats (provider, subject);

CREATE TABLE pseudonyms (
    organization_id uuid NOT NULL REFERENCES organizations (id),
    provider        text NOT NULL,
    subject         text NOT NULL,
    name            text NOT NULL,
    created_at      timestamptz NOT NULL,
    PRIMARY KEY (organization_id, provider, subject),
    UNIQUE (organization_id, name)
);

ALTER TABLE users ADD COLUMN show_name_on_boards boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE users DROP COLUMN show_name_on_boards;
DROP TABLE pseudonyms;
DROP TABLE match_stats;
DROP TABLE matches;
