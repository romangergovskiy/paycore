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

func TestPostEmptyResult(t *testing.T) {
	cmd := ledger.Command{
		JournalID:      uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"),
		Operation:      ledger.OpSimTopup,
		ReferenceType:  "SIM_TOPUP",
		ReferenceID:    uuid.MustParse("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"),
		AccountingDate: time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC),
		Entries: []ledger.Entry{{
			ID:              uuid.MustParse("cccccccc-cccc-4ccc-8ccc-cccccccccccc"),
			LedgerAccountID: uuid.MustParse("dddddddd-dddd-4ddd-8ddd-dddddddddddd"),
			Type:            ledger.Debit, Amount: 1, Currency: "RUB",
		}},
	}
	res, err := NewLedger(emptyQueryer{}).Post(context.Background(), cmd)
	if err == nil {
		t.Fatal("empty post_journal result must be an error")
	}
	if res.JournalID != uuid.Nil || res.AlreadyPosted || len(res.Entries) != 0 {
		t.Fatalf("want zero Result, got %+v", res)
	}
	if !errors.Is(err, errEmptyJournal) {
		t.Fatalf("want errEmptyJournal, got %v", err)
	}
	for _, s := range []error{
		ledger.ErrInvalidJournal,
		ledger.ErrInsufficientFunds,
		ledger.ErrSystemAccountInsufficient,
		ledger.ErrAccountNotActive,
		ledger.ErrConflict,
		ledger.ErrLockProtocol,
	} {
		if errors.Is(err, s) {
			t.Fatalf("empty result mapped to PC sentinel %v", s)
		}
	}
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		t.Fatalf("empty result must not carry PgError, got %v", pe)
	}
	var ae *apperror.Error
	if errors.As(err, &ae) {
		t.Fatalf("empty result must not be apperror, got %v", ae)
	}
}

type emptyQueryer struct{}

func (emptyQueryer) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return emptyRows{}, nil
}

type emptyRows struct{}

func (emptyRows) Close()                                       {}
func (emptyRows) Err() error                                   { return nil }
func (emptyRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (emptyRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (emptyRows) Next() bool                                   { return false }
func (emptyRows) Scan(...any) error                            { return errors.New("scan on empty rows") }
func (emptyRows) Values() ([]any, error)                       { return nil, errors.New("values on empty rows") }
func (emptyRows) RawValues() [][]byte                          { return nil }
func (emptyRows) Conn() *pgx.Conn                              { return nil }

func TestMapPostJournalError(t *testing.T) {
	ser := &pgconn.PgError{Code: "40001", Message: "could not serialize access"}
	err := mapPostJournalError(ser)
	if errors.Is(err, ledger.ErrConflict) || errors.Is(err, ledger.ErrInvalidJournal) {
		t.Fatalf("serialization mapped to domain PC: %v", err)
	}
	var pe *pgconn.PgError
	if !errors.As(err, &pe) || pe.Code != "40001" {
		t.Fatalf("want wrapped 40001, got %v", err)
	}

	dead := mapPostJournalError(&pgconn.PgError{Code: "40P01", Message: "deadlock detected"})
	if errors.Is(dead, ledger.ErrConflict) {
		t.Fatal("deadlock mapped to CONFLICT")
	}

	pc := mapPostJournalError(&pgconn.PgError{Code: "PC400", Message: "INVALID_JOURNAL: unbalanced"})
	if !errors.Is(pc, ledger.ErrInvalidJournal) {
		t.Fatalf("PC400: %v", pc)
	}
	if !errors.As(pc, &pe) || pe.Code != "PC400" {
		t.Fatalf("PC400 lost PgError: %v", pc)
	}
	var ae *apperror.Error
	if !errors.As(pc, &ae) || ae.Kind != apperror.KindInvalid || ae.Code != ledger.CodeInvalidJournal {
		t.Fatalf("PC400 missing apperror conversion: %v", pc)
	}

	sys := mapPostJournalError(&pgconn.PgError{Code: "PC402", Message: "SYSTEM_ACCOUNT_INSUFFICIENT"})
	if !errors.Is(sys, ledger.ErrSystemAccountInsufficient) || errors.Is(sys, ledger.ErrInsufficientFunds) {
		t.Fatalf("system PC402: %v", sys)
	}
}

func TestLedgerPost(t *testing.T) {
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
	cust := seedCustomer(t, ctx, owner, app, ids, "PC-LG-1")
	recv := seedCustomer(t, ctx, owner, app, ids, "PC-LG-2")
	day := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)
	sys := mustUUID(t, sys1100)

	t.Run("SIM_TOPUP multiple entries", func(t *testing.T) {
		cmd := simTopupCmd(t, ids, cust, sys, 5000, day)
		res, err := postLedger(ctx, app, []string{cust.accountID}, cmd)
		if err != nil {
			t.Fatalf("Post: %v", err)
		}
		if res.AlreadyPosted || res.JournalID != cmd.JournalID || len(res.Entries) != 2 {
			t.Fatalf("result = %+v", res)
		}
		var nBiz int
		for _, e := range res.Entries {
			if e.AccountVersion != nil {
				nBiz++
				if *e.AccountVersion < 1 || e.BalanceAfter == nil || *e.BalanceAfter != 5000 {
					t.Fatalf("business entry = %+v", e)
				}
			}
		}
		if nBiz != 1 {
			t.Fatalf("business entries = %d", nBiz)
		}
		var avail int64
		if err := app.QueryRow(ctx, `SELECT available_amount FROM ledger.account_balances WHERE account_id = $1`, cust.accountID).Scan(&avail); err != nil {
			t.Fatal(err)
		}
		if avail != 5000 {
			t.Fatalf("avail = %d", avail)
		}
	})

	t.Run("nullable args and accounting_date", func(t *testing.T) {
		cmd := simTopupCmd(t, ids, cust, sys, 100, day)
		cmd.Description = ""
		cmd.InitiatedBy = nil
		cmd.ReversesJournalID = nil
		cmd.AdjustmentID = nil
		cmd.Hold = nil
		cmd.AccountingDate = time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)
		res, err := postLedger(ctx, app, []string{cust.accountID}, cmd)
		if err != nil {
			t.Fatalf("Post: %v", err)
		}
		var desc *string
		var initiated *string
		var acct time.Time
		if err := app.QueryRow(ctx, `
			SELECT description, initiated_by::text, accounting_date
			  FROM ledger.journal_transactions WHERE id = $1`, res.JournalID).Scan(&desc, &initiated, &acct); err != nil {
			t.Fatal(err)
		}
		if desc != nil && *desc != "" {
			t.Fatalf("description = %v", desc)
		}
		if initiated != nil {
			t.Fatalf("initiated_by = %v", initiated)
		}
		if acct.Format("2006-01-02") != "2026-01-15" {
			t.Fatalf("accounting_date = %s", acct.Format("2006-01-02"))
		}
	})

	t.Run("AlreadyPosted", func(t *testing.T) {
		cmd := simTopupCmd(t, ids, cust, sys, 40, day)
		first, err := postLedger(ctx, app, []string{cust.accountID}, cmd)
		if err != nil {
			t.Fatal(err)
		}
		replay := cmd
		replay.JournalID = mustUUID(t, ids.next())
		second, err := postLedger(ctx, app, []string{cust.accountID}, replay)
		if err != nil {
			t.Fatal(err)
		}
		if !second.AlreadyPosted || second.JournalID != first.JournalID {
			t.Fatalf("replay = %+v first = %+v", second, first)
		}
	})

	t.Run("PC409 conflict", func(t *testing.T) {
		cmd := simTopupCmd(t, ids, cust, sys, 15, day)
		if _, err := postLedger(ctx, app, []string{cust.accountID}, cmd); err != nil {
			t.Fatal(err)
		}
		other := cmd
		other.JournalID = mustUUID(t, ids.next())
		other.Entries = []ledger.Entry{
			{ID: mustUUID(t, ids.next()), LedgerAccountID: sys, Type: ledger.Debit, Amount: 16, Currency: "RUB"},
			{ID: mustUUID(t, ids.next()), LedgerAccountID: mustUUID(t, cust.avail), Type: ledger.Credit, Amount: 16, Currency: "RUB"},
		}
		_, err := postLedger(ctx, app, []string{cust.accountID}, other)
		requireLedgerErr(t, err, ledger.ErrConflict)
	})

	t.Run("PC402 insufficient funds", func(t *testing.T) {
		cmd := ledger.Command{
			JournalID:      mustUUID(t, ids.next()),
			Operation:      ledger.OpTransfer,
			ReferenceType:  "TRANSFER",
			ReferenceID:    mustUUID(t, ids.next()),
			AccountingDate: day,
			Entries: []ledger.Entry{
				{ID: mustUUID(t, ids.next()), LedgerAccountID: mustUUID(t, recv.avail), Type: ledger.Debit, Amount: 1_000_000, Currency: "RUB"},
				{ID: mustUUID(t, ids.next()), LedgerAccountID: mustUUID(t, cust.avail), Type: ledger.Credit, Amount: 1_000_000, Currency: "RUB"},
			},
		}
		_, err := postLedger(ctx, app, []string{cust.accountID, recv.accountID}, cmd)
		requireLedgerErr(t, err, ledger.ErrInsufficientFunds)
	})

	t.Run("HOLD_PLACE", func(t *testing.T) {
		funded := seedCustomer(t, ctx, owner, app, ids, "PC-LG-H")
		topupCmd := simTopupCmd(t, ids, funded, sys, 800, day)
		if _, err := postLedger(ctx, app, []string{funded.accountID}, topupCmd); err != nil {
			t.Fatal(err)
		}
		holdID := mustUUID(t, ids.next())
		payRef := mustUUID(t, ids.next())
		exp := time.Date(2026, 12, 1, 12, 0, 0, 0, time.UTC)
		cmd := ledger.Command{
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
		res, err := postLedger(ctx, app, []string{funded.accountID}, cmd)
		if err != nil {
			t.Fatalf("HOLD_PLACE: %v", err)
		}
		if res.AlreadyPosted || len(res.Entries) != 2 {
			t.Fatalf("HOLD_PLACE result = %+v", res)
		}
		var status string
		var amount int64
		if err := app.QueryRow(ctx, `SELECT status, amount FROM ledger.holds WHERE id = $1`, holdID).Scan(&status, &amount); err != nil {
			t.Fatal(err)
		}
		if status != "ACTIVE" || amount != 200 {
			t.Fatalf("hold status=%s amount=%d", status, amount)
		}
	})

	t.Run("ctx cancel", func(t *testing.T) {
		tx, err := app.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		_, err = NewLedger(tx).Post(canceled, simTopupCmd(t, ids, cust, sys, 1, day))
		if err == nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("want context.Canceled, got %v", err)
		}
		var pe *pgconn.PgError
		if errors.As(err, &pe) && isPaycoreSQLState(pe.Code) {
			t.Fatalf("ctx cancel mapped to PC code %s", pe.Code)
		}
	})

	t.Run("app still cannot DML financial tables", func(t *testing.T) {
		mustFail(t, ctx, app, `INSERT INTO ledger.journal_transactions (
			id, operation, reference_type, reference_id, accounting_date, status)
			VALUES ($1, 'SIM_TOPUP', 'SIM_TOPUP', $1, CURRENT_DATE, 'POSTED')`, ids.next())
		mustFail(t, ctx, app, `UPDATE ledger.account_balances SET available_amount = 0 WHERE account_id = $1`, cust.accountID)
	})
}

func postLedger(ctx context.Context, conn *pgx.Conn, lockIDs []string, cmd ledger.Command) (ledger.Result, error) {
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
		if _, err := tx.Exec(ctx, `SELECT * FROM ledger.lock_account_balances($1::uuid[])`, lockIDs); err != nil {
			return ledger.Result{}, err
		}
	}
	res, err := NewLedger(tx).Post(ctx, cmd)
	if err != nil {
		return ledger.Result{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ledger.Result{}, err
	}
	return res, nil
}

func simTopupCmd(t *testing.T, ids *idgen, acc seededAccount, sys uuid.UUID, amount int64, day time.Time) ledger.Command {
	t.Helper()
	return ledger.Command{
		JournalID:      mustUUID(t, ids.next()),
		Operation:      ledger.OpSimTopup,
		ReferenceType:  "SIM_TOPUP",
		ReferenceID:    mustUUID(t, ids.next()),
		AccountingDate: day,
		Entries: []ledger.Entry{
			{ID: mustUUID(t, ids.next()), LedgerAccountID: sys, Type: ledger.Debit, Amount: amount, Currency: "RUB"},
			{ID: mustUUID(t, ids.next()), LedgerAccountID: mustUUID(t, acc.avail), Type: ledger.Credit, Amount: amount, Currency: "RUB"},
		},
	}
}

func requireLedgerErr(t *testing.T, err error, sentinel error) {
	t.Helper()
	if err == nil {
		t.Fatalf("want %v, got nil", sentinel)
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("want %v, got %v", sentinel, err)
	}
	var pe *pgconn.PgError
	if !errors.As(err, &pe) {
		t.Fatalf("missing wrapped PgError in %v", err)
	}
	var ae *apperror.Error
	if !errors.As(err, &ae) {
		t.Fatalf("missing apperror conversion outside domain in %v", err)
	}
}

func isPaycoreSQLState(code string) bool {
	return len(code) == 5 && code[0] == 'P' && code[1] == 'C'
}

func mustUUID(t *testing.T, s string) uuid.UUID {
	t.Helper()
	id, err := uuid.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return id
}
