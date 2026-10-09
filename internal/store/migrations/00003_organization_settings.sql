-- +goose Up
-- The Organization settings resource (gravel#7): every host-configurable knob in one JSON
-- document, versioned by its own "version" key, managed through the API (and the hub's
-- `settings apply` for a committed manifest). Theme tokens and navigation links first; the
-- Discord role mapping, token lifetimes and layout overrides join it with their features.
ALTER TABLE organizations
    ADD COLUMN settings            jsonb NOT NULL DEFAULT '{}'::jsonb,
    ADD COLUMN settings_updated_at timestamptz;

-- +goose Down
ALTER TABLE organizations
    DROP COLUMN settings_updated_at,
    DROP COLUMN settings;
