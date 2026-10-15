-- +goose Up
-- gravel:cost instant
-- Every foreign key may be deferred (gravel#213): an import loads a whole hub in one transaction
-- with SET CONSTRAINTS ALL DEFERRED, so its tables load in any order and a cycle (an organization's
-- owner is a user of the organization) needs no special case; the keys are checked at commit. They
-- stay INITIALLY IMMEDIATE, so nothing else changes: every other transaction checks them per
-- statement, as before. A new foreign key is DEFERRABLE too (TestEveryForeignKeyIsDeferrable).
-- +goose StatementBegin
DO $$
DECLARE c record;
BEGIN
    FOR c IN SELECT conrelid::regclass AS tbl, conname FROM pg_constraint
              WHERE contype = 'f' AND connamespace = 'public'::regnamespace AND NOT condeferrable
    LOOP
        EXECUTE format('ALTER TABLE %s ALTER CONSTRAINT %I DEFERRABLE INITIALLY IMMEDIATE', c.tbl, c.conname);
    END LOOP;
END $$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DO $$
DECLARE c record;
BEGIN
    FOR c IN SELECT conrelid::regclass AS tbl, conname FROM pg_constraint
              WHERE contype = 'f' AND connamespace = 'public'::regnamespace AND condeferrable
    LOOP
        EXECUTE format('ALTER TABLE %s ALTER CONSTRAINT %I NOT DEFERRABLE', c.tbl, c.conname);
    END LOOP;
END $$;
-- +goose StatementEnd
