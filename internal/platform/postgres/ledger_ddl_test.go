package postgres

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	sys1100    = "00000000-0000-4000-8000-000000001100"
	sys4100    = "00000000-0000-4000-8000-000000004100"
	testUserID = "11111111-1111-4111-8111-111111111111"
	testAccID  = "22222222-2222-4222-8222-222222222222"
	testJID    = "33333333-3333-4333-8333-333333333333"
	testE1     = "44444444-4444-4444-8444-444444444441"
	testE2     = "44444444-4444-4444-8444-444444444442"
)

func TestLedgerDDL(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	c := startCluster(t)
	if err := Bootstrap(ctx, c.admin, c.owner, c.app, c.readonly); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if err := Up(ctx, c.owner); err != nil {
		t.Fatalf("up: %v", err)
	}

	owner := mustConnect(t, ctx, c.owner)
	defer owner.Close(ctx)
	app := mustConnect(t, ctx, c.app)
	defer app.Close(ctx)
	ro := mustConnect(t, ctx, c.readonly)
	defer ro.Close(ctx)

	t.Run("tables and seed", func(t *testing.T) {
		var n int
		if err := owner.QueryRow(ctx, `SELECT count(*) FROM ledger.chart_of_accounts`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 15 {
			t.Fatalf("chart rows = %d", n)
		}
		var code string
		var minor int
		if err := app.QueryRow(ctx, `SELECT code, minor_units FROM accounts.currencies WHERE code = 'RUB'`).Scan(&code, &minor); err != nil {
			t.Fatal(err)
		}
		if minor != 2 {
			t.Fatalf("RUB minor_units = %d", minor)
		}
		var buckets int
		if err := owner.QueryRow(ctx, `
			SELECT count(*) FROM ledger.system_balance_buckets b
			JOIN ledger.ledger_accounts a ON a.id = b.ledger_account_id
			WHERE a.system_code = 'SIM_FUNDING_SOURCE'`).Scan(&buckets); err != nil {
			t.Fatal(err)
		}
		if buckets != 16 {
			t.Fatalf("1100 buckets = %d", buckets)
		}
		if err := owner.QueryRow(ctx, `
			SELECT count(*) FROM ledger.system_balance_buckets b
			JOIN ledger.ledger_accounts a ON a.id = b.ledger_account_id
			WHERE a.system_code = 'OPERATIONAL_LOSSES'`).Scan(&buckets); err != nil {
			t.Fatal(err)
		}
		if buckets != 1 {
			t.Fatalf("5900 buckets = %d", buckets)
		}
		var sysN int
		if err := owner.QueryRow(ctx, `SELECT count(*) FROM ledger.ledger_accounts WHERE owner_type = 'SYSTEM'`).Scan(&sysN); err != nil {
			t.Fatal(err)
		}
		if sysN != 4 {
			t.Fatalf("system accounts = %d", sysN)
		}
	})

	t.Run("paycore_app cannot write immutable ledger tables", func(t *testing.T) {
		mustFail(t, ctx, app, `INSERT INTO ledger.journal_transactions (
			id, operation, reference_type, reference_id, accounting_date, status)
			VALUES ($1, 'SIM_TOPUP', 'SIM_TOPUP', $1, CURRENT_DATE, 'POSTED')`, testJID)
		mustFail(t, ctx, app, `INSERT INTO ledger.ledger_entries (
			id, journal_transaction_id, ledger_account_id, entry_type, amount, currency)
			VALUES ($1, $2, $3, 'DEBIT', 1, 'RUB')`, testE1, testJID, sys1100)
		mustFail(t, ctx, app, `INSERT INTO ledger.account_balances (account_id, currency) VALUES ($1, 'RUB')`, testAccID)
		mustFail(t, ctx, app, `INSERT INTO ledger.holds (
			id, account_id, currency, amount, status, reference_type, reference_id, expires_at)
			VALUES ($1, $2, 'RUB', 1, 'ACTIVE', 'PAYMENT', $1, now())`, testJID, testAccID)
		mustFail(t, ctx, app, `INSERT INTO ledger.ledger_accounts (
			id, owner_type, account_type, system_code, currency, account_class, normal_balance, allow_negative, bucket_count, status)
			VALUES ($1, 'SYSTEM', '1100', 'SIM_FUNDING_SOURCE', 'RUB', 'ASSET', 'DEBIT', false, 1, 'ACTIVE')`, testAccID)
	})

	t.Run("paycore_app can insert accounts", func(t *testing.T) {
		if _, err := owner.Exec(ctx, `INSERT INTO auth.users (id) VALUES ($1)`, testUserID); err != nil {
			t.Fatal(err)
		}
		if _, err := app.Exec(ctx, `
			INSERT INTO accounts.accounts (id, account_number, user_id, account_purpose, currency, status)
			VALUES ($1, 'PC-TEST-1', $2, 'MAIN', 'RUB', 'ACTIVE')`, testAccID, testUserID); err != nil {
			t.Fatalf("app insert account: %v", err)
		}
		if _, err := app.Exec(ctx, `
			INSERT INTO accounts.account_status_history (id, account_id, to_status, reason, changed_by)
			VALUES ($1, $2, 'ACTIVE', 'open', $3)`, testJID, testAccID, testUserID); err != nil {
			t.Fatalf("app insert status history: %v", err)
		}
	})

	t.Run("readonly can select and cannot write", func(t *testing.T) {
		var code string
		if err := ro.QueryRow(ctx, `SELECT code FROM accounts.currencies WHERE code = 'RUB'`).Scan(&code); err != nil {
			t.Fatalf("readonly select currency: %v", err)
		}
		if err := ro.QueryRow(ctx, `SELECT code FROM ledger.chart_of_accounts WHERE code = '1100'`).Scan(&code); err != nil {
			t.Fatalf("readonly select chart: %v", err)
		}
		mustFail(t, ctx, ro, `INSERT INTO accounts.currencies (code, minor_units, name, is_active)
			VALUES ('USD', 2, 'US Dollar', true)`)
		mustFail(t, ctx, ro, `UPDATE accounts.accounts SET name = 'x' WHERE id = $1`, testAccID)
	})

	t.Run("check bad amounts", func(t *testing.T) {
		mustFail(t, ctx, owner, `INSERT INTO ledger.ledger_entries (
			id, journal_transaction_id, ledger_account_id, entry_type, amount, currency)
			VALUES ($1, $2, $3, 'DEBIT', 0, 'RUB')`, testE1, testJID, sys1100)
		mustFail(t, ctx, owner, `INSERT INTO ledger.ledger_entries (
			id, journal_transaction_id, ledger_account_id, entry_type, amount, currency)
			VALUES ($1, $2, $3, 'DEBIT', -1, 'RUB')`, testE1, testJID, sys1100)
		mustFail(t, ctx, owner, `INSERT INTO ledger.ledger_entries (
			id, journal_transaction_id, ledger_account_id, entry_type, amount, currency)
			VALUES ($1, $2, $3, 'DEBIT', 100000000000001, 'RUB')`, testE1, testJID, sys1100)
	})

	t.Run("currency composite fk", func(t *testing.T) {
		mustFail(t, ctx, owner, `INSERT INTO ledger.ledger_entries (
			id, journal_transaction_id, ledger_account_id, entry_type, amount, currency)
			VALUES ($1, $2, $3, 'DEBIT', 1, 'USD')`, testE1, testJID, sys1100)
	})

	t.Run("xor owner and closed_at", func(t *testing.T) {
		mustFail(t, ctx, owner, `
			INSERT INTO accounts.accounts (id, account_number, account_purpose, currency, status)
			VALUES ($1, 'PC-BAD-XOR', 'MAIN', 'RUB', 'ACTIVE')`, "55555555-5555-4555-8555-555555555555")
		mustFail(t, ctx, owner, `
			INSERT INTO accounts.accounts (id, account_number, user_id, account_purpose, currency, status, closed_at)
			VALUES ($1, 'PC-BAD-CLOSED', $2, 'MAIN', 'RUB', 'ACTIVE', now())`,
			"55555555-5555-4555-8555-555555555556", testUserID)
	})

	t.Run("negative projection rejected", func(t *testing.T) {
		mustFail(t, ctx, owner, `
			INSERT INTO ledger.account_balances (account_id, currency, available_amount)
			VALUES ($1, 'RUB', -1)`, testAccID)
	})

	t.Run("journal unique and balance trigger", func(t *testing.T) {
		jid := "66666666-6666-4666-8666-666666666666"
		if err := insertBalancedJournal(ctx, owner, jid,
			"77777777-7777-4777-8777-777777777771",
			"77777777-7777-4777-8777-777777777772"); err != nil {
			t.Fatalf("balanced journal: %v", err)
		}
		mustFail(t, ctx, owner, `INSERT INTO ledger.journal_transactions (
			id, operation, reference_type, reference_id, accounting_date, status)
			VALUES ($1, 'SIM_TOPUP', 'SIM_TOPUP', $2, CURRENT_DATE, 'POSTED')`,
			"66666666-6666-4666-8666-666666666667", jid)

		unbalanced := "88888888-8888-4888-8888-888888888888"
		tx, err := owner.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO ledger.journal_transactions (
			id, operation, reference_type, reference_id, accounting_date, status)
			VALUES ($1, 'ADJUSTMENT', 'ADJUSTMENT', $1, CURRENT_DATE, 'POSTED')`, unbalanced); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("insert unbalanced journal header: %v", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO ledger.ledger_entries (
			id, journal_transaction_id, ledger_account_id, entry_type, amount, currency)
			VALUES ($1, $2, $3, 'DEBIT', 100, 'RUB')`,
			"99999999-9999-4999-8999-999999999991", unbalanced, sys1100); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("insert debit: %v", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO ledger.ledger_entries (
			id, journal_transaction_id, ledger_account_id, entry_type, amount, currency)
			VALUES ($1, $2, $3, 'CREDIT', 50, 'RUB')`,
			"99999999-9999-4999-8999-999999999992", unbalanced, sys4100); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("insert credit: %v", err)
		}
		if err := tx.Commit(ctx); err == nil {
			t.Fatal("unbalanced journal committed")
		}

		single := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
		tx, err = owner.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO ledger.journal_transactions (
			id, operation, reference_type, reference_id, accounting_date, status)
			VALUES ($1, 'ADJUSTMENT', 'ADJUSTMENT', $1, CURRENT_DATE, 'POSTED')`, single); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("insert single journal header: %v", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO ledger.ledger_entries (
			id, journal_transaction_id, ledger_account_id, entry_type, amount, currency)
			VALUES ($1, $2, $3, 'DEBIT', 100, 'RUB')`,
			"bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", single, sys1100); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("insert single entry: %v", err)
		}
		if err := tx.Commit(ctx); err == nil {
			t.Fatal("single-entry journal committed")
		}
	})

	t.Run("immutable journal and entries", func(t *testing.T) {
		mustFail(t, ctx, owner, `UPDATE ledger.journal_transactions SET description = 'x' WHERE id = $1`,
			"66666666-6666-4666-8666-666666666666")
		mustFail(t, ctx, owner, `DELETE FROM ledger.ledger_entries WHERE journal_transaction_id = $1`,
			"66666666-6666-4666-8666-666666666666")
		mustFail(t, ctx, app, `UPDATE ledger.journal_transactions SET description = 'x' WHERE id = $1`,
			"66666666-6666-4666-8666-666666666666")
	})
}

func insertBalancedJournal(ctx context.Context, conn *pgx.Conn, jid, e1, e2 string) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `INSERT INTO ledger.journal_transactions (
		id, operation, reference_type, reference_id, accounting_date, status)
		VALUES ($1, 'ADJUSTMENT', 'ADJUSTMENT', $1, CURRENT_DATE, 'POSTED')`, jid); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO ledger.ledger_entries (
		id, journal_transaction_id, ledger_account_id, entry_type, amount, currency)
		VALUES ($1, $2, $3, 'DEBIT', 100, 'RUB')`, e1, jid, sys1100); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO ledger.ledger_entries (
		id, journal_transaction_id, ledger_account_id, entry_type, amount, currency)
		VALUES ($1, $2, $3, 'CREDIT', 100, 'RUB')`, e2, jid, sys4100); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func mustConnect(t *testing.T, ctx context.Context, dsn string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	return conn
}

func mustFail(t *testing.T, ctx context.Context, conn *pgx.Conn, sql string, args ...any) {
	t.Helper()
	if _, err := conn.Exec(ctx, sql, args...); err == nil {
		t.Fatalf("query succeeded, want error: %s", sql)
	} else if strings.Contains(strings.ToLower(err.Error()), "cannot find") {
		t.Fatalf("unexpected error: %v", err)
	}
}
