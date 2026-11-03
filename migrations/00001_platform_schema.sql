-- +goose Up
-- Technical objects only. No financial tables, ledger accounts, or postings.

CREATE SCHEMA platform;
COMMENT ON SCHEMA platform IS
    'Infrastructure (outbox, idempotency, inbox). Not a financial schema.';

REVOKE ALL ON SCHEMA public FROM PUBLIC;
REVOKE ALL ON SCHEMA platform FROM PUBLIC;

GRANT USAGE ON SCHEMA platform TO paycore_app, paycore_readonly;

CREATE TABLE platform.schema_info (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

INSERT INTO platform.schema_info (key, value) VALUES ('core', 'initialized');

GRANT SELECT, INSERT, UPDATE ON TABLE platform.schema_info TO paycore_app;
GRANT SELECT ON TABLE platform.schema_info TO paycore_readonly;

ALTER DEFAULT PRIVILEGES FOR ROLE paycore_owner IN SCHEMA platform
    GRANT SELECT, INSERT, UPDATE ON TABLES TO paycore_app;
ALTER DEFAULT PRIVILEGES FOR ROLE paycore_owner IN SCHEMA platform
    GRANT SELECT ON TABLES TO paycore_readonly;

-- +goose Down
DROP SCHEMA platform CASCADE;
GRANT USAGE ON SCHEMA public TO PUBLIC;
