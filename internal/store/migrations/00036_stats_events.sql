-- +goose Up
-- gravel:cost scan ingest_batches
-- The events a server's feed carries, parsed from its stored batches (ADR-0011 §4, ADR-0012 §1;
-- gravel#4, #3): the detail beside match_stats' totals. Every row says where it came from and how
-- far to trust it, as columns: the source, the server and its game, the source's own key for the
-- event (a second copy of which is dropped, never stored twice), the game's time and when the hub
-- received it, the server's trust then (copied, never joined: trust is not retroactive), the batch
-- it was parsed from (no foreign key: batches are a 30-day buffer) and the schema version.
--
-- An event is of up to two players, keyed by provider identity (README principle 5): the one it
-- is about (a kill's killer) and the other (its victim); either is absent for an event of nobody
-- (a match's state, a gap). Personal data: erased with the stats (ADR-0012 §7), and anonymised
-- past stats.raw_retention_months with the rest of the raw rows (§6), never deleted.
--
-- A gap (kind 'gap') is missing data as a row (ADR-0011 §5): its window is observed_at to
-- extensions.to, its reason in extensions.reason.
CREATE TABLE stats_events (
    id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    organization_id uuid NOT NULL,
    server_id       text NOT NULL,
    game_id         text NOT NULL,
    source          text NOT NULL CHECK (source IN ('server_adapter', 'platform_api', 'first_party', 'self_report')),
    source_key      text NOT NULL CHECK (source_key <> ''),
    kind            text NOT NULL CHECK (kind <> ''),
    observed_at     timestamptz NOT NULL,
    received_at     timestamptz NOT NULL,
    trust           text NOT NULL CHECK (trust IN ('official', 'community')),
    batch_id        bigint,
    schema_version  integer NOT NULL CHECK (schema_version > 0),
    provider        text,
    subject         text,
    target_provider text,
    target_subject  text,
    extensions      jsonb NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(extensions) = 'object'),
    CHECK ((provider IS NULL) = (subject IS NULL)),
    CHECK ((target_provider IS NULL) = (target_subject IS NULL)),
    UNIQUE (organization_id, server_id, source, source_key),
    FOREIGN KEY (organization_id, server_id) REFERENCES managed_servers (organization_id, id) DEFERRABLE INITIALLY IMMEDIATE
);
-- A server's events of a kind over a window (its gaps, its kills), and the retention's oldest.
CREATE INDEX stats_events_server_kind ON stats_events (organization_id, server_id, kind, observed_at);
CREATE INDEX stats_events_observed ON stats_events (organization_id, observed_at);
-- Erasure finds an identity in either place.
CREATE INDEX stats_events_identity ON stats_events (provider, subject) WHERE provider IS NOT NULL;
CREATE INDEX stats_events_target ON stats_events (target_provider, target_subject) WHERE target_provider IS NOT NULL;

-- The parser version that last read a batch (0: none). Raising a source's parser version puts its
-- batches back to pending, and the events' source keys make reading one again safe.
ALTER TABLE ingest_batches ADD COLUMN parser_version integer NOT NULL DEFAULT 0;
-- The parse job reads one source's pending batches; other sources' (a log a driver reads as it
-- arrives) stay pending and must not be in its way.
DROP INDEX ingest_batches_pending;
CREATE INDEX ingest_batches_pending ON ingest_batches (source, id) WHERE state = 'pending';

-- +goose Down
DROP INDEX ingest_batches_pending;
CREATE INDEX ingest_batches_pending ON ingest_batches (id) WHERE state = 'pending';
ALTER TABLE ingest_batches DROP COLUMN parser_version;
DROP TABLE stats_events;
