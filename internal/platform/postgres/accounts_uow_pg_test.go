package postgres

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/romangergovskiy/paycore/internal/accounts/application"
	"github.com/romangergovskiy/paycore/internal/accounts/domain"
	"github.com/romangergovskiy/paycore/internal/ledger"
	"github.com/romangergovskiy/paycore/pkg/money"
)

func TestAccountUnitOfWork(t *testing.T) {
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

	user := insertUser(t, ctx, owner)
	uow := NewAccountUoW(app)

	t.Run("A commit OpenIn", func(t *testing.T) {
		id := mustNewID(t)
		opened, err := application.OpenIn(ctx, uow, domain.OpenCommand{
			ID: id, AccountNumber: "PC-UOW-A", UserID: &user,
			Purpose: domain.PurposeMain, Currency: money.RUB,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(opened.Ledgers) != 2 {
			t.Fatalf("ledgers = %d", len(opened.Ledgers))
		}
		got, err := NewAccounts(app).Get(ctx, id)
		if err != nil || got.ID != id {
			t.Fatalf("Get after commit: %+v %v", got, err)
		}
		var n int
		if err := app.QueryRow(ctx, `SELECT count(*) FROM ledger.ledger_accounts WHERE account_id = $1`, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 2 {
			t.Fatalf("ledger_accounts = %d", n)
		}
	})

	t.Run("B opener error rolls back account", func(t *testing.T) {
		acc := customerAcc(t, user, "PC-UOW-B")
		err := uow.Do(ctx, func(ctx context.Context, d application.Deps) error {
			if err := d.Store.Create(ctx, acc); err != nil {
				return err
			}
			_, err := d.Opener.Open(ctx, acc.ID, ledger.OwnerMerchant, "RUB")
			return err
		})
		if !errors.Is(err, ledger.ErrInvalidJournal) {
			t.Fatalf("got %v", err)
		}
		assertGone(t, ctx, app, acc.ID)
	})

	t.Run("C callback error after Open rolls back both", func(t *testing.T) {
		id := mustNewID(t)
		forced := errors.New("forced after open")
		err := uow.Do(ctx, func(ctx context.Context, d application.Deps) error {
			_, err := application.Service{Store: d.Store, Opener: d.Opener}.Open(ctx, domain.OpenCommand{
				ID: id, AccountNumber: "PC-UOW-C", UserID: &user,
				Purpose: domain.PurposeMain, Currency: money.RUB,
			})
			if err != nil {
				return err
			}
			return forced
		})
		if !errors.Is(err, forced) {
			t.Fatalf("got %v", err)
		}
		assertGone(t, ctx, app, id)
	})

	t.Run("D cancel after Open does not commit", func(t *testing.T) {
		id := mustNewID(t)
		runCtx, stop := context.WithCancel(ctx)
		err := uow.Do(runCtx, func(ctx context.Context, d application.Deps) error {
			_, err := application.Service{Store: d.Store, Opener: d.Opener}.Open(ctx, domain.OpenCommand{
				ID: id, AccountNumber: "PC-UOW-D", UserID: &user,
				Purpose: domain.PurposeMain, Currency: money.RUB,
			})
			if err != nil {
				return err
			}
			stop()
			return nil
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v", err)
		}
		assertGone(t, ctx, app, id)
	})

	t.Run("E same Tx not only same pool", func(t *testing.T) {
		id := mustNewID(t)
		err := uow.Do(ctx, func(ctx context.Context, d application.Deps) error {
			_, err := application.Service{Store: d.Store, Opener: d.Opener}.Open(ctx, domain.OpenCommand{
				ID: id, AccountNumber: "PC-UOW-E", UserID: &user,
				Purpose: domain.PurposeMain, Currency: money.RUB,
			})
			if err != nil {
				return err
			}
			_, err = NewAccounts(other).Get(ctx, id)
			if !errors.Is(err, domain.ErrAccountNotFound) {
				return fmt.Errorf("other conn saw uncommitted row: %v", err)
			}
			got, err := d.Store.Get(ctx, id)
			if err != nil || got.ID != id {
				return fmt.Errorf("same tx Get: %+v %v", got, err)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		got, err := NewAccounts(other).Get(ctx, id)
		if err != nil || got.ID != id {
			t.Fatalf("after commit: %+v %v", got, err)
		}
	})

	t.Run("Begin canceled", func(t *testing.T) {
		dead, stop := context.WithCancel(ctx)
		stop()
		err := uow.Do(dead, func(context.Context, application.Deps) error {
			t.Fatal("callback ran")
			return nil
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v", err)
		}
	})
}
