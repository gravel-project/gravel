-- +goose Up
-- Inbound ingestion (ADR-0011). A server that pushes events (War Dogs' [WDServerFeed]) has a feed:
-- the origin it posts to and the file its token is read from, never the token. Every batch the
-- ingest route authenticates is kept here, as received, before the route answers: the sources do
-- not retry, so this insert is all the durability they get. Parsing reads the rows afterwards and
-- marks them; nothing updates a body. Rows are a 30-day buffer (the ingest_prune job), not the
-- record: the events parsed from them are.
ALTER TABLE managed_servers ADD COLUMN feed jsonb;

CREATE TABLE ingest_batches (
    id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    organization_id uuid NOT NULL,
    server_id       text NOT NULL,
    -- The kind of source, which says how to parse the body: "wardogs".
    source          text NOT NULL,
    received_at     timestamptz NOT NULL,
    body            bytea NOT NULL,
    -- The request headers the source defines (CS2's log offsets), nothing else.
    headers         jsonb NOT NULL DEFAULT '{}',
    body_sha256     bytea NOT NULL,
    state           text NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'parsed', 'failed')),
    FOREIGN KEY (organization_id, server_id) REFERENCES managed_servers (organization_id, id)
);

CREATE INDEX ingest_batches_received_at ON ingest_batches (received_at);
CREATE INDEX ingest_batches_server ON ingest_batches (organization_id, server_id, id);
CREATE INDEX ingest_batches_pending ON ingest_batches (id) WHERE state = 'pending';

-- +goose Down
DROP TABLE ingest_batches;
ALTER TABLE managed_servers DROP COLUMN feed;
