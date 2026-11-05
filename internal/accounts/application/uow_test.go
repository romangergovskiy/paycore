package application

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/romangergovskiy/paycore/internal/accounts/domain"
	"github.com/romangergovskiy/paycore/internal/ledger"
	"github.com/romangergovskiy/paycore/pkg/money"
)

func TestOpenInSuccessCommits(t *testing.T) {
	user := uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-000000000001")
	st := newMem()
	op := &stubOpener{pair: []ledger.OpenedLedger{
		{Kind: ledger.KindAvailable, ID: uuid.MustParse("cccccccc-cccc-4ccc-8ccc-000000000001")},
		{Kind: ledger.KindHold, ID: uuid.MustParse("cccccccc-cccc-4ccc-8ccc-000000000002")},
	}}
	uow := &fakeUoW{store: st, opener: op}

	got, err := OpenIn(context.Background(), uow, domain.OpenCommand{
		AccountNumber: "PC-UOW-1",
		UserID:        &user,
		Purpose:       domain.PurposeMain,
		Currency:      money.RUB,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Account.ID == uuid.Nil || len(got.Ledgers) != 2 {
		t.Fatalf("opened %+v", got)
	}
	if !uow.committed || uow.rolled {
		t.Fatalf("commit=%v rollback=%v", uow.committed, uow.rolled)
	}
}

func TestOpenInErrorRollsBack(t *testing.T) {
	user := uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-000000000001")
	want := errors.New("opener failed")
	uow := &fakeUoW{store: newMem(), opener: &stubOpener{err: want}}

	_, err := OpenIn(context.Background(), uow, domain.OpenCommand{
		AccountNumber: "PC-UOW-2",
		UserID:        &user,
		Purpose:       domain.PurposeMain,
		Currency:      money.RUB,
	})
	if !errors.Is(err, want) {
		t.Fatalf("got %v want %v", err, want)
	}
	if uow.committed || !uow.rolled {
		t.Fatalf("commit=%v rollback=%v", uow.committed, uow.rolled)
	}
}

func TestOpenInCanceledBeforeDo(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	uow := &fakeUoW{store: newMem(), opener: &stubOpener{}}
	_, err := OpenIn(ctx, uow, domain.OpenCommand{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	if uow.committed || uow.calls != 0 {
		t.Fatalf("commit=%v calls=%d", uow.committed, uow.calls)
	}
}

type fakeUoW struct {
	store     Store
	opener    ledger.AccountOpener
	committed bool
	rolled    bool
	calls     int
}

func (f *fakeUoW) Do(ctx context.Context, fn func(context.Context, Deps) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.calls++
	err := fn(ctx, Deps{Store: f.store, Opener: f.opener})
	if err != nil {
		f.rolled = true
		return err
	}
	if err := ctx.Err(); err != nil {
		f.rolled = true
		return err
	}
	f.committed = true
	return nil
}
