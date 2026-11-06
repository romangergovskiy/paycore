package application

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/romangergovskiy/paycore/internal/accounts/domain"
	"github.com/romangergovskiy/paycore/pkg/money"
)

func TestBalance(t *testing.T) {
	ctx := context.Background()
	user := uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-000000000001")
	st := newMem()
	svc := Service{Store: st, Opener: &stubOpener{}, Balances: &stubBalances{}}
	opened, err := svc.Open(ctx, domain.OpenCommand{
		AccountNumber: "PC-BAL-1", UserID: &user, Purpose: domain.PurposeMain, Currency: money.RUB,
		Reason: "open", ChangedBy: testOpenActor,
	})
	if err != nil {
		t.Fatal(err)
	}

	avail, reserved, version, err := svc.Balance(ctx, opened.Account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if avail != 0 || reserved != 0 || version != 0 {
		t.Fatalf("after open %d %d %d", avail, reserved, version)
	}

	svc.Balances = &stubBalances{available: 1500, reserved: 200, version: 3}
	avail, reserved, version, err = svc.Balance(ctx, opened.Account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if avail != 1500 || reserved != 200 || version != 3 {
		t.Fatalf("mapped %d %d %d", avail, reserved, version)
	}

	if _, _, _, err := svc.Balance(ctx, uuid.MustParse("ffffffff-ffff-4fff-8fff-ffffffffffff")); !errors.Is(err, domain.ErrAccountNotFound) {
		t.Fatalf("missing account: %v", err)
	}
	if _, _, _, err := svc.Balance(ctx, uuid.Nil); !errors.Is(err, domain.ErrAccountNotFound) {
		t.Fatalf("nil id: %v", err)
	}

	svc.Balances = &stubBalances{err: domain.ErrAccountNotFound}
	avail, reserved, version, err = svc.Balance(ctx, opened.Account.ID)
	if !errors.Is(err, domain.ErrAccountNotFound) {
		t.Fatalf("missing snapshot: %v", err)
	}
	if avail != 0 || reserved != 0 || version != 0 {
		t.Fatalf("zeros on error %d %d %d", avail, reserved, version)
	}

	want := errors.New("store failed")
	st.getErr = want
	if _, _, _, err := svc.Balance(ctx, opened.Account.ID); !errors.Is(err, want) {
		t.Fatalf("store err: %v", err)
	}
	st.getErr = nil

	want = errors.New("balances failed")
	svc.Balances = &stubBalances{err: want}
	if _, _, _, err := svc.Balance(ctx, opened.Account.ID); !errors.Is(err, want) {
		t.Fatalf("reader err: %v", err)
	}

	dead, cancel := context.WithCancel(ctx)
	cancel()
	svc.Balances = &stubBalances{available: 1}
	if _, _, _, err := svc.Balance(dead, opened.Account.ID); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled: %v", err)
	}

	if _, _, _, err := (Service{}).Balance(ctx, opened.Account.ID); !errors.Is(err, errNotWired) {
		t.Fatalf("unwired: %v", err)
	}
}

type stubBalances struct {
	available, reserved, version int64
	err                          error
}

func (s *stubBalances) Get(ctx context.Context, _ uuid.UUID) (int64, int64, int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, 0, 0, err
	}
	if s.err != nil {
		return 0, 0, 0, s.err
	}
	return s.available, s.reserved, s.version, nil
}
