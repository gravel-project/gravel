-- +goose Up
-- gravel:cost scan organizations, apps, app_grants
-- The owner claim, settings updates, apps and members' grants to apps are events now (gravel#189,
-- ADR-0028); their writers are internal/store's ClaimOrganization, UpdateOrganizationSettings,
-- CreateApp, RevokeApp, SaveAppGrant and DeleteAppGrant. This backfills what the hub already knew:
-- the claim, every app's registration and revocation, and the grants members hold now, oldest
-- first, marked backfilled and keyed as the writers key them, so nothing is written twice. No
-- settings history was ever kept, so there is none to backfill, and a grant taken back before
-- today left no row. Only the claim and a grant have an actor: who registered an app was never
-- recorded.
INSERT INTO events (organization_id, at, kind, severity, actor_user_id, subject_user_id, summary, detail, source, source_id)
SELECT organization_id, at, kind, severity, actor_user_id, subject_user_id, summary, detail, source, source_id
  FROM (
    SELECT o.id AS organization_id, o.claimed_at AS at, 'ownership.claimed' AS kind, 'notice' AS severity,
           o.owner_user_id AS actor_user_id, o.owner_user_id AS subject_user_id,
           coalesce(u.display_name, 'a member') || ' claimed the hub and became its owner' AS summary,
           jsonb_build_object('backfilled', true) AS detail, 'organizations.claim' AS source, o.id::text AS source_id
      FROM organizations o LEFT JOIN users u ON u.id = o.owner_user_id
     WHERE o.claimed_at IS NOT NULL
    UNION ALL
    SELECT a.organization_id, a.created_at, 'app.created', 'notice', NULL, NULL,
           'The app "' || a.name || '" was registered',
           jsonb_build_object('app_id', a.id::text, 'client_id', a.client_id, 'name', a.name, 'scopes', to_jsonb(a.scopes),
                              'redirect_uris', to_jsonb(a.redirect_uris), 'backfilled', true),
           'apps.created', a.id::text
      FROM apps a
    UNION ALL
    SELECT a.organization_id, a.revoked_at, 'app.revoked', 'notice', NULL, NULL,
           'The app "' || a.name || '" was revoked',
           jsonb_build_object('app_id', a.id::text, 'client_id', a.client_id, 'name', a.name, 'scopes', to_jsonb(a.scopes),
                              'redirect_uris', to_jsonb(a.redirect_uris), 'backfilled', true),
           'apps.revoked', a.id::text
      FROM apps a
     WHERE a.revoked_at IS NOT NULL
    UNION ALL
    SELECT a.organization_id, g.granted_at, 'app.granted', 'info', g.user_id, g.user_id,
           u.display_name || ' let "' || a.name || '" act for them',
           jsonb_build_object('app_id', a.id::text, 'client_id', a.client_id, 'name', a.name, 'scopes', to_jsonb(g.scopes),
                              'added', to_jsonb(g.scopes), 'backfilled', true),
           'app_grants', g.user_id::text || ':' || g.app_id::text || ':' || (extract(epoch FROM g.granted_at) * 1000000)::bigint::text
      FROM app_grants g JOIN apps a ON a.id = g.app_id JOIN users u ON u.id = g.user_id
  ) backfill
 ORDER BY at, source, source_id
ON CONFLICT (source, source_id) WHERE source_id <> '' DO NOTHING;

-- +goose Down
SET LOCAL gravel.events_prune = 'on';
DELETE FROM events
 WHERE kind IN ('ownership.claimed', 'app.created', 'app.revoked', 'app.granted') AND detail @> '{"backfilled": true}';
