// Package ledger is the domain contract for posting journals.
//
// It does not import pgx or HTTP. Accounting invariants live in
// ledger.post_journal (SQL); this package only types the call and names
// the SQLSTATE-mapped errors.
package ledger

import (
	"context"
	"time"

	"github.com/google/uuid"
)

type Operation string

const (
	OpSimTopup    Operation = "SIM_TOPUP"
	OpTransfer    Operation = "TRANSFER"
	OpHoldPlace   Operation = "HOLD_PLACE"
	OpHoldCapture Operation = "HOLD_CAPTURE"
	OpHoldRelease Operation = "HOLD_RELEASE"
	OpRefund      Operation = "REFUND"
	OpPayout      Operation = "PAYOUT"
	OpReversal    Operation = "REVERSAL"
	OpAdjustment  Operation = "ADJUSTMENT"
)

type EntryType string

const (
	Debit  EntryType = "DEBIT"
	Credit EntryType = "CREDIT"
)

type HoldOp string

const (
	HoldPlace   HoldOp = "PLACE"
	HoldCapture HoldOp = "CAPTURE"
	HoldRelease HoldOp = "RELEASE"
	HoldExpire  HoldOp = "EXPIRE"
)

// Entry is one side of a journal. Amount is minor units (D-17).
type Entry struct {
	ID              uuid.UUID
	LedgerAccountID uuid.UUID
	Type            EntryType
	Amount          int64
	Currency        string
}

// HoldAction is hold_action jsonb for HOLD_* operations. Nil fields stay SQL NULL.
type HoldAction struct {
	Action        HoldOp
	HoldID        uuid.UUID
	AccountID     uuid.UUID
	Amount        int64
	Currency      string
	ExpiresAt     *time.Time
	ReferenceType string
	ReferenceID   *uuid.UUID
}

// Command is the typed argument list of ledger.post_journal.
type Command struct {
	JournalID         uuid.UUID
	Operation         Operation
	ReferenceType     string
	ReferenceID       uuid.UUID
	OperationSeq      int16 // 0 means SQL default 1
	ReversesJournalID *uuid.UUID
	AdjustmentID      *uuid.UUID
	Description       string
	InitiatedBy       *uuid.UUID
	AccountingDate    time.Time
	Entries           []Entry
	Hold              *HoldAction
}

type PostedEntry struct {
	ID              uuid.UUID
	LedgerAccountID uuid.UUID
	Type            EntryType
	Amount          int64
	Currency        string
	AccountVersion  *int64
	BalanceAfter    *int64
}

type Result struct {
	JournalID     uuid.UUID
	AlreadyPosted bool
	Entries       []PostedEntry
}

// Poster posts a journal through the SQL gateway. The implementation must not
// open a nested transaction: the caller owns Tx or uses a pool statement.
type Poster interface {
	Post(ctx context.Context, cmd Command) (Result, error)
}

// Stable codes for SQLSTATE PC400/402/403/409/460 (ledger-model.md §11.3).
// HTTP Kind is applied outside this package.
const (
	CodeInvalidJournal            = "INVALID_JOURNAL"
	CodeInsufficientFunds         = "INSUFFICIENT_FUNDS"
	CodeSystemAccountInsufficient = "SYSTEM_ACCOUNT_INSUFFICIENT"
	CodeAccountNotActive          = "ACCOUNT_NOT_ACTIVE"
	CodeConflict                  = "CONFLICT"
	CodeLockProtocol              = "LOCK_PROTOCOL_VIOLATION"
)

// Error is a domain posting error. Compare with errors.Is against the sentinels.
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	return e.Code + ": " + e.Message
}

func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && e != nil && t.Code != "" && t.Code == e.Code
}

var (
	ErrInvalidJournal            = &Error{Code: CodeInvalidJournal, Message: "Journal is invalid."}
	ErrInsufficientFunds         = &Error{Code: CodeInsufficientFunds, Message: "Insufficient funds."}
	ErrSystemAccountInsufficient = &Error{
		Code: CodeSystemAccountInsufficient, Message: "System account has insufficient funds.",
	}
	ErrAccountNotActive = &Error{Code: CodeAccountNotActive, Message: "Account is not active for this operation."}
	ErrConflict         = &Error{Code: CodeConflict, Message: "Journal conflicts with an existing posting."}
	ErrLockProtocol     = &Error{Code: CodeLockProtocol, Message: "Caller missed L3 FOR SHARE on accounts."}
)
