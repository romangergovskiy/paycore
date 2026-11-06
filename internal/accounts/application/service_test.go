package application

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/romangergovskiy/paycore/internal/accounts/domain"
	"github.com/romangergovskiy/paycore/internal/ledger"
	"github.com/romangergovskiy/paycore/pkg/money"
)

func TestOpenRUB(t *testing.T) {
	ctx := context.Background()
	user := uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-000000000001")
	st := newMem()
	op := &stubOpener{pair: []ledger.OpenedLedger{
		{Kind: ledger.KindAvailable, ID: uuid.MustParse("cccccccc-cccc-4ccc-8ccc-000000000001")},
		{Kind: ledger.KindHold, ID: uuid.MustParse("cccccccc-cccc-4ccc-8ccc-000000000002")},
	}}
	svc := Service{Store: st, Opener: op}

	got, err := svc.Open(ctx, domain.OpenCommand{
		AccountNumber: "PC-OPEN-1",
		UserID:        &user,
		Purpose:       domain.PurposeMain,
		Currency:      money.RUB,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Account.Status != domain.StatusActive {
		t.Fatalf("status = %s", got.Account.Status)
	}
	if got.Account.ID == uuid.Nil || got.Account.Currency != money.RUB {
		t.Fatalf("account = %+v", got.Account)
	}
	if len(got.Ledgers) != 2 {
		t.Fatalf("ledgers = %d", len(got.Ledgers))
	}
	if op.calls != 1 || op.lastOwner != ledger.OwnerCustomer || op.lastCur != "RUB" {
		t.Fatalf("opener calls=%d owner=%s cur=%s", op.calls, op.lastOwner, op.lastCur)
	}
	if op.lastID != got.Account.ID {
		t.Fatalf("opener id %s want %s", op.lastID, got.Account.ID)
	}
	stored, err := svc.Get(ctx, got.Account.ID)
	if err != nil || stored.AccountNumber != "PC-OPEN-1" {
		t.Fatalf("Get: %+v %v", stored, err)
	}
}

func TestOpenMerchant(t *testing.T) {
	ctx := context.Background()
	mid := uuid.MustParse("bbbbbbbb-bbbb-4bbb-8bbb-000000000002")
	op := &stubOpener{}
	svc := Service{Store: newMem(), Opener: op}
	got, err := svc.Open(ctx, domain.OpenCommand{
		AccountNumber: "PC-OPEN-M",
		MerchantID:    &mid,
		Purpose:       domain.PurposeMerchantSettlement,
		Currency:      money.RUB,
	})
	if err != nil {
		t.Fatal(err)
	}
	if op.lastOwner != ledger.OwnerMerchant || got.Account.Purpose != domain.PurposeMerchantSettlement {
		t.Fatalf("owner=%s purpose=%s", op.lastOwner, got.Account.Purpose)
	}
}

func TestOpenRejectsCurrencyAndOwner(t *testing.T) {
	ctx := context.Background()
	user := uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-000000000001")
	st := newMem()
	op := &stubOpener{}
	svc := Service{Store: st, Opener: op}
	base := domain.OpenCommand{
		AccountNumber: "PC-BAD",
		UserID:        &user,
		Purpose:       domain.PurposeMain,
		Currency:      money.RUB,
	}

	cases := []struct {
		name string
		cmd  domain.OpenCommand
		want error
	}{
		{name: "USD", cmd: withCur(base, "USD"), want: domain.ErrCurrencyNotSupported},
		{name: "EUR", cmd: withCur(base, "EUR"), want: domain.ErrCurrencyNotSupported},
		{name: "invalid", cmd: withCur(base, "rub"), want: money.ErrInvalidCurrency},
		{name: "merchant purpose on customer", cmd: withPurpose(base, domain.PurposeMerchantSettlement), want: domain.ErrInvalidAccount},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.Open(ctx, tc.cmd)
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v want %v", err, tc.want)
			}
			if op.calls != 0 {
				t.Fatalf("opener called on reject: %d", op.calls)
			}
			if st.len() != 0 {
				t.Fatal("store wrote on reject")
			}
		})
	}
}

func TestOpenDoesNotCreateBlockedOrClosed(t *testing.T) {
	ctx := context.Background()
	user := uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-000000000001")
	svc := Service{Store: newMem(), Opener: &stubOpener{}}
	got, err := svc.Open(ctx, domain.OpenCommand{
		AccountNumber: "PC-ST",
		UserID:        &user,
		Purpose:       domain.PurposeMain,
		Currency:      money.RUB,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Account.Status != domain.StatusActive || got.Account.ClosedAt != nil {
		t.Fatalf("opened %+v", got.Account)
	}
}

func TestGetList(t *testing.T) {
	ctx := context.Background()
	u1 := uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-000000000001")
	u2 := uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-000000000002")
	st := newMem()
	svc := Service{Store: st, Opener: &stubOpener{}}

	a, err := svc.Open(ctx, domain.OpenCommand{
		AccountNumber: "PC-A", UserID: &u1, Purpose: domain.PurposeMain, Currency: money.RUB,
	})
	if err != nil {
		t.Fatal(err)
	}
	b, err := svc.Open(ctx, domain.OpenCommand{
		AccountNumber: "PC-B", UserID: &u1, Purpose: domain.PurposeSavings, Currency: money.RUB,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Open(ctx, domain.OpenCommand{
		AccountNumber: "PC-C", UserID: &u2, Purpose: domain.PurposeMain, Currency: money.RUB,
	}); err != nil {
		t.Fatal(err)
	}

	got, err := svc.Get(ctx, a.Account.ID)
	if err != nil || got.ID != a.Account.ID {
		t.Fatalf("Get: %+v %v", got, err)
	}
	if _, err := svc.Get(ctx, uuid.MustParse("ffffffff-ffff-4fff-8fff-ffffffffffff")); !errors.Is(err, domain.ErrAccountNotFound) {
		t.Fatalf("missing: %v", err)
	}

	list, err := svc.ListByOwner(ctx, &u1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("list u1 = %d", len(list))
	}
	seen := map[uuid.UUID]bool{a.Account.ID: false, b.Account.ID: false}
	for _, acc := range list {
		seen[acc.ID] = true
		if acc.Status != domain.StatusActive {
			t.Fatalf("listed status %s", acc.Status)
		}
	}
	if !seen[a.Account.ID] || !seen[b.Account.ID] {
		t.Fatalf("list = %+v", list)
	}

	if _, err := svc.ListByOwner(ctx, nil, nil); !errors.Is(err, domain.ErrInvalidAccount) {
		t.Fatalf("list no owner: %v", err)
	}
}

func TestOpenOpenerError(t *testing.T) {
	ctx := context.Background()
	user := uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-000000000001")
	op := &stubOpener{err: ledger.ErrInvalidJournal}
	svc := Service{Store: newMem(), Opener: op}
	_, err := svc.Open(ctx, domain.OpenCommand{
		AccountNumber: "PC-OP",
		UserID:        &user,
		Purpose:       domain.PurposeMain,
		Currency:      money.RUB,
	})
	if !errors.Is(err, ledger.ErrInvalidJournal) {
		t.Fatalf("got %v", err)
	}
}

func TestGetByNumber(t *testing.T) {
	ctx := context.Background()
	user := uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-000000000001")
	st := newMem()
	svc := Service{Store: st, Opener: &stubOpener{}}
	opened, err := svc.Open(ctx, domain.OpenCommand{
		AccountNumber: "PC-NUM-1", UserID: &user, Purpose: domain.PurposeMain, Currency: money.RUB,
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := svc.GetByNumber(ctx, "PC-NUM-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != opened.Account.ID || got.AccountNumber != "PC-NUM-1" || got.Status != domain.StatusActive {
		t.Fatalf("hit %+v", got)
	}
	if !domain.OwnerSet(got.UserID) || *got.UserID != user || got.MerchantID != nil {
		t.Fatalf("owner %+v", got)
	}
	if got.Currency != money.RUB || got.Purpose != domain.PurposeMain {
		t.Fatalf("mapped %+v", got)
	}

	if _, err := svc.GetByNumber(ctx, "PC-UNKNOWN"); !errors.Is(err, domain.ErrAccountNotFound) {
		t.Fatalf("unknown: %v", err)
	}
	if _, err := svc.GetByNumber(ctx, ""); !errors.Is(err, domain.ErrInvalidAccount) {
		t.Fatalf("empty: %v", err)
	}

	want := errors.New("store failed")
	st.getByNumberErr = want
	if _, err := svc.GetByNumber(ctx, ""); !errors.Is(err, domain.ErrInvalidAccount) {
		t.Fatalf("empty skipped store: %v", err)
	}
	if _, err := svc.GetByNumber(ctx, "PC-NUM-1"); !errors.Is(err, want) {
		t.Fatalf("store err: %v", err)
	}

	dead, cancel := context.WithCancel(ctx)
	cancel()
	st.getByNumberErr = nil
	if _, err := svc.GetByNumber(dead, "PC-NUM-1"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled: %v", err)
	}
}

func TestGetBlockedFromStore(t *testing.T) {
	ctx := context.Background()
	id := uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-000000000009")
	user := uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-000000000001")
	st := newMem()
	st.byID[id] = domain.Account{
		ID: id, AccountNumber: "PC-BL", UserID: &user,
		Purpose: domain.PurposeMain, Currency: money.RUB, Status: domain.StatusBlocked,
	}
	svc := Service{Store: st}
	got, err := svc.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.StatusBlocked {
		t.Fatalf("status = %s", got.Status)
	}
}

func withCur(cmd domain.OpenCommand, c money.Currency) domain.OpenCommand {
	cmd.Currency = c
	return cmd
}

func withPurpose(cmd domain.OpenCommand, p domain.Purpose) domain.OpenCommand {
	cmd.Purpose = p
	return cmd
}

type stubOpener struct {
	pair      []ledger.OpenedLedger
	err       error
	calls     int
	lastID    uuid.UUID
	lastOwner ledger.OwnerType
	lastCur   string
}

func (s *stubOpener) Open(_ context.Context, accountID uuid.UUID, owner ledger.OwnerType, currency string) ([]ledger.OpenedLedger, error) {
	s.calls++
	s.lastID = accountID
	s.lastOwner = owner
	s.lastCur = currency
	if s.err != nil {
		return nil, s.err
	}
	return s.pair, nil
}

type memStore struct {
	mu             sync.Mutex
	byID           map[uuid.UUID]domain.Account
	history        []domain.StatusHistory
	applyCalls     int
	appendCalls    int
	appendErr      error
	getErr         error
	getByNumberErr error
	lastHistory    domain.StatusHistory
}

func newMem() *memStore {
	return &memStore{byID: map[uuid.UUID]domain.Account{}}
}

func (m *memStore) len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.byID)
}

func (m *memStore) Create(_ context.Context, acc domain.Account) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.byID[acc.ID]; ok {
		return domain.ErrInvalidAccount
	}
	m.byID[acc.ID] = acc
	return nil
}

func (m *memStore) Get(ctx context.Context, id uuid.UUID) (domain.Account, error) {
	if err := ctx.Err(); err != nil {
		return domain.Account{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.getErr != nil {
		return domain.Account{}, m.getErr
	}
	acc, ok := m.byID[id]
	if !ok {
		return domain.Account{}, domain.ErrAccountNotFound
	}
	return acc, nil
}

func (m *memStore) GetByNumber(ctx context.Context, number string) (domain.Account, error) {
	if err := ctx.Err(); err != nil {
		return domain.Account{}, err
	}
	if number == "" {
		return domain.Account{}, domain.ErrInvalidAccount
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.getByNumberErr != nil {
		return domain.Account{}, m.getByNumberErr
	}
	for _, acc := range m.byID {
		if acc.AccountNumber == number {
			return acc, nil
		}
	}
	return domain.Account{}, domain.ErrAccountNotFound
}

func (m *memStore) GetForUpdate(ctx context.Context, id uuid.UUID) (domain.Account, error) {
	return m.Get(ctx, id)
}

func (m *memStore) ApplyStatus(_ context.Context, acc domain.Account) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.applyCalls++
	if _, ok := m.byID[acc.ID]; !ok {
		return domain.ErrAccountNotFound
	}
	m.byID[acc.ID] = acc
	return nil
}

func (m *memStore) AppendHistory(_ context.Context, h domain.StatusHistory) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.appendCalls++
	m.lastHistory = h
	if m.appendErr != nil {
		return m.appendErr
	}
	m.history = append(m.history, h)
	return nil
}

func (m *memStore) ListByOwner(_ context.Context, userID, merchantID *uuid.UUID) ([]domain.Account, error) {
	if err := domain.ValidateOwner(userID, merchantID); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]domain.Account, 0)
	for _, acc := range m.byID {
		if domain.OwnerSet(userID) && domain.OwnerSet(acc.UserID) && *acc.UserID == *userID {
			out = append(out, acc)
		}
		if domain.OwnerSet(merchantID) && domain.OwnerSet(acc.MerchantID) && *acc.MerchantID == *merchantID {
			out = append(out, acc)
		}
	}
	return out, nil
}
