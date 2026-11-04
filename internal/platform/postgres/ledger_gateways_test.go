package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/romangergovskiy/paycore/internal/ledger"
	"github.com/romangergovskiy/paycore/internal/platform/apperror"
)

func TestLedgerAccountAPI(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
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
	ids := &idgen{}
	api := NewLedger(app)

	t.Run("CUSTOMER pair", func(t *testing.T) {
		cust := insertBusinessAccount(t, ctx, owner, app, ids, "PC-GA-C", ledger.OwnerCustomer)
		rows, err := api.Open(ctx, mustUUID(t, cust.accountID), ledger.OwnerCustomer, "RUB")
		if err != nil {
			t.Fatalf("Open CUSTOMER: %v", err)
		}
		requireOpenedPair(t, rows)
		var n int
		if err := app.QueryRow(ctx, `SELECT count(*) FROM ledger.ledger_accounts WHERE account_id = $1`, cust.accountID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 2 {
			t.Fatalf("ledger_accounts = %d", n)
		}
	})

	t.Run("MERCHANT pair", func(t *testing.T) {
		merch := insertBusinessAccount(t, ctx, owner, app, ids, "PC-GA-M", ledger.OwnerMerchant)
		rows, err := api.Open(ctx, mustUUID(t, merch.accountID), ledger.OwnerMerchant, "RUB")
		if err != nil {
			t.Fatalf("Open MERCHANT: %v", err)
		}
		requireOpenedPair(t, rows)
	})

	t.Run("Open replay is idempotent not conflict", func(t *testing.T) {
		cust := insertBusinessAccount(t, ctx, owner, app, ids, "PC-GA-R", ledger.OwnerCustomer)
		first, err := api.Open(ctx, mustUUID(t, cust.accountID), ledger.OwnerCustomer, "RUB")
		if err != nil {
			t.Fatal(err)
		}
		second, err := api.Open(ctx, mustUUID(t, cust.accountID), ledger.OwnerCustomer, "RUB")
		if err != nil {
			t.Fatalf("replay Open: %v", err)
		}
		if len(first) != 2 || len(second) != 2 {
			t.Fatalf("first=%v second=%v", first, second)
		}
		if first[0].ID != second[0].ID || first[1].ID != second[1].ID {
			t.Fatalf("replay ids changed %v → %v", first, second)
		}
		var n int
		if err := app.QueryRow(ctx, `SELECT count(*) FROM ledger.ledger_accounts WHERE account_id = $1`, cust.accountID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 2 {
			t.Fatalf("replay created extra rows: %d", n)
		}
	})

	t.Run("Open invalid args SQLSTATE", func(t *testing.T) {
		cust := insertBusinessAccount(t, ctx, owner, app, ids, "PC-GA-BAD", ledger.OwnerCustomer)
		_, err := api.Open(ctx, mustUUID(t, cust.accountID), "FOO", "RUB")
		requireGatewayPC400(t, err)

		_, err = api.Open(ctx, mustUUID(t, cust.accountID), ledger.OwnerMerchant, "RUB")
		requireGatewayPC400(t, err)

		_, err = api.Open(ctx, mustUUID(t, cust.accountID), ledger.OwnerCustomer, "USD")
		requireGatewayPC400(t, err)

		missing := mustUUID(t, ids.next())
		_, err = api.Open(ctx, missing, ledger.OwnerCustomer, "RUB")
		requireGatewayPC400(t, err)

		_, err = api.Open(ctx, uuid.Nil, ledger.OwnerCustomer, "RUB")
		requireGatewayPC400(t, err)
	})

	t.Run("Lock in Tx sorted", func(t *testing.T) {
		a := seedCustomer(t, ctx, owner, app, ids, "PC-GA-L1")
		b := seedCustomer(t, ctx, owner, app, ids, "PC-GA-L2")
		tx, err := app.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		idA, idB := mustUUID(t, a.accountID), mustUUID(t, b.accountID)
		got, err := NewLedger(tx).Lock(ctx, []uuid.UUID{idB, idA})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 {
			t.Fatalf("lock rows = %d", len(got))
		}
		if got[0].AccountID.String() > got[1].AccountID.String() {
			t.Fatalf("lock order = %s %s", got[0].AccountID, got[1].AccountID)
		}
		if got[0].Currency != "RUB" || got[0].Available != 0 || got[0].Version != 0 {
			t.Fatalf("snapshot = %+v", got[0])
		}
	})

	t.Run("Lock empty and missing", func(t *testing.T) {
		tx, err := app.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		lg := NewLedger(tx)
		empty, err := lg.Lock(ctx, nil)
		if err != nil {
			t.Fatalf("nil ids: %v", err)
		}
		if len(empty) != 0 {
			t.Fatalf("nil ids rows = %v", empty)
		}
		empty, err = lg.Lock(ctx, []uuid.UUID{})
		if err != nil {
			t.Fatalf("empty ids: %v", err)
		}
		if len(empty) != 0 {
			t.Fatalf("empty ids rows = %v", empty)
		}
		_, err = lg.Lock(ctx, []uuid.UUID{mustUUID(t, ids.next())})
		requireGatewayPC400(t, err)
	})

	t.Run("Open Rollback", func(t *testing.T) {
		acc := insertBusinessAccount(t, ctx, owner, app, ids, "PC-GA-RB", ledger.OwnerCustomer)
		tx, err := app.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := NewLedger(tx).Open(ctx, mustUUID(t, acc.accountID), ledger.OwnerCustomer, "RUB"); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatal(err)
		}
		if err := tx.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
		var n int
		if err := app.QueryRow(ctx, `SELECT count(*) FROM ledger.ledger_accounts WHERE account_id = $1`, acc.accountID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("rollback leaked ledger_accounts = %d", n)
		}
	})

	t.Run("EXECUTE app not readonly", func(t *testing.T) {
		cust := seedCustomer(t, ctx, owner, app, ids, "PC-GA-RO")
		if _, err := api.Lock(ctx, []uuid.UUID{mustUUID(t, cust.accountID)}); err != nil {
			t.Fatalf("app Lock: %v", err)
		}
		_, err := NewLedger(ro).Lock(ctx, []uuid.UUID{mustUUID(t, cust.accountID)})
		if err == nil {
			t.Fatal("readonly Lock succeeded")
		}
		var pe *pgconn.PgError
		if !errors.As(err, &pe) || isPaycoreSQLState(pe.Code) {
			t.Fatalf("readonly want grant error, got %v", err)
		}
		_, err = NewLedger(ro).Open(ctx, mustUUID(t, cust.accountID), ledger.OwnerCustomer, "RUB")
		if err == nil {
			t.Fatal("readonly Open succeeded")
		}
	})
}

func requireOpenedPair(t *testing.T, rows []ledger.OpenedLedger) {
	t.Helper()
	if len(rows) != 2 {
		t.Fatalf("rows = %d %+v", len(rows), rows)
	}
	kinds := map[ledger.AccountKind]uuid.UUID{}
	for _, r := range rows {
		if r.ID == uuid.Nil {
			t.Fatalf("nil ledger id in %+v", r)
		}
		kinds[r.Kind] = r.ID
	}
	if _, ok := kinds[ledger.KindAvailable]; !ok {
		t.Fatalf("missing AVAILABLE: %+v", rows)
	}
	if _, ok := kinds[ledger.KindHold]; !ok {
		t.Fatalf("missing HOLD: %+v", rows)
	}
}

func requireGatewayPC400(t *testing.T, err error) {
	t.Helper()
	requireLedgerErr(t, err, ledger.ErrInvalidJournal)
	var pe *pgconn.PgError
	if !errors.As(err, &pe) || pe.Code != "PC400" {
		t.Fatalf("want PC400, got %v", err)
	}
	var ae *apperror.Error
	if !errors.As(err, &ae) || ae.Message != ledger.ErrInvalidJournal.Message {
		t.Fatalf("public Message leaked SQL: %v", err)
	}
}

func insertBusinessAccount(t *testing.T, ctx context.Context, owner, app *pgx.Conn, ids *idgen, number string, ownerType ledger.OwnerType) seededAccount {
	t.Helper()
	out := seededAccount{userID: ids.next(), accountID: ids.next()}
	switch ownerType {
	case ledger.OwnerCustomer:
		if _, err := owner.Exec(ctx, `INSERT INTO auth.users (id) VALUES ($1)`, out.userID); err != nil {
			t.Fatal(err)
		}
		if _, err := app.Exec(ctx, `
			INSERT INTO accounts.accounts (id, account_number, user_id, account_purpose, currency, status)
			VALUES ($1, $2, $3, 'MAIN', 'RUB', 'ACTIVE')`, out.accountID, number, out.userID); err != nil {
			t.Fatal(err)
		}
	case ledger.OwnerMerchant:
		if _, err := owner.Exec(ctx, `INSERT INTO merchants.merchants (id) VALUES ($1)`, out.userID); err != nil {
			t.Fatal(err)
		}
		if _, err := app.Exec(ctx, `
			INSERT INTO accounts.accounts (id, account_number, merchant_id, account_purpose, currency, status)
			VALUES ($1, $2, $3, 'MERCHANT_SETTLEMENT', 'RUB', 'ACTIVE')`, out.accountID, number, out.userID); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("owner %s", ownerType)
	}
	return out
}
