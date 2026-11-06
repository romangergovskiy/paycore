package application

import (
	"bytes"
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
	same := time.Unix(1, 0).UTC()
	lines := []StatementLine{
		{
			ID:        uuid.MustParse("cccccccc-cccc-4ccc-8ccc-000000000001"),
			JournalID: uuid.MustParse("dddddddd-dddd-4ddd-8ddd-000000000001"),
			Kind:      ledger.KindAvailable, Side: ledger.Credit, Amount: 1000, Currency: "RUB",
			CreatedAt: same, PostedAt: same,
		},
		{
			ID:        uuid.MustParse("cccccccc-cccc-4ccc-8ccc-000000000002"),
			JournalID: uuid.MustParse("dddddddd-dddd-4ddd-8ddd-000000000002"),
			Kind:      ledger.KindAvailable, Side: ledger.Credit, Amount: 2000, Currency: "RUB",
			CreatedAt: same, PostedAt: time.Unix(2, 0).UTC(),
		},
		{
			ID:        uuid.MustParse("cccccccc-cccc-4ccc-8ccc-000000000003"),
			JournalID: uuid.MustParse("dddddddd-dddd-4ddd-8ddd-000000000003"),
			Kind:      ledger.KindHold, Side: ledger.Credit, Amount: 300, Currency: "RUB",
			CreatedAt: time.Unix(3, 0).UTC(), PostedAt: time.Unix(3, 0).UTC(),
		},
		{
			ID:        uuid.MustParse("cccccccc-cccc-4ccc-8ccc-000000000004"),
			JournalID: uuid.MustParse("dddddddd-dddd-4ddd-8ddd-000000000004"),
			Kind:      ledger.KindAvailable, Side: ledger.Credit, Amount: 400, Currency: "RUB",
			CreatedAt: time.Unix(4, 0).UTC(), PostedAt: time.Unix(4, 0).UTC(),
		},
	}
	rd := &stubStatements{lines: lines}
	st := newMem()
	svc := Service{Store: st, Opener: &stubOpener{}, Statements: rd}
	opened, err := svc.Open(ctx, domain.OpenCommand{
		AccountNumber: "PC-STMT-1", UserID: &user, Purpose: domain.PurposeMain, Currency: money.RUB,
		Reason: "open", ChangedBy: testOpenActor,
	})
	if err != nil {
		t.Fatal(err)
	}
	id := opened.Account.ID

	t.Run("1 empty has no cursor", func(t *testing.T) {
		rd.lines = nil
		got, err := svc.Statement(ctx, id, StatementQuery{Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		if got.Lines == nil || len(got.Lines) != 0 || got.NextCursor != nil {
			t.Fatalf("empty %+v", got)
		}
		rd.lines = lines
	})

	t.Run("2 first page fits", func(t *testing.T) {
		got, err := svc.Statement(ctx, id, StatementQuery{Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Lines) != 4 || got.NextCursor != nil {
			t.Fatalf("page %+v", got)
		}
		if got.Lines[0].Amount != 1000 || got.Lines[0].Side != ledger.Credit || got.Lines[0].Currency != "RUB" {
			t.Fatalf("mapped %+v", got.Lines[0])
		}
	})

	t.Run("3 NextCursor is last returned", func(t *testing.T) {
		got, err := svc.Statement(ctx, id, StatementQuery{Limit: 2})
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Lines) != 2 || got.NextCursor == nil {
			t.Fatalf("page %+v", got)
		}
		if got.Lines[0].ID != lines[0].ID || got.Lines[1].ID != lines[1].ID {
			t.Fatalf("lines %+v", got.Lines)
		}
		if got.NextCursor.ID != lines[1].ID || !got.NextCursor.CreatedAt.Equal(lines[1].CreatedAt) {
			t.Fatalf("cursor %+v want last returned %s", got.NextCursor, lines[1].ID)
		}
	})

	t.Run("4-5 next then last page", func(t *testing.T) {
		first, err := svc.Statement(ctx, id, StatementQuery{Limit: 3})
		if err != nil {
			t.Fatal(err)
		}
		if first.NextCursor == nil || len(first.Lines) != 3 {
			t.Fatalf("first %+v", first)
		}
		second, err := svc.Statement(ctx, id, StatementQuery{Limit: 3, Cursor: first.NextCursor})
		if err != nil {
			t.Fatal(err)
		}
		if len(second.Lines) != 1 || second.NextCursor != nil {
			t.Fatalf("last %+v", second)
		}
		if second.Lines[0].ID != lines[3].ID {
			t.Fatalf("overlap/skip %+v", second.Lines)
		}
		if second.Lines[0].ID == first.Lines[0].ID || second.Lines[0].ID == first.Lines[2].ID {
			t.Fatal("duplicate across pages")
		}
	})

	t.Run("6 exact Limit multiple has no extra page", func(t *testing.T) {
		first, err := svc.Statement(ctx, id, StatementQuery{Limit: 2})
		if err != nil {
			t.Fatal(err)
		}
		second, err := svc.Statement(ctx, id, StatementQuery{Limit: 2, Cursor: first.NextCursor})
		if err != nil {
			t.Fatal(err)
		}
		if len(second.Lines) != 2 || second.NextCursor != nil {
			t.Fatalf("exact multiple %+v", second)
		}
	})

	t.Run("7 same created_at uses id", func(t *testing.T) {
		first, err := svc.Statement(ctx, id, StatementQuery{Limit: 1})
		if err != nil {
			t.Fatal(err)
		}
		second, err := svc.Statement(ctx, id, StatementQuery{Limit: 1, Cursor: first.NextCursor})
		if err != nil {
			t.Fatal(err)
		}
		if len(first.Lines) != 1 || len(second.Lines) != 1 {
			t.Fatalf("pages %v %v", first.Lines, second.Lines)
		}
		if first.Lines[0].ID != lines[0].ID || second.Lines[0].ID != lines[1].ID {
			t.Fatalf("tiebreak %s then %s", first.Lines[0].ID, second.Lines[0].ID)
		}
		if !first.Lines[0].CreatedAt.Equal(second.Lines[0].CreatedAt) {
			t.Fatal("want same created_at")
		}
	})

	t.Run("8-13 invalid query is error not empty page", func(t *testing.T) {
		st.getErr = errors.New("store failed")
		cases := []struct {
			name  string
			id    uuid.UUID
			query StatementQuery
			want  error
		}{
			{name: "limit 0", id: id, query: StatementQuery{Limit: 0}, want: domain.ErrInvalidAccount},
			{name: "limit 101", id: id, query: StatementQuery{Limit: MaxStatementLimit + 1}, want: domain.ErrInvalidAccount},
			{name: "limit -1", id: id, query: StatementQuery{Limit: -1}, want: domain.ErrInvalidAccount},
			{name: "nil account", id: uuid.Nil, query: StatementQuery{Limit: 10}, want: domain.ErrAccountNotFound},
			{name: "nil cursor id", id: id, query: StatementQuery{Limit: 10, Cursor: &StatementCursor{CreatedAt: same, ID: uuid.Nil}}, want: domain.ErrInvalidAccount},
			{name: "zero time", id: id, query: StatementQuery{Limit: 10, Cursor: &StatementCursor{ID: lines[0].ID}}, want: domain.ErrInvalidAccount},
		}
		for _, tc := range cases {
			got, err := svc.Statement(ctx, tc.id, tc.query)
			if !errors.Is(err, tc.want) {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if len(got.Lines) != 0 || got.NextCursor != nil {
				t.Fatalf("%s returned page %+v", tc.name, got)
			}
		}
		st.getErr = nil
	})

	t.Run("14 missing account", func(t *testing.T) {
		if _, err := svc.Statement(ctx, uuid.MustParse("ffffffff-ffff-4fff-8fff-ffffffffffff"), StatementQuery{Limit: 10}); !errors.Is(err, domain.ErrAccountNotFound) {
			t.Fatalf("missing: %v", err)
		}
	})

	t.Run("16 full walk no dupes or skips", func(t *testing.T) {
		got := walkStatement(t, ctx, svc, id, 1)
		if len(got) != 4 {
			t.Fatalf("walk %d %+v", len(got), got)
		}
		for i := range lines {
			if got[i].ID != lines[i].ID {
				t.Fatalf("walk[%d]=%s want %s", i, got[i].ID, lines[i].ID)
			}
		}
	})

	t.Run("17 errors pass through", func(t *testing.T) {
		want := errors.New("store failed")
		st.getErr = want
		if _, err := svc.Statement(ctx, id, StatementQuery{Limit: 10}); !errors.Is(err, want) {
			t.Fatalf("store err: %v", err)
		}
		st.getErr = nil
		rd.err = errors.New("reader failed")
		if _, err := svc.Statement(ctx, id, StatementQuery{Limit: 10}); !errors.Is(err, rd.err) {
			t.Fatalf("reader err: %v", err)
		}
		rd.err = nil
		dead, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := svc.Statement(dead, id, StatementQuery{Limit: 10}); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled: %v", err)
		}
		if _, err := (Service{}).Statement(ctx, id, StatementQuery{Limit: 10}); !errors.Is(err, errNotWired) {
			t.Fatalf("unwired: %v", err)
		}
	})
}

func walkStatement(t *testing.T, ctx context.Context, svc Service, id uuid.UUID, limit int) []StatementLine {
	t.Helper()
	var (
		all    []StatementLine
		cursor *StatementCursor
		seen   = map[uuid.UUID]bool{}
	)
	for i := 0; i < 32; i++ {
		page, err := svc.Statement(ctx, id, StatementQuery{Limit: limit, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range page.Lines {
			if seen[line.ID] {
				t.Fatalf("duplicate %s", line.ID)
			}
			seen[line.ID] = true
			all = append(all, line)
		}
		if page.NextCursor == nil {
			return all
		}
		if len(page.Lines) != limit {
			t.Fatalf("NextCursor with short page %d", len(page.Lines))
		}
		last := page.Lines[len(page.Lines)-1]
		if page.NextCursor.ID != last.ID || !page.NextCursor.CreatedAt.Equal(last.CreatedAt) {
			t.Fatalf("NextCursor %+v last %s", page.NextCursor, last.ID)
		}
		cursor = page.NextCursor
	}
	t.Fatal("walk did not end")
	return all
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
	start := 0
	if query.Cursor != nil {
		start = len(s.lines)
		for i, line := range s.lines {
			if afterStatementKey(line.CreatedAt, line.ID, *query.Cursor) {
				start = i
				break
			}
		}
	}
	n := query.Limit + 1
	if start+n > len(s.lines) {
		n = len(s.lines) - start
	}
	if n < 0 {
		n = 0
	}
	out := make([]StatementLine, n)
	copy(out, s.lines[start:start+n])
	return out, nil
}

func afterStatementKey(createdAt time.Time, id uuid.UUID, cursor StatementCursor) bool {
	if createdAt.After(cursor.CreatedAt) {
		return true
	}
	if createdAt.Equal(cursor.CreatedAt) {
		a, b := id, cursor.ID
		return bytes.Compare(a[:], b[:]) > 0
	}
	return false
}
