-- +goose Up
-- gravel:cost scan identity_events, role_changes, audit_log, server_host_events
-- The events log (gravel#188, ADR-0028): one append-only record of what the hub did, read by
-- notifiers, the admin dashboard and the CLI through a cursor. Every source appends its event in
-- its own transaction; the kinds and their severities are internal/events' registry. Rows are never
-- changed; only the prune deletes, past settings.events.retention_days, and only when it says so
-- (SET LOCAL gravel.events_prune = 'on').
CREATE TABLE events (
    id                 bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    organization_id    uuid NOT NULL REFERENCES organizations (id),
    at                 timestamptz NOT NULL,
    kind               text NOT NULL,
    severity           text NOT NULL CHECK (severity IN ('info', 'notice', 'warning', 'critical')),
    actor_user_id      uuid REFERENCES users (id),
    actor_app_id       uuid REFERENCES apps (id),
    on_behalf_provider text NOT NULL DEFAULT '',
    on_behalf_subject  text NOT NULL DEFAULT '',
    server_id          text NOT NULL DEFAULT '',
    game_id            text NOT NULL DEFAULT '',
    subject_user_id    uuid REFERENCES users (id),
    -- One line a person reads; never a secret, an address or a SteamID.
    summary            text NOT NULL,
    detail             jsonb NOT NULL DEFAULT '{}',
    -- Where it came from (audit_log, identity_events, role_changes, server_host_events,
    -- server_leases, match_results, backup) and that row's id, so a backfill runs once.
    source             text NOT NULL,
    source_id          text NOT NULL DEFAULT '',
    request_id         text NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX events_source ON events (source, source_id) WHERE source_id <> '';
CREATE INDEX events_by_org ON events (organization_id, id DESC);
CREATE INDEX events_by_server ON events (organization_id, server_id, id DESC) WHERE server_id <> '';
CREATE INDEX events_at ON events (at);

-- +goose StatementBegin
CREATE FUNCTION events_append_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' AND current_setting('gravel.events_prune', true) = 'on' THEN
        RETURN OLD;
    END IF;
    RAISE EXCEPTION 'events is append-only: % refused', TG_OP USING ERRCODE = 'insufficient_privilege';
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER events_append_only BEFORE UPDATE OR DELETE ON events
    FOR EACH ROW EXECUTE FUNCTION events_append_only();
CREATE TRIGGER events_no_truncate BEFORE TRUNCATE ON events
    FOR EACH STATEMENT EXECUTE FUNCTION events_append_only();

-- The backfill: the history the hub already kept, oldest first across every source, with the
-- kinds the writers use (internal/store/events.go). Logins stay out. ON CONFLICT makes it safe to
-- run again.
INSERT INTO events (organization_id, at, kind, severity, actor_user_id, actor_app_id, on_behalf_provider, on_behalf_subject, server_id, game_id,
                    subject_user_id, summary, detail, source, source_id, request_id)
SELECT organization_id, at, kind, severity, actor_user_id, actor_app_id, on_behalf_provider, on_behalf_subject, server_id, game_id,
       subject_user_id, summary, detail, source, source_id, request_id
  FROM (
    SELECT u.organization_id, e.at, 'identity.' || e.event AS kind,
           CASE WHEN e.event IN ('registered', 'linked', 'proven') THEN 'info' ELSE 'notice' END AS severity,
           NULL::uuid AS actor_user_id, NULL::uuid AS actor_app_id, '' AS on_behalf_provider, '' AS on_behalf_subject,
           '' AS server_id, '' AS game_id, e.user_id AS subject_user_id,
           u.display_name || ': ' || replace(e.event, '_', ' ') || ' (' || e.provider || ')' AS summary,
           jsonb_build_object('provider', e.provider) AS detail, 'identity_events' AS source, e.id::text AS source_id, '' AS request_id
      FROM identity_events e JOIN users u ON u.id = e.user_id
     WHERE e.event IN ('registered', 'linked', 'unlinked', 'proven', 'password_set', 'password_reset', 'password_reset_issued')
    UNION ALL
    SELECT u.organization_id, rc.at, CASE WHEN rc.granted THEN 'role.granted' ELSE 'role.revoked' END, 'notice',
           rc.by_user_id, rc.by_app_id, '', '', '', coalesce(rc.game_id, ''), rc.user_id,
           u.display_name || CASE WHEN rc.granted THEN ' was granted ' ELSE ' lost ' END || rc.role
             || CASE WHEN rc.game_id IS NULL THEN '' ELSE ' for ' || rc.game_id END,
           jsonb_build_object('role', rc.role, 'game_id', coalesce(rc.game_id, '')), 'role_changes', rc.id::text, ''
      FROM role_changes rc JOIN users u ON u.id = rc.user_id
    UNION ALL
    SELECT a.organization_id, a.finished_at,
           CASE WHEN a.action IN ('kick', 'ban', 'unban', 'message', 'broadcast', 'move_player') THEN 'moderation.' || a.action ELSE 'action.' || a.action END,
           CASE WHEN a.action = 'host_destroy' THEN 'warning'
                WHEN a.action IN ('message', 'broadcast', 'move_player', 'host_expect', 'host_unpin', 'lease_request', 'lease_release') THEN 'info'
                ELSE 'notice' END,
           a.actor_user_id, a.actor_app_id, a.on_behalf_provider, a.on_behalf_subject, a.server_id, coalesce(s.game_id, ''), NULL::uuid,
           replace(a.action, '_', ' ') || ' on ' || a.server_id || ': ' || a.outcome,
           jsonb_build_object('action', a.action, 'outcome', a.outcome, 'audit_id', a.id), 'audit_log', a.id::text, a.request_id
      FROM audit_log a LEFT JOIN managed_servers s ON s.organization_id = a.organization_id AND s.id = a.server_id
     WHERE a.outcome IS NOT NULL
    UNION ALL
    SELECT h.organization_id, h.at,
           CASE WHEN h.from_state = 'resizing' AND h.to_state = 'running' THEN 'host.resized'
                WHEN h.from_state <> h.to_state THEN 'host.' || h.to_state
                WHEN h.step IN ('expect', 'snapshot', 'scale', 'scale_cancelled', 'env_drift') THEN 'host.' || h.step
                ELSE 'host.step' END,
           CASE WHEN h.from_state = 'resizing' AND h.to_state = 'running' THEN 'notice'
                WHEN h.from_state <> h.to_state AND h.to_state = 'destroying' THEN 'notice'
                WHEN h.from_state = h.to_state AND h.step IN ('snapshot', 'env_drift') THEN 'notice'
                ELSE 'info' END,
           NULL::uuid, NULL::uuid, '', '', h.server_id, coalesce(s.game_id, ''), NULL::uuid,
           h.server_id || ': ' || CASE WHEN h.from_state <> h.to_state THEN h.from_state || ' → ' || h.to_state ELSE coalesce(nullif(h.step, ''), 'step') END,
           jsonb_build_object('from', h.from_state, 'to', h.to_state, 'step', h.step, 'detail', h.detail, 'size', h.size), 'server_host_events', h.id::text, ''
      FROM server_host_events h LEFT JOIN managed_servers s ON s.organization_id = h.organization_id AND s.id = h.server_id
  ) AS b
 ORDER BY at, source, source_id
ON CONFLICT DO NOTHING;

-- +goose Down
DROP TABLE events;
DROP FUNCTION events_append_only();
