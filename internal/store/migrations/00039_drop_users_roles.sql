-- +goose Up
-- gravel:cost instant
-- ADR-0013's users.roles goes (gravel#199): migration 00025 copied it into role_grants, and nothing
-- has read or written it since #185, which has run Hidden Token Gaming's hub since 2026-10-15. The
-- column's CHECK goes with it. Postgres only marks the column dropped, so no table is rewritten.
ALTER TABLE users DROP COLUMN roles;

-- +goose Down
-- The column comes back holding what ADR-0013 could say: moderator, for every member with an
-- instance-wide moderator grant. Admin and operator grants, and every game-scoped one, have no
-- place in it.
ALTER TABLE users ADD COLUMN roles text[] NOT NULL DEFAULT '{}' CHECK (roles <@ ARRAY['moderator']::text[]);
UPDATE users u SET roles = ARRAY['moderator']
 WHERE EXISTS (SELECT 1 FROM role_grants g WHERE g.user_id = u.id AND g.role = 'moderator' AND g.game_id IS NULL);
