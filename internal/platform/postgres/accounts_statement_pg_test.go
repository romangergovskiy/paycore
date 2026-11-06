package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/romangergovskiy/paycore/internal/accounts/application"
	"github.com/romangergovskiy/paycore/internal/accounts/domain"
	"github.com/romangergovskiy/paycore/internal/ledger"
)

func TestAccountStatement(t *testing.T) {
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

	user := insertUser(t, ctx, owner)
	uow := NewAccountUoW(app)
	svc := application.Service{
		Store:      NewAccounts(app),
		Statements: NewAccountStatements(app),
	}
	q := application.StatementQuery{Limit: 10}

	t.Run("empty after Open", func(t *testing.T) {
		opened := mustOpen(t, ctx, uow, user, "PC-STMT-OPEN")
		got, err := svc.Statement(ctx, opened.Account.ID, q)
		if err != nil {
			t.Fatal(err)
		}
		if got.Lines == nil || len(got.Lines) != 0 || got.NextCursor != nil {
			t.Fatalf("empty %+v", got)
		}
	})

	t.Run("SIM_TOPUP maps this account only", func(t *testing.T) {
		opened := mustOpen(t, ctx, uow, user, "PC-STMT-TOP")
		other := mustOpen(t, ctx, uow, user, "PC-STMT-OTH")
		const amount int64 = 1500
		topupOpened(t, ctx, app, opened, amount)

		got, err := svc.Statement(ctx, opened.Account.ID, q)
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Lines) != 1 || got.NextCursor != nil {
			t.Fatalf("lines = %+v", got)
		}
		line := got.Lines[0]
		if line.Kind != ledger.KindAvailable || line.Side != ledger.Credit || line.Amount != amount {
			t.Fatalf("line %+v", line)
		}
		if line.Currency != "RUB" || line.JournalID == uuid.Nil || line.ID == uuid.Nil {
			t.Fatalf("ids %+v", line)
		}
		if line.BalanceAfter == nil || *line.BalanceAfter != amount {
			t.Fatalf("balance_after %+v", line.BalanceAfter)
		}
		if line.CreatedAt.IsZero() || line.PostedAt.IsZero() {
			t.Fatal("timestamps")
		}
		avail := ledgerID(t, opened, ledger.KindAvailable)
		if line.LedgerAccountID != avail {
			t.Fatalf("ledger %s want %s", line.LedgerAccountID, avail)
		}

		otherLines, err := svc.Statement(ctx, other.Account.ID, q)
		if err != nil {
			t.Fatal(err)
		}
		if len(otherLines.Lines) != 0 || otherLines.NextCursor != nil {
			t.Fatalf("other %+v", otherLines)
		}
	})

	t.Run("HOLD_PLACE includes AVAILABLE and HOLD", func(t *testing.T) {
		opened := mustOpen(t, ctx, uow, user, "PC-STMT-HLD")
		topupOpened(t, ctx, app, opened, 800)
		placeHoldOpened(t, ctx, app, opened, 200)

		got, err := svc.Statement(ctx, opened.Account.ID, q)
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Lines) != 3 {
			t.Fatalf("lines = %d %+v", len(got.Lines), got)
		}
		var sawAvailDebit, sawHoldCredit bool
		for _, line := range got.Lines {
			if line.Kind != ledger.KindAvailable && line.Kind != ledger.KindHold {
				t.Fatalf("kind %+v", line)
			}
			if line.Kind == ledger.KindAvailable && line.Side == ledger.Debit && line.Amount == 200 {
				sawAvailDebit = true
			}
			if line.Kind == ledger.KindHold && line.Side == ledger.Credit && line.Amount == 200 {
				sawHoldCredit = true
			}
		}
		if !sawAvailDebit || !sawHoldCredit {
			t.Fatalf("hold pair %+v", got)
		}
	})

	t.Run("limit orders by created_at id", func(t *testing.T) {
		opened := mustOpen(t, ctx, uow, user, "PC-STMT-PAGE")
		topupOpened(t, ctx, app, opened, 100)
		topupOpened(t, ctx, app, opened, 200)
		topupOpened(t, ctx, app, opened, 300)
		all, err := svc.Statement(ctx, opened.Account.ID, q)
		if err != nil {
			t.Fatal(err)
		}
		if len(all.Lines) != 3 || all.Lines[0].Amount != 100 || all.Lines[1].Amount != 200 || all.Lines[2].Amount != 300 {
			t.Fatalf("all %+v", all)
		}
		for i := 1; i < len(all.Lines); i++ {
			if all.Lines[i].CreatedAt.Before(all.Lines[i-1].CreatedAt) {
				t.Fatalf("created_at order %+v", all.Lines)
			}
			if all.Lines[i].CreatedAt.Equal(all.Lines[i-1].CreatedAt) && all.Lines[i].ID.String() <= all.Lines[i-1].ID.String() {
				t.Fatalf("id tiebreak %+v", all.Lines)
			}
		}
		page, err := svc.Statement(ctx, opened.Account.ID, application.StatementQuery{Limit: 2})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Lines) != 2 || page.NextCursor == nil {
			t.Fatalf("page %+v", page)
		}
		if page.Lines[0].ID != all.Lines[0].ID || page.Lines[1].ID != all.Lines[1].ID {
			t.Fatalf("page %+v want first of %+v", page.Lines, all.Lines)
		}
		if page.NextCursor.ID != page.Lines[1].ID {
			t.Fatalf("cursor from +1 row: %+v", page.NextCursor)
		}
		rest, err := svc.Statement(ctx, opened.Account.ID, application.StatementQuery{Limit: 2, Cursor: page.NextCursor})
		if err != nil {
			t.Fatal(err)
		}
		if len(rest.Lines) != 1 || rest.NextCursor != nil || rest.Lines[0].ID != all.Lines[2].ID {
			t.Fatalf("rest %+v", rest)
		}
	})

	t.Run("keyset walk no dupes or skips", func(t *testing.T) {
		opened := mustOpen(t, ctx, uow, user, "PC-STMT-WALK")
		for _, amount := range []int64{10, 20, 30, 40, 50} {
			topupOpened(t, ctx, app, opened, amount)
		}
		full, err := svc.Statement(ctx, opened.Account.ID, application.StatementQuery{Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		if len(full.Lines) != 5 || full.NextCursor != nil {
			t.Fatalf("full %+v", full)
		}
		walked := walkAccountStatement(t, ctx, svc, opened.Account.ID, 2)
		if len(walked) != 5 {
			t.Fatalf("walk %d", len(walked))
		}
		for i := range full.Lines {
			if walked[i].ID != full.Lines[i].ID || walked[i].Amount != full.Lines[i].Amount {
				t.Fatalf("walk[%d]=%+v want %+v", i, walked[i], full.Lines[i])
			}
		}
		exact := walkAccountStatement(t, ctx, svc, opened.Account.ID, 5)
		if len(exact) != 5 {
			t.Fatalf("exact %d", len(exact))
		}
	})

	t.Run("invalid cursor is error not empty page", func(t *testing.T) {
		opened := mustOpen(t, ctx, uow, user, "PC-STMT-CUR")
		topupOpened(t, ctx, app, opened, 7)
		got, err := svc.Statement(ctx, opened.Account.ID, application.StatementQuery{
			Limit: 10, Cursor: &application.StatementCursor{CreatedAt: time.Now().UTC(), ID: uuid.Nil},
		})
		if !errors.Is(err, domain.ErrInvalidAccount) || len(got.Lines) != 0 {
			t.Fatalf("nil cursor id: %+v %v", got, err)
		}
		got, err = svc.Statement(ctx, opened.Account.ID, application.StatementQuery{
			Limit: 10, Cursor: &application.StatementCursor{ID: mustNewID(t)},
		})
		if !errors.Is(err, domain.ErrInvalidAccount) || len(got.Lines) != 0 {
			t.Fatalf("zero time: %+v %v", got, err)
		}
		got, err = svc.Statement(ctx, opened.Account.ID, application.StatementQuery{Limit: 101})
		if !errors.Is(err, domain.ErrInvalidAccount) || len(got.Lines) != 0 {
			t.Fatalf("limit 101: %+v %v", got, err)
		}
	})

	t.Run("missing and invalid", func(t *testing.T) {
		if _, err := svc.Statement(ctx, uuid.MustParse("ffffffff-ffff-4fff-8fff-ffffffffffff"), q); !errors.Is(err, domain.ErrAccountNotFound) {
			t.Fatalf("missing: %v", err)
		}
		if _, err := svc.Statement(ctx, uuid.Nil, q); !errors.Is(err, domain.ErrAccountNotFound) {
			t.Fatalf("nil: %v", err)
		}
		opened := mustOpen(t, ctx, uow, user, "PC-STMT-INV")
		if _, err := svc.Statement(ctx, opened.Account.ID, application.StatementQuery{Limit: 0}); !errors.Is(err, domain.ErrInvalidAccount) {
			t.Fatalf("limit 0: %v", err)
		}
	})

	t.Run("canceled context", func(t *testing.T) {
		opened := mustOpen(t, ctx, uow, user, "PC-STMT-CXL")
		dead, stop := context.WithCancel(ctx)
		stop()
		if _, err := svc.Statement(dead, opened.Account.ID, q); !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("read does not write", func(t *testing.T) {
		opened := mustOpen(t, ctx, uow, user, "PC-STMT-RO")
		topupOpened(t, ctx, app, opened, 50)
		var entries, journals int
		if err := app.QueryRow(ctx, `SELECT count(*) FROM ledger.ledger_entries`).Scan(&entries); err != nil {
			t.Fatal(err)
		}
		if err := app.QueryRow(ctx, `SELECT count(*) FROM ledger.journal_transactions`).Scan(&journals); err != nil {
			t.Fatal(err)
		}
		var version int64
		var updated time.Time
		if err := app.QueryRow(ctx, `
SELECT version, updated_at FROM ledger.account_balances WHERE account_id = $1`, opened.Account.ID).Scan(&version, &updated); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.Statement(ctx, opened.Account.ID, q); err != nil {
			t.Fatal(err)
		}
		var entriesAfter, journalsAfter int
		if err := app.QueryRow(ctx, `SELECT count(*) FROM ledger.ledger_entries`).Scan(&entriesAfter); err != nil {
			t.Fatal(err)
		}
		if err := app.QueryRow(ctx, `SELECT count(*) FROM ledger.journal_transactions`).Scan(&journalsAfter); err != nil {
			t.Fatal(err)
		}
		var versionAfter int64
		var updatedAfter time.Time
		if err := app.QueryRow(ctx, `
SELECT version, updated_at FROM ledger.account_balances WHERE account_id = $1`, opened.Account.ID).Scan(&versionAfter, &updatedAfter); err != nil {
			t.Fatal(err)
		}
		if entriesAfter != entries || journalsAfter != journals || versionAfter != version || !updatedAfter.Equal(updated) {
			t.Fatalf("write on read entries %d→%d journals %d→%d version %d→%d", entries, entriesAfter, journals, journalsAfter, version, versionAfter)
		}
	})
}

func walkAccountStatement(t *testing.T, ctx context.Context, svc application.Service, id uuid.UUID, limit int) []application.StatementLine {
	t.Helper()
	var (
		all    []application.StatementLine
		cursor *application.StatementCursor
		seen   = map[uuid.UUID]bool{}
	)
	for i := 0; i < 32; i++ {
		page, err := svc.Statement(ctx, id, application.StatementQuery{Limit: limit, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range page.Lines {
			if seen[line.ID] {
				t.Fatalf("duplicate %s", line.ID)
			}
			seen[line.ID] = true
			all = append(all, line)
		}
		if page.NextCursor == nil {
			return all
		}
		if len(page.Lines) != limit {
			t.Fatalf("NextCursor with short page %d", len(page.Lines))
		}
		last := page.Lines[len(page.Lines)-1]
		if page.NextCursor.ID != last.ID || !page.NextCursor.CreatedAt.Equal(last.CreatedAt) {
			t.Fatalf("NextCursor %+v last %s", page.NextCursor, last.ID)
		}
		cursor = page.NextCursor
	}
	t.Fatal("walk did not end")
	return all
}

func ledgerID(t *testing.T, opened application.Opened, kind ledger.AccountKind) uuid.UUID {
	t.Helper()
	for _, l := range opened.Ledgers {
		if l.Kind == kind {
			return l.ID
		}
	}
	t.Fatalf("no %s ledger", kind)
	return uuid.Nil
}

func placeHoldOpened(t *testing.T, ctx context.Context, app *pgx.Conn, opened application.Opened, amount int64) {
	t.Helper()
	avail := ledgerID(t, opened, ledger.KindAvailable)
	hold := ledgerID(t, opened, ledger.KindHold)
	holdID := mustNewID(t)
	payRef := mustNewID(t)
	exp := time.Now().UTC().Add(24 * time.Hour)
	cmd := ledger.Command{
		JournalID:      mustNewID(t),
		Operation:      ledger.OpHoldPlace,
		ReferenceType:  "HOLD",
		ReferenceID:    holdID,
		AccountingDate: time.Now().UTC(),
		Entries: []ledger.Entry{
			{ID: mustNewID(t), LedgerAccountID: avail, Type: ledger.Debit, Amount: amount, Currency: "RUB"},
			{ID: mustNewID(t), LedgerAccountID: hold, Type: ledger.Credit, Amount: amount, Currency: "RUB"},
		},
		Hold: &ledger.HoldAction{
			Action:        ledger.HoldPlace,
			HoldID:        holdID,
			AccountID:     opened.Account.ID,
			Amount:        amount,
			Currency:      "RUB",
			ExpiresAt:     &exp,
			ReferenceType: "PAYMENT",
			ReferenceID:   &payRef,
		},
	}
	if _, err := postLedger(ctx, app, []string{opened.Account.ID.String()}, cmd); err != nil {
		t.Fatal(err)
	}
}
