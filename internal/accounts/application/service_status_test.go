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

func TestBlockUnblock(t *testing.T) {
	ctx := context.Background()
	st := seededActive()
	id := firstID(st)
	actor := uuid.MustParse("dddddddd-dddd-4ddd-8ddd-000000000001")
	svc := Service{Store: st, Locker: panicLocker{}}

	got, err := svc.Block(ctx, domain.StatusCommand{ID: id, Reason: "aml", ChangedBy: actor})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.StatusBlocked || got.Version != 1 || got.ClosedAt != nil {
		t.Fatalf("blocked %+v", got)
	}
	if st.applyCalls != 1 || st.appendCalls != 1 {
		t.Fatalf("apply=%d append=%d", st.applyCalls, st.appendCalls)
	}
	if st.lastHistory.ChangedBy != actor || st.lastHistory.From != domain.StatusActive {
		t.Fatalf("history %+v", st.lastHistory)
	}

	got, err = svc.Unblock(ctx, domain.StatusCommand{ID: id, Reason: "cleared", ChangedBy: actor})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.StatusActive || got.Version != 2 || got.ClosedAt != nil {
		t.Fatalf("unblocked %+v", got)
	}
}

func TestBlockRejectsInvalidStateAndCommand(t *testing.T) {
	ctx := context.Background()
	st := seededActive()
	id := firstID(st)
	actor := uuid.MustParse("dddddddd-dddd-4ddd-8ddd-000000000001")
	svc := Service{Store: st, Locker: panicLocker{}}
	if _, err := svc.Block(ctx, domain.StatusCommand{ID: id, Reason: "aml", ChangedBy: actor}); err != nil {
		t.Fatal(err)
	}
	st.applyCalls, st.appendCalls = 0, 0
	if _, err := svc.Block(ctx, domain.StatusCommand{ID: id, Reason: "again", ChangedBy: actor}); !errors.Is(err, domain.ErrInvalidState) {
		t.Fatalf("second block: %v", err)
	}
	if st.applyCalls != 0 || st.appendCalls != 0 {
		t.Fatal("wrote on invalid state")
	}
	if _, err := svc.Block(ctx, domain.StatusCommand{ID: id, ChangedBy: actor}); !errors.Is(err, domain.ErrInvalidAccount) {
		t.Fatalf("empty reason: %v", err)
	}
	if _, err := svc.Block(ctx, domain.StatusCommand{ID: id, Reason: "aml"}); !errors.Is(err, domain.ErrInvalidAccount) {
		t.Fatalf("nil actor: %v", err)
	}
}

func TestCloseEmpty(t *testing.T) {
	ctx := context.Background()
	st := seededActive()
	id := firstID(st)
	actor := uuid.MustParse("dddddddd-dddd-4ddd-8ddd-000000000001")
	lk := &stubLocker{snaps: []ledger.BalanceSnapshot{{AccountID: id, Available: 0, Reserved: 0}}}
	svc := Service{Store: st, Locker: lk}

	got, err := svc.Close(ctx, domain.StatusCommand{ID: id, Reason: "done", ChangedBy: actor})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.StatusClosed || got.ClosedAt == nil || got.Version != 1 {
		t.Fatalf("closed %+v", got)
	}
	if lk.calls != 1 || st.applyCalls != 1 || st.appendCalls != 1 {
		t.Fatalf("lock=%d apply=%d append=%d", lk.calls, st.applyCalls, st.appendCalls)
	}
	if st.lastHistory.ChangedBy != actor {
		t.Fatalf("changed_by %s", st.lastHistory.ChangedBy)
	}
}

func TestCloseFromBlocked(t *testing.T) {
	ctx := context.Background()
	st := seededActive()
	id := firstID(st)
	acc := st.byID[id]
	acc.Status = domain.StatusBlocked
	st.byID[id] = acc
	actor := uuid.MustParse("dddddddd-dddd-4ddd-8ddd-000000000001")
	svc := Service{Store: st, Locker: &stubLocker{snaps: []ledger.BalanceSnapshot{{AccountID: id}}}}
	got, err := svc.Close(ctx, domain.StatusCommand{ID: id, Reason: "done", ChangedBy: actor})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.StatusClosed {
		t.Fatalf("status %s", got.Status)
	}
}

func TestCloseRejectedBalances(t *testing.T) {
	ctx := context.Background()
	id := uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-000000000009")
	actor := uuid.MustParse("dddddddd-dddd-4ddd-8ddd-000000000001")
	cmd := domain.StatusCommand{ID: id, Reason: "done", ChangedBy: actor}

	cases := []struct {
		name  string
		snaps []ledger.BalanceSnapshot
		want  error
	}{
		{
			name:  "available",
			snaps: []ledger.BalanceSnapshot{{AccountID: id, Available: 1, Reserved: 0}},
			want:  domain.ErrAccountNotEmpty,
		},
		{
			name:  "reserved",
			snaps: []ledger.BalanceSnapshot{{AccountID: id, Available: 0, Reserved: 1}},
			want:  domain.ErrAccountNotEmpty,
		},
		{
			name:  "missing",
			snaps: nil,
			want:  errCloseSnapshot,
		},
		{
			name:  "empty set",
			snaps: []ledger.BalanceSnapshot{},
			want:  errCloseSnapshot,
		},
		{
			name: "extra row",
			snaps: []ledger.BalanceSnapshot{
				{AccountID: id},
				{AccountID: id, Available: 0},
			},
			want: errCloseSnapshot,
		},
		{
			name:  "other account",
			snaps: []ledger.BalanceSnapshot{{AccountID: uuid.MustParse("bbbbbbbb-bbbb-4bbb-8bbb-000000000002")}},
			want:  errCloseSnapshot,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := seededActive()
			lk := &stubLocker{snaps: tc.snaps}
			svc := Service{Store: st, Locker: lk}
			_, err := svc.Close(ctx, cmd)
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v want %v", err, tc.want)
			}
			if st.applyCalls != 0 || st.appendCalls != 0 {
				t.Fatalf("wrote apply=%d append=%d", st.applyCalls, st.appendCalls)
			}
			if errors.Is(err, domain.ErrAccountNotEmpty) && errors.Is(err, errCloseSnapshot) {
				t.Fatal("empty and snapshot errors must not alias")
			}
		})
	}
}

func TestCloseInvalidFromClosed(t *testing.T) {
	ctx := context.Background()
	st := seededActive()
	id := firstID(st)
	acc := st.byID[id]
	acc.Status = domain.StatusClosed
	st.byID[id] = acc
	lk := &stubLocker{snaps: []ledger.BalanceSnapshot{{AccountID: id}}}
	svc := Service{Store: st, Locker: lk}
	actor := uuid.MustParse("dddddddd-dddd-4ddd-8ddd-000000000001")
	_, err := svc.Close(ctx, domain.StatusCommand{ID: id, Reason: "again", ChangedBy: actor})
	if !errors.Is(err, domain.ErrInvalidState) {
		t.Fatalf("got %v", err)
	}
	if lk.calls != 0 || st.applyCalls != 0 {
		t.Fatal("locked or wrote closed account")
	}
}

func TestCloseAppendHistoryError(t *testing.T) {
	ctx := context.Background()
	st := seededActive()
	want := errors.New("history insert failed")
	st.appendErr = want
	id := firstID(st)
	actor := uuid.MustParse("dddddddd-dddd-4ddd-8ddd-000000000001")
	svc := Service{Store: st, Locker: &stubLocker{snaps: []ledger.BalanceSnapshot{{AccountID: id}}}}
	_, err := svc.Close(ctx, domain.StatusCommand{ID: id, Reason: "done", ChangedBy: actor})
	if !errors.Is(err, want) {
		t.Fatalf("got %v", err)
	}
	if st.applyCalls != 1 || st.appendCalls != 1 {
		t.Fatalf("apply=%d append=%d", st.applyCalls, st.appendCalls)
	}
}

func TestChangedByNotReplaced(t *testing.T) {
	ctx := context.Background()
	st := seededActive()
	id := firstID(st)
	actor := uuid.MustParse("eeeeeeee-eeee-4eee-8eee-0000000000aa")
	svc := Service{Store: st, Locker: panicLocker{}}
	if _, err := svc.Block(ctx, domain.StatusCommand{ID: id, Reason: "aml", ChangedBy: actor}); err != nil {
		t.Fatal(err)
	}
	if st.lastHistory.ChangedBy != actor || st.lastHistory.ChangedBy == id {
		t.Fatalf("changed_by %s account %s", st.lastHistory.ChangedBy, id)
	}
}

func seededActive() *memStore {
	user := uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-000000000001")
	id := uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-000000000009")
	st := newMem()
	st.byID[id] = domain.Account{
		ID: id, AccountNumber: "PC-ST", UserID: &user,
		Purpose: domain.PurposeMain, Currency: money.RUB, Status: domain.StatusActive,
	}
	return st
}

func firstID(st *memStore) uuid.UUID {
	for id := range st.byID {
		return id
	}
	return uuid.Nil
}

type stubLocker struct {
	snaps []ledger.BalanceSnapshot
	err   error
	calls int
}

func (s *stubLocker) Lock(_ context.Context, _ []uuid.UUID) ([]ledger.BalanceSnapshot, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	return s.snaps, nil
}

type panicLocker struct{}

func (panicLocker) Lock(context.Context, []uuid.UUID) ([]ledger.BalanceSnapshot, error) {
	panic("locker must not be called")
}
