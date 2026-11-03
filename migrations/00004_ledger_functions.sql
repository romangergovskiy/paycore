-- +goose Up
-- SECURITY DEFINER ledger gateway. Ownership transfer is D-40.
-- No Goose Down: financial forward-only (D-38).

GRANT CREATE ON SCHEMA ledger TO paycore_ledger;

-- +goose StatementBegin
CREATE FUNCTION ledger.entry_status_allowed(
    p_operation text,
    p_owner_type text,
    p_kind text,
    p_entry_type text,
    p_biz_status text,
    p_adjustment_approved boolean
) RETURNS boolean
LANGUAGE plpgsql
IMMUTABLE
SET search_path = pg_catalog, ledger, pg_temp
AS $$
BEGIN
    IF p_owner_type = 'SYSTEM' THEN
        RETURN true;
    END IF;
    IF p_biz_status = 'CLOSED' THEN
        RETURN false;
    END IF;
    IF p_operation IN ('REVERSAL', 'ADJUSTMENT') THEN
        RETURN p_biz_status = 'ACTIVE'
            OR (p_biz_status = 'BLOCKED' AND p_adjustment_approved);
    END IF;
    IF p_operation = 'SIM_TOPUP' THEN
        RETURN p_owner_type = 'CUSTOMER'
            AND p_kind = 'AVAILABLE'
            AND p_entry_type = 'CREDIT'
            AND p_biz_status = 'ACTIVE';
    END IF;
    IF p_operation = 'TRANSFER' THEN
        RETURN p_owner_type = 'CUSTOMER'
            AND p_kind = 'AVAILABLE'
            AND p_entry_type IN ('DEBIT', 'CREDIT')
            AND p_biz_status = 'ACTIVE';
    END IF;
    IF p_operation = 'HOLD_PLACE' THEN
        RETURN p_owner_type = 'CUSTOMER'
            AND p_biz_status = 'ACTIVE'
            AND (
                (p_kind = 'AVAILABLE' AND p_entry_type = 'DEBIT')
                OR (p_kind = 'HOLD' AND p_entry_type = 'CREDIT')
            );
    END IF;
    IF p_operation = 'HOLD_CAPTURE' THEN
        RETURN p_biz_status = 'ACTIVE'
            AND (
                (p_owner_type = 'CUSTOMER' AND p_kind = 'HOLD' AND p_entry_type = 'DEBIT')
                OR (p_owner_type = 'MERCHANT' AND p_kind = 'AVAILABLE' AND p_entry_type = 'CREDIT')
            );
    END IF;
    IF p_operation = 'HOLD_RELEASE' THEN
        RETURN p_owner_type = 'CUSTOMER'
            AND p_biz_status IN ('ACTIVE', 'BLOCKED')
            AND (
                (p_kind = 'HOLD' AND p_entry_type = 'DEBIT')
                OR (p_kind = 'AVAILABLE' AND p_entry_type = 'CREDIT')
            );
    END IF;
    IF p_operation = 'REFUND' THEN
        RETURN (
            (p_owner_type = 'MERCHANT' AND p_kind = 'AVAILABLE' AND p_entry_type = 'DEBIT' AND p_biz_status = 'ACTIVE')
            OR (p_owner_type = 'CUSTOMER' AND p_kind = 'AVAILABLE' AND p_entry_type = 'CREDIT' AND p_biz_status IN ('ACTIVE', 'BLOCKED'))
        );
    END IF;
    IF p_operation = 'PAYOUT' THEN
        RETURN p_owner_type = 'MERCHANT'
            AND p_kind = 'AVAILABLE'
            AND p_entry_type = 'DEBIT'
            AND p_biz_status = 'ACTIVE';
    END IF;
    RETURN false;
END;
$$;
-- +goose StatementEnd

-- Forms follow money-flows.md / ledger-model.md §6–§8. ADJUSTMENT has no
-- single posting shape there; it may use MVP accounts only (not 2210, not
-- post-MVP CoA codes). HOLD_CAPTURE n=2 is fee=0 (D-27 fee < amount).
-- +goose StatementBegin
CREATE FUNCTION ledger.assert_journal_form(
    p_operation text,
    p_reference_type text,
    p_rows jsonb,
    p_hold_action jsonb
) RETURNS void
LANGUAGE plpgsql
IMMUTABLE
SET search_path = pg_catalog, ledger, pg_temp
AS $$
DECLARE
    v_n integer;
    v_hold record;
    v_debit record;
    v_credit record;
    v_fee record;
    v_other integer;
BEGIN
    v_n := jsonb_array_length(p_rows);

    IF p_operation = 'SIM_TOPUP' THEN
        IF p_reference_type <> 'SIM_TOPUP' OR v_n <> 2 THEN
            RAISE EXCEPTION 'INVALID_JOURNAL: invalid SIM_TOPUP form'
                USING ERRCODE = 'PC400';
        END IF;
        IF p_hold_action IS NOT NULL AND p_hold_action <> 'null'::jsonb THEN
            RAISE EXCEPTION 'INVALID_JOURNAL: hold_action is not valid for this operation'
                USING ERRCODE = 'PC400';
        END IF;
        SELECT count(*) INTO v_other
          FROM jsonb_to_recordset(p_rows) AS x(owner_type text, account_type text, kind text, entry_type text)
         WHERE NOT (
                (x.owner_type = 'SYSTEM' AND x.account_type = '1100' AND x.kind IS NULL AND x.entry_type = 'DEBIT')
             OR (x.owner_type = 'CUSTOMER' AND x.account_type = '2100' AND x.kind = 'AVAILABLE' AND x.entry_type = 'CREDIT')
         );
        IF v_other <> 0
           OR NOT EXISTS (
               SELECT 1 FROM jsonb_to_recordset(p_rows) AS x(owner_type text, account_type text, entry_type text)
                WHERE x.owner_type = 'SYSTEM' AND x.account_type = '1100' AND x.entry_type = 'DEBIT')
           OR NOT EXISTS (
               SELECT 1 FROM jsonb_to_recordset(p_rows) AS x(owner_type text, kind text, account_type text, entry_type text)
                WHERE x.owner_type = 'CUSTOMER' AND x.kind = 'AVAILABLE' AND x.account_type = '2100' AND x.entry_type = 'CREDIT')
        THEN
            RAISE EXCEPTION 'INVALID_JOURNAL: invalid SIM_TOPUP form'
                USING ERRCODE = 'PC400';
        END IF;
        RETURN;
    END IF;

    IF p_operation = 'TRANSFER' THEN
        IF p_reference_type <> 'TRANSFER' OR v_n NOT IN (2, 3) THEN
            RAISE EXCEPTION 'INVALID_JOURNAL: invalid TRANSFER form'
                USING ERRCODE = 'PC400';
        END IF;
        IF p_hold_action IS NOT NULL AND p_hold_action <> 'null'::jsonb THEN
            RAISE EXCEPTION 'INVALID_JOURNAL: hold_action is not valid for this operation'
                USING ERRCODE = 'PC400';
        END IF;
        SELECT count(*) INTO v_other
          FROM jsonb_to_recordset(p_rows) AS x(owner_type text, kind text, account_type text, entry_type text)
         WHERE x.owner_type = 'CUSTOMER' AND x.kind = 'AVAILABLE' AND x.account_type = '2100' AND x.entry_type = 'DEBIT';
        IF v_other <> 1 THEN
            RAISE EXCEPTION 'INVALID_JOURNAL: invalid TRANSFER form'
                USING ERRCODE = 'PC400';
        END IF;
        SELECT x.account_id, x.amount
          INTO v_debit
          FROM jsonb_to_recordset(p_rows) AS x(owner_type text, kind text, account_type text, entry_type text, account_id uuid, amount bigint)
         WHERE x.owner_type = 'CUSTOMER' AND x.kind = 'AVAILABLE' AND x.account_type = '2100' AND x.entry_type = 'DEBIT';
        SELECT count(*) INTO v_other
          FROM jsonb_to_recordset(p_rows) AS x(owner_type text, kind text, account_type text, entry_type text)
         WHERE x.owner_type = 'CUSTOMER' AND x.kind = 'AVAILABLE' AND x.account_type = '2100' AND x.entry_type = 'CREDIT';
        IF v_other <> 1 THEN
            RAISE EXCEPTION 'INVALID_JOURNAL: invalid TRANSFER form'
                USING ERRCODE = 'PC400';
        END IF;
        SELECT x.account_id, x.amount
          INTO v_credit
          FROM jsonb_to_recordset(p_rows) AS x(owner_type text, kind text, account_type text, entry_type text, account_id uuid, amount bigint)
         WHERE x.owner_type = 'CUSTOMER' AND x.kind = 'AVAILABLE' AND x.account_type = '2100' AND x.entry_type = 'CREDIT';
        IF v_debit.account_id IS NOT DISTINCT FROM v_credit.account_id THEN
            RAISE EXCEPTION 'INVALID_JOURNAL: invalid TRANSFER form'
                USING ERRCODE = 'PC400';
        END IF;
        SELECT count(*) INTO v_other
          FROM jsonb_to_recordset(p_rows) AS x(owner_type text, account_type text, entry_type text)
         WHERE x.owner_type = 'SYSTEM' AND x.account_type = '4100' AND x.entry_type = 'CREDIT';
        SELECT x.amount
          INTO v_fee
          FROM jsonb_to_recordset(p_rows) AS x(owner_type text, account_type text, entry_type text, amount bigint)
         WHERE x.owner_type = 'SYSTEM' AND x.account_type = '4100' AND x.entry_type = 'CREDIT';
        IF v_n = 2 THEN
            IF v_other <> 0 OR v_debit.amount <> v_credit.amount THEN
                RAISE EXCEPTION 'INVALID_JOURNAL: invalid TRANSFER form'
                    USING ERRCODE = 'PC400';
            END IF;
        ELSE
            IF v_other <> 1 OR v_fee.amount IS NULL OR v_fee.amount <= 0
               OR v_debit.amount <> v_credit.amount + v_fee.amount THEN
                RAISE EXCEPTION 'INVALID_JOURNAL: invalid TRANSFER form'
                    USING ERRCODE = 'PC400';
            END IF;
        END IF;
        SELECT count(*) INTO v_other
          FROM jsonb_to_recordset(p_rows) AS x(owner_type text, kind text, account_type text, entry_type text)
         WHERE NOT (
                (x.owner_type = 'CUSTOMER' AND x.kind = 'AVAILABLE' AND x.account_type = '2100' AND x.entry_type IN ('DEBIT', 'CREDIT'))
             OR (x.owner_type = 'SYSTEM' AND x.account_type = '4100' AND x.entry_type = 'CREDIT')
         );
        IF v_other <> 0 THEN
            RAISE EXCEPTION 'INVALID_JOURNAL: invalid TRANSFER form'
                USING ERRCODE = 'PC400';
        END IF;
        RETURN;
    END IF;

    IF p_operation IN ('HOLD_PLACE', 'HOLD_CAPTURE', 'HOLD_RELEASE') THEN
        IF p_reference_type <> 'HOLD' THEN
            RAISE EXCEPTION 'INVALID_JOURNAL: invalid % form', p_operation
                USING ERRCODE = 'PC400';
        END IF;
        IF p_hold_action IS NULL OR jsonb_typeof(p_hold_action) <> 'object' THEN
            RAISE EXCEPTION 'INVALID_JOURNAL: hold_action is required'
                USING ERRCODE = 'PC400';
        END IF;
        SELECT x.action, x.hold_id, x.account_id, x.amount, x.currency, x.expires_at,
               x.reference_type, x.reference_id
          INTO v_hold
          FROM jsonb_to_record(p_hold_action) AS x(
              action text, hold_id uuid, account_id uuid, amount bigint, currency text,
              expires_at timestamptz, reference_type text, reference_id uuid);
        IF v_hold.hold_id IS NULL OR v_hold.account_id IS NULL OR v_hold.action IS NULL
           OR v_hold.amount IS NULL OR v_hold.currency IS NULL THEN
            RAISE EXCEPTION 'INVALID_JOURNAL: hold_action fields are required'
                USING ERRCODE = 'PC400';
        END IF;
    END IF;

    IF p_operation = 'HOLD_PLACE' THEN
        IF v_hold.action <> 'PLACE' OR v_n <> 2 OR v_hold.expires_at IS NULL
           OR v_hold.reference_type IS DISTINCT FROM 'PAYMENT' OR v_hold.reference_id IS NULL THEN
            RAISE EXCEPTION 'INVALID_JOURNAL: invalid HOLD_PLACE form'
                USING ERRCODE = 'PC400';
        END IF;
        SELECT count(*) INTO v_other
          FROM jsonb_to_recordset(p_rows) AS x(owner_type text, kind text, account_type text, entry_type text)
         WHERE x.owner_type = 'CUSTOMER' AND x.kind = 'AVAILABLE' AND x.account_type = '2100' AND x.entry_type = 'DEBIT';
        IF v_other <> 1 THEN
            RAISE EXCEPTION 'INVALID_JOURNAL: invalid HOLD_PLACE form'
                USING ERRCODE = 'PC400';
        END IF;
        SELECT x.account_id, x.amount, x.currency
          INTO v_debit
          FROM jsonb_to_recordset(p_rows) AS x(owner_type text, kind text, account_type text, entry_type text, account_id uuid, amount bigint, currency text)
         WHERE x.owner_type = 'CUSTOMER' AND x.kind = 'AVAILABLE' AND x.account_type = '2100' AND x.entry_type = 'DEBIT';
        SELECT count(*) INTO v_other
          FROM jsonb_to_recordset(p_rows) AS x(owner_type text, kind text, account_type text, entry_type text)
         WHERE x.owner_type = 'CUSTOMER' AND x.kind = 'HOLD' AND x.account_type = '2110' AND x.entry_type = 'CREDIT';
        IF v_other <> 1 THEN
            RAISE EXCEPTION 'INVALID_JOURNAL: invalid HOLD_PLACE form'
                USING ERRCODE = 'PC400';
        END IF;
        SELECT x.account_id, x.amount, x.currency
          INTO v_credit
          FROM jsonb_to_recordset(p_rows) AS x(owner_type text, kind text, account_type text, entry_type text, account_id uuid, amount bigint, currency text)
         WHERE x.owner_type = 'CUSTOMER' AND x.kind = 'HOLD' AND x.account_type = '2110' AND x.entry_type = 'CREDIT';
        IF v_debit.account_id IS DISTINCT FROM v_credit.account_id
           OR v_debit.account_id IS DISTINCT FROM v_hold.account_id
           OR v_debit.amount IS DISTINCT FROM v_credit.amount
           OR v_debit.amount IS DISTINCT FROM v_hold.amount
           OR v_debit.currency IS DISTINCT FROM v_hold.currency
           OR v_credit.currency IS DISTINCT FROM v_hold.currency THEN
            RAISE EXCEPTION 'INVALID_JOURNAL: HOLD_PLACE hold_action does not match entries'
                USING ERRCODE = 'PC400';
        END IF;
        RETURN;
    END IF;

    IF p_operation = 'HOLD_RELEASE' THEN
        IF v_hold.action NOT IN ('RELEASE', 'EXPIRE') OR v_n <> 2 THEN
            RAISE EXCEPTION 'INVALID_JOURNAL: invalid HOLD_RELEASE form'
                USING ERRCODE = 'PC400';
        END IF;
        SELECT count(*) INTO v_other
          FROM jsonb_to_recordset(p_rows) AS x(owner_type text, kind text, account_type text, entry_type text)
         WHERE x.owner_type = 'CUSTOMER' AND x.kind = 'HOLD' AND x.account_type = '2110' AND x.entry_type = 'DEBIT';
        IF v_other <> 1 THEN
            RAISE EXCEPTION 'INVALID_JOURNAL: invalid HOLD_RELEASE form'
                USING ERRCODE = 'PC400';
        END IF;
        SELECT x.account_id, x.amount, x.currency
          INTO v_debit
          FROM jsonb_to_recordset(p_rows) AS x(owner_type text, kind text, account_type text, entry_type text, account_id uuid, amount bigint, currency text)
         WHERE x.owner_type = 'CUSTOMER' AND x.kind = 'HOLD' AND x.account_type = '2110' AND x.entry_type = 'DEBIT';
        SELECT count(*) INTO v_other
          FROM jsonb_to_recordset(p_rows) AS x(owner_type text, kind text, account_type text, entry_type text)
         WHERE x.owner_type = 'CUSTOMER' AND x.kind = 'AVAILABLE' AND x.account_type = '2100' AND x.entry_type = 'CREDIT';
        IF v_other <> 1 THEN
            RAISE EXCEPTION 'INVALID_JOURNAL: invalid HOLD_RELEASE form'
                USING ERRCODE = 'PC400';
        END IF;
        SELECT x.account_id, x.amount, x.currency
          INTO v_credit
          FROM jsonb_to_recordset(p_rows) AS x(owner_type text, kind text, account_type text, entry_type text, account_id uuid, amount bigint, currency text)
         WHERE x.owner_type = 'CUSTOMER' AND x.kind = 'AVAILABLE' AND x.account_type = '2100' AND x.entry_type = 'CREDIT';
        IF v_debit.account_id IS DISTINCT FROM v_credit.account_id
           OR v_debit.account_id IS DISTINCT FROM v_hold.account_id
           OR v_debit.amount IS DISTINCT FROM v_credit.amount
           OR v_debit.amount IS DISTINCT FROM v_hold.amount
           OR v_debit.currency IS DISTINCT FROM v_hold.currency
           OR v_credit.currency IS DISTINCT FROM v_hold.currency THEN
            RAISE EXCEPTION 'INVALID_JOURNAL: HOLD_RELEASE must match the hold account, amount and currency'
                USING ERRCODE = 'PC400';
        END IF;
        RETURN;
    END IF;

    IF p_operation = 'HOLD_CAPTURE' THEN
        IF v_hold.action <> 'CAPTURE' OR v_n NOT IN (2, 3) THEN
            RAISE EXCEPTION 'INVALID_JOURNAL: invalid HOLD_CAPTURE form'
                USING ERRCODE = 'PC400';
        END IF;
        SELECT count(*) INTO v_other
          FROM jsonb_to_recordset(p_rows) AS x(owner_type text, kind text, account_type text, entry_type text)
         WHERE x.owner_type = 'CUSTOMER' AND x.kind = 'HOLD' AND x.account_type = '2110' AND x.entry_type = 'DEBIT';
        IF v_other <> 1 THEN
            RAISE EXCEPTION 'INVALID_JOURNAL: invalid HOLD_CAPTURE form'
                USING ERRCODE = 'PC400';
        END IF;
        SELECT x.account_id, x.amount, x.currency
          INTO v_debit
          FROM jsonb_to_recordset(p_rows) AS x(owner_type text, kind text, account_type text, entry_type text, account_id uuid, amount bigint, currency text)
         WHERE x.owner_type = 'CUSTOMER' AND x.kind = 'HOLD' AND x.account_type = '2110' AND x.entry_type = 'DEBIT';
        SELECT count(*) INTO v_other
          FROM jsonb_to_recordset(p_rows) AS x(owner_type text, kind text, account_type text, entry_type text)
         WHERE x.owner_type = 'MERCHANT' AND x.kind = 'AVAILABLE' AND x.account_type = '2200' AND x.entry_type = 'CREDIT';
        IF v_other <> 1 THEN
            RAISE EXCEPTION 'INVALID_JOURNAL: invalid HOLD_CAPTURE form'
                USING ERRCODE = 'PC400';
        END IF;
        SELECT x.account_id, x.amount
          INTO v_credit
          FROM jsonb_to_recordset(p_rows) AS x(owner_type text, kind text, account_type text, entry_type text, account_id uuid, amount bigint)
         WHERE x.owner_type = 'MERCHANT' AND x.kind = 'AVAILABLE' AND x.account_type = '2200' AND x.entry_type = 'CREDIT';
        SELECT count(*) INTO v_other
          FROM jsonb_to_recordset(p_rows) AS x(owner_type text, account_type text, entry_type text)
         WHERE x.owner_type = 'SYSTEM' AND x.account_type = '4200' AND x.entry_type = 'CREDIT';
        SELECT x.amount
          INTO v_fee
          FROM jsonb_to_recordset(p_rows) AS x(owner_type text, account_type text, entry_type text, amount bigint)
         WHERE x.owner_type = 'SYSTEM' AND x.account_type = '4200' AND x.entry_type = 'CREDIT';
        -- fee = 0 is allowed by D-27 (fee < amount, amount > 0). money-flows.md
        -- payment examples use fee > 0 (3 entries). The 2-entry form is
        -- Dt 2110 / Ct 2200 full amount, no 4200 — same pattern as TRANSFER
        -- fee = 0 (money-flows §4.1). Not a new decision.
        IF v_n = 2 THEN
            IF v_other <> 0 OR v_debit.amount <> v_credit.amount THEN
                RAISE EXCEPTION 'INVALID_JOURNAL: invalid HOLD_CAPTURE form'
                    USING ERRCODE = 'PC400';
            END IF;
        ELSE
            IF v_other <> 1 OR v_fee.amount IS NULL OR v_fee.amount <= 0
               OR v_fee.amount >= v_debit.amount
               OR v_debit.amount <> v_credit.amount + v_fee.amount THEN
                RAISE EXCEPTION 'INVALID_JOURNAL: invalid HOLD_CAPTURE form'
                    USING ERRCODE = 'PC400';
            END IF;
        END IF;
        IF v_debit.account_id IS DISTINCT FROM v_hold.account_id
           OR v_debit.amount IS DISTINCT FROM v_hold.amount
           OR v_debit.currency IS DISTINCT FROM v_hold.currency THEN
            RAISE EXCEPTION 'INVALID_JOURNAL: HOLD_CAPTURE cannot move a hold of another account'
                USING ERRCODE = 'PC400';
        END IF;
        SELECT count(*) INTO v_other
          FROM jsonb_to_recordset(p_rows) AS x(owner_type text, kind text, account_type text, entry_type text)
         WHERE NOT (
                (x.owner_type = 'CUSTOMER' AND x.kind = 'HOLD' AND x.account_type = '2110' AND x.entry_type = 'DEBIT')
             OR (x.owner_type = 'MERCHANT' AND x.kind = 'AVAILABLE' AND x.account_type = '2200' AND x.entry_type = 'CREDIT')
             OR (x.owner_type = 'SYSTEM' AND x.account_type = '4200' AND x.entry_type = 'CREDIT')
         );
        IF v_other <> 0 THEN
            RAISE EXCEPTION 'INVALID_JOURNAL: invalid HOLD_CAPTURE form'
                USING ERRCODE = 'PC400';
        END IF;
        RETURN;
    END IF;

    IF p_operation = 'REFUND' THEN
        IF p_reference_type <> 'REFUND' OR v_n <> 2 THEN
            RAISE EXCEPTION 'INVALID_JOURNAL: invalid REFUND form'
                USING ERRCODE = 'PC400';
        END IF;
        IF p_hold_action IS NOT NULL AND p_hold_action <> 'null'::jsonb THEN
            RAISE EXCEPTION 'INVALID_JOURNAL: hold_action is not valid for this operation'
                USING ERRCODE = 'PC400';
        END IF;
        SELECT count(*) INTO v_other
          FROM jsonb_to_recordset(p_rows) AS x(owner_type text, kind text, account_type text, entry_type text)
         WHERE NOT (
                (x.owner_type = 'MERCHANT' AND x.kind = 'AVAILABLE' AND x.account_type = '2200' AND x.entry_type = 'DEBIT')
             OR (x.owner_type = 'CUSTOMER' AND x.kind = 'AVAILABLE' AND x.account_type = '2100' AND x.entry_type = 'CREDIT')
         );
        IF v_other <> 0
           OR (SELECT count(*) FROM jsonb_to_recordset(p_rows) AS x(entry_type text) WHERE x.entry_type = 'DEBIT') <> 1 THEN
            RAISE EXCEPTION 'INVALID_JOURNAL: invalid REFUND form'
                USING ERRCODE = 'PC400';
        END IF;
        RETURN;
    END IF;

    IF p_operation = 'PAYOUT' THEN
        IF p_reference_type <> 'PAYOUT' OR v_n <> 2 THEN
            RAISE EXCEPTION 'INVALID_JOURNAL: invalid PAYOUT form'
                USING ERRCODE = 'PC400';
        END IF;
        IF p_hold_action IS NOT NULL AND p_hold_action <> 'null'::jsonb THEN
            RAISE EXCEPTION 'INVALID_JOURNAL: hold_action is not valid for this operation'
                USING ERRCODE = 'PC400';
        END IF;
        SELECT count(*) INTO v_other
          FROM jsonb_to_recordset(p_rows) AS x(owner_type text, kind text, account_type text, entry_type text)
         WHERE NOT (
                (x.owner_type = 'MERCHANT' AND x.kind = 'AVAILABLE' AND x.account_type = '2200' AND x.entry_type = 'DEBIT')
             OR (x.owner_type = 'SYSTEM' AND x.account_type = '1100' AND x.kind IS NULL AND x.entry_type = 'CREDIT')
         );
        IF v_other <> 0 THEN
            RAISE EXCEPTION 'INVALID_JOURNAL: invalid PAYOUT form'
                USING ERRCODE = 'PC400';
        END IF;
        RETURN;
    END IF;

    IF p_operation = 'ADJUSTMENT' THEN
        IF p_reference_type <> 'ADJUSTMENT' OR v_n < 2 THEN
            RAISE EXCEPTION 'INVALID_JOURNAL: invalid ADJUSTMENT form'
                USING ERRCODE = 'PC400';
        END IF;
        IF p_hold_action IS NOT NULL AND p_hold_action <> 'null'::jsonb THEN
            RAISE EXCEPTION 'INVALID_JOURNAL: hold_action is not valid for this operation'
                USING ERRCODE = 'PC400';
        END IF;
        SELECT count(*) INTO v_other
          FROM jsonb_to_recordset(p_rows) AS x(owner_type text, kind text, account_type text)
         WHERE NOT (
                (x.owner_type = 'CUSTOMER' AND x.kind IN ('AVAILABLE', 'HOLD') AND x.account_type IN ('2100', '2110'))
             OR (x.owner_type = 'MERCHANT' AND x.kind = 'AVAILABLE' AND x.account_type = '2200')
             OR (x.owner_type = 'SYSTEM' AND x.account_type IN ('1100', '4100', '4200', '5900'))
         );
        IF v_other <> 0 THEN
            RAISE EXCEPTION 'INVALID_JOURNAL: ADJUSTMENT uses non-MVP accounts'
                USING ERRCODE = 'PC400';
        END IF;
        RETURN;
    END IF;

    IF p_operation = 'REVERSAL' THEN
        IF p_hold_action IS NOT NULL AND p_hold_action <> 'null'::jsonb THEN
            RAISE EXCEPTION 'INVALID_JOURNAL: hold_action is not valid for this operation'
                USING ERRCODE = 'PC400';
        END IF;
        RETURN;
    END IF;

    RAISE EXCEPTION 'INVALID_JOURNAL: unknown operation'
        USING ERRCODE = 'PC400';
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION ledger.open_ledger_accounts(
    p_account_id uuid,
    p_owner_type text,
    p_currency char(3)
) RETURNS TABLE (
    kind text,
    ledger_account_id uuid
)
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, ledger, pg_temp
AS $$
DECLARE
    v_user_id uuid;
    v_merchant_id uuid;
    v_currency char(3);
    v_avail_code text;
    v_hold_code text;
    v_class text;
    v_normal text;
    v_allow boolean;
    v_avail_id uuid;
    v_hold_id uuid;
BEGIN
    IF p_account_id IS NULL OR p_owner_type IS NULL OR p_currency IS NULL THEN
        RAISE EXCEPTION 'INVALID_JOURNAL: open_ledger_accounts arguments are required'
            USING ERRCODE = 'PC400';
    END IF;
    IF p_owner_type NOT IN ('CUSTOMER', 'MERCHANT') THEN
        RAISE EXCEPTION 'INVALID_JOURNAL: owner_type must be CUSTOMER or MERCHANT'
            USING ERRCODE = 'PC400';
    END IF;

    SELECT a.user_id, a.merchant_id, a.currency
      INTO v_user_id, v_merchant_id, v_currency
      FROM accounts.accounts AS a
     WHERE a.id = p_account_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'INVALID_JOURNAL: account % not found', p_account_id
            USING ERRCODE = 'PC400';
    END IF;
    IF v_currency IS DISTINCT FROM p_currency THEN
        RAISE EXCEPTION 'INVALID_JOURNAL: currency mismatch'
            USING ERRCODE = 'PC400';
    END IF;
    IF p_owner_type = 'CUSTOMER' AND v_user_id IS NULL THEN
        RAISE EXCEPTION 'INVALID_JOURNAL: account is not CUSTOMER'
            USING ERRCODE = 'PC400';
    END IF;
    IF p_owner_type = 'MERCHANT' AND v_merchant_id IS NULL THEN
        RAISE EXCEPTION 'INVALID_JOURNAL: account is not MERCHANT'
            USING ERRCODE = 'PC400';
    END IF;

    IF EXISTS (
        SELECT 1 FROM ledger.ledger_accounts AS la WHERE la.account_id = p_account_id
    ) THEN
        RETURN QUERY
        SELECT la.kind, la.id
          FROM ledger.ledger_accounts AS la
         WHERE la.account_id = p_account_id
         ORDER BY la.kind;
        RETURN;
    END IF;

    IF p_owner_type = 'CUSTOMER' THEN
        v_avail_code := '2100';
        v_hold_code := '2110';
    ELSE
        v_avail_code := '2200';
        v_hold_code := '2210';
    END IF;

    SELECT c.account_class, c.normal_balance, c.allow_negative
      INTO STRICT v_class, v_normal, v_allow
      FROM ledger.chart_of_accounts AS c
     WHERE c.code = v_avail_code;

    v_avail_id := gen_random_uuid();
    INSERT INTO ledger.ledger_accounts (
        id, owner_type, account_id, account_type, kind, system_code, currency,
        account_class, normal_balance, allow_negative, bucket_count, status
    ) VALUES (
        v_avail_id, p_owner_type, p_account_id, v_avail_code, 'AVAILABLE', NULL, p_currency,
        v_class, v_normal, v_allow, 1, 'ACTIVE'
    );

    SELECT c.account_class, c.normal_balance, c.allow_negative
      INTO STRICT v_class, v_normal, v_allow
      FROM ledger.chart_of_accounts AS c
     WHERE c.code = v_hold_code;

    v_hold_id := gen_random_uuid();
    INSERT INTO ledger.ledger_accounts (
        id, owner_type, account_id, account_type, kind, system_code, currency,
        account_class, normal_balance, allow_negative, bucket_count, status
    ) VALUES (
        v_hold_id, p_owner_type, p_account_id, v_hold_code, 'HOLD', NULL, p_currency,
        v_class, v_normal, v_allow, 1, 'ACTIVE'
    );

    INSERT INTO ledger.account_balances (account_id, currency, available_amount, reserved_amount, version)
    VALUES (p_account_id, p_currency, 0, 0, 0);

    RETURN QUERY
    SELECT la.kind, la.id
      FROM ledger.ledger_accounts AS la
     WHERE la.account_id = p_account_id
     ORDER BY la.kind;
EXCEPTION
    WHEN unique_violation THEN
        RETURN QUERY
        SELECT la.kind, la.id
          FROM ledger.ledger_accounts AS la
         WHERE la.account_id = p_account_id
         ORDER BY la.kind;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION ledger.lock_account_balances(p_account_ids uuid[])
RETURNS TABLE (
    account_id uuid,
    currency char(3),
    available_amount bigint,
    reserved_amount bigint,
    version bigint
)
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, ledger, pg_temp
AS $$
DECLARE
    v_want integer;
    v_got integer;
BEGIN
    IF p_account_ids IS NULL THEN
        RETURN;
    END IF;
    SELECT count(DISTINCT x) INTO v_want
      FROM unnest(p_account_ids) AS x
     WHERE x IS NOT NULL;
    IF v_want = 0 THEN
        RETURN;
    END IF;

    -- L5: FOR UPDATE in account_id order. Locks belong to the caller transaction.
    PERFORM 1
      FROM ledger.account_balances AS b
     WHERE b.account_id IN (SELECT DISTINCT x FROM unnest(p_account_ids) AS x WHERE x IS NOT NULL)
     ORDER BY b.account_id
       FOR UPDATE OF b;

    SELECT count(*) INTO v_got
      FROM ledger.account_balances AS b
     WHERE b.account_id IN (SELECT DISTINCT x FROM unnest(p_account_ids) AS x WHERE x IS NOT NULL);
    IF v_got <> v_want THEN
        RAISE EXCEPTION 'INVALID_JOURNAL: account projection is missing'
            USING ERRCODE = 'PC400';
    END IF;

    RETURN QUERY
    SELECT b.account_id, b.currency, b.available_amount, b.reserved_amount, b.version
      FROM ledger.account_balances AS b
     WHERE b.account_id IN (SELECT DISTINCT x FROM unnest(p_account_ids) AS x WHERE x IS NOT NULL)
     ORDER BY b.account_id;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION ledger.post_journal(
    p_journal_id uuid,
    p_operation text,
    p_reference_type text,
    p_reference_id uuid,
    p_operation_seq smallint,
    p_reverses_journal_id uuid,
    p_adjustment_id uuid,
    p_description text,
    p_initiated_by uuid,
    p_accounting_date date,
    p_entries jsonb,
    p_hold_action jsonb
) RETURNS TABLE (
    posted_journal_id uuid,
    already_posted boolean,
    posted_entry_id uuid,
    posted_ledger_account_id uuid,
    posted_entry_type text,
    posted_amount bigint,
    posted_currency char(3),
    posted_account_version bigint,
    posted_balance_after bigint
)
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, ledger, pg_temp
AS $$
DECLARE
    v_seq smallint;
    v_existing uuid;
    v_rows jsonb := '[]'::jsonb;
    r record;
    v_la record;
    v_signed bigint;
    v_active boolean;
    v_debit numeric;
    v_credit numeric;
    v_dup integer;
    v_biz_ids uuid[];
    v_acc record;
    v_status text;
    v_adj_ok boolean := false;
    v_sys_status text;
    v_avail bigint;
    v_reserved bigint;
    v_version bigint;
    v_need bigint;
    v_sum bigint;
    v_left bigint;
    v_take bigint;
    v_bucket integer;
    v_hold record;
    v_hold_row record;
    v_new_hold_status text;
    v_n integer;
    v_orig_op text;
    v_orig_ref text;
    v_stored_rev uuid;
    v_stored_date date;
    v_acct_updated timestamptz;
BEGIN
    IF p_journal_id IS NULL OR p_operation IS NULL OR p_reference_type IS NULL
       OR p_reference_id IS NULL OR p_accounting_date IS NULL THEN
        RAISE EXCEPTION 'INVALID_JOURNAL: required arguments are missing'
            USING ERRCODE = 'PC400';
    END IF;

    v_seq := coalesce(p_operation_seq, 1);
    IF v_seq < 1 THEN
        RAISE EXCEPTION 'INVALID_JOURNAL: operation_seq must be >= 1'
            USING ERRCODE = 'PC400';
    END IF;

    -- Serialize replays of the same journal business key. Not xmin; not L1 (HTTP).
    PERFORM pg_advisory_xact_lock(
        ('x' || substr(md5(concat_ws('|', p_reference_type, p_reference_id::text, p_operation, v_seq::text)), 1, 16))::bit(64)::bigint
    );

    -- 1. Existing journal identity. Lookup key:
    -- (reference_type, reference_id, operation, operation_seq).
    -- Compared (economic payload): entries, reverses_journal_id, hold_action,
    -- accounting_date (D-29), extra adjustment_id on non-REVERSAL.
    -- Not compared: description, initiated_by — audit metadata. Journals are
    -- immutable, so a retry must not UPDATE them. HTTP retries may vary
    -- commentary/actor; treating those as identity would PC409 a true replay.
    -- adjustment_id is not a journal_transactions column (00003). A REVERSAL
    -- replay cannot detect a different adjustment_id. Future: a later
    -- migration (not 00003) adds the column, INSERT stores it, this step
    -- compares it.
    SELECT j.id, j.reverses_journal_id, j.accounting_date
      INTO v_existing, v_stored_rev, v_stored_date
      FROM ledger.journal_transactions AS j
     WHERE j.reference_type = p_reference_type
       AND j.reference_id = p_reference_id
       AND j.operation = p_operation
       AND j.operation_seq = v_seq;
    IF v_existing IS NOT NULL THEN
        IF EXISTS (
            SELECT 1 FROM (
                SELECT e.ledger_account_id, e.entry_type, e.amount, e.currency
                  FROM ledger.ledger_entries AS e
                 WHERE e.journal_transaction_id = v_existing
                EXCEPT ALL
                SELECT x.ledger_account_id, x.entry_type, x.amount, x.currency::char(3)
                  FROM jsonb_to_recordset(coalesce(p_entries, '[]'::jsonb)) AS x(
                      ledger_account_id uuid, entry_type text, amount bigint, currency text)
            ) AS s
        ) OR EXISTS (
            SELECT 1 FROM (
                SELECT x.ledger_account_id, x.entry_type, x.amount, x.currency::char(3)
                  FROM jsonb_to_recordset(coalesce(p_entries, '[]'::jsonb)) AS x(
                      ledger_account_id uuid, entry_type text, amount bigint, currency text)
                EXCEPT ALL
                SELECT e.ledger_account_id, e.entry_type, e.amount, e.currency
                  FROM ledger.ledger_entries AS e
                 WHERE e.journal_transaction_id = v_existing
            ) AS s
        ) THEN
            RAISE EXCEPTION 'CONFLICT: journal already posted with different entries'
                USING ERRCODE = 'PC409';
        END IF;
        IF v_stored_rev IS DISTINCT FROM p_reverses_journal_id THEN
            RAISE EXCEPTION 'CONFLICT: journal already posted with different reverses_journal_id'
                USING ERRCODE = 'PC409';
        END IF;
        IF v_stored_date IS DISTINCT FROM p_accounting_date THEN
            RAISE EXCEPTION 'CONFLICT: journal already posted with different accounting_date'
                USING ERRCODE = 'PC409';
        END IF;
        IF p_operation <> 'REVERSAL' AND p_adjustment_id IS NOT NULL THEN
            RAISE EXCEPTION 'CONFLICT: journal already posted with different adjustment_id'
                USING ERRCODE = 'PC409';
        END IF;
        IF p_operation NOT IN ('HOLD_PLACE', 'HOLD_CAPTURE', 'HOLD_RELEASE') THEN
            IF p_hold_action IS NOT NULL AND p_hold_action <> 'null'::jsonb THEN
                RAISE EXCEPTION 'CONFLICT: journal already posted with different hold_action'
                    USING ERRCODE = 'PC409';
            END IF;
        ELSE
            IF p_hold_action IS NULL OR jsonb_typeof(p_hold_action) <> 'object' THEN
                RAISE EXCEPTION 'CONFLICT: journal already posted with different hold_action'
                    USING ERRCODE = 'PC409';
            END IF;
            SELECT x.action, x.hold_id, x.account_id, x.amount, x.currency, x.expires_at,
                   x.reference_type, x.reference_id
              INTO v_hold
              FROM jsonb_to_record(p_hold_action) AS x(
                  action text, hold_id uuid, account_id uuid, amount bigint, currency text,
                  expires_at timestamptz, reference_type text, reference_id uuid);
            SELECT h.id, h.account_id, h.currency, h.amount, h.status,
                   h.reference_type, h.reference_id, h.expires_at
              INTO v_hold_row
              FROM ledger.holds AS h
             WHERE h.id = v_hold.hold_id;
            IF NOT FOUND
               OR v_hold.hold_id IS NULL OR v_hold.account_id IS NULL OR v_hold.action IS NULL
               OR v_hold.amount IS NULL OR v_hold.currency IS NULL
               OR v_hold_row.account_id IS DISTINCT FROM v_hold.account_id
               OR v_hold_row.amount IS DISTINCT FROM v_hold.amount
               OR v_hold_row.currency IS DISTINCT FROM v_hold.currency::char(3) THEN
                RAISE EXCEPTION 'CONFLICT: journal already posted with different hold_action'
                    USING ERRCODE = 'PC409';
            END IF;
            IF p_operation = 'HOLD_PLACE' THEN
                IF v_hold.action <> 'PLACE'
                   OR v_hold_row.reference_type IS DISTINCT FROM v_hold.reference_type
                   OR v_hold_row.reference_id IS DISTINCT FROM v_hold.reference_id
                   OR v_hold_row.expires_at IS DISTINCT FROM v_hold.expires_at THEN
                    RAISE EXCEPTION 'CONFLICT: journal already posted with different hold_action'
                        USING ERRCODE = 'PC409';
                END IF;
            ELSIF p_operation = 'HOLD_CAPTURE' THEN
                IF v_hold.action <> 'CAPTURE' OR v_hold_row.status <> 'CAPTURED' THEN
                    RAISE EXCEPTION 'CONFLICT: journal already posted with different hold_action'
                        USING ERRCODE = 'PC409';
                END IF;
            ELSE
                IF (v_hold.action = 'RELEASE' AND v_hold_row.status = 'RELEASED')
                   OR (v_hold.action = 'EXPIRE' AND v_hold_row.status = 'EXPIRED') THEN
                    NULL;
                ELSE
                    RAISE EXCEPTION 'CONFLICT: journal already posted with different hold_action'
                        USING ERRCODE = 'PC409';
                END IF;
            END IF;
        END IF;
        RETURN QUERY
        SELECT v_existing, true, e.id, e.ledger_account_id, e.entry_type, e.amount,
               e.currency, e.account_version, e.balance_after
          FROM ledger.ledger_entries AS e
         WHERE e.journal_transaction_id = v_existing
         ORDER BY e.id;
        RETURN;
    END IF;

    IF EXISTS (SELECT 1 FROM ledger.journal_transactions AS j WHERE j.id = p_journal_id) THEN
        RAISE EXCEPTION 'CONFLICT: journal_id already used'
            USING ERRCODE = 'PC409';
    END IF;

    -- 2. Accounting validation.
    IF p_entries IS NULL OR jsonb_typeof(p_entries) <> 'array' THEN
        RAISE EXCEPTION 'INVALID_JOURNAL: entries must be a JSON array'
            USING ERRCODE = 'PC400';
    END IF;
    IF jsonb_array_length(p_entries) < 2 THEN
        RAISE EXCEPTION 'INVALID_JOURNAL: journal must have at least 2 entries'
            USING ERRCODE = 'PC400';
    END IF;

    IF p_operation = 'REVERSAL' THEN
        IF p_reverses_journal_id IS NULL THEN
            RAISE EXCEPTION 'INVALID_JOURNAL: REVERSAL requires reverses_journal_id'
                USING ERRCODE = 'PC400';
        END IF;
    ELSIF p_reverses_journal_id IS NOT NULL THEN
        RAISE EXCEPTION 'INVALID_JOURNAL: reverses_journal_id is only valid for REVERSAL'
            USING ERRCODE = 'PC400';
    END IF;

    SELECT count(*) INTO v_dup
      FROM (
          SELECT x.ledger_account_id
            FROM jsonb_to_recordset(p_entries) AS x(ledger_account_id uuid)
           GROUP BY x.ledger_account_id
          HAVING count(*) > 1
      ) AS d;
    IF v_dup > 0 THEN
        RAISE EXCEPTION 'INVALID_JOURNAL: duplicate ledger account in journal'
            USING ERRCODE = 'PC400';
    END IF;

    FOR r IN
        SELECT x.id, x.ledger_account_id, x.entry_type, x.amount, x.currency
          FROM jsonb_to_recordset(p_entries) AS x(
              id uuid, ledger_account_id uuid, entry_type text, amount bigint, currency text)
    LOOP
        IF r.id IS NULL OR r.ledger_account_id IS NULL OR r.entry_type IS NULL
           OR r.amount IS NULL OR r.currency IS NULL THEN
            RAISE EXCEPTION 'INVALID_JOURNAL: entry fields are required'
                USING ERRCODE = 'PC400';
        END IF;
        IF r.entry_type NOT IN ('DEBIT', 'CREDIT') THEN
            RAISE EXCEPTION 'INVALID_JOURNAL: entry_type must be DEBIT or CREDIT'
                USING ERRCODE = 'PC400';
        END IF;
        IF r.amount <= 0 OR r.amount > 100000000000000 THEN
            RAISE EXCEPTION 'INVALID_JOURNAL: amount out of range'
                USING ERRCODE = 'PC400';
        END IF;

        SELECT la.owner_type, la.account_id, la.kind, la.account_type, la.normal_balance,
               la.allow_negative, la.bucket_count, la.status, la.currency AS la_currency
          INTO v_la
          FROM ledger.ledger_accounts AS la
         WHERE la.id = r.ledger_account_id;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'INVALID_JOURNAL: ledger account % not found', r.ledger_account_id
                USING ERRCODE = 'PC400';
        END IF;
        IF v_la.la_currency IS DISTINCT FROM r.currency THEN
            RAISE EXCEPTION 'INVALID_JOURNAL: entry currency does not match account'
                USING ERRCODE = 'PC400';
        END IF;

        SELECT c.is_active INTO v_active
          FROM accounts.currencies AS c
         WHERE c.code = r.currency;
        IF NOT FOUND OR NOT v_active THEN
            RAISE EXCEPTION 'INVALID_JOURNAL: currency is not active'
                USING ERRCODE = 'PC400';
        END IF;

        IF r.entry_type = v_la.normal_balance THEN
            v_signed := r.amount;
        ELSE
            v_signed := -r.amount;
        END IF;

        v_rows := v_rows || jsonb_build_array(jsonb_build_object(
            'id', r.id,
            'ledger_account_id', r.ledger_account_id,
            'entry_type', r.entry_type,
            'amount', r.amount,
            'currency', r.currency,
            'owner_type', v_la.owner_type,
            'account_id', v_la.account_id,
            'kind', v_la.kind,
            'account_type', v_la.account_type,
            'normal_balance', v_la.normal_balance,
            'allow_negative', v_la.allow_negative,
            'bucket_count', v_la.bucket_count,
            'la_status', v_la.status,
            'signed_delta', v_signed
        ));
    END LOOP;

    SELECT count(*) INTO v_n
      FROM (
          SELECT x.currency,
                 sum(CASE WHEN x.entry_type = 'DEBIT' THEN x.amount ELSE 0 END) AS d,
                 sum(CASE WHEN x.entry_type = 'CREDIT' THEN x.amount ELSE 0 END) AS c
            FROM jsonb_to_recordset(p_entries) AS x(entry_type text, amount bigint, currency text)
           GROUP BY x.currency
          HAVING sum(CASE WHEN x.entry_type = 'DEBIT' THEN x.amount ELSE 0 END)
              <> sum(CASE WHEN x.entry_type = 'CREDIT' THEN x.amount ELSE 0 END)
      ) AS u;
    IF v_n > 0 THEN
        RAISE EXCEPTION 'INVALID_JOURNAL: journal is not balanced'
            USING ERRCODE = 'PC400';
    END IF;

    -- Operation form (money-flows.md). Status is step 3.
    PERFORM ledger.assert_journal_form(p_operation, p_reference_type, v_rows, p_hold_action);

    IF p_operation IN ('HOLD_PLACE', 'HOLD_CAPTURE', 'HOLD_RELEASE') THEN
        SELECT x.action, x.hold_id, x.account_id, x.amount, x.currency, x.expires_at,
               x.reference_type, x.reference_id
          INTO v_hold
          FROM jsonb_to_record(p_hold_action) AS x(
              action text, hold_id uuid, account_id uuid, amount bigint, currency text,
              expires_at timestamptz, reference_type text, reference_id uuid);
    ELSIF p_adjustment_id IS NOT NULL AND p_operation <> 'REVERSAL' THEN
        RAISE EXCEPTION 'INVALID_JOURNAL: adjustment_id is only valid for REVERSAL'
            USING ERRCODE = 'PC400';
    END IF;

    IF p_operation = 'REVERSAL' THEN
        SELECT j.operation, j.reference_type INTO v_orig_op, v_orig_ref
          FROM ledger.journal_transactions AS j
         WHERE j.id = p_reverses_journal_id;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'INVALID_JOURNAL: reversed journal not found'
                USING ERRCODE = 'PC400';
        END IF;
        IF v_orig_op = 'REVERSAL' THEN
            RAISE EXCEPTION 'INVALID_JOURNAL: cannot reverse a reversal'
                USING ERRCODE = 'PC400';
        END IF;
        IF p_reference_type IS DISTINCT FROM v_orig_ref THEN
            RAISE EXCEPTION 'INVALID_JOURNAL: REVERSAL reference_type must match the original'
                USING ERRCODE = 'PC400';
        END IF;
        IF EXISTS (
            SELECT 1 FROM (
                SELECT e.ledger_account_id,
                       CASE e.entry_type WHEN 'DEBIT' THEN 'CREDIT' ELSE 'DEBIT' END AS entry_type,
                       e.amount, e.currency
                  FROM ledger.ledger_entries AS e
                 WHERE e.journal_transaction_id = p_reverses_journal_id
                EXCEPT ALL
                SELECT x.ledger_account_id, x.entry_type, x.amount, x.currency::char(3)
                  FROM jsonb_to_recordset(p_entries) AS x(
                      ledger_account_id uuid, entry_type text, amount bigint, currency text)
            ) AS s
        ) OR EXISTS (
            SELECT 1 FROM (
                SELECT x.ledger_account_id, x.entry_type, x.amount, x.currency::char(3)
                  FROM jsonb_to_recordset(p_entries) AS x(
                      ledger_account_id uuid, entry_type text, amount bigint, currency text)
                EXCEPT ALL
                SELECT e.ledger_account_id,
                       CASE e.entry_type WHEN 'DEBIT' THEN 'CREDIT' ELSE 'DEBIT' END,
                       e.amount, e.currency
                  FROM ledger.ledger_entries AS e
                 WHERE e.journal_transaction_id = p_reverses_journal_id
            ) AS s
        ) THEN
            RAISE EXCEPTION 'INVALID_JOURNAL: REVERSAL entries must mirror the original'
                USING ERRCODE = 'PC400';
        END IF;
    END IF;

    -- 3. Account status. L3: FOR SHARE NOWAIT on business accounts (PC460, never wait down).
    SELECT array_agg(DISTINCT x.account_id) INTO v_biz_ids
      FROM jsonb_to_recordset(v_rows) AS x(account_id uuid, owner_type text)
     WHERE x.owner_type <> 'SYSTEM' AND x.account_id IS NOT NULL;

    IF p_operation IN ('ADJUSTMENT', 'REVERSAL') THEN
        IF EXISTS (
            SELECT 1 FROM jsonb_to_recordset(v_rows) AS x(owner_type text)
             WHERE x.owner_type <> 'SYSTEM'
        ) THEN
            IF p_operation = 'ADJUSTMENT' THEN
                SELECT ma.status = 'APPROVED' INTO v_adj_ok
                  FROM ledger.manual_adjustments AS ma
                 WHERE ma.id = p_reference_id;
                v_adj_ok := coalesce(v_adj_ok, false);
            ELSE
                IF p_adjustment_id IS NULL THEN
                    v_adj_ok := false;
                ELSE
                    SELECT ma.status = 'APPROVED' INTO v_adj_ok
                      FROM ledger.manual_adjustments AS ma
                     WHERE ma.id = p_adjustment_id;
                    v_adj_ok := coalesce(v_adj_ok, false);
                END IF;
            END IF;
        ELSE
            v_adj_ok := true;
        END IF;
    END IF;

    IF v_biz_ids IS NOT NULL THEN
        BEGIN
            PERFORM 1
              FROM accounts.accounts AS a
             WHERE a.id = ANY (v_biz_ids)
             ORDER BY a.id
               FOR SHARE OF a NOWAIT;
        EXCEPTION
            WHEN lock_not_available THEN
                RAISE EXCEPTION 'LOCK_PROTOCOL_VIOLATION: missing L3 FOR SHARE on accounts'
                    USING ERRCODE = 'PC460';
        END;

        SELECT count(*) INTO v_n FROM accounts.accounts AS a WHERE a.id = ANY (v_biz_ids);
        IF v_n <> cardinality(v_biz_ids) THEN
            RAISE EXCEPTION 'INVALID_JOURNAL: business account not found'
                USING ERRCODE = 'PC400';
        END IF;

        FOR r IN
            SELECT x.owner_type, x.kind, x.entry_type, x.account_id, x.la_status, x.account_type
              FROM jsonb_to_recordset(v_rows) AS x(
                  owner_type text, kind text, entry_type text, account_id uuid,
                  la_status text, account_type text)
        LOOP
            IF r.owner_type = 'SYSTEM' THEN
                IF r.la_status <> 'ACTIVE' THEN
                    RAISE EXCEPTION 'ACCOUNT_NOT_ACTIVE: system ledger account is not ACTIVE'
                        USING ERRCODE = 'PC403';
                END IF;
            ELSE
                SELECT a.status, a.updated_at INTO v_status, v_acct_updated
                  FROM accounts.accounts AS a
                 WHERE a.id = r.account_id;
                IF NOT ledger.entry_status_allowed(
                    p_operation, r.owner_type, r.kind, r.entry_type, v_status, v_adj_ok
                ) THEN
                    RAISE EXCEPTION 'ACCOUNT_NOT_ACTIVE: operation % not allowed for % % in status %',
                        p_operation, r.owner_type, r.kind, v_status
                        USING ERRCODE = 'PC403';
                END IF;
            END IF;
        END LOOP;
    ELSE
        FOR r IN
            SELECT x.la_status
              FROM jsonb_to_recordset(v_rows) AS x(la_status text, owner_type text)
             WHERE x.owner_type = 'SYSTEM'
        LOOP
            IF r.la_status <> 'ACTIVE' THEN
                RAISE EXCEPTION 'ACCOUNT_NOT_ACTIVE: system ledger account is not ACTIVE'
                    USING ERRCODE = 'PC403';
            END IF;
        END LOOP;
    END IF;

    -- 4–5. L5 business projections (account_id order), then L6 system buckets.
    FOR r IN
        SELECT x.account_id,
               sum(CASE WHEN x.kind = 'AVAILABLE' THEN x.signed_delta ELSE 0 END)::bigint AS avail_delta,
               sum(CASE WHEN x.kind = 'HOLD' THEN x.signed_delta ELSE 0 END)::bigint AS reserved_delta
          FROM jsonb_to_recordset(v_rows) AS x(account_id uuid, owner_type text, kind text, signed_delta bigint)
         WHERE x.owner_type <> 'SYSTEM' AND x.account_id IS NOT NULL
         GROUP BY x.account_id
         ORDER BY x.account_id
    LOOP
        SELECT b.available_amount, b.reserved_amount, b.version
          INTO v_avail, v_reserved, v_version
          FROM ledger.account_balances AS b
         WHERE b.account_id = r.account_id
           FOR UPDATE OF b;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'INVALID_JOURNAL: account projection is missing'
                USING ERRCODE = 'PC400';
        END IF;
        v_avail := v_avail + r.avail_delta;
        v_reserved := v_reserved + r.reserved_delta;
        IF v_avail < 0 OR v_reserved < 0 THEN
            RAISE EXCEPTION 'INSUFFICIENT_FUNDS'
                USING ERRCODE = 'PC402';
        END IF;
        UPDATE ledger.account_balances
           SET available_amount = v_avail,
               reserved_amount = v_reserved,
               version = version + 1,
               updated_at = now()
         WHERE account_id = r.account_id
     RETURNING version INTO v_version;

        -- Stamp version / balance_after onto the in-memory rows for this business account.
        v_rows := (
            SELECT jsonb_agg(
                CASE
                    WHEN (e->>'owner_type') <> 'SYSTEM' AND (e->>'account_id')::uuid = r.account_id THEN
                        e || jsonb_build_object(
                            'account_version', v_version,
                            'balance_after', CASE e->>'kind'
                                WHEN 'AVAILABLE' THEN v_avail
                                WHEN 'HOLD' THEN v_reserved
                            END
                        )
                    ELSE e
                END
            )
            FROM jsonb_array_elements(v_rows) AS e
        );
    END LOOP;

    FOR r IN
        SELECT x.ledger_account_id, x.signed_delta, x.allow_negative, x.bucket_count
          FROM jsonb_to_recordset(v_rows) AS x(
              ledger_account_id uuid, owner_type text, signed_delta bigint,
              allow_negative boolean, bucket_count integer)
         WHERE x.owner_type = 'SYSTEM'
         ORDER BY x.ledger_account_id
    LOOP
        IF r.signed_delta > 0 THEN
            v_bucket := (('x' || substr(md5(p_journal_id::text), 1, 7))::bit(28)::int) % r.bucket_count;
            UPDATE ledger.system_balance_buckets
               SET balance = balance + r.signed_delta,
                   updated_at = now()
             WHERE ledger_account_id = r.ledger_account_id
               AND bucket_no = v_bucket;
            IF NOT FOUND THEN
                RAISE EXCEPTION 'INVALID_JOURNAL: system bucket missing'
                    USING ERRCODE = 'PC400';
            END IF;
        ELSIF r.signed_delta < 0 THEN
            v_need := -r.signed_delta;
            PERFORM 1
              FROM ledger.system_balance_buckets AS b
             WHERE b.ledger_account_id = r.ledger_account_id
             ORDER BY b.bucket_no
               FOR UPDATE OF b;
            SELECT coalesce(sum(b.balance), 0) INTO v_sum
              FROM ledger.system_balance_buckets AS b
             WHERE b.ledger_account_id = r.ledger_account_id;
            IF v_sum < v_need THEN
                RAISE EXCEPTION 'SYSTEM_ACCOUNT_INSUFFICIENT'
                    USING ERRCODE = 'PC402';
            END IF;
            v_left := v_need;
            FOR v_acc IN
                SELECT b.bucket_no, b.balance
                  FROM ledger.system_balance_buckets AS b
                 WHERE b.ledger_account_id = r.ledger_account_id
                 ORDER BY b.bucket_no
            LOOP
                v_take := LEAST(v_acc.balance, v_left);
                IF v_take > 0 THEN
                    UPDATE ledger.system_balance_buckets
                       SET balance = balance - v_take,
                           updated_at = now()
                     WHERE ledger_account_id = r.ledger_account_id
                       AND bucket_no = v_acc.bucket_no;
                    v_left := v_left - v_take;
                END IF;
                EXIT WHEN v_left = 0;
            END LOOP;
        END IF;
    END LOOP;

    -- 6. L7 holds (concurrency.md): insert PLACE or lock+transition CAPTURE/RELEASE.
    -- SELECT FOR UPDATE is the same L7 row lock as the subsequent UPDATE, taken
    -- after L5/L6 so amount/currency/account/status are re-checked atomically.
    -- Concurrent CAPTURE vs RELEASE of one hold serialize here. Not a new lock
    -- level; not xmin.
    IF p_operation = 'HOLD_PLACE' THEN
        BEGIN
            INSERT INTO ledger.holds (
                id, account_id, currency, amount, status, reference_type, reference_id, expires_at
            ) VALUES (
                v_hold.hold_id,
                v_hold.account_id,
                v_hold.currency,
                v_hold.amount,
                'ACTIVE',
                v_hold.reference_type,
                v_hold.reference_id,
                v_hold.expires_at
            );
        EXCEPTION
            WHEN unique_violation THEN
                RAISE EXCEPTION 'CONFLICT: hold already exists'
                    USING ERRCODE = 'PC409';
        END;
    ELSIF p_operation IN ('HOLD_CAPTURE', 'HOLD_RELEASE') THEN
        SELECT h.id, h.account_id, h.currency, h.amount, h.status
          INTO v_hold_row
          FROM ledger.holds AS h
         WHERE h.id = v_hold.hold_id
           FOR UPDATE OF h;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'CONFLICT: hold not found'
                USING ERRCODE = 'PC409';
        END IF;
        IF v_hold_row.status <> 'ACTIVE' THEN
            RAISE EXCEPTION 'CONFLICT: hold is not ACTIVE'
                USING ERRCODE = 'PC409';
        END IF;
        IF v_hold_row.account_id IS DISTINCT FROM v_hold.account_id THEN
            RAISE EXCEPTION 'INVALID_JOURNAL: cannot change a hold while posting another account'
                USING ERRCODE = 'PC400';
        END IF;
        IF v_hold_row.amount IS DISTINCT FROM v_hold.amount
           OR v_hold_row.currency IS DISTINCT FROM v_hold.currency::char(3) THEN
            RAISE EXCEPTION 'INVALID_JOURNAL: hold amount or currency does not match'
                USING ERRCODE = 'PC400';
        END IF;
        v_new_hold_status := CASE v_hold.action
            WHEN 'CAPTURE' THEN 'CAPTURED'
            WHEN 'RELEASE' THEN 'RELEASED'
            WHEN 'EXPIRE' THEN 'EXPIRED'
        END;
        UPDATE ledger.holds
           SET status = v_new_hold_status,
               finalized_at = now()
         WHERE id = v_hold.hold_id
           AND status = 'ACTIVE'
           AND account_id = v_hold.account_id;
        GET DIAGNOSTICS v_n = ROW_COUNT;
        IF v_n = 0 THEN
            RAISE EXCEPTION 'CONFLICT: hold is not ACTIVE'
                USING ERRCODE = 'PC409';
        END IF;
    END IF;

    -- 7. L8 insert journal and all entries.
    INSERT INTO ledger.journal_transactions (
        id, operation, reference_type, reference_id, operation_seq, reverses_journal_id,
        status, description, initiated_by, accounting_date
    ) VALUES (
        p_journal_id, p_operation, p_reference_type, p_reference_id, v_seq, p_reverses_journal_id,
        'POSTED', p_description, p_initiated_by, p_accounting_date
    );

    INSERT INTO ledger.ledger_entries (
        id, journal_transaction_id, ledger_account_id, entry_type, amount, currency,
        account_version, balance_after
    )
    SELECT (e->>'id')::uuid,
           p_journal_id,
           (e->>'ledger_account_id')::uuid,
           e->>'entry_type',
           (e->>'amount')::bigint,
           (e->>'currency')::char(3),
           NULLIF(e->>'account_version', '')::bigint,
           NULLIF(e->>'balance_after', '')::bigint
      FROM jsonb_array_elements(v_rows) AS e;

    RETURN QUERY
    SELECT p_journal_id, false, e.id, e.ledger_account_id, e.entry_type, e.amount,
           e.currency, e.account_version, e.balance_after
      FROM ledger.ledger_entries AS e
     WHERE e.journal_transaction_id = p_journal_id
     ORDER BY e.id;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
DO $acl$
DECLARE
    r record;
BEGIN
    FOR r IN
        SELECT p.oid::regprocedure AS sig, p.proname
          FROM pg_proc AS p
          JOIN pg_namespace AS n ON n.oid = p.pronamespace
         WHERE n.nspname = 'ledger'
           AND p.proname IN (
               'entry_status_allowed',
               'assert_journal_form',
               'open_ledger_accounts',
               'lock_account_balances',
               'post_journal'
           )
    LOOP
        EXECUTE format('REVOKE ALL ON FUNCTION %s FROM PUBLIC', r.sig);
        IF r.proname NOT IN ('entry_status_allowed', 'assert_journal_form') THEN
            EXECUTE format('GRANT EXECUTE ON FUNCTION %s TO paycore_app', r.sig);
        END IF;
        EXECUTE format('ALTER FUNCTION %s OWNER TO paycore_ledger', r.sig);
    END LOOP;
END
$acl$;
-- +goose StatementEnd
