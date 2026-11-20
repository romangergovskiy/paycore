-- +goose Up
-- HTTP idempotency keys, transactional outbox, and inbox (Stage A).
-- Grants are table-level only. 00001 still has blanket defaults on platform;
-- this migration REVOKE/GRANTs the intended rights on these three tables.

CREATE TABLE platform.idempotency_keys (
    id             UUID PRIMARY KEY,
    scope          TEXT NOT NULL,
    endpoint       TEXT NOT NULL,
    key            TEXT NOT NULL,
    request_hash   BYTEA NOT NULL,
    status         TEXT NOT NULL,
    response_code  SMALLINT,
    response_body  JSONB,
    resource_type  TEXT,
    resource_id    UUID,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at   TIMESTAMPTZ,
    expires_at     TIMESTAMPTZ NOT NULL,
    CONSTRAINT uq_idempotency_keys_scope_endpoint_key UNIQUE (scope, endpoint, key),
    CONSTRAINT ck_idempotency_keys_scope CHECK (scope ~ '^(user|merchant):'),
    CONSTRAINT ck_idempotency_keys_endpoint CHECK (char_length(endpoint) BETWEEN 1 AND 255),
    CONSTRAINT ck_idempotency_keys_key CHECK (key ~ '^[ -~]{1,255}$'),
    CONSTRAINT ck_idempotency_keys_request_hash CHECK (octet_length(request_hash) = 32),
    CONSTRAINT ck_idempotency_keys_status CHECK (status IN ('IN_PROGRESS', 'COMPLETED')),
    CONSTRAINT ck_idempotency_keys_completed CHECK (
        (status = 'COMPLETED' AND response_code IS NOT NULL AND completed_at IS NOT NULL)
        OR (status = 'IN_PROGRESS' AND response_code IS NULL AND response_body IS NULL AND completed_at IS NULL AND resource_type IS NULL AND resource_id IS NULL)
    ),
    CONSTRAINT ck_idempotency_keys_response_code CHECK (
        response_code IS NULL OR (response_code >= 200 AND response_code < 500)
    ),
    CONSTRAINT ck_idempotency_keys_response_body CHECK (
        response_body IS NULL OR pg_column_size(response_body) <= 65536
    )
);

COMMENT ON TABLE platform.idempotency_keys IS
    'HTTP idempotency keys (D-07). IN_PROGRESS and COMPLETED are written in the same transaction as the business effect.';
COMMENT ON COLUMN platform.idempotency_keys.scope IS
    'user:<id> or merchant:<id>. Not an API key: keys rotate.';
COMMENT ON COLUMN platform.idempotency_keys.request_hash IS
    'SHA-256 of endpoint, path values, and canonical JSON body. Raw secrets are not stored.';
COMMENT ON COLUMN platform.idempotency_keys.expires_at IS
    'Replay window; created_at + 24 hours. Cleanup deletes expired rows; durable business UNIQUE still blocks a second effect.';

CREATE INDEX ix_idempotency_keys_expires_at
    ON platform.idempotency_keys (expires_at);

CREATE TABLE platform.outbox_events (
    id                 UUID PRIMARY KEY,
    seq                BIGINT GENERATED ALWAYS AS IDENTITY,
    aggregate_type     TEXT NOT NULL,
    aggregate_id       UUID NOT NULL,
    aggregate_version  BIGINT NOT NULL,
    event_type         TEXT NOT NULL,
    event_version      SMALLINT NOT NULL,
    payload            JSONB NOT NULL DEFAULT '{}'::jsonb,
    headers            JSONB NOT NULL DEFAULT '{}'::jsonb,
    status             TEXT NOT NULL DEFAULT 'PENDING',
    attempts           INT NOT NULL DEFAULT 0,
    next_attempt_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    processing_until   TIMESTAMPTZ,
    lease_owner        TEXT,
    last_error         TEXT,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    published_at       TIMESTAMPTZ,
    CONSTRAINT uq_outbox_events_seq UNIQUE (seq),
    CONSTRAINT ck_outbox_events_aggregate_type CHECK (char_length(aggregate_type) BETWEEN 1 AND 100),
    CONSTRAINT ck_outbox_events_event_type CHECK (char_length(event_type) BETWEEN 1 AND 100),
    CONSTRAINT ck_outbox_events_event_version CHECK (event_version >= 1),
    CONSTRAINT ck_outbox_events_aggregate_version CHECK (aggregate_version >= 0),
    CONSTRAINT ck_outbox_events_attempts CHECK (attempts >= 0),
    CONSTRAINT ck_outbox_events_status CHECK (status IN ('PENDING', 'PUBLISHED', 'DEAD')),
    CONSTRAINT ck_outbox_events_published CHECK (
        (status = 'PUBLISHED' AND published_at IS NOT NULL)
        OR (status <> 'PUBLISHED' AND published_at IS NULL)
    ),
    CONSTRAINT ck_outbox_events_lease CHECK ((processing_until IS NULL) = (lease_owner IS NULL))
);

COMMENT ON TABLE platform.outbox_events IS
    'Transactional outbox (D-23). Insert PENDING in the business transaction. Relay claims with FOR UPDATE SKIP LOCKED, publishes outside the transaction, ACKs in a separate transaction.';
COMMENT ON COLUMN platform.outbox_events.seq IS
    'Order within one aggregate_id. A later event is not claimed while an earlier PENDING (or, in Stage A, DEAD) row of the same aggregate exists. No global order across aggregates.';
COMMENT ON COLUMN platform.outbox_events.lease_owner IS
    'Relay instance id. ACK and FAIL require a matching live lease; a stale owner updates 0 rows.';

CREATE INDEX ix_outbox_events_pending_seq
    ON platform.outbox_events (seq)
    WHERE status = 'PENDING';

CREATE INDEX ix_outbox_events_pending_aggregate_seq
    ON platform.outbox_events (aggregate_id, seq)
    WHERE status = 'PENDING';

-- Supports NOT EXISTS on earlier PENDING or DEAD seq of the same aggregate.
-- Whether DEAD unblocks later seq is not a locked product decision (D-23
-- defines the status only). Stage A does not auto-advance past DEAD.
CREATE INDEX ix_outbox_events_open_aggregate_seq
    ON platform.outbox_events (aggregate_id, seq)
    WHERE status IN ('PENDING', 'DEAD');

CREATE INDEX ix_outbox_events_published_at
    ON platform.outbox_events (published_at)
    WHERE status = 'PUBLISHED';

CREATE TABLE platform.inbox_messages (
    consumer      TEXT NOT NULL,
    message_id    UUID NOT NULL,
    received_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    processed_at  TIMESTAMPTZ,
    CONSTRAINT pk_inbox_messages PRIMARY KEY (consumer, message_id),
    CONSTRAINT ck_inbox_messages_consumer CHECK (char_length(consumer) BETWEEN 1 AND 255)
);

COMMENT ON TABLE platform.inbox_messages IS
    'Consumer inbox (D-23). Dedup on (consumer, message_id). Insert + effect + processed_at are one transaction. At-least-once delivery; not exactly-once.';

REVOKE ALL ON TABLE platform.idempotency_keys FROM PUBLIC;
REVOKE ALL ON TABLE platform.outbox_events FROM PUBLIC;
REVOKE ALL ON TABLE platform.inbox_messages FROM PUBLIC;

REVOKE ALL ON TABLE platform.idempotency_keys FROM paycore_app;
REVOKE ALL ON TABLE platform.outbox_events FROM paycore_app;
REVOKE ALL ON TABLE platform.inbox_messages FROM paycore_app;
REVOKE ALL ON TABLE platform.idempotency_keys FROM paycore_readonly;
REVOKE ALL ON TABLE platform.outbox_events FROM paycore_readonly;
REVOKE ALL ON TABLE platform.inbox_messages FROM paycore_readonly;

GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE platform.idempotency_keys TO paycore_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE platform.outbox_events TO paycore_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE platform.inbox_messages TO paycore_app;

GRANT SELECT ON TABLE platform.idempotency_keys TO paycore_readonly;
GRANT SELECT ON TABLE platform.outbox_events TO paycore_readonly;
GRANT SELECT ON TABLE platform.inbox_messages TO paycore_readonly;

-- +goose Down
DROP TABLE IF EXISTS platform.inbox_messages;
DROP TABLE IF EXISTS platform.outbox_events;
DROP TABLE IF EXISTS platform.idempotency_keys;
