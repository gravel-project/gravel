-- +goose Up
-- The servers manifest's seeding section is gone (0.8.0): seeding rules are the Organization
-- settings' discord.seeding (ADR-0016), which the bot's seeding module reads. Nothing read the
-- column, and a manifest that still has the section is refused at parse.
ALTER TABLE managed_servers DROP COLUMN seeding;

-- +goose Down
-- The column comes back empty: what it held is not restored, and nothing reads it.
ALTER TABLE managed_servers ADD COLUMN seeding jsonb;
