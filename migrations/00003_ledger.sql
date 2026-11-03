-- +goose Up
-- Ledger tables, immutability triggers, chart seed, and system accounts.
-- No post_journal or other SECURITY DEFINER gateway functions.

CREATE SCHEMA ledger;
COMMENT ON SCHEMA ledger IS 'Double-entry journal, projections, holds. Written later only via ledger functions.';

REVOKE ALL ON SCHEMA ledger FROM PUBLIC;
GRANT USAGE ON SCHEMA ledger TO paycore_app, paycore_readonly, paycore_ledger;

CREATE TABLE ledger.chart_of_accounts (
    code            TEXT PRIMARY KEY,
    name            TEXT NOT NULL,
    account_class   TEXT NOT NULL,
    normal_balance  TEXT NOT NULL,
    allow_negative  BOOLEAN NOT NULL,
    is_contra       BOOLEAN NOT NULL,
    in_mvp          BOOLEAN NOT NULL,
    CONSTRAINT ck_coa_class CHECK (account_class IN ('ASSET', 'LIABILITY', 'EQUITY', 'REVENUE', 'EXPENSE')),
    CONSTRAINT ck_coa_normal CHECK (normal_balance IN ('DEBIT', 'CREDIT')),
    CONSTRAINT ck_coa_class_normal CHECK (
        (account_class IN ('ASSET', 'EXPENSE') AND normal_balance = 'DEBIT')
        OR (account_class IN ('LIABILITY', 'EQUITY') AND normal_balance = 'CREDIT')
        OR (account_class = 'REVENUE' AND (
            (NOT is_contra AND normal_balance = 'CREDIT')
            OR (is_contra AND normal_balance = 'DEBIT')
        ))
    ),
    CONSTRAINT uq_coa_attrs UNIQUE (code, account_class, normal_balance, allow_negative)
);

INSERT INTO ledger.chart_of_accounts
    (code, name, account_class, normal_balance, allow_negative, is_contra, in_mvp)
VALUES
    ('1100', 'Внешний источник виртуальных средств (симулятор)', 'ASSET',      'DEBIT',  false, false, true),
    ('1200', 'Расчёты с платёжным провайдером',                  'ASSET',      'DEBIT',  false, false, false),
    ('1300', 'Задолженность магазинов',                          'ASSET',      'DEBIT',  false, false, false),
    ('2100', 'Средства клиента — доступно',                      'LIABILITY',  'CREDIT', false, false, true),
    ('2110', 'Средства клиента — зарезервировано',               'LIABILITY',  'CREDIT', false, false, true),
    ('2200', 'Средства магазина — доступно',                     'LIABILITY',  'CREDIT', false, false, true),
    ('2210', 'Средства магазина — зарезервировано',              'LIABILITY',  'CREDIT', false, false, false),
    ('2300', 'Выплаты в пути',                                   'LIABILITY',  'CREDIT', false, false, false),
    ('2900', 'Невыясненные суммы (suspense)',                    'LIABILITY',  'CREDIT', true,  false, false),
    ('3100', 'Капитал (демо-эмиссия)',                           'EQUITY',     'CREDIT', false, false, false),
    ('4100', 'Комиссионный доход — переводы',                    'REVENUE',    'CREDIT', false, false, true),
    ('4200', 'Комиссионный доход — платежи',                     'REVENUE',    'CREDIT', false, false, true),
    ('4900', 'Возвращённые комиссии (контр-доход)',              'REVENUE',    'DEBIT',  false, true,  false),
    ('5100', 'Комиссии провайдера',                              'EXPENSE',    'DEBIT',  false, false, false),
    ('5900', 'Операционные потери и компенсации',                'EXPENSE',    'DEBIT',  false, false, true);

CREATE TABLE ledger.ledger_accounts (
    id               UUID PRIMARY KEY,
    owner_type       TEXT NOT NULL,
    account_id       UUID,
    account_type     TEXT NOT NULL,
    kind             TEXT,
    system_code      TEXT,
    currency         CHAR(3) NOT NULL REFERENCES accounts.currencies (code),
    account_class    TEXT NOT NULL,
    normal_balance   TEXT NOT NULL,
    allow_negative   BOOLEAN NOT NULL,
    bucket_count     SMALLINT NOT NULL,
    status           TEXT NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT uq_ledger_accounts_id_currency UNIQUE (id, currency),
    CONSTRAINT fk_ledger_accounts_business
        FOREIGN KEY (account_id, currency) REFERENCES accounts.accounts (id, currency),
    CONSTRAINT ck_ledger_accounts_owner CHECK (owner_type IN ('CUSTOMER', 'MERCHANT', 'SYSTEM')),
    CONSTRAINT ck_ledger_accounts_kind CHECK (kind IS NULL OR kind IN ('AVAILABLE', 'HOLD')),
    CONSTRAINT ck_ledger_accounts_system_shape CHECK (
        (owner_type = 'SYSTEM'
            AND account_id IS NULL
            AND system_code IS NOT NULL
            AND kind IS NULL)
        OR (owner_type IN ('CUSTOMER', 'MERCHANT')
            AND account_id IS NOT NULL
            AND system_code IS NULL
            AND kind IN ('AVAILABLE', 'HOLD'))
    ),
    CONSTRAINT ck_ledger_accounts_system_code CHECK (
        system_code IS NULL OR system_code IN (
            'SIM_FUNDING_SOURCE',
            'FEE_INCOME_TRANSFERS',
            'FEE_INCOME_PAYMENTS',
            'OPERATIONAL_LOSSES'
        )
    ),
    CONSTRAINT ck_ledger_accounts_class CHECK (account_class IN ('ASSET', 'LIABILITY', 'EQUITY', 'REVENUE', 'EXPENSE')),
    CONSTRAINT ck_ledger_accounts_normal CHECK (normal_balance IN ('DEBIT', 'CREDIT')),
    CONSTRAINT ck_ledger_accounts_class_normal CHECK (
        (account_class IN ('ASSET', 'EXPENSE') AND normal_balance = 'DEBIT')
        OR (account_class IN ('LIABILITY', 'EQUITY') AND normal_balance = 'CREDIT')
        OR (account_class = 'REVENUE' AND normal_balance IN ('DEBIT', 'CREDIT'))
    ),
    CONSTRAINT ck_ledger_accounts_bucket_count CHECK (bucket_count BETWEEN 1 AND 256),
    CONSTRAINT ck_ledger_accounts_user_buckets CHECK (owner_type = 'SYSTEM' OR bucket_count = 1),
    CONSTRAINT ck_ledger_accounts_status CHECK (status IN ('ACTIVE', 'CLOSED')),
    CONSTRAINT ck_ledger_accounts_user_active CHECK (owner_type = 'SYSTEM' OR status = 'ACTIVE'),
    CONSTRAINT uq_ledger_accounts_bucket_parent UNIQUE (id, owner_type, allow_negative, bucket_count),
    CONSTRAINT fk_ledger_accounts_chart_attrs
        FOREIGN KEY (account_type, account_class, normal_balance, allow_negative)
        REFERENCES ledger.chart_of_accounts (code, account_class, normal_balance, allow_negative)
);

CREATE UNIQUE INDEX uq_ledger_accounts_business_kind
    ON ledger.ledger_accounts (account_id, kind)
    WHERE account_id IS NOT NULL;

CREATE UNIQUE INDEX uq_ledger_accounts_system_code_currency
    ON ledger.ledger_accounts (system_code, currency)
    WHERE owner_type = 'SYSTEM';

CREATE TABLE ledger.account_balances (
    account_id         UUID PRIMARY KEY,
    currency           CHAR(3) NOT NULL,
    available_amount   BIGINT NOT NULL DEFAULT 0,
    reserved_amount    BIGINT NOT NULL DEFAULT 0,
    version            BIGINT NOT NULL DEFAULT 0,
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT fk_account_balances_account
        FOREIGN KEY (account_id, currency) REFERENCES accounts.accounts (id, currency),
    CONSTRAINT ck_account_balances_available CHECK (available_amount >= 0),
    CONSTRAINT ck_account_balances_reserved CHECK (reserved_amount >= 0),
    CONSTRAINT ck_account_balances_version CHECK (version >= 0)
);

-- Parent attributes are copied onto the bucket row and pinned by a composite
-- FK so allow_negative / bucket_count / SYSTEM cannot drift. bucket_no <
-- bucket_count is a CHECK on the copy. A trigger cannot do this better:
-- PostgreSQL FKs cannot target a partial unique index on SYSTEM ids only.
CREATE TABLE ledger.system_balance_buckets (
    ledger_account_id  UUID NOT NULL,
    bucket_no          SMALLINT NOT NULL,
    balance            BIGINT NOT NULL DEFAULT 0,
    owner_type         TEXT NOT NULL,
    allow_negative     BOOLEAN NOT NULL,
    bucket_count       SMALLINT NOT NULL,
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (ledger_account_id, bucket_no),
    CONSTRAINT fk_buckets_parent
        FOREIGN KEY (ledger_account_id, owner_type, allow_negative, bucket_count)
        REFERENCES ledger.ledger_accounts (id, owner_type, allow_negative, bucket_count),
    CONSTRAINT ck_system_balance_buckets_system CHECK (owner_type = 'SYSTEM'),
    CONSTRAINT ck_system_balance_buckets_no CHECK (bucket_no >= 0 AND bucket_no < bucket_count),
    CONSTRAINT ck_system_balance_buckets_sign CHECK (allow_negative OR balance >= 0)
);

CREATE TABLE ledger.journal_transactions (
    id                   UUID PRIMARY KEY,
    operation            TEXT NOT NULL,
    reference_type       TEXT NOT NULL,
    reference_id         UUID NOT NULL,
    operation_seq        SMALLINT NOT NULL DEFAULT 1,
    reverses_journal_id  UUID REFERENCES ledger.journal_transactions (id),
    status               TEXT NOT NULL,
    description          TEXT,
    initiated_by         UUID,
    accounting_date      DATE NOT NULL,
    posted_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT uq_journal_business_key UNIQUE (reference_type, reference_id, operation, operation_seq),
    CONSTRAINT ck_journal_operation CHECK (operation IN (
        'SIM_TOPUP', 'TRANSFER', 'HOLD_PLACE', 'HOLD_CAPTURE', 'HOLD_RELEASE',
        'REFUND', 'PAYOUT', 'REVERSAL', 'ADJUSTMENT'
    )),
    CONSTRAINT ck_journal_reference_type CHECK (reference_type IN (
        'SIM_TOPUP', 'TRANSFER', 'HOLD', 'REFUND', 'PAYOUT', 'ADJUSTMENT'
    )),
    CONSTRAINT ck_journal_status CHECK (status = 'POSTED'),
    CONSTRAINT ck_journal_reversal CHECK ((operation = 'REVERSAL') = (reverses_journal_id IS NOT NULL)),
    CONSTRAINT ck_journal_operation_seq CHECK (operation_seq >= 1)
);

CREATE UNIQUE INDEX uq_journal_reverses
    ON ledger.journal_transactions (reverses_journal_id)
    WHERE reverses_journal_id IS NOT NULL;

CREATE INDEX ix_journal_posted_at_brin
    ON ledger.journal_transactions USING BRIN (posted_at);

CREATE TABLE ledger.ledger_entries (
    id                      UUID PRIMARY KEY,
    journal_transaction_id  UUID NOT NULL REFERENCES ledger.journal_transactions (id),
    ledger_account_id       UUID NOT NULL,
    entry_type              TEXT NOT NULL,
    amount                  BIGINT NOT NULL,
    currency                CHAR(3) NOT NULL,
    account_version         BIGINT,
    balance_after           BIGINT,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT fk_entries_account_currency
        FOREIGN KEY (ledger_account_id, currency) REFERENCES ledger.ledger_accounts (id, currency),
    CONSTRAINT uq_entries_journal_account UNIQUE (journal_transaction_id, ledger_account_id),
    CONSTRAINT ck_entries_type CHECK (entry_type IN ('DEBIT', 'CREDIT')),
    CONSTRAINT ck_entries_amount CHECK (amount > 0 AND amount <= 100000000000000),
    CONSTRAINT ck_entries_version_balance CHECK ((account_version IS NULL) = (balance_after IS NULL))
);

CREATE UNIQUE INDEX uq_entries_account_version
    ON ledger.ledger_entries (ledger_account_id, account_version)
    WHERE account_version IS NOT NULL;

CREATE INDEX ix_entries_account_created
    ON ledger.ledger_entries (ledger_account_id, created_at, id);

CREATE TABLE ledger.ledger_balance_snapshots (
    ledger_account_id  UUID NOT NULL REFERENCES ledger.ledger_accounts (id),
    snapshot_date      DATE NOT NULL,
    debit_total        BIGINT NOT NULL,
    credit_total       BIGINT NOT NULL,
    balance            BIGINT NOT NULL,
    computed_at        TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (ledger_account_id, snapshot_date)
);

CREATE TABLE ledger.holds (
    id                  UUID PRIMARY KEY,
    account_id          UUID NOT NULL,
    currency            CHAR(3) NOT NULL,
    amount              BIGINT NOT NULL,
    status              TEXT NOT NULL,
    reference_type      TEXT NOT NULL,
    reference_id        UUID NOT NULL,
    limit_period_start  DATE,
    expires_at          TIMESTAMPTZ NOT NULL,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    finalized_at        TIMESTAMPTZ,
    CONSTRAINT fk_holds_account_currency
        FOREIGN KEY (account_id, currency) REFERENCES accounts.accounts (id, currency),
    CONSTRAINT uq_holds_reference UNIQUE (reference_type, reference_id),
    CONSTRAINT ck_holds_amount CHECK (amount > 0),
    CONSTRAINT ck_holds_status CHECK (status IN ('ACTIVE', 'CAPTURED', 'RELEASED', 'EXPIRED')),
    CONSTRAINT ck_holds_reference_type CHECK (reference_type = 'PAYMENT'),
    CONSTRAINT ck_holds_finalized CHECK (
        (status = 'ACTIVE' AND finalized_at IS NULL)
        OR (status <> 'ACTIVE' AND finalized_at IS NOT NULL)
    )
);

CREATE INDEX ix_holds_account_active
    ON ledger.holds (account_id)
    WHERE status = 'ACTIVE';

CREATE INDEX ix_holds_expires_active
    ON ledger.holds (expires_at)
    WHERE status = 'ACTIVE';

CREATE TABLE ledger.manual_adjustments (
    id                      UUID PRIMARY KEY,
    status                  TEXT NOT NULL,
    proposed_entries        JSONB NOT NULL,
    reason                  TEXT NOT NULL,
    incident_ref            TEXT NOT NULL,
    created_by              UUID NOT NULL,
    approved_by             UUID,
    journal_transaction_id  UUID REFERENCES ledger.journal_transactions (id),
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    decided_at              TIMESTAMPTZ,
    CONSTRAINT ck_manual_adjustments_status CHECK (status IN (
        'DRAFT', 'PENDING_APPROVAL', 'APPROVED', 'REJECTED', 'POSTED'
    )),
    CONSTRAINT ck_manual_adjustments_maker_checker CHECK (
        approved_by IS NULL OR approved_by <> created_by
    ),
    CONSTRAINT ck_manual_adjustments_posted CHECK (
        (status = 'POSTED' AND journal_transaction_id IS NOT NULL)
        OR (status <> 'POSTED' AND journal_transaction_id IS NULL)
    )
);

-- Seed MVP system ledger accounts (fixed ids) and their buckets.
INSERT INTO ledger.ledger_accounts (
    id, owner_type, account_id, account_type, kind, system_code, currency,
    account_class, normal_balance, allow_negative, bucket_count, status
) VALUES
    ('00000000-0000-4000-8000-000000001100', 'SYSTEM', NULL, '1100', NULL,
        'SIM_FUNDING_SOURCE', 'RUB', 'ASSET', 'DEBIT', false, 16, 'ACTIVE'),
    ('00000000-0000-4000-8000-000000004100', 'SYSTEM', NULL, '4100', NULL,
        'FEE_INCOME_TRANSFERS', 'RUB', 'REVENUE', 'CREDIT', false, 16, 'ACTIVE'),
    ('00000000-0000-4000-8000-000000004200', 'SYSTEM', NULL, '4200', NULL,
        'FEE_INCOME_PAYMENTS', 'RUB', 'REVENUE', 'CREDIT', false, 16, 'ACTIVE'),
    ('00000000-0000-4000-8000-000000005900', 'SYSTEM', NULL, '5900', NULL,
        'OPERATIONAL_LOSSES', 'RUB', 'EXPENSE', 'DEBIT', false, 1, 'ACTIVE');

INSERT INTO ledger.system_balance_buckets (
    ledger_account_id, bucket_no, balance, owner_type, allow_negative, bucket_count, updated_at
)
SELECT a.id, g.n, 0, a.owner_type, a.allow_negative, a.bucket_count, now()
FROM ledger.ledger_accounts AS a
CROSS JOIN LATERAL generate_series(0, a.bucket_count - 1) AS g(n)
WHERE a.owner_type = 'SYSTEM';

-- +goose StatementBegin
CREATE FUNCTION ledger.forbid_mutation() RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog, ledger, pg_temp
AS $$
BEGIN
    RAISE EXCEPTION 'ledger is append-only: % on % is forbidden', TG_OP, TG_TABLE_NAME
        USING ERRCODE = 'P0001';
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER trg_entries_immutable
    BEFORE UPDATE OR DELETE ON ledger.ledger_entries
    FOR EACH ROW EXECUTE FUNCTION ledger.forbid_mutation();
CREATE TRIGGER trg_entries_no_truncate
    BEFORE TRUNCATE ON ledger.ledger_entries
    FOR EACH STATEMENT EXECUTE FUNCTION ledger.forbid_mutation();

CREATE TRIGGER trg_journal_immutable
    BEFORE UPDATE OR DELETE ON ledger.journal_transactions
    FOR EACH ROW EXECUTE FUNCTION ledger.forbid_mutation();
CREATE TRIGGER trg_journal_no_truncate
    BEFORE TRUNCATE ON ledger.journal_transactions
    FOR EACH STATEMENT EXECUTE FUNCTION ledger.forbid_mutation();

CREATE TRIGGER trg_account_status_history_immutable
    BEFORE UPDATE OR DELETE ON accounts.account_status_history
    FOR EACH ROW EXECUTE FUNCTION ledger.forbid_mutation();
CREATE TRIGGER trg_account_status_history_no_truncate
    BEFORE TRUNCATE ON accounts.account_status_history
    FOR EACH STATEMENT EXECUTE FUNCTION ledger.forbid_mutation();

-- +goose StatementBegin
CREATE FUNCTION ledger.check_journal_balanced() RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog, ledger, pg_temp
AS $$
DECLARE
    n integer;
BEGIN
    SELECT count(*) INTO n
    FROM ledger.ledger_entries
    WHERE journal_transaction_id = NEW.id;
    IF n < 2 THEN
        RAISE EXCEPTION 'journal must have at least 2 entries'
            USING ERRCODE = 'P0001';
    END IF;
    IF EXISTS (
        SELECT 1
        FROM ledger.ledger_entries
        WHERE journal_transaction_id = NEW.id
        GROUP BY currency
        HAVING sum(CASE WHEN entry_type = 'DEBIT' THEN amount ELSE 0 END)
             <> sum(CASE WHEN entry_type = 'CREDIT' THEN amount ELSE 0 END)
    ) THEN
        RAISE EXCEPTION 'journal is not balanced'
            USING ERRCODE = 'P0001';
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

CREATE CONSTRAINT TRIGGER trg_journal_balanced
    AFTER INSERT ON ledger.journal_transactions
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION ledger.check_journal_balanced();

REVOKE ALL ON FUNCTION ledger.forbid_mutation() FROM PUBLIC;
REVOKE ALL ON FUNCTION ledger.check_journal_balanced() FROM PUBLIC;

GRANT SELECT ON TABLE
    ledger.journal_transactions,
    ledger.ledger_entries,
    ledger.account_balances,
    ledger.system_balance_buckets,
    ledger.holds,
    ledger.ledger_accounts,
    ledger.chart_of_accounts,
    ledger.ledger_balance_snapshots,
    ledger.manual_adjustments
TO paycore_app, paycore_readonly;

GRANT SELECT, INSERT ON TABLE ledger.journal_transactions, ledger.ledger_entries TO paycore_ledger;
GRANT SELECT, INSERT ON TABLE ledger.ledger_accounts TO paycore_ledger;
GRANT SELECT, INSERT, UPDATE ON TABLE
    ledger.account_balances,
    ledger.system_balance_buckets,
    ledger.holds
TO paycore_ledger;
GRANT SELECT ON TABLE
    ledger.chart_of_accounts,
    ledger.ledger_balance_snapshots,
    ledger.manual_adjustments
TO paycore_ledger;

GRANT SELECT, INSERT, UPDATE ON TABLE ledger.manual_adjustments TO paycore_app;
