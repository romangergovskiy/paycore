package ledger

import (
	"context"
	"math"

	"github.com/google/uuid"
)

// MaxAmount is D-35: 10^14 minor units. Matches ledger_entries CHECK.
const MaxAmount int64 = 100_000_000_000_000

const (
	refSimTopup   = "SIM_TOPUP"
	refTransfer   = "TRANSFER"
	refHold       = "HOLD"
	refRefund     = "REFUND"
	refPayout     = "PAYOUT"
	refAdjustment = "ADJUSTMENT"
	refPayment    = "PAYMENT"
)

// Validate checks Command shape without database state. Call it before
// Poster.Post. Use cases compose WithValidation(postgres.NewLedger(tx)).
//
// It does not re-check post_journal forms, account existence, hold rows,
// or status. Failures are ErrInvalidJournal (errors.Is); Message has no SQL.
func Validate(cmd Command) error {
	if cmd.JournalID == uuid.Nil || cmd.Operation == "" || cmd.ReferenceType == "" ||
		cmd.ReferenceID == uuid.Nil || cmd.AccountingDate.IsZero() {
		return ErrInvalidJournal
	}
	if cmd.OperationSeq < 0 {
		return ErrInvalidJournal
	}
	if !knownOperation(cmd.Operation) {
		return ErrInvalidJournal
	}
	if !validReferenceType(cmd.Operation, cmd.ReferenceType) {
		return ErrInvalidJournal
	}
	if err := validateReversalAdjustment(cmd); err != nil {
		return err
	}
	if err := validateHold(cmd); err != nil {
		return err
	}
	if len(cmd.Entries) < 2 {
		return ErrInvalidJournal
	}

	seen := make(map[uuid.UUID]struct{}, len(cmd.Entries))
	type totals struct{ debit, credit int64 }
	byCur := make(map[string]*totals, 2)
	for _, e := range cmd.Entries {
		if e.ID == uuid.Nil || e.LedgerAccountID == uuid.Nil {
			return ErrInvalidJournal
		}
		if e.Type != Debit && e.Type != Credit {
			return ErrInvalidJournal
		}
		if e.Amount <= 0 || e.Amount > MaxAmount {
			return ErrInvalidJournal
		}
		if !validCurrency(e.Currency) {
			return ErrInvalidJournal
		}
		if _, dup := seen[e.LedgerAccountID]; dup {
			return ErrInvalidJournal
		}
		seen[e.LedgerAccountID] = struct{}{}

		t := byCur[e.Currency]
		if t == nil {
			t = &totals{}
			byCur[e.Currency] = t
		}
		if e.Type == Debit {
			sum, ok := addMinor(t.debit, e.Amount)
			if !ok {
				return ErrInvalidJournal
			}
			t.debit = sum
		} else {
			sum, ok := addMinor(t.credit, e.Amount)
			if !ok {
				return ErrInvalidJournal
			}
			t.credit = sum
		}
	}
	for _, t := range byCur {
		if t.debit != t.credit {
			return ErrInvalidJournal
		}
	}
	return nil
}

type validatingPoster struct{ next Poster }

// WithValidation runs Validate before delegating to p.Post.
func WithValidation(p Poster) Poster {
	return validatingPoster{next: p}
}

func (v validatingPoster) Post(ctx context.Context, cmd Command) (Result, error) {
	if err := Validate(cmd); err != nil {
		return Result{}, err
	}
	return v.next.Post(ctx, cmd)
}

func knownOperation(op Operation) bool {
	switch op {
	case OpSimTopup, OpTransfer, OpHoldPlace, OpHoldCapture, OpHoldRelease,
		OpRefund, OpPayout, OpReversal, OpAdjustment:
		return true
	default:
		return false
	}
}

func validReferenceType(op Operation, ref string) bool {
	switch op {
	case OpSimTopup:
		return ref == refSimTopup
	case OpTransfer:
		return ref == refTransfer
	case OpHoldPlace, OpHoldCapture, OpHoldRelease:
		return ref == refHold
	case OpRefund:
		return ref == refRefund
	case OpPayout:
		return ref == refPayout
	case OpAdjustment:
		return ref == refAdjustment
	case OpReversal:
		switch ref {
		case refSimTopup, refTransfer, refHold, refRefund, refPayout, refAdjustment:
			return true
		default:
			return false
		}
	default:
		return false
	}
}

func validateReversalAdjustment(cmd Command) error {
	if cmd.Operation == OpReversal {
		if cmd.ReversesJournalID == nil || *cmd.ReversesJournalID == uuid.Nil {
			return ErrInvalidJournal
		}
	} else if cmd.ReversesJournalID != nil {
		return ErrInvalidJournal
	}
	if cmd.Operation != OpReversal {
		if cmd.AdjustmentID != nil {
			return ErrInvalidJournal
		}
	} else if cmd.AdjustmentID != nil && *cmd.AdjustmentID == uuid.Nil {
		return ErrInvalidJournal
	}
	return nil
}

func validateHold(cmd Command) error {
	needsHold := cmd.Operation == OpHoldPlace || cmd.Operation == OpHoldCapture || cmd.Operation == OpHoldRelease
	if !needsHold {
		if cmd.Hold != nil {
			return ErrInvalidJournal
		}
		return nil
	}
	if cmd.Hold == nil {
		return ErrInvalidJournal
	}
	h := cmd.Hold
	if h.HoldID == uuid.Nil || h.AccountID == uuid.Nil {
		return ErrInvalidJournal
	}
	if h.Amount <= 0 || h.Amount > MaxAmount {
		return ErrInvalidJournal
	}
	if !validCurrency(h.Currency) {
		return ErrInvalidJournal
	}
	switch cmd.Operation {
	case OpHoldPlace:
		if h.Action != HoldPlace || h.ExpiresAt == nil || h.ExpiresAt.IsZero() ||
			h.ReferenceType != refPayment || h.ReferenceID == nil || *h.ReferenceID == uuid.Nil {
			return ErrInvalidJournal
		}
	case OpHoldCapture:
		if h.Action != HoldCapture {
			return ErrInvalidJournal
		}
	case OpHoldRelease:
		if h.Action != HoldRelease && h.Action != HoldExpire {
			return ErrInvalidJournal
		}
	}
	return nil
}

func validCurrency(code string) bool {
	if len(code) != 3 {
		return false
	}
	for i := 0; i < 3; i++ {
		if code[i] < 'A' || code[i] > 'Z' {
			return false
		}
	}
	return true
}

func addMinor(a, b int64) (int64, bool) {
	if b > 0 && a > math.MaxInt64-b {
		return 0, false
	}
	if b < 0 && a < math.MinInt64-b {
		return 0, false
	}
	return a + b, true
}
