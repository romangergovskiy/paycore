package postgres

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/romangergovskiy/paycore/internal/accounts/application"
	"github.com/romangergovskiy/paycore/internal/accounts/domain"
	"github.com/romangergovskiy/paycore/internal/ledger"
	"github.com/romangergovskiy/paycore/pkg/money"
)

func TestAccountStatusTransitions(t *testing.T) {
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
	other := mustConnect(t, ctx, c.app)
	defer other.Close(ctx)
	ro := mustConnect(t, ctx, c.readonly)
	defer ro.Close(ctx)

	user := insertUser(t, ctx, owner)
	uow := NewAccountUoW(app)
	actor := mustNewID(t)

	t.Run("A close empty after OpenIn", func(t *testing.T) {
		opened := mustOpen(t, ctx, uow, user, "PC-ST-A")
		got, err := application.CloseIn(ctx, uow, domain.StatusCommand{
			ID: opened.Account.ID, Reason: "empty", ChangedBy: actor,
		})
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != domain.StatusClosed || got.ClosedAt == nil || got.Version != 1 {
			t.Fatalf("closed %+v", got)
		}
		row, err := NewAccounts(app).Get(ctx, opened.Account.ID)
		if err != nil || row.Status != domain.StatusClosed || row.ClosedAt == nil || row.Version != 1 {
			t.Fatalf("Get %+v %v", row, err)
		}
		h := history(t, ctx, app, opened.Account.ID)
		if len(h) != 1 || h[0].from != "ACTIVE" || h[0].to != "CLOSED" || h[0].changedBy != actor {
			t.Fatalf("history %+v", h)
		}
	})

	t.Run("B block then unblock", func(t *testing.T) {
		opened := mustOpen(t, ctx, uow, user, "PC-ST-B")
		if _, err := application.BlockIn(ctx, uow, domain.StatusCommand{
			ID: opened.Account.ID, Reason: "aml", ChangedBy: actor,
		}); err != nil {
			t.Fatal(err)
		}
		got, err := application.UnblockIn(ctx, uow, domain.StatusCommand{
			ID: opened.Account.ID, Reason: "cleared", ChangedBy: actor,
		})
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != domain.StatusActive || got.ClosedAt != nil || got.Version != 2 {
			t.Fatalf("unblocked %+v", got)
		}
		h := history(t, ctx, app, opened.Account.ID)
		if len(h) != 2 || h[0].to != "BLOCKED" || h[1].to != "ACTIVE" {
			t.Fatalf("history %+v", h)
		}
		if h[0].changedBy != actor || h[1].changedBy != actor {
			t.Fatalf("changed_by %+v actor %s", h, actor)
		}
	})

	t.Run("C close with available is not empty", func(t *testing.T) {
		opened := mustOpen(t, ctx, uow, user, "PC-ST-C")
		topupOpened(t, ctx, app, opened, 250)
		_, err := application.CloseIn(ctx, uow, domain.StatusCommand{
			ID: opened.Account.ID, Reason: "try", ChangedBy: actor,
		})
		if !errors.Is(err, domain.ErrAccountNotEmpty) {
			t.Fatalf("got %v", err)
		}
		row, err := NewAccounts(app).Get(ctx, opened.Account.ID)
		if err != nil || row.Status != domain.StatusActive || row.ClosedAt != nil {
			t.Fatalf("status %+v %v", row, err)
		}
		if n := len(history(t, ctx, app, opened.Account.ID)); n != 0 {
			t.Fatalf("history %d", n)
		}
	})

	t.Run("D error after L3 rolls back", func(t *testing.T) {
		opened := mustOpen(t, ctx, uow, user, "PC-ST-D")
		forced := errors.New("after l3")
		err := uow.Do(ctx, func(ctx context.Context, d application.Deps) error {
			if _, err := d.Store.GetForUpdate(ctx, opened.Account.ID); err != nil {
				return err
			}
			return forced
		})
		if !errors.Is(err, forced) {
			t.Fatalf("got %v", err)
		}
		row, err := NewAccounts(app).Get(ctx, opened.Account.ID)
		if err != nil || row.Status != domain.StatusActive {
			t.Fatalf("status %+v %v", row, err)
		}
		if n := len(history(t, ctx, app, opened.Account.ID)); n != 0 {
			t.Fatalf("history %d", n)
		}
	})

	t.Run("D2 AppendHistory error rolls back status", func(t *testing.T) {
		opened := mustOpen(t, ctx, uow, user, "PC-ST-D2")
		forced := errors.New("history failed")
		err := uow.Do(ctx, func(ctx context.Context, d application.Deps) error {
			_, err := application.Service{
				Store:  failAppend{Store: d.Store, err: forced},
				Locker: d.Locker,
			}.Close(ctx, domain.StatusCommand{
				ID: opened.Account.ID, Reason: "done", ChangedBy: actor,
			})
			return err
		})
		if !errors.Is(err, forced) {
			t.Fatalf("got %v", err)
		}
		row, err := NewAccounts(app).Get(ctx, opened.Account.ID)
		if err != nil || row.Status != domain.StatusActive || row.Version != 0 {
			t.Fatalf("rolled status %+v %v", row, err)
		}
		if n := len(history(t, ctx, app, opened.Account.ID)); n != 0 {
			t.Fatalf("history %d", n)
		}
	})

	t.Run("E same Tx holds status change", func(t *testing.T) {
		opened := mustOpen(t, ctx, uow, user, "PC-ST-E")
		err := uow.Do(ctx, func(ctx context.Context, d application.Deps) error {
			svc := application.Service{Store: d.Store}
			if _, err := svc.Block(ctx, domain.StatusCommand{
				ID: opened.Account.ID, Reason: "aml", ChangedBy: actor,
			}); err != nil {
				return err
			}
			got, err := NewAccounts(other).Get(ctx, opened.Account.ID)
			if err != nil || got.Status != domain.StatusActive {
				return fmt.Errorf("other conn saw %s %v", got.Status, err)
			}
			locked, err := d.Store.GetForUpdate(ctx, opened.Account.ID)
			if err != nil || locked.Status != domain.StatusBlocked {
				return fmt.Errorf("same tx %+v %v", locked, err)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		got, err := NewAccounts(other).Get(ctx, opened.Account.ID)
		if err != nil || got.Status != domain.StatusBlocked {
			t.Fatalf("after commit %+v %v", got, err)
		}
	})

	t.Run("GetForUpdate is NO KEY UPDATE", func(t *testing.T) {
		opened := mustOpen(t, ctx, uow, user, "PC-ST-NK")
		err := uow.Do(ctx, func(ctx context.Context, d application.Deps) error {
			if _, err := d.Store.GetForUpdate(ctx, opened.Account.ID); err != nil {
				return err
			}
			tx2, err := other.Begin(ctx)
			if err != nil {
				return err
			}
			defer func() { _ = tx2.Rollback(ctx) }()
			if _, err := tx2.Exec(ctx, `SET LOCAL lock_timeout = '250ms'`); err != nil {
				return err
			}
			if _, err := tx2.Exec(ctx, `SELECT id FROM accounts.accounts WHERE id = $1 FOR KEY SHARE`, opened.Account.ID); err != nil {
				return fmt.Errorf("FOR KEY SHARE should succeed under NO KEY UPDATE: %w", err)
			}
			_, err = tx2.Exec(ctx, `SELECT id FROM accounts.accounts WHERE id = $1 FOR UPDATE`, opened.Account.ID)
			var pe *pgconn.PgError
			if err == nil || !errors.As(err, &pe) || pe.Code != "55P03" {
				return fmt.Errorf("FOR UPDATE want lock_timeout, got %v", err)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})

	t.Run("readonly cannot ApplyStatus", func(t *testing.T) {
		opened := mustOpen(t, ctx, uow, user, "PC-ST-RO")
		acc := opened.Account
		acc.Status = domain.StatusBlocked
		acc.Version = 1
		err := NewAccounts(ro).ApplyStatus(ctx, acc)
		var pe *pgconn.PgError
		if err == nil || !errors.As(err, &pe) || pe.Code != "42501" {
			t.Fatalf("got %v", err)
		}
	})
}

func mustOpen(t *testing.T, ctx context.Context, uow application.UnitOfWork, user uuid.UUID, number string) application.Opened {
	t.Helper()
	opened, err := application.OpenIn(ctx, uow, domain.OpenCommand{
		ID: mustNewID(t), AccountNumber: number, UserID: &user,
		Purpose: domain.PurposeMain, Currency: money.RUB,
	})
	if err != nil {
		t.Fatal(err)
	}
	return opened
}

func topupOpened(t *testing.T, ctx context.Context, app *pgx.Conn, opened application.Opened, amount int64) {
	t.Helper()
	var avail uuid.UUID
	for _, l := range opened.Ledgers {
		if l.Kind == ledger.KindAvailable {
			avail = l.ID
		}
	}
	if avail == uuid.Nil {
		t.Fatal("no AVAILABLE ledger")
	}
	cmd := ledger.Command{
		JournalID:      mustNewID(t),
		Operation:      ledger.OpSimTopup,
		ReferenceType:  "SIM_TOPUP",
		ReferenceID:    mustNewID(t),
		AccountingDate: time.Now().UTC(),
		Entries: []ledger.Entry{
			{ID: mustNewID(t), LedgerAccountID: mustUUID(t, sys1100), Type: ledger.Debit, Amount: amount, Currency: "RUB"},
			{ID: mustNewID(t), LedgerAccountID: avail, Type: ledger.Credit, Amount: amount, Currency: "RUB"},
		},
	}
	if _, err := postLedger(ctx, app, []string{opened.Account.ID.String()}, cmd); err != nil {
		t.Fatal(err)
	}
}

type histRow struct {
	from, to  string
	changedBy uuid.UUID
}

func history(t *testing.T, ctx context.Context, app *pgx.Conn, id uuid.UUID) []histRow {
	t.Helper()
	rows, err := app.Query(ctx, `
SELECT from_status, to_status, changed_by
  FROM accounts.account_status_history
 WHERE account_id = $1
 ORDER BY changed_at ASC, id ASC`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []histRow
	for rows.Next() {
		var r histRow
		if err := rows.Scan(&r.from, &r.to, &r.changedBy); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

type failAppend struct {
	application.Store
	err error
}

func (f failAppend) AppendHistory(context.Context, domain.StatusHistory) error {
	return f.err
}
