-- +goose Up
-- The audit log (ADR-0010 §5): one row per moderation call through the hub, from the web or a bot.
-- A row is written BEFORE the driver is called, so nothing is done that is not recorded; the
-- outcome is filled in once, afterwards. Nothing else ever changes and nothing is deleted: the
-- trigger refuses an UPDATE of anything but a first outcome, every DELETE and a TRUNCATE. A row
-- whose outcome stays NULL is a call whose result the hub never learned (it stopped mid-call).
CREATE TABLE audit_log (
    id                 bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    organization_id    uuid NOT NULL REFERENCES organizations (id),
    at                 timestamptz NOT NULL,
    -- Who: a logged-in user (the owner) or an app (a bot), exactly one.
    actor_user_id      uuid REFERENCES users (id),
    actor_app_id       uuid REFERENCES apps (id),
    -- The person an app says it acts for (a Discord moderator behind a bot command), as the app
    -- asserted it; empty for a user, who acts as themselves.
    on_behalf_provider text NOT NULL DEFAULT '',
    on_behalf_subject  text NOT NULL DEFAULT '',
    server_id          text NOT NULL,
    action             text NOT NULL,
    -- The player acted on; empty for an action on the whole server (a broadcast).
    target_provider    text NOT NULL DEFAULT '',
    target_subject     text NOT NULL DEFAULT '',
    reason             text NOT NULL DEFAULT '',
    -- What else the action carried: the message, the team.
    detail             jsonb NOT NULL DEFAULT '{}',
    request_id         text NOT NULL DEFAULT '',
    -- "ok" or a failure word ("player_not_found", "unreachable", …); never error text, which can
    -- name the server's control address.
    outcome            text,
    finished_at        timestamptz,
    FOREIGN KEY (organization_id, server_id) REFERENCES managed_servers (organization_id, id),
    CONSTRAINT audit_log_one_actor CHECK (num_nonnulls(actor_user_id, actor_app_id) = 1),
    CONSTRAINT audit_log_finished CHECK ((outcome IS NULL) = (finished_at IS NULL))
);

CREATE INDEX audit_log_by_server ON audit_log (organization_id, server_id, id DESC);
CREATE INDEX audit_log_by_org ON audit_log (organization_id, id DESC);

-- +goose StatementBegin
CREATE FUNCTION audit_log_append_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'UPDATE' AND OLD.outcome IS NULL AND NEW.outcome IS NOT NULL
       AND (to_jsonb(NEW) - 'outcome' - 'finished_at') = (to_jsonb(OLD) - 'outcome' - 'finished_at') THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'audit_log is append-only: % refused', TG_OP USING ERRCODE = 'insufficient_privilege';
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER audit_log_append_only BEFORE UPDATE OR DELETE ON audit_log
    FOR EACH ROW EXECUTE FUNCTION audit_log_append_only();
CREATE TRIGGER audit_log_no_truncate BEFORE TRUNCATE ON audit_log
    FOR EACH STATEMENT EXECUTE FUNCTION audit_log_append_only();

-- +goose Down
DROP TABLE audit_log;
DROP FUNCTION audit_log_append_only();
