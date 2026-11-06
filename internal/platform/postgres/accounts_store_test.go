package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/romangergovskiy/paycore/internal/accounts/application"
	"github.com/romangergovskiy/paycore/internal/accounts/domain"
	"github.com/romangergovskiy/paycore/internal/ledger"
	"github.com/romangergovskiy/paycore/internal/platform/apperror"
	"github.com/romangergovskiy/paycore/pkg/money"
)

func TestAccountsStore(t *testing.T) {
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

	userA := insertUser(t, ctx, owner)
	userB := insertUser(t, ctx, owner)
	merchA := insertMerchant(t, ctx, owner)
	merchB := insertMerchant(t, ctx, owner)
	store := NewAccounts(app)

	t.Run("Create Get CUSTOMER", func(t *testing.T) {
		acc := customerAcc(t, userA, "PC-ST-C1")
		if err := store.Create(ctx, acc); err != nil {
			t.Fatal(err)
		}
		got, err := store.Get(ctx, acc.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.ID != acc.ID || got.AccountNumber != acc.AccountNumber || got.Status != domain.StatusActive {
			t.Fatalf("got %+v", got)
		}
		if !domain.OwnerSet(got.UserID) || *got.UserID != userA || got.MerchantID != nil {
			t.Fatalf("owner %+v", got)
		}
		if got.Currency != money.RUB || got.Name != "" || got.ClosedAt != nil {
			t.Fatalf("nulls %+v", got)
		}
		if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
			t.Fatal("timestamps")
		}
	})

	t.Run("Create Get MERCHANT", func(t *testing.T) {
		acc := merchantAcc(t, merchA, "PC-ST-M1")
		acc.Name = "Shop"
		if err := store.Create(ctx, acc); err != nil {
			t.Fatal(err)
		}
		got, err := store.Get(ctx, acc.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Purpose != domain.PurposeMerchantSettlement || got.Name != "Shop" {
			t.Fatalf("got %+v", got)
		}
		if !domain.OwnerSet(got.MerchantID) || *got.MerchantID != merchA || got.UserID != nil {
			t.Fatalf("owner %+v", got)
		}
	})

	t.Run("Get missing", func(t *testing.T) {
		_, err := store.Get(ctx, uuid.MustParse("ffffffff-ffff-4fff-8fff-ffffffffffff"))
		if !errors.Is(err, domain.ErrAccountNotFound) {
			t.Fatalf("got %v", err)
		}
		_, err = store.Get(ctx, uuid.Nil)
		if !errors.Is(err, domain.ErrAccountNotFound) {
			t.Fatalf("nil id: %v", err)
		}
	})

	t.Run("GetByNumber hit maps Account", func(t *testing.T) {
		acc := customerAcc(t, userA, "PC-ST-N1")
		acc.Name = "ByNumber"
		if err := store.Create(ctx, acc); err != nil {
			t.Fatal(err)
		}
		before, err := store.Get(ctx, acc.ID)
		if err != nil {
			t.Fatal(err)
		}
		var histBefore int
		if err := app.QueryRow(ctx, `SELECT count(*) FROM accounts.account_status_history WHERE account_id = $1`, acc.ID).Scan(&histBefore); err != nil {
			t.Fatal(err)
		}
		got, err := store.GetByNumber(ctx, acc.AccountNumber)
		if err != nil {
			t.Fatal(err)
		}
		if got.ID != before.ID || got.AccountNumber != before.AccountNumber || got.Status != before.Status {
			t.Fatalf("got %+v want %+v", got, before)
		}
		if got.Name != "ByNumber" || got.Currency != money.RUB || got.Purpose != domain.PurposeMain {
			t.Fatalf("mapped %+v", got)
		}
		if !domain.OwnerSet(got.UserID) || *got.UserID != userA || got.MerchantID != nil {
			t.Fatalf("owner %+v", got)
		}
		after, err := store.Get(ctx, acc.ID)
		if err != nil {
			t.Fatal(err)
		}
		if after.Version != before.Version || !after.UpdatedAt.Equal(before.UpdatedAt) {
			t.Fatalf("write on read: before %+v after %+v", before, after)
		}
		var histAfter int
		if err := app.QueryRow(ctx, `SELECT count(*) FROM accounts.account_status_history WHERE account_id = $1`, acc.ID).Scan(&histAfter); err != nil {
			t.Fatal(err)
		}
		if histAfter != histBefore {
			t.Fatalf("history wrote %d → %d", histBefore, histAfter)
		}
	})

	t.Run("GetByNumber unknown and empty", func(t *testing.T) {
		if _, err := store.GetByNumber(ctx, "PC-ST-NO-SUCH"); !errors.Is(err, domain.ErrAccountNotFound) {
			t.Fatalf("unknown: %v", err)
		}
		if _, err := store.GetByNumber(ctx, ""); !errors.Is(err, domain.ErrInvalidAccount) {
			t.Fatalf("empty: %v", err)
		}
	})

	t.Run("GetByNumber canceled context", func(t *testing.T) {
		dead, stop := context.WithCancel(ctx)
		stop()
		_, err := store.GetByNumber(dead, "PC-ST-N1")
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("ListByOwner isolation", func(t *testing.T) {
		c1 := customerAcc(t, userA, "PC-ST-L-C1")
		c2 := customerAcc(t, userA, "PC-ST-L-C2")
		c2.Purpose = domain.PurposeSavings
		otherC := customerAcc(t, userB, "PC-ST-L-C3")
		otherM := merchantAcc(t, merchB, "PC-ST-L-M2")
		for _, acc := range []domain.Account{c1, c2, otherC, otherM} {
			if err := store.Create(ctx, acc); err != nil {
				t.Fatal(err)
			}
		}
		// merchA already has PC-ST-M1 from the MERCHANT create case (one account per currency).
		var merchAID uuid.UUID
		listExisting, err := store.ListByOwner(ctx, nil, &merchA)
		if err != nil || len(listExisting) != 1 {
			t.Fatalf("seed merchant: %v %#v", err, listExisting)
		}
		merchAID = listExisting[0].ID

		listC, err := store.ListByOwner(ctx, &userA, nil)
		if err != nil {
			t.Fatal(err)
		}
		if listC == nil {
			t.Fatal("nil slice")
		}
		ids := ownerIDs(listC)
		if !ids[c1.ID] || !ids[c2.ID] || ids[otherC.ID] || ids[merchAID] || ids[otherM.ID] {
			t.Fatalf("customer list %v", ids)
		}
		if !sortedByCreatedID(listC) {
			t.Fatalf("sort %+v", listC)
		}

		listM, err := store.ListByOwner(ctx, nil, &merchA)
		if err != nil {
			t.Fatal(err)
		}
		ids = ownerIDs(listM)
		if !ids[merchAID] || ids[c1.ID] || ids[otherM.ID] {
			t.Fatalf("merchant list %v", ids)
		}

		none, err := store.ListByOwner(ctx, ptrUUID(uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-ffffffffffff")), nil)
		if err != nil {
			t.Fatal(err)
		}
		if none == nil || len(none) != 0 {
			t.Fatalf("empty list = %#v", none)
		}

		if _, err := store.ListByOwner(ctx, &userA, &merchA); !errors.Is(err, domain.ErrInvalidAccount) {
			t.Fatalf("both owners: %v", err)
		}
		if _, err := store.ListByOwner(ctx, nil, nil); !errors.Is(err, domain.ErrInvalidAccount) {
			t.Fatalf("no owner: %v", err)
		}
		nilID := uuid.Nil
		if _, err := store.ListByOwner(ctx, &nilID, nil); !errors.Is(err, domain.ErrInvalidAccount) {
			t.Fatalf("nil uuid: %v", err)
		}
	})

	t.Run("unique and CHECK", func(t *testing.T) {
		acc := customerAcc(t, userA, "PC-ST-UQ")
		if err := store.Create(ctx, acc); err != nil {
			t.Fatal(err)
		}
		dup := acc
		dup.ID = mustNewID(t)
		err := store.Create(ctx, dup)
		requireAccountInvalid(t, err, apperror.KindConflict)

		both := customerAcc(t, userA, "PC-ST-BOTH")
		m := merchA
		both.MerchantID = &m
		err = store.Create(ctx, both)
		requireAccountInvalid(t, err, apperror.KindInvalid)
	})

	t.Run("paycore_app not readonly", func(t *testing.T) {
		acc := customerAcc(t, userA, "PC-ST-RO")
		if err := store.Create(ctx, acc); err != nil {
			t.Fatal(err)
		}
		if err := NewAccounts(ro).Create(ctx, customerAcc(t, userA, "PC-ST-RO-X")); err == nil {
			t.Fatal("readonly Create succeeded")
		} else {
			var pe *pgconn.PgError
			if !errors.As(err, &pe) || pe.Code != "42501" {
				t.Fatalf("readonly Create want 42501, got %v", err)
			}
			var ae *apperror.Error
			if errors.As(err, &ae) {
				t.Fatalf("privilege mapped to domain: %v", err)
			}
		}
		got, err := NewAccounts(ro).Get(ctx, acc.ID)
		if err != nil || got.ID != acc.ID {
			t.Fatalf("readonly Get: %+v %v", got, err)
		}
	})

	t.Run("atomicity A rollback after Open", func(t *testing.T) {
		acc := customerAcc(t, userA, "PC-ST-A")
		tx, err := app.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		svc := application.Service{Store: NewAccounts(tx), Opener: NewLedger(tx)}
		opened, err := svc.Open(ctx, domain.OpenCommand{
			ID: acc.ID, AccountNumber: acc.AccountNumber, UserID: acc.UserID,
			Purpose: acc.Purpose, Currency: acc.Currency,
			Reason: "open", ChangedBy: mustNewID(t),
		})
		if err != nil {
			_ = tx.Rollback(ctx)
			t.Fatal(err)
		}
		if len(opened.Ledgers) != 2 {
			_ = tx.Rollback(ctx)
			t.Fatalf("ledgers = %d", len(opened.Ledgers))
		}
		if err := tx.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
		assertGone(t, ctx, app, acc.ID)
	})

	t.Run("atomicity B rollback after opener error", func(t *testing.T) {
		acc := customerAcc(t, userA, "PC-ST-B")
		tx, err := app.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := NewAccounts(tx).Create(ctx, acc); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatal(err)
		}
		_, err = NewLedger(tx).Open(ctx, acc.ID, ledger.OwnerMerchant, "RUB")
		if !errors.Is(err, ledger.ErrInvalidJournal) {
			_ = tx.Rollback(ctx)
			t.Fatalf("opener: %v", err)
		}
		if err := tx.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
		assertGone(t, ctx, app, acc.ID)
	})
}

func requireAccountInvalid(t *testing.T, err error, kind apperror.Kind) {
	t.Helper()
	if !errors.Is(err, domain.ErrInvalidAccount) {
		t.Fatalf("want ErrInvalidAccount, got %v", err)
	}
	var ae *apperror.Error
	if !errors.As(err, &ae) || ae.Message != domain.ErrInvalidAccount.Message || ae.Kind != kind {
		t.Fatalf("public wrap: %+v %v", ae, err)
	}
	var pe *pgconn.PgError
	if !errors.As(err, &pe) {
		t.Fatalf("missing PgError in %v", err)
	}
}

func assertGone(t *testing.T, ctx context.Context, app *pgx.Conn, id uuid.UUID) {
	t.Helper()
	var nAcc, nLed int
	if err := app.QueryRow(ctx, `SELECT count(*) FROM accounts.accounts WHERE id = $1`, id).Scan(&nAcc); err != nil {
		t.Fatal(err)
	}
	if err := app.QueryRow(ctx, `SELECT count(*) FROM ledger.ledger_accounts WHERE account_id = $1`, id).Scan(&nLed); err != nil {
		t.Fatal(err)
	}
	if nAcc != 0 || nLed != 0 {
		t.Fatalf("leaked accounts=%d ledger=%d", nAcc, nLed)
	}
}

func customerAcc(t *testing.T, user uuid.UUID, number string) domain.Account {
	t.Helper()
	u := user
	return domain.Account{
		ID: mustNewID(t), AccountNumber: number, UserID: &u,
		Purpose: domain.PurposeMain, Currency: money.RUB, Status: domain.StatusActive,
	}
}

func merchantAcc(t *testing.T, merchant uuid.UUID, number string) domain.Account {
	t.Helper()
	m := merchant
	return domain.Account{
		ID: mustNewID(t), AccountNumber: number, MerchantID: &m,
		Purpose: domain.PurposeMerchantSettlement, Currency: money.RUB, Status: domain.StatusActive,
	}
}

func insertUser(t *testing.T, ctx context.Context, owner *pgx.Conn) uuid.UUID {
	t.Helper()
	id := mustNewID(t)
	insertAuthUser(t, ctx, owner, id.String())
	return id
}

func insertMerchant(t *testing.T, ctx context.Context, owner *pgx.Conn) uuid.UUID {
	t.Helper()
	id := mustNewID(t)
	if _, err := owner.Exec(ctx, `INSERT INTO merchants.merchants (id) VALUES ($1)`, id); err != nil {
		t.Fatal(err)
	}
	return id
}

func mustNewID(t *testing.T) uuid.UUID {
	t.Helper()
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func ptrUUID(id uuid.UUID) *uuid.UUID { return &id }

func ownerIDs(list []domain.Account) map[uuid.UUID]bool {
	out := map[uuid.UUID]bool{}
	for _, a := range list {
		out[a.ID] = true
	}
	return out
}

func sortedByCreatedID(list []domain.Account) bool {
	for i := 1; i < len(list); i++ {
		prev, cur := list[i-1], list[i]
		if prev.CreatedAt.After(cur.CreatedAt) {
			return false
		}
		if prev.CreatedAt.Equal(cur.CreatedAt) && prev.ID.String() > cur.ID.String() {
			return false
		}
	}
	return true
}
