package ledger

import (
	"context"

	"github.com/google/uuid"
)

type OwnerType string

const (
	OwnerCustomer OwnerType = "CUSTOMER"
	OwnerMerchant OwnerType = "MERCHANT"
)

type AccountKind string

const (
	KindAvailable AccountKind = "AVAILABLE"
	KindHold      AccountKind = "HOLD"
)

// OpenedLedger is one row of ledger.open_ledger_accounts.
type OpenedLedger struct {
	Kind AccountKind
	ID   uuid.UUID
}

// AccountOpener creates (or returns) the AVAILABLE/HOLD pair for a business account.
// The implementation must not open a nested transaction.
type AccountOpener interface {
	Open(ctx context.Context, accountID uuid.UUID, owner OwnerType, currency string) ([]OpenedLedger, error)
}

// BalanceSnapshot is one row of ledger.lock_account_balances (L5).
type BalanceSnapshot struct {
	AccountID uuid.UUID
	Currency  string
	Available int64
	Reserved  int64
	Version   int64
}

// BalanceLocker takes L5 FOR UPDATE on account_balances. Locks last until the
// caller's transaction ends. The implementation must not Begin.
type BalanceLocker interface {
	Lock(ctx context.Context, accountIDs []uuid.UUID) ([]BalanceSnapshot, error)
}
