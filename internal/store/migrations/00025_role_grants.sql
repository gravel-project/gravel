-- +goose Up
-- Role grants (gravel#185, ADR-0027 §3 to §5): a member holds admin, operator or moderator,
-- instance-wide or for one game (game_id). The owner is never a grant (organizations.owner_user_id)
-- and member is the base everyone holds. users.roles is no longer written or read; #199 drops it.
CREATE TABLE role_grants (
    id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    organization_id uuid NOT NULL REFERENCES organizations (id),
    user_id         uuid NOT NULL REFERENCES users (id),
    role            text NOT NULL CHECK (role IN ('admin', 'operator', 'moderator')),
    -- NULL is instance-wide; a game this hub has (MATCH SIMPLE: no check while NULL).
    game_id         text,
    granted_by      uuid REFERENCES users (id),
    granted_by_app  uuid REFERENCES apps (id),
    granted_at      timestamptz NOT NULL,
    UNIQUE NULLS NOT DISTINCT (user_id, role, game_id),
    FOREIGN KEY (organization_id, game_id) REFERENCES games (organization_id, id)
);
CREATE INDEX role_grants_user ON role_grants (user_id);

-- role_changes records the game and an app that granted; it stays append-only.
ALTER TABLE role_changes ADD COLUMN game_id text;
ALTER TABLE role_changes ADD COLUMN by_app_id uuid REFERENCES apps (id);

-- Every moderator keeps what they could do: moderator, and operator for the park and unpark that
-- moved to it. The owner prunes the operator grants later. by_user_id NULL is the migration.
INSERT INTO role_grants (organization_id, user_id, role, granted_at)
SELECT u.organization_id, u.id, r.role, now()
  FROM users u CROSS JOIN (VALUES ('moderator'), ('operator')) AS r(role)
 WHERE 'moderator' = ANY(u.roles);
INSERT INTO role_changes (user_id, role, granted, by_user_id, at)
SELECT u.id, 'operator', true, NULL, now() FROM users u WHERE 'moderator' = ANY(u.roles);

-- +goose Down
ALTER TABLE role_changes DROP COLUMN by_app_id;
ALTER TABLE role_changes DROP COLUMN game_id;
DROP TABLE role_grants;
