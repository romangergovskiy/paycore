-- +goose Up
-- Business accounts and currency catalog. Stub auth/merchants tables exist
-- only so accounts FKs can be declared; those modules are not implemented here.

CREATE SCHEMA auth;
CREATE SCHEMA merchants;
CREATE SCHEMA accounts;

COMMENT ON SCHEMA auth IS 'Identity. Stub PK table only until the auth module exists.';
COMMENT ON SCHEMA merchants IS 'Merchants. Stub PK table only until the merchants module exists.';
COMMENT ON SCHEMA accounts IS 'Business accounts and currencies.';

REVOKE ALL ON SCHEMA auth FROM PUBLIC;
REVOKE ALL ON SCHEMA merchants FROM PUBLIC;
REVOKE ALL ON SCHEMA accounts FROM PUBLIC;

GRANT USAGE ON SCHEMA accounts TO paycore_app, paycore_readonly, paycore_ledger;

CREATE TABLE auth.users (
    id UUID PRIMARY KEY
);

CREATE TABLE merchants.merchants (
    id UUID PRIMARY KEY
);

CREATE TABLE accounts.currencies (
    code         CHAR(3) PRIMARY KEY,
    minor_units  SMALLINT NOT NULL,
    name         TEXT NOT NULL,
    is_active    BOOLEAN NOT NULL,
    CONSTRAINT ck_currencies_code CHECK (code ~ '^[A-Z]{3}$'),
    CONSTRAINT ck_currencies_minor_units CHECK (minor_units BETWEEN 0 AND 4)
);

CREATE TABLE accounts.accounts (
    id               UUID PRIMARY KEY,
    account_number   TEXT NOT NULL,
    user_id          UUID REFERENCES auth.users (id),
    merchant_id      UUID REFERENCES merchants.merchants (id),
    account_purpose  TEXT NOT NULL,
    currency         CHAR(3) NOT NULL REFERENCES accounts.currencies (code),
    status           TEXT NOT NULL,
    name             TEXT,
    version          BIGINT NOT NULL DEFAULT 0,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    closed_at        TIMESTAMPTZ,
    CONSTRAINT uq_accounts_id_currency UNIQUE (id, currency),
    CONSTRAINT uq_accounts_account_number UNIQUE (account_number),
    CONSTRAINT ck_accounts_one_owner CHECK (num_nonnulls(user_id, merchant_id) = 1),
    CONSTRAINT ck_accounts_purpose CHECK (account_purpose IN ('MAIN', 'SAVINGS', 'MERCHANT_SETTLEMENT')),
    CONSTRAINT ck_accounts_status CHECK (status IN ('ACTIVE', 'BLOCKED', 'CLOSED')),
    CONSTRAINT ck_accounts_closed_at CHECK (
        (status = 'CLOSED' AND closed_at IS NOT NULL)
        OR (status <> 'CLOSED' AND closed_at IS NULL)
    ),
    CONSTRAINT ck_accounts_merchant_purpose CHECK (
        (merchant_id IS NOT NULL AND account_purpose = 'MERCHANT_SETTLEMENT')
        OR (merchant_id IS NULL AND account_purpose IN ('MAIN', 'SAVINGS'))
    ),
    CONSTRAINT ck_accounts_version CHECK (version >= 0)
);

CREATE UNIQUE INDEX uq_accounts_merchant_currency
    ON accounts.accounts (merchant_id, currency)
    WHERE merchant_id IS NOT NULL;

CREATE INDEX ix_accounts_user_id
    ON accounts.accounts (user_id)
    WHERE user_id IS NOT NULL;

CREATE INDEX ix_accounts_merchant_id
    ON accounts.accounts (merchant_id)
    WHERE merchant_id IS NOT NULL;

CREATE TABLE accounts.account_status_history (
    id           UUID PRIMARY KEY,
    account_id   UUID NOT NULL REFERENCES accounts.accounts (id),
    from_status  TEXT,
    to_status    TEXT NOT NULL,
    reason       TEXT NOT NULL,
    changed_by   UUID NOT NULL,
    changed_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT ck_account_status_history_from CHECK (
        from_status IS NULL OR from_status IN ('ACTIVE', 'BLOCKED', 'CLOSED')
    ),
    CONSTRAINT ck_account_status_history_to CHECK (
        to_status IN ('ACTIVE', 'BLOCKED', 'CLOSED')
    )
);

CREATE INDEX ix_account_status_history_account_changed
    ON accounts.account_status_history (account_id, changed_at DESC);

INSERT INTO accounts.currencies (code, minor_units, name, is_active)
VALUES ('RUB', 2, 'Russian Ruble', true);

GRANT SELECT ON TABLE accounts.currencies TO paycore_app, paycore_readonly, paycore_ledger;
GRANT SELECT, INSERT, UPDATE ON TABLE accounts.accounts TO paycore_app;
GRANT SELECT ON TABLE accounts.accounts TO paycore_readonly, paycore_ledger;
GRANT UPDATE (updated_at) ON TABLE accounts.accounts TO paycore_ledger;
GRANT SELECT, INSERT ON TABLE accounts.account_status_history TO paycore_app;
GRANT SELECT ON TABLE accounts.account_status_history TO paycore_readonly;

-- +goose Down
DROP SCHEMA accounts CASCADE;
DROP SCHEMA auth CASCADE;
DROP SCHEMA merchants CASCADE;
