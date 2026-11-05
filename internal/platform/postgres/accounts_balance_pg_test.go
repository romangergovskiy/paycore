package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/romangergovskiy/paycore/internal/accounts/application"
	"github.com/romangergovskiy/paycore/internal/accounts/domain"
)

func TestAccountBalance(t *testing.T) {
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
		Store:    NewAccounts(app),
		Balances: NewAccountBalances(app),
	}

	t.Run("zeros after Open", func(t *testing.T) {
		opened := mustOpen(t, ctx, uow, user, "PC-BAL-OPEN")
		avail, reserved, version, err := svc.Balance(ctx, opened.Account.ID)
		if err != nil {
			t.Fatal(err)
		}
		if avail != 0 || reserved != 0 || version != 0 {
			t.Fatalf("after open %d %d %d", avail, reserved, version)
		}
	})

	t.Run("available after SIM_TOPUP", func(t *testing.T) {
		opened := mustOpen(t, ctx, uow, user, "PC-BAL-TOP")
		const amount int64 = 2500
		topupOpened(t, ctx, app, opened, amount)
		avail, reserved, version, err := svc.Balance(ctx, opened.Account.ID)
		if err != nil {
			t.Fatal(err)
		}
		if avail != amount {
			t.Fatalf("available = %d want %d", avail, amount)
		}
		if reserved != 0 {
			t.Fatalf("reserved = %d", reserved)
		}
		if version <= 0 {
			t.Fatalf("version = %d", version)
		}
	})

	t.Run("missing account", func(t *testing.T) {
		avail, reserved, version, err := svc.Balance(ctx, uuid.MustParse("ffffffff-ffff-4fff-8fff-ffffffffffff"))
		if !errors.Is(err, domain.ErrAccountNotFound) {
			t.Fatalf("got %v", err)
		}
		if avail != 0 || reserved != 0 || version != 0 {
			t.Fatalf("zeros on missing %d %d %d", avail, reserved, version)
		}
		if _, _, _, err := svc.Balance(ctx, uuid.Nil); !errors.Is(err, domain.ErrAccountNotFound) {
			t.Fatalf("nil id: %v", err)
		}
	})

	t.Run("missing snapshot is not zeros", func(t *testing.T) {
		acc := customerAcc(t, user, "PC-BAL-NOSNAP")
		if err := NewAccounts(app).Create(ctx, acc); err != nil {
			t.Fatal(err)
		}
		avail, reserved, version, err := svc.Balance(ctx, acc.ID)
		if !errors.Is(err, domain.ErrAccountNotFound) {
			t.Fatalf("got %v", err)
		}
		if avail != 0 || reserved != 0 || version != 0 {
			t.Fatalf("invented zeros %d %d %d", avail, reserved, version)
		}
	})

	t.Run("canceled context", func(t *testing.T) {
		opened := mustOpen(t, ctx, uow, user, "PC-BAL-CXL")
		dead, stop := context.WithCancel(ctx)
		stop()
		if _, _, _, err := svc.Balance(dead, opened.Account.ID); !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("read does not write", func(t *testing.T) {
		opened := mustOpen(t, ctx, uow, user, "PC-BAL-RO")
		var avail, reserved, version int64
		var updated time.Time
		if err := app.QueryRow(ctx, `
SELECT available_amount, reserved_amount, version, updated_at
  FROM ledger.account_balances WHERE account_id = $1`, opened.Account.ID).Scan(&avail, &reserved, &version, &updated); err != nil {
			t.Fatal(err)
		}
		var journals int
		if err := app.QueryRow(ctx, `SELECT count(*) FROM ledger.journal_transactions`).Scan(&journals); err != nil {
			t.Fatal(err)
		}
		gotAvail, gotReserved, gotVersion, err := svc.Balance(ctx, opened.Account.ID)
		if err != nil {
			t.Fatal(err)
		}
		if gotAvail != avail || gotReserved != reserved || gotVersion != version {
			t.Fatalf("read %d %d %d want %d %d %d", gotAvail, gotReserved, gotVersion, avail, reserved, version)
		}
		var availAfter, reservedAfter, versionAfter int64
		var updatedAfter time.Time
		if err := app.QueryRow(ctx, `
SELECT available_amount, reserved_amount, version, updated_at
  FROM ledger.account_balances WHERE account_id = $1`, opened.Account.ID).Scan(&availAfter, &reservedAfter, &versionAfter, &updatedAfter); err != nil {
			t.Fatal(err)
		}
		if availAfter != avail || reservedAfter != reserved || versionAfter != version || !updatedAfter.Equal(updated) {
			t.Fatalf("projection wrote %d %d %d %s", availAfter, reservedAfter, versionAfter, updatedAfter)
		}
		var journalsAfter int
		if err := app.QueryRow(ctx, `SELECT count(*) FROM ledger.journal_transactions`).Scan(&journalsAfter); err != nil {
			t.Fatal(err)
		}
		if journalsAfter != journals {
			t.Fatalf("journals %d → %d", journals, journalsAfter)
		}
	})
}
