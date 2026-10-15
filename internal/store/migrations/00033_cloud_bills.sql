-- +goose Up
-- gravel:cost instant
-- Cloud spend (gravel#207, ADR-0034): a snapshot a day of each cloud location's bill so far, as
-- the cloud's own cost reader (cloud.Coster) answers it, for the location's project only. The
-- cloud's preview is the month to date up to its last nightly run, so the newest snapshot is the
-- month so far and two days' difference is one day's spend. The cloud_spend job replaces the
-- day's snapshot each time it runs. Nothing personal is in it; it is kept.
CREATE TABLE cloud_bills (
    organization_id uuid NOT NULL,
    location_id     text NOT NULL,
    -- The day the snapshot is for, in the organization's timezone.
    taken_on        date NOT NULL,
    -- The bill's month: its first day.
    month           date NOT NULL CHECK (extract(day FROM month) = 1),
    -- When the cloud last brought the bill up to date (NULL when it doesn't say), and when the
    -- hub read it.
    generated_at    timestamptz,
    taken_at        timestamptz NOT NULL,
    PRIMARY KEY (organization_id, location_id, taken_on)
);
CREATE INDEX cloud_bills_month ON cloud_bills (organization_id, month);

CREATE TABLE cloud_bill_lines (
    organization_id uuid NOT NULL,
    location_id     text NOT NULL,
    taken_on        date NOT NULL,
    -- Its place on the bill.
    line            integer NOT NULL,
    resource_id     text NOT NULL DEFAULT '',
    name            text NOT NULL DEFAULT '',
    product         text NOT NULL DEFAULT '',
    kind            text NOT NULL CHECK (kind IN ('compute', 'storage', 'network', 'other')),
    description     text NOT NULL DEFAULT '',
    usd             numeric(12, 4) NOT NULL,
    -- The server whose resource it is (ADR-0034 §3); '' for none.
    server_id       text NOT NULL DEFAULT '',
    PRIMARY KEY (organization_id, location_id, taken_on, line),
    FOREIGN KEY (organization_id, location_id, taken_on) REFERENCES cloud_bills (organization_id, location_id, taken_on) ON DELETE CASCADE
);

-- +goose Down
DROP TABLE cloud_bill_lines;
DROP TABLE cloud_bills;
