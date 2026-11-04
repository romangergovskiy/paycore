package ledger

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestValidate(t *testing.T) {
	day := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)
	id := func(n byte) uuid.UUID {
		var u uuid.UUID
		u[15] = n
		u[6] = 0x40
		u[8] = 0x80
		return u
	}
	orig := id(9)
	adj := id(10)
	pay := id(11)
	exp := time.Date(2026, 12, 1, 12, 0, 0, 0, time.UTC)

	balanced := func() []Entry {
		return []Entry{
			{ID: id(3), LedgerAccountID: id(4), Type: Debit, Amount: 100, Currency: "RUB"},
			{ID: id(5), LedgerAccountID: id(6), Type: Credit, Amount: 100, Currency: "RUB"},
		}
	}
	sim := func() Command {
		return Command{
			JournalID:      id(1),
			Operation:      OpSimTopup,
			ReferenceType:  refSimTopup,
			ReferenceID:    id(2),
			AccountingDate: day,
			Entries:        balanced(),
		}
	}

	tests := []struct {
		name    string
		cmd     Command
		wantErr bool
	}{
		{name: "SIM_TOPUP ok", cmd: sim()},
		{
			name: "required journal_id",
			cmd: func() Command {
				c := sim()
				c.JournalID = uuid.Nil
				return c
			}(),
			wantErr: true,
		},
		{
			name: "required operation",
			cmd: func() Command {
				c := sim()
				c.Operation = ""
				return c
			}(),
			wantErr: true,
		},
		{
			name: "required reference_type",
			cmd: func() Command {
				c := sim()
				c.ReferenceType = ""
				return c
			}(),
			wantErr: true,
		},
		{
			name: "required reference_id",
			cmd: func() Command {
				c := sim()
				c.ReferenceID = uuid.Nil
				return c
			}(),
			wantErr: true,
		},
		{
			name: "required accounting_date",
			cmd: func() Command {
				c := sim()
				c.AccountingDate = time.Time{}
				return c
			}(),
			wantErr: true,
		},
		{
			name: "required entry id",
			cmd: func() Command {
				c := sim()
				c.Entries[0].ID = uuid.Nil
				return c
			}(),
			wantErr: true,
		},
		{
			name: "fewer than 2 entries",
			cmd: func() Command {
				c := sim()
				c.Entries = c.Entries[:1]
				return c
			}(),
			wantErr: true,
		},
		{
			name: "entry type not DEBIT/CREDIT",
			cmd: func() Command {
				c := sim()
				c.Entries[0].Type = "FOO"
				return c
			}(),
			wantErr: true,
		},
		{
			name: "amount zero",
			cmd: func() Command {
				c := sim()
				c.Entries[0].Amount = 0
				c.Entries[1].Amount = 0
				return c
			}(),
			wantErr: true,
		},
		{
			name: "amount negative",
			cmd: func() Command {
				c := sim()
				c.Entries[0].Amount = -1
				c.Entries[1].Amount = -1
				return c
			}(),
			wantErr: true,
		},
		{
			name: "amount above MAX",
			cmd: func() Command {
				c := sim()
				c.Entries[0].Amount = MaxAmount + 1
				c.Entries[1].Amount = MaxAmount + 1
				return c
			}(),
			wantErr: true,
		},
		{
			name: "amount at MAX",
			cmd: func() Command {
				c := sim()
				c.Entries[0].Amount = MaxAmount
				c.Entries[1].Amount = MaxAmount
				return c
			}(),
		},
		{
			name: "duplicate ledger_account_id",
			cmd: func() Command {
				c := sim()
				c.Entries[1].LedgerAccountID = c.Entries[0].LedgerAccountID
				return c
			}(),
			wantErr: true,
		},
		{
			name: "currency empty",
			cmd: func() Command {
				c := sim()
				c.Entries[0].Currency = ""
				c.Entries[1].Currency = ""
				return c
			}(),
			wantErr: true,
		},
		{
			name: "currency lowercase",
			cmd: func() Command {
				c := sim()
				c.Entries[0].Currency = "rub"
				c.Entries[1].Currency = "rub"
				return c
			}(),
			wantErr: true,
		},
		{
			name: "unbalanced",
			cmd: func() Command {
				c := sim()
				c.Entries[1].Amount = 99
				return c
			}(),
			wantErr: true,
		},
		{
			name: "balanced overall, not per currency",
			cmd: func() Command {
				c := sim()
				c.Entries = []Entry{
					{ID: id(3), LedgerAccountID: id(4), Type: Debit, Amount: 10, Currency: "RUB"},
					{ID: id(5), LedgerAccountID: id(6), Type: Credit, Amount: 5, Currency: "RUB"},
					{ID: id(7), LedgerAccountID: id(8), Type: Debit, Amount: 5, Currency: "USD"},
					{ID: id(12), LedgerAccountID: id(13), Type: Credit, Amount: 10, Currency: "USD"},
				}
				return c
			}(),
			wantErr: true,
		},
		{
			name: "two currencies balanced",
			cmd: func() Command {
				c := sim()
				c.Entries = []Entry{
					{ID: id(3), LedgerAccountID: id(4), Type: Debit, Amount: 10, Currency: "RUB"},
					{ID: id(5), LedgerAccountID: id(6), Type: Credit, Amount: 10, Currency: "RUB"},
					{ID: id(7), LedgerAccountID: id(8), Type: Debit, Amount: 5, Currency: "USD"},
					{ID: id(12), LedgerAccountID: id(13), Type: Credit, Amount: 5, Currency: "USD"},
				}
				return c
			}(),
		},
		{
			name: "three entries",
			cmd: func() Command {
				c := sim()
				c.Operation = OpTransfer
				c.ReferenceType = refTransfer
				c.Entries = []Entry{
					{ID: id(3), LedgerAccountID: id(4), Type: Debit, Amount: 100, Currency: "RUB"},
					{ID: id(5), LedgerAccountID: id(6), Type: Credit, Amount: 90, Currency: "RUB"},
					{ID: id(7), LedgerAccountID: id(8), Type: Credit, Amount: 10, Currency: "RUB"},
				}
				return c
			}(),
		},
		{
			name: "unknown operation",
			cmd: func() Command {
				c := sim()
				c.Operation = "PAYOUT_INIT"
				return c
			}(),
			wantErr: true,
		},
		{
			name: "SIM_TOPUP wrong reference_type",
			cmd: func() Command {
				c := sim()
				c.ReferenceType = refTransfer
				return c
			}(),
			wantErr: true,
		},
		{
			name: "negative operation_seq",
			cmd: func() Command {
				c := sim()
				c.OperationSeq = -1
				return c
			}(),
			wantErr: true,
		},
		{
			name: "hold_action on SIM_TOPUP",
			cmd: func() Command {
				c := sim()
				c.Hold = &HoldAction{Action: HoldPlace, HoldID: id(20), AccountID: id(21), Amount: 1, Currency: "RUB"}
				return c
			}(),
			wantErr: true,
		},
		{
			name: "HOLD_PLACE missing hold",
			cmd: func() Command {
				c := sim()
				c.Operation = OpHoldPlace
				c.ReferenceType = refHold
				return c
			}(),
			wantErr: true,
		},
		{
			name: "HOLD_PLACE ok",
			cmd: func() Command {
				c := sim()
				c.Operation = OpHoldPlace
				c.ReferenceType = refHold
				c.Hold = &HoldAction{
					Action: HoldPlace, HoldID: id(20), AccountID: id(21),
					Amount: 100, Currency: "RUB", ExpiresAt: &exp,
					ReferenceType: refPayment, ReferenceID: &pay,
				}
				return c
			}(),
		},
		{
			name: "HOLD_PLACE wrong action",
			cmd: func() Command {
				c := sim()
				c.Operation = OpHoldPlace
				c.ReferenceType = refHold
				c.Hold = &HoldAction{
					Action: HoldCapture, HoldID: id(20), AccountID: id(21),
					Amount: 100, Currency: "RUB", ExpiresAt: &exp,
					ReferenceType: refPayment, ReferenceID: &pay,
				}
				return c
			}(),
			wantErr: true,
		},
		{
			name: "HOLD_PLACE missing expires_at",
			cmd: func() Command {
				c := sim()
				c.Operation = OpHoldPlace
				c.ReferenceType = refHold
				c.Hold = &HoldAction{
					Action: HoldPlace, HoldID: id(20), AccountID: id(21),
					Amount: 100, Currency: "RUB",
					ReferenceType: refPayment, ReferenceID: &pay,
				}
				return c
			}(),
			wantErr: true,
		},
		{
			name: "HOLD_RELEASE EXPIRE ok",
			cmd: func() Command {
				c := sim()
				c.Operation = OpHoldRelease
				c.ReferenceType = refHold
				c.Hold = &HoldAction{
					Action: HoldExpire, HoldID: id(20), AccountID: id(21),
					Amount: 100, Currency: "RUB",
				}
				return c
			}(),
		},
		{
			name: "REVERSAL missing reverses_journal_id",
			cmd: func() Command {
				c := sim()
				c.Operation = OpReversal
				c.ReferenceType = refSimTopup
				return c
			}(),
			wantErr: true,
		},
		{
			name: "REVERSAL ok",
			cmd: func() Command {
				c := sim()
				c.Operation = OpReversal
				c.ReferenceType = refSimTopup
				c.ReversesJournalID = &orig
				return c
			}(),
		},
		{
			name: "reverses_journal_id on SIM_TOPUP",
			cmd: func() Command {
				c := sim()
				c.ReversesJournalID = &orig
				return c
			}(),
			wantErr: true,
		},
		{
			name: "adjustment_id on ADJUSTMENT",
			cmd: func() Command {
				c := sim()
				c.Operation = OpAdjustment
				c.ReferenceType = refAdjustment
				c.AdjustmentID = &adj
				return c
			}(),
			wantErr: true,
		},
		{
			name: "ADJUSTMENT ok",
			cmd: func() Command {
				c := sim()
				c.Operation = OpAdjustment
				c.ReferenceType = refAdjustment
				return c
			}(),
		},
		{
			name: "REVERSAL with adjustment_id",
			cmd: func() Command {
				c := sim()
				c.Operation = OpReversal
				c.ReferenceType = refSimTopup
				c.ReversesJournalID = &orig
				c.AdjustmentID = &adj
				return c
			}(),
		},
		{
			name: "REVERSAL nil adjustment uuid",
			cmd: func() Command {
				c := sim()
				c.Operation = OpReversal
				c.ReferenceType = refSimTopup
				c.ReversesJournalID = &orig
				z := uuid.Nil
				c.AdjustmentID = &z
				return c
			}(),
			wantErr: true,
		},
		{
			name: "nullable description and initiated_by",
			cmd: func() Command {
				c := sim()
				c.Description = ""
				c.InitiatedBy = nil
				c.ReversesJournalID = nil
				c.AdjustmentID = nil
				c.Hold = nil
				return c
			}(),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate(tc.cmd)
			if tc.wantErr {
				if !errors.Is(err, ErrInvalidJournal) {
					t.Fatalf("want ErrInvalidJournal, got %v", err)
				}
				var de *Error
				if !errors.As(err, &de) || de.Message != ErrInvalidJournal.Message {
					t.Fatalf("public Message = %v", err)
				}
				if strings.Contains(de.Message, "INVALID_JOURNAL:") || strings.Contains(err.Error(), "RAISE") {
					t.Fatalf("SQL text in error: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Validate: %v", err)
			}
		})
	}
}

func TestAddMinorOverflow(t *testing.T) {
	if _, ok := addMinor(math.MaxInt64, 1); ok {
		t.Fatal("MaxInt64+1 must overflow")
	}
	sum, ok := addMinor(MaxAmount, MaxAmount)
	if !ok || sum != 2*MaxAmount {
		t.Fatalf("MAX+MAX = %d ok=%v", sum, ok)
	}
}

func TestWithValidation(t *testing.T) {
	day := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)
	var u uuid.UUID
	u[15] = 1
	u[6] = 0x40
	u[8] = 0x80
	okCmd := Command{
		JournalID: u, Operation: OpSimTopup, ReferenceType: refSimTopup,
		ReferenceID: u, AccountingDate: day,
		Entries: []Entry{
			{ID: u, LedgerAccountID: uuid.MustParse("00000000-0000-4000-8000-000000000002"), Type: Debit, Amount: 1, Currency: "RUB"},
			{ID: uuid.MustParse("00000000-0000-4000-8000-000000000003"), LedgerAccountID: uuid.MustParse("00000000-0000-4000-8000-000000000004"), Type: Credit, Amount: 1, Currency: "RUB"},
		},
	}
	stub := &stubPoster{res: Result{JournalID: okCmd.JournalID}}
	p := WithValidation(stub)

	bad := okCmd
	bad.Entries = nil
	if _, err := p.Post(context.Background(), bad); !errors.Is(err, ErrInvalidJournal) {
		t.Fatalf("invalid must fail before Post, got %v", err)
	}
	if stub.called {
		t.Fatal("Poster.Post called for invalid Command")
	}

	res, err := p.Post(context.Background(), okCmd)
	if err != nil || !stub.called || res.JournalID != okCmd.JournalID {
		t.Fatalf("valid Post: res=%+v err=%v called=%v", res, err, stub.called)
	}
}

type stubPoster struct {
	called bool
	res    Result
}

func (s *stubPoster) Post(context.Context, Command) (Result, error) {
	s.called = true
	return s.res, nil
}
