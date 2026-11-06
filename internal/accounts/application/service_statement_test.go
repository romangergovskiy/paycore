package application

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/romangergovskiy/paycore/internal/accounts/domain"
	"github.com/romangergovskiy/paycore/internal/ledger"
	"github.com/romangergovskiy/paycore/pkg/money"
)

func TestStatement(t *testing.T) {
	ctx := context.Background()
	user := uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-000000000001")
	st := newMem()
	lines := []StatementLine{
		{
			ID:        uuid.MustParse("cccccccc-cccc-4ccc-8ccc-000000000001"),
			JournalID: uuid.MustParse("dddddddd-dddd-4ddd-8ddd-000000000001"),
			Kind:      ledger.KindAvailable, Side: ledger.Credit, Amount: 1000, Currency: "RUB",
			CreatedAt: time.Unix(1, 0).UTC(), PostedAt: time.Unix(1, 0).UTC(),
		},
		{
			ID:        uuid.MustParse("cccccccc-cccc-4ccc-8ccc-000000000002"),
			JournalID: uuid.MustParse("dddddddd-dddd-4ddd-8ddd-000000000002"),
			Kind:      ledger.KindAvailable, Side: ledger.Credit, Amount: 2000, Currency: "RUB",
			CreatedAt: time.Unix(1, 0).UTC(), PostedAt: time.Unix(2, 0).UTC(),
		},
		{
			ID:        uuid.MustParse("cccccccc-cccc-4ccc-8ccc-000000000003"),
			JournalID: uuid.MustParse("dddddddd-dddd-4ddd-8ddd-000000000003"),
			Kind:      ledger.KindHold, Side: ledger.Credit, Amount: 300, Currency: "RUB",
			CreatedAt: time.Unix(3, 0).UTC(), PostedAt: time.Unix(3, 0).UTC(),
		},
	}
	rd := &stubStatements{lines: lines}
	svc := Service{Store: st, Opener: &stubOpener{}, Statements: rd}
	opened, err := svc.Open(ctx, domain.OpenCommand{
		AccountNumber: "PC-STMT-1", UserID: &user, Purpose: domain.PurposeMain, Currency: money.RUB,
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := svc.Statement(ctx, opened.Account.ID, StatementQuery{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Amount != 1000 || got[1].Amount != 2000 || got[2].Kind != ledger.KindHold {
		t.Fatalf("mapped %+v", got)
	}
	if got[0].Side != ledger.Credit || got[0].Currency != "RUB" {
		t.Fatalf("side/currency %+v", got[0])
	}
	if !got[0].CreatedAt.Equal(got[1].CreatedAt) || got[0].ID.String() >= got[1].ID.String() {
		t.Fatalf("same created_at order %+v", got)
	}

	page, err := svc.Statement(ctx, opened.Account.ID, StatementQuery{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 2 || page[0].ID != lines[0].ID || page[1].ID != lines[1].ID {
		t.Fatalf("limit 2 %+v", page)
	}

	rd.lines = nil
	empty, err := svc.Statement(ctx, opened.Account.ID, StatementQuery{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if empty == nil || len(empty) != 0 {
		t.Fatalf("empty %+v", empty)
	}

	if _, err := svc.Statement(ctx, uuid.MustParse("ffffffff-ffff-4fff-8fff-ffffffffffff"), StatementQuery{Limit: 10}); !errors.Is(err, domain.ErrAccountNotFound) {
		t.Fatalf("missing: %v", err)
	}
	if _, err := svc.Statement(ctx, uuid.Nil, StatementQuery{Limit: 10}); !errors.Is(err, domain.ErrAccountNotFound) {
		t.Fatalf("nil id: %v", err)
	}

	st.getErr = errors.New("store failed")
	if _, err := svc.Statement(ctx, opened.Account.ID, StatementQuery{Limit: 0}); !errors.Is(err, domain.ErrInvalidAccount) {
		t.Fatalf("limit 0: %v", err)
	}
	if _, err := svc.Statement(ctx, opened.Account.ID, StatementQuery{Limit: -1}); !errors.Is(err, domain.ErrInvalidAccount) {
		t.Fatalf("limit -1: %v", err)
	}
	if _, err := svc.Statement(ctx, opened.Account.ID, StatementQuery{Limit: MaxStatementLimit + 1}); !errors.Is(err, domain.ErrInvalidAccount) {
		t.Fatalf("limit max: %v", err)
	}
	if _, err := svc.Statement(ctx, opened.Account.ID, StatementQuery{Limit: 10}); !errors.Is(err, st.getErr) {
		t.Fatalf("store err: %v", err)
	}
	st.getErr = nil

	want := errors.New("reader failed")
	rd.err = want
	rd.lines = lines
	if _, err := svc.Statement(ctx, opened.Account.ID, StatementQuery{Limit: 10}); !errors.Is(err, want) {
		t.Fatalf("reader err: %v", err)
	}
	rd.err = nil

	dead, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := svc.Statement(dead, opened.Account.ID, StatementQuery{Limit: 10}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled: %v", err)
	}

	if _, err := (Service{}).Statement(ctx, opened.Account.ID, StatementQuery{Limit: 10}); !errors.Is(err, errNotWired) {
		t.Fatalf("unwired: %v", err)
	}
}

type stubStatements struct {
	lines []StatementLine
	err   error
}

func (s *stubStatements) List(ctx context.Context, _ uuid.UUID, query StatementQuery) ([]StatementLine, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.err != nil {
		return nil, s.err
	}
	if query.Limit >= len(s.lines) {
		out := make([]StatementLine, len(s.lines))
		copy(out, s.lines)
		return out, nil
	}
	out := make([]StatementLine, query.Limit)
	copy(out, s.lines[:query.Limit])
	return out, nil
}
