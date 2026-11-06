-- +goose Up
-- Expand the auth.users stub and add identity tables. Do not DROP auth.users:
-- accounts.user_id already references it. No ADMIN user and no password seed;
-- bootstrap admin is a future Go command.

COMMENT ON SCHEMA auth IS 'Identity: users, roles, sessions, tokens, login attempts.';

GRANT USAGE ON SCHEMA auth TO paycore_app, paycore_readonly;

ALTER TABLE auth.users
    ADD COLUMN email               TEXT NOT NULL,
    ADD COLUMN password_hash       TEXT NOT NULL,
    ADD COLUMN first_name          TEXT,
    ADD COLUMN last_name           TEXT,
    ADD COLUMN status              TEXT NOT NULL DEFAULT 'ACTIVE',
    ADD COLUMN failed_login_count  INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN locked_at           TIMESTAMPTZ,
    ADD COLUMN created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    ADD COLUMN updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    ADD CONSTRAINT ck_users_email_lower
        CHECK (email = lower(email) AND char_length(email) BETWEEN 3 AND 254),
    ADD CONSTRAINT ck_users_password_hash
        CHECK (password_hash LIKE '$argon2id$%'),
    ADD CONSTRAINT ck_users_status
        CHECK (status IN ('ACTIVE', 'LOCKED', 'DISABLED')),
    ADD CONSTRAINT ck_users_failed_login_count
        CHECK (failed_login_count >= 0),
    ADD CONSTRAINT ck_users_locked_at
        CHECK (
            (status = 'LOCKED' AND locked_at IS NOT NULL)
            OR (status <> 'LOCKED' AND locked_at IS NULL)
        ),
    ADD CONSTRAINT ck_users_first_name
        CHECK (first_name IS NULL OR char_length(first_name) BETWEEN 1 AND 100),
    ADD CONSTRAINT ck_users_last_name
        CHECK (last_name IS NULL OR char_length(last_name) BETWEEN 1 AND 100);

CREATE UNIQUE INDEX uq_users_email ON auth.users (email);

CREATE TABLE auth.roles (
    code        TEXT PRIMARY KEY,
    description TEXT NOT NULL,
    CONSTRAINT ck_roles_code CHECK (code IN (
        'CUSTOMER', 'MERCHANT', 'SUPPORT', 'OPERATOR', 'RISK_ANALYST', 'ADMIN'
    ))
);

INSERT INTO auth.roles (code, description) VALUES
    ('CUSTOMER', 'Own accounts and customer operations'),
    ('MERCHANT', 'Own merchant operations'),
    ('SUPPORT', 'Limited read of profiles and tickets'),
    ('OPERATOR', 'Create manual adjustments; payment incidents'),
    ('RISK_ANALYST', 'Read suspicious operations and audit'),
    ('ADMIN', 'Administration; this role does not move money')
ON CONFLICT (code) DO NOTHING;

CREATE TABLE auth.user_roles (
    user_id     UUID NOT NULL REFERENCES auth.users (id),
    role_code   TEXT NOT NULL REFERENCES auth.roles (code),
    granted_by  UUID REFERENCES auth.users (id),
    granted_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, role_code)
);

CREATE TABLE auth.sessions (
    id            UUID PRIMARY KEY,
    user_id       UUID NOT NULL REFERENCES auth.users (id),
    user_agent    TEXT,
    ip            INET,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at    TIMESTAMPTZ,
    revoke_reason TEXT,
    CONSTRAINT ck_sessions_revoke CHECK (
        (revoked_at IS NULL AND revoke_reason IS NULL)
        OR (revoked_at IS NOT NULL AND revoke_reason IN (
            'LOGOUT', 'LOGOUT_ALL', 'PASSWORD_CHANGED', 'TOKEN_REUSE',
            'USER_LOCKED', 'USER_DISABLED', 'ADMIN_REVOKE'
        ))
    )
);

CREATE INDEX ix_sessions_user_live
    ON auth.sessions (user_id)
    WHERE revoked_at IS NULL;

CREATE TABLE auth.refresh_tokens (
    id           UUID PRIMARY KEY,
    session_id   UUID NOT NULL REFERENCES auth.sessions (id),
    token_hash   BYTEA NOT NULL,
    replaced_by  UUID REFERENCES auth.refresh_tokens (id),
    expires_at   TIMESTAMPTZ NOT NULL,
    used_at      TIMESTAMPTZ,
    revoked_at   TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT uq_refresh_tokens_hash UNIQUE (token_hash),
    CONSTRAINT ck_refresh_tokens_hash_len CHECK (octet_length(token_hash) = 32),
    CONSTRAINT ck_refresh_tokens_expires CHECK (expires_at > created_at),
    CONSTRAINT ck_refresh_tokens_replaced CHECK (
        (used_at IS NULL AND replaced_by IS NULL)
        OR (used_at IS NOT NULL AND replaced_by IS NOT NULL)
    ),
    CONSTRAINT ck_refresh_tokens_not_self CHECK (replaced_by IS DISTINCT FROM id)
);

CREATE INDEX ix_refresh_tokens_session ON auth.refresh_tokens (session_id);

CREATE TABLE auth.login_attempts (
    id              UUID PRIMARY KEY,
    email           TEXT NOT NULL,
    user_id         UUID REFERENCES auth.users (id),
    ip              INET,
    success         BOOLEAN NOT NULL,
    failure_reason  TEXT,
    attempted_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT ck_login_attempts_failure CHECK (
        (success AND failure_reason IS NULL)
        OR (
            NOT success
            AND failure_reason IS NOT NULL
            AND failure_reason IN (
                'INVALID_CREDENTIALS', 'RATE_LIMITED', 'LOCKED', 'DISABLED', 'INVALID_REQUEST'
            )
        )
    )
);

CREATE INDEX ix_login_attempts_email ON auth.login_attempts (email, attempted_at DESC);
CREATE INDEX ix_login_attempts_ip ON auth.login_attempts (ip, attempted_at DESC);

CREATE TABLE auth.rate_limit_counters (
    bucket        TEXT NOT NULL,
    counter_key   TEXT NOT NULL,
    window_start  TIMESTAMPTZ NOT NULL,
    count         INTEGER NOT NULL,
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (bucket, counter_key, window_start),
    CONSTRAINT ck_rate_limit_bucket CHECK (bucket IN (
        'LOGIN_EMAIL', 'LOGIN_IP', 'REGISTER_IP', 'REFRESH_IP'
    )),
    CONSTRAINT ck_rate_limit_count CHECK (count >= 0)
);

CREATE TABLE auth.action_confirmations (
    id             UUID PRIMARY KEY,
    user_id        UUID NOT NULL REFERENCES auth.users (id),
    action_type    TEXT NOT NULL,
    code_hash      BYTEA NOT NULL,
    attempts_left  INTEGER NOT NULL,
    expires_at     TIMESTAMPTZ NOT NULL,
    confirmed_at   TIMESTAMPTZ,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT ck_action_confirmations_hash_len CHECK (octet_length(code_hash) = 32),
    CONSTRAINT ck_action_confirmations_attempts CHECK (attempts_left >= 0),
    CONSTRAINT ck_action_confirmations_expires CHECK (expires_at > created_at)
);

-- +goose StatementBegin
CREATE FUNCTION auth.forbid_login_attempt_mutation() RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog, auth, pg_temp
AS $$
BEGIN
    RAISE EXCEPTION 'auth.login_attempts is append-only: % is forbidden', TG_OP
        USING ERRCODE = 'P0001';
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER trg_login_attempts_immutable
    BEFORE UPDATE OR DELETE ON auth.login_attempts
    FOR EACH ROW EXECUTE FUNCTION auth.forbid_login_attempt_mutation();
CREATE TRIGGER trg_login_attempts_no_truncate
    BEFORE TRUNCATE ON auth.login_attempts
    FOR EACH STATEMENT EXECUTE FUNCTION auth.forbid_login_attempt_mutation();

-- +goose StatementBegin
CREATE FUNCTION auth.forbid_refresh_token_hash_change() RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog, auth, pg_temp
AS $$
BEGIN
    IF NEW.token_hash IS DISTINCT FROM OLD.token_hash THEN
        RAISE EXCEPTION 'auth.refresh_tokens.token_hash is immutable'
            USING ERRCODE = 'P0001';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER trg_refresh_tokens_hash_immutable
    BEFORE UPDATE ON auth.refresh_tokens
    FOR EACH ROW EXECUTE FUNCTION auth.forbid_refresh_token_hash_change();

REVOKE ALL ON FUNCTION auth.forbid_login_attempt_mutation() FROM PUBLIC;
REVOKE ALL ON FUNCTION auth.forbid_refresh_token_hash_change() FROM PUBLIC;

GRANT SELECT, INSERT, UPDATE ON TABLE auth.users TO paycore_app;
GRANT SELECT ON TABLE auth.users TO paycore_readonly;

GRANT SELECT ON TABLE auth.roles TO paycore_app, paycore_readonly;

GRANT SELECT, INSERT, DELETE ON TABLE auth.user_roles TO paycore_app;
GRANT SELECT ON TABLE auth.user_roles TO paycore_readonly;

GRANT SELECT, INSERT, UPDATE ON TABLE auth.sessions TO paycore_app;
GRANT SELECT ON TABLE auth.sessions TO paycore_readonly;

GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE auth.refresh_tokens TO paycore_app;
GRANT SELECT ON TABLE auth.refresh_tokens TO paycore_readonly;

GRANT SELECT, INSERT ON TABLE auth.login_attempts TO paycore_app;
GRANT SELECT ON TABLE auth.login_attempts TO paycore_readonly;

GRANT SELECT, INSERT, UPDATE ON TABLE auth.rate_limit_counters TO paycore_app;
GRANT SELECT ON TABLE auth.rate_limit_counters TO paycore_readonly;

GRANT SELECT, INSERT, UPDATE ON TABLE auth.action_confirmations TO paycore_app;
GRANT SELECT ON TABLE auth.action_confirmations TO paycore_readonly;

-- +goose Down
-- Refuse if any identity rows exist (role catalog is dropped with the table).
-- auth.users is not dropped: only columns added here are removed.

-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM auth.users)
        OR EXISTS (SELECT 1 FROM auth.user_roles)
        OR EXISTS (SELECT 1 FROM auth.sessions)
        OR EXISTS (SELECT 1 FROM auth.refresh_tokens)
        OR EXISTS (SELECT 1 FROM auth.login_attempts)
        OR EXISTS (SELECT 1 FROM auth.rate_limit_counters)
        OR EXISTS (SELECT 1 FROM auth.action_confirmations)
    THEN
        RAISE EXCEPTION 'auth down refused: identity data present'
            USING ERRCODE = 'P0001';
    END IF;
END;
$$;
-- +goose StatementEnd

DROP TRIGGER IF EXISTS trg_refresh_tokens_hash_immutable ON auth.refresh_tokens;
DROP TRIGGER IF EXISTS trg_login_attempts_no_truncate ON auth.login_attempts;
DROP TRIGGER IF EXISTS trg_login_attempts_immutable ON auth.login_attempts;
DROP FUNCTION IF EXISTS auth.forbid_refresh_token_hash_change();
DROP FUNCTION IF EXISTS auth.forbid_login_attempt_mutation();

DROP TABLE IF EXISTS auth.action_confirmations;
DROP TABLE IF EXISTS auth.rate_limit_counters;
DROP TABLE IF EXISTS auth.login_attempts;
DROP TABLE IF EXISTS auth.refresh_tokens;
DROP TABLE IF EXISTS auth.sessions;
DROP TABLE IF EXISTS auth.user_roles;
DROP TABLE IF EXISTS auth.roles;

DROP INDEX IF EXISTS auth.uq_users_email;

ALTER TABLE auth.users
    DROP CONSTRAINT IF EXISTS ck_users_last_name,
    DROP CONSTRAINT IF EXISTS ck_users_first_name,
    DROP CONSTRAINT IF EXISTS ck_users_locked_at,
    DROP CONSTRAINT IF EXISTS ck_users_failed_login_count,
    DROP CONSTRAINT IF EXISTS ck_users_status,
    DROP CONSTRAINT IF EXISTS ck_users_password_hash,
    DROP CONSTRAINT IF EXISTS ck_users_email_lower,
    DROP COLUMN IF EXISTS updated_at,
    DROP COLUMN IF EXISTS created_at,
    DROP COLUMN IF EXISTS locked_at,
    DROP COLUMN IF EXISTS failed_login_count,
    DROP COLUMN IF EXISTS status,
    DROP COLUMN IF EXISTS last_name,
    DROP COLUMN IF EXISTS first_name,
    DROP COLUMN IF EXISTS password_hash,
    DROP COLUMN IF EXISTS email;

REVOKE USAGE ON SCHEMA auth FROM paycore_app, paycore_readonly;

COMMENT ON SCHEMA auth IS 'Identity. Stub PK table only until the auth module exists.';
