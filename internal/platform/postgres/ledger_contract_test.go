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
)

// TestLedgerPosterContract is the Go Poster ↔ PostgreSQL contract (stage 3.7).
// It does not clone TestPostJournalGateway (SQL forms, idempotency, SQL races).
func TestLedgerPosterContract(t *testing.T) {
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
	ids := &idgen{}
	cust := seedCustomer(t, ctx, owner, app, ids, "PC-CT-1")
	peer := seedCustomer(t, ctx, owner, app, ids, "PC-CT-2")
	merch := seedMerchant(t, ctx, owner, app, ids, "PC-CT-M")
	day := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)
	sys := mustUUID(t, sys1100)

	t.Run("A WithValidation real NewLedger", func(t *testing.T) {
		before := snapshotLedger(t, ctx, app, cust.accountID)
		bad := simTopupCmd(t, ids, cust, sys, 50, day)
		bad.Entries = bad.Entries[:1]
		if err := ledger.Validate(bad); err == nil {
			t.Fatal("fixture must fail Validate")
		}
		_, err := postValidated(ctx, app, []string{cust.accountID}, bad)
		if !errors.Is(err, ledger.ErrInvalidJournal) {
			t.Fatalf("invalid Command: %v", err)
		}
		var pe *pgconn.PgError
		if errors.As(err, &pe) {
			t.Fatalf("Validate must not hit SQL, got PgError %s", pe.Code)
		}
		requireSnap(t, snapshotLedger(t, ctx, app, cust.accountID), before)

		res, err := postValidated(ctx, app, []string{cust.accountID}, simTopupCmd(t, ids, cust, sys, 50, day))
		if err != nil {
			t.Fatalf("valid WithValidation Post: %v", err)
		}
		if res.AlreadyPosted || len(res.Entries) != 2 {
			t.Fatalf("result = %+v", res)
		}
		assertJournalI01(t, ctx, app, res.JournalID)
	})

	t.Run("B Poster error Rollback", func(t *testing.T) {
		beforeCust := snapshotLedger(t, ctx, app, cust.accountID)
		beforePeer := snapshotLedger(t, ctx, app, peer.accountID)
		exp := time.Date(2026, 12, 1, 12, 0, 0, 0, time.UTC)
		pay := mustUUID(t, ids.next())
		cmd := ledger.Command{
			JournalID:      mustUUID(t, ids.next()),
			Operation:      ledger.OpHoldPlace,
			ReferenceType:  "HOLD",
			ReferenceID:    mustUUID(t, ids.next()),
			AccountingDate: day,
			Entries: []ledger.Entry{
				{ID: mustUUID(t, ids.next()), LedgerAccountID: mustUUID(t, cust.avail), Type: ledger.Debit, Amount: 10, Currency: "RUB"},
				{ID: mustUUID(t, ids.next()), LedgerAccountID: mustUUID(t, peer.avail), Type: ledger.Credit, Amount: 10, Currency: "RUB"},
			},
			Hold: &ledger.HoldAction{
				Action:        ledger.HoldPlace,
				HoldID:        mustUUID(t, ids.next()),
				AccountID:     mustUUID(t, cust.accountID),
				Amount:        10,
				Currency:      "RUB",
				ExpiresAt:     &exp,
				ReferenceType: "PAYMENT",
				ReferenceID:   &pay,
			},
		}
		if err := ledger.Validate(cmd); err != nil {
			t.Fatalf("fixture must pass Validate: %v", err)
		}
		_, err := postValidated(ctx, app, []string{cust.accountID, peer.accountID}, cmd)
		requireLedgerErr(t, err, ledger.ErrInvalidJournal)
		requireSnap(t, snapshotLedger(t, ctx, app, cust.accountID), beforeCust)
		requireSnap(t, snapshotLedger(t, ctx, app, peer.accountID), beforePeer)
	})

	t.Run("C PC400 through Poster", func(t *testing.T) {
		cmd := ledger.Command{
			JournalID:      mustUUID(t, ids.next()),
			Operation:      ledger.OpSimTopup,
			ReferenceType:  "SIM_TOPUP",
			ReferenceID:    mustUUID(t, ids.next()),
			AccountingDate: day,
			Entries: []ledger.Entry{
				{ID: mustUUID(t, ids.next()), LedgerAccountID: mustUUID(t, cust.avail), Type: ledger.Debit, Amount: 7, Currency: "RUB"},
				{ID: mustUUID(t, ids.next()), LedgerAccountID: mustUUID(t, peer.avail), Type: ledger.Credit, Amount: 7, Currency: "RUB"},
			},
		}
		if err := ledger.Validate(cmd); err != nil {
			t.Fatalf("fixture must pass Validate: %v", err)
		}
		_, err := postValidated(ctx, app, []string{cust.accountID, peer.accountID}, cmd)
		requireLedgerErr(t, err, ledger.ErrInvalidJournal)
		var pe *pgconn.PgError
		if !errors.As(err, &pe) || pe.Code != "PC400" {
			t.Fatalf("want live PC400, got %v", err)
		}
	})

	t.Run("D PC403", func(t *testing.T) {
		blocked := seedCustomer(t, ctx, owner, app, ids, "PC-CT-BLK")
		if _, err := app.Exec(ctx, `UPDATE accounts.accounts SET status = 'BLOCKED', version = version + 1 WHERE id = $1`, blocked.accountID); err != nil {
			t.Fatal(err)
		}
		before := snapshotLedger(t, ctx, app, blocked.accountID)
		cmd := simTopupCmd(t, ids, blocked, sys, 5, day)
		if err := ledger.Validate(cmd); err != nil {
			t.Fatalf("fixture must pass Validate: %v", err)
		}
		_, err := postValidated(ctx, app, []string{blocked.accountID}, cmd)
		requireLedgerErr(t, err, ledger.ErrAccountNotActive)
		var pe *pgconn.PgError
		if !errors.As(err, &pe) || pe.Code != "PC403" {
			t.Fatalf("want live PC403, got %v", err)
		}
		requireSnap(t, snapshotLedger(t, ctx, app, blocked.accountID), before)
	})

	t.Run("E PC460 two connections", func(t *testing.T) {
		target := seedCustomer(t, ctx, owner, app, ids, "PC-CT-460")
		blocker, err := pgx.Connect(ctx, c.app)
		if err != nil {
			t.Fatal(err)
		}
		defer blocker.Close(ctx)
		btx, err := blocker.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = btx.Rollback(ctx) }()
		if _, err := btx.Exec(ctx, `SELECT id FROM accounts.accounts WHERE id = $1 FOR NO KEY UPDATE`, target.accountID); err != nil {
			t.Fatal(err)
		}

		cmd := simTopupCmd(t, ids, target, sys, 3, day)
		if err := ledger.Validate(cmd); err != nil {
			t.Fatalf("fixture must pass Validate: %v", err)
		}
		started := time.Now()
		_, err = postValidated(ctx, app, nil, cmd)
		elapsed := time.Since(started)
		requireLedgerErr(t, err, ledger.ErrLockProtocol)
		var pe *pgconn.PgError
		if !errors.As(err, &pe) || pe.Code != "PC460" {
			t.Fatalf("want live PC460, got %v", err)
		}
		if elapsed > 2*time.Second {
			t.Fatalf("PC460 waited %s, want NOWAIT", elapsed)
		}
	})

	t.Run("F HOLD_CAPTURE fee=0 Command", func(t *testing.T) {
		funded := seedCustomer(t, ctx, owner, app, ids, "PC-CT-CAP")
		if _, err := postValidated(ctx, app, []string{funded.accountID}, simTopupCmd(t, ids, funded, sys, 500, day)); err != nil {
			t.Fatal(err)
		}
		holdID := mustUUID(t, ids.next())
		payRef := mustUUID(t, ids.next())
		exp := time.Date(2026, 12, 5, 0, 0, 0, 0, time.UTC)
		place := ledger.Command{
			JournalID:      mustUUID(t, ids.next()),
			Operation:      ledger.OpHoldPlace,
			ReferenceType:  "HOLD",
			ReferenceID:    holdID,
			AccountingDate: day,
			Entries: []ledger.Entry{
				{ID: mustUUID(t, ids.next()), LedgerAccountID: mustUUID(t, funded.avail), Type: ledger.Debit, Amount: 200, Currency: "RUB"},
				{ID: mustUUID(t, ids.next()), LedgerAccountID: mustUUID(t, funded.hold), Type: ledger.Credit, Amount: 200, Currency: "RUB"},
			},
			Hold: &ledger.HoldAction{
				Action:        ledger.HoldPlace,
				HoldID:        holdID,
				AccountID:     mustUUID(t, funded.accountID),
				Amount:        200,
				Currency:      "RUB",
				ExpiresAt:     &exp,
				ReferenceType: "PAYMENT",
				ReferenceID:   &payRef,
			},
		}
		if _, err := postValidated(ctx, app, []string{funded.accountID}, place); err != nil {
			t.Fatalf("HOLD_PLACE setup: %v", err)
		}

		capCmd := ledger.Command{
			JournalID:      mustUUID(t, ids.next()),
			Operation:      ledger.OpHoldCapture,
			ReferenceType:  "HOLD",
			ReferenceID:    mustUUID(t, ids.next()),
			AccountingDate: day,
			Entries: []ledger.Entry{
				{ID: mustUUID(t, ids.next()), LedgerAccountID: mustUUID(t, funded.hold), Type: ledger.Debit, Amount: 200, Currency: "RUB"},
				{ID: mustUUID(t, ids.next()), LedgerAccountID: mustUUID(t, merch.avail), Type: ledger.Credit, Amount: 200, Currency: "RUB"},
			},
			Hold: &ledger.HoldAction{
				Action:    ledger.HoldCapture,
				HoldID:    holdID,
				AccountID: mustUUID(t, funded.accountID),
				Amount:    200,
				Currency:  "RUB",
			},
		}
		if err := ledger.Validate(capCmd); err != nil {
			t.Fatalf("CAPTURE must pass Validate: %v", err)
		}
		res, err := postValidated(ctx, app, []string{funded.accountID, merch.accountID}, capCmd)
		if err != nil {
			t.Fatalf("HOLD_CAPTURE: %v", err)
		}
		if res.AlreadyPosted || len(res.Entries) != 2 {
			t.Fatalf("CAPTURE result = %+v", res)
		}
		var avail, reserved, merchAvail int64
		if err := app.QueryRow(ctx, `SELECT available_amount, reserved_amount FROM ledger.account_balances WHERE account_id = $1`, funded.accountID).Scan(&avail, &reserved); err != nil {
			t.Fatal(err)
		}
		if err := app.QueryRow(ctx, `SELECT available_amount FROM ledger.account_balances WHERE account_id = $1`, merch.accountID).Scan(&merchAvail); err != nil {
			t.Fatal(err)
		}
		if avail != 300 || reserved != 0 || merchAvail != 200 {
			t.Fatalf("fee=0 capture avail=%d reserved=%d merch=%d", avail, reserved, merchAvail)
		}
		var status string
		if err := app.QueryRow(ctx, `SELECT status FROM ledger.holds WHERE id = $1`, holdID).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status != "CAPTURED" {
			t.Fatalf("hold status = %s", status)
		}
		var n4200 int64
		if err := app.QueryRow(ctx, `
			SELECT count(*) FROM ledger.ledger_entries
			 WHERE journal_transaction_id = $1 AND ledger_account_id = $2`, res.JournalID, sys4200).Scan(&n4200); err != nil {
			t.Fatal(err)
		}
		if n4200 != 0 {
			t.Fatalf("fee=0 capture wrote 4200: %d", n4200)
		}
		assertJournalI01(t, ctx, app, res.JournalID)
	})

	t.Run("G I-01 debit equals credit from PG", func(t *testing.T) {
		res, err := postValidated(ctx, app, []string{cust.accountID}, simTopupCmd(t, ids, cust, sys, 11, day))
		if err != nil {
			t.Fatal(err)
		}
		assertJournalI01(t, ctx, app, res.JournalID)
	})
}

func postValidated(ctx context.Context, conn *pgx.Conn, lockIDs []string, cmd ledger.Command) (ledger.Result, error) {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return ledger.Result{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '3s'`); err != nil {
		return ledger.Result{}, err
	}
	if len(lockIDs) > 0 {
		if err := shareAccounts(ctx, tx, lockIDs...); err != nil {
			return ledger.Result{}, err
		}
		if err := lockBalances(ctx, tx, lockIDs); err != nil {
			return ledger.Result{}, err
		}
	}
	res, err := ledger.WithValidation(NewLedger(tx)).Post(ctx, cmd)
	if err != nil {
		return ledger.Result{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ledger.Result{}, err
	}
	return res, nil
}

type ledgerSnap struct {
	journals, holds, avail, reserved int64
}

func snapshotLedger(t *testing.T, ctx context.Context, app *pgx.Conn, accountID string) ledgerSnap {
	t.Helper()
	var s ledgerSnap
	if err := app.QueryRow(ctx, `SELECT count(*) FROM ledger.journal_transactions`).Scan(&s.journals); err != nil {
		t.Fatal(err)
	}
	if err := app.QueryRow(ctx, `SELECT count(*) FROM ledger.holds`).Scan(&s.holds); err != nil {
		t.Fatal(err)
	}
	if err := app.QueryRow(ctx, `
		SELECT available_amount, reserved_amount FROM ledger.account_balances WHERE account_id = $1`, accountID).Scan(&s.avail, &s.reserved); err != nil {
		t.Fatal(err)
	}
	return s
}

func requireSnap(t *testing.T, got, want ledgerSnap) {
	t.Helper()
	if got != want {
		t.Fatalf("snapshot got %+v want %+v", got, want)
	}
}

func assertJournalI01(t *testing.T, ctx context.Context, app *pgx.Conn, journalID uuid.UUID) {
	t.Helper()
	rows, err := app.Query(ctx, `
		SELECT currency::text,
		       sum(CASE WHEN entry_type = 'DEBIT' THEN amount ELSE 0 END),
		       sum(CASE WHEN entry_type = 'CREDIT' THEN amount ELSE 0 END),
		       count(*)
		  FROM ledger.ledger_entries
		 WHERE journal_transaction_id = $1
		 GROUP BY currency`, journalID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var cur string
		var debit, credit, cnt int64
		if err := rows.Scan(&cur, &debit, &credit, &cnt); err != nil {
			t.Fatal(err)
		}
		if cnt < 1 || debit != credit {
			t.Fatalf("I-01 %s debit=%d credit=%d n=%d", cur, debit, credit, cnt)
		}
		n++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("I-01: no entries in PostgreSQL")
	}
}
