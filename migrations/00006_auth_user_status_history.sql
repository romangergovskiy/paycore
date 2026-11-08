-- +goose Up
-- Append-only history of Lock/Unlock/DisableUser. audit.audit_logs does not
-- exist yet; this is not project-wide audit (D-43).

CREATE TABLE auth.user_status_history (
    id               UUID PRIMARY KEY,
    user_id          UUID NOT NULL REFERENCES auth.users (id),
    changed_by       UUID NOT NULL REFERENCES auth.users (id),
    previous_status  TEXT NOT NULL,
    new_status       TEXT NOT NULL,
    reason           TEXT NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT ck_user_status_history_previous CHECK (
        previous_status IN ('ACTIVE', 'LOCKED', 'DISABLED')
    ),
    CONSTRAINT ck_user_status_history_new CHECK (
        new_status IN ('ACTIVE', 'LOCKED', 'DISABLED')
    ),
    CONSTRAINT ck_user_status_history_changed CHECK (
        previous_status <> new_status
    ),
    CONSTRAINT ck_user_status_history_reason CHECK (
        char_length(reason) BETWEEN 1 AND 500
    )
);

CREATE INDEX ix_user_status_history_user_created
    ON auth.user_status_history (user_id, created_at DESC);

-- +goose StatementBegin
CREATE FUNCTION auth.forbid_user_status_history_mutation() RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog, auth, pg_temp
AS $$
BEGIN
    RAISE EXCEPTION 'auth.user_status_history is append-only: % is forbidden', TG_OP
        USING ERRCODE = 'P0001';
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER trg_user_status_history_immutable
    BEFORE UPDATE OR DELETE ON auth.user_status_history
    FOR EACH ROW EXECUTE FUNCTION auth.forbid_user_status_history_mutation();
CREATE TRIGGER trg_user_status_history_no_truncate
    BEFORE TRUNCATE ON auth.user_status_history
    FOR EACH STATEMENT EXECUTE FUNCTION auth.forbid_user_status_history_mutation();

REVOKE ALL ON FUNCTION auth.forbid_user_status_history_mutation() FROM PUBLIC;

GRANT SELECT, INSERT ON TABLE auth.user_status_history TO paycore_app;
GRANT SELECT ON TABLE auth.user_status_history TO paycore_readonly;

-- +goose Down

-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM auth.user_status_history) THEN
        RAISE EXCEPTION 'auth down refused: user_status_history data present'
            USING ERRCODE = 'P0001';
    END IF;
END;
$$;
-- +goose StatementEnd

DROP TRIGGER IF EXISTS trg_user_status_history_no_truncate ON auth.user_status_history;
DROP TRIGGER IF EXISTS trg_user_status_history_immutable ON auth.user_status_history;
DROP FUNCTION IF EXISTS auth.forbid_user_status_history_mutation();
DROP TABLE IF EXISTS auth.user_status_history;
