-- +goose Up
-- Hub roles (ADR-0013): the owner grants a member a role, today only "moderator", who may then
-- moderate servers from the web as the owner does. A role is on the member, the hub is its
-- authority, and every grant and revocation is kept in role_changes (who, by whom, when), which
-- is appended to, never changed.
ALTER TABLE users ADD COLUMN roles text[] NOT NULL DEFAULT '{}' CHECK (roles <@ ARRAY['moderator']::text[]);

CREATE TABLE role_changes (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id    uuid NOT NULL REFERENCES users (id),
    role       text NOT NULL,
    granted    boolean NOT NULL,
    by_user_id uuid REFERENCES users (id),
    at         timestamptz NOT NULL
);
CREATE INDEX role_changes_user ON role_changes (user_id, at);

-- +goose StatementBegin
CREATE FUNCTION role_changes_append_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'role_changes is append-only';
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER role_changes_append_only BEFORE UPDATE OR DELETE ON role_changes
    FOR EACH ROW EXECUTE FUNCTION role_changes_append_only();

-- +goose Down
DROP TABLE role_changes;
DROP FUNCTION role_changes_append_only();
ALTER TABLE users DROP COLUMN roles;
