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
		Reason:        "open",
		ChangedBy:     testOpenActor,
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
	if st.appendCalls != 1 || st.lastHistory.To != domain.StatusActive || st.lastHistory.From != "" {
		t.Fatalf("history calls=%d %+v", st.appendCalls, st.lastHistory)
	}
}

func TestOpenInErrorRollsBack(t *testing.T) {
	user := uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-000000000001")
	want := errors.New("opener failed")
	st := newMem()
	uow := &fakeUoW{store: st, opener: &stubOpener{err: want}}

	_, err := OpenIn(context.Background(), uow, domain.OpenCommand{
		AccountNumber: "PC-UOW-2",
		UserID:        &user,
		Purpose:       domain.PurposeMain,
		Currency:      money.RUB,
		Reason:        "open",
		ChangedBy:     testOpenActor,
	})
	if !errors.Is(err, want) {
		t.Fatalf("got %v want %v", err, want)
	}
	if uow.committed || !uow.rolled {
		t.Fatalf("commit=%v rollback=%v", uow.committed, uow.rolled)
	}
	if st.appendCalls != 0 {
		t.Fatalf("history on opener error: %d", st.appendCalls)
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

func TestBlockInCommits(t *testing.T) {
	st := seededActive()
	id := firstID(st)
	actor := uuid.MustParse("dddddddd-dddd-4ddd-8ddd-000000000001")
	uow := &fakeUoW{store: st, locker: panicLocker{}}
	got, err := BlockIn(context.Background(), uow, domain.StatusCommand{ID: id, Reason: "aml", ChangedBy: actor})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.StatusBlocked || !uow.committed || uow.rolled {
		t.Fatalf("got %+v commit=%v rollback=%v", got, uow.committed, uow.rolled)
	}
}

func TestCloseInEmptyCommits(t *testing.T) {
	st := seededActive()
	id := firstID(st)
	actor := uuid.MustParse("dddddddd-dddd-4ddd-8ddd-000000000001")
	uow := &fakeUoW{
		store:  st,
		locker: &stubLocker{snaps: []ledger.BalanceSnapshot{{AccountID: id}}},
	}
	got, err := CloseIn(context.Background(), uow, domain.StatusCommand{ID: id, Reason: "done", ChangedBy: actor})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.StatusClosed || !uow.committed {
		t.Fatalf("got %+v commit=%v", got, uow.committed)
	}
}

func TestCloseInNotEmptyRollsBack(t *testing.T) {
	st := seededActive()
	id := firstID(st)
	actor := uuid.MustParse("dddddddd-dddd-4ddd-8ddd-000000000001")
	uow := &fakeUoW{
		store:  st,
		locker: &stubLocker{snaps: []ledger.BalanceSnapshot{{AccountID: id, Available: 5}}},
	}
	_, err := CloseIn(context.Background(), uow, domain.StatusCommand{ID: id, Reason: "done", ChangedBy: actor})
	if !errors.Is(err, domain.ErrAccountNotEmpty) {
		t.Fatalf("got %v", err)
	}
	if uow.committed || !uow.rolled || st.applyCalls != 0 || st.appendCalls != 0 {
		t.Fatalf("commit=%v rollback=%v apply=%d append=%d", uow.committed, uow.rolled, st.applyCalls, st.appendCalls)
	}
}

func TestOpenInAppendHistoryRollsBack(t *testing.T) {
	user := uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-000000000001")
	want := errors.New("history insert failed")
	st := newMem()
	st.appendErr = want
	uow := &fakeUoW{store: st, opener: &stubOpener{}}
	_, err := OpenIn(context.Background(), uow, domain.OpenCommand{
		AccountNumber: "PC-UOW-H",
		UserID:        &user,
		Purpose:       domain.PurposeMain,
		Currency:      money.RUB,
		Reason:        "open",
		ChangedBy:     testOpenActor,
	})
	if !errors.Is(err, want) {
		t.Fatalf("got %v", err)
	}
	if uow.committed || !uow.rolled {
		t.Fatalf("commit=%v rollback=%v", uow.committed, uow.rolled)
	}
}

func TestCloseInAppendHistoryRollsBack(t *testing.T) {
	st := seededActive()
	want := errors.New("history insert failed")
	st.appendErr = want
	id := firstID(st)
	actor := uuid.MustParse("dddddddd-dddd-4ddd-8ddd-000000000001")
	uow := &fakeUoW{
		store:  st,
		locker: &stubLocker{snaps: []ledger.BalanceSnapshot{{AccountID: id}}},
	}
	_, err := CloseIn(context.Background(), uow, domain.StatusCommand{ID: id, Reason: "done", ChangedBy: actor})
	if !errors.Is(err, want) {
		t.Fatalf("got %v", err)
	}
	if uow.committed || !uow.rolled {
		t.Fatalf("commit=%v rollback=%v", uow.committed, uow.rolled)
	}
}

type fakeUoW struct {
	store     Store
	opener    ledger.AccountOpener
	locker    ledger.BalanceLocker
	committed bool
	rolled    bool
	calls     int
}

func (f *fakeUoW) Do(ctx context.Context, fn func(context.Context, Deps) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.calls++
	err := fn(ctx, Deps{Store: f.store, Opener: f.opener, Locker: f.locker})
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
