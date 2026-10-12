-- +goose Up
-- Match results from a game's match plugin (#20's PUG flow, ADR-0024): MatchZy posts a series' events
-- to the hub, which keeps the series (teams, score, winner), each map (score, winner, its demo in
-- object storage) and each player's totals on each map. A series is a server's plugin match id; a
-- lease's match is linked to the lease. Player rows are keyed by the provider identity (SteamID64),
-- never a member (README principle 5); who they are is resolved at read time.
CREATE TABLE match_results (
    id               uuid PRIMARY KEY,
    organization_id  uuid NOT NULL,
    server_id        text NOT NULL,
    lease_id         uuid REFERENCES server_leases (id),
    source           text NOT NULL,
    external_id      bigint NOT NULL,
    team1_name       text NOT NULL DEFAULT '',
    team2_name       text NOT NULL DEFAULT '',
    num_maps         integer NOT NULL DEFAULT 1,
    team1_score      integer NOT NULL DEFAULT 0,
    team2_score      integer NOT NULL DEFAULT 0,
    winner           text NOT NULL DEFAULT '' CHECK (winner IN ('', 'team1', 'team2', 'none')),
    started_at       timestamptz NOT NULL,
    ended_at         timestamptz,
    FOREIGN KEY (organization_id, server_id) REFERENCES managed_servers (organization_id, id),
    UNIQUE (organization_id, server_id, source, external_id)
);
CREATE INDEX match_results_ended ON match_results (organization_id, ended_at DESC NULLS LAST);
CREATE INDEX match_results_lease ON match_results (lease_id);

CREATE TABLE match_result_maps (
    result_id    uuid NOT NULL REFERENCES match_results (id) ON DELETE CASCADE,
    map_number   integer NOT NULL,
    map_name     text NOT NULL DEFAULT '',
    team1_score  integer NOT NULL DEFAULT 0,
    team2_score  integer NOT NULL DEFAULT 0,
    winner       text NOT NULL DEFAULT '' CHECK (winner IN ('', 'team1', 'team2', 'none')),
    started_at   timestamptz,
    ended_at     timestamptz,
    -- The demo's key in object storage; '' while there is none.
    demo_key     text NOT NULL DEFAULT '',
    demo_bytes   bigint NOT NULL DEFAULT 0,
    PRIMARY KEY (result_id, map_number)
);

CREATE TABLE match_result_players (
    result_id      uuid NOT NULL REFERENCES match_results (id) ON DELETE CASCADE,
    map_number     integer NOT NULL,
    provider       text NOT NULL,
    subject        text NOT NULL,
    name           text NOT NULL DEFAULT '',
    team           text NOT NULL CHECK (team IN ('team1', 'team2')),
    kills          integer NOT NULL DEFAULT 0,
    deaths         integer NOT NULL DEFAULT 0,
    assists        integer NOT NULL DEFAULT 0,
    damage         integer NOT NULL DEFAULT 0,
    headshot_kills integer NOT NULL DEFAULT 0,
    mvps           integer NOT NULL DEFAULT 0,
    score          integer NOT NULL DEFAULT 0,
    PRIMARY KEY (result_id, map_number, provider, subject)
);
CREATE INDEX match_result_players_identity ON match_result_players (provider, subject);

-- +goose Down
DROP TABLE match_result_players;
DROP TABLE match_result_maps;
DROP TABLE match_results;
