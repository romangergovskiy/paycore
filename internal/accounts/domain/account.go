// Package domain is the Accounts business model (accounts.accounts).
//
// It does not import pgx or HTTP. Balances live in Ledger; this package
// models the business account row. Opening a ledger pair is AccountOpener
// at the application boundary (string currency, not money.Money).
package domain

import (
	"time"

	"github.com/google/uuid"

	"github.com/romangergovskiy/paycore/pkg/money"
)

type Purpose string

const (
	PurposeMain               Purpose = "MAIN"
	PurposeSavings            Purpose = "SAVINGS"
	PurposeMerchantSettlement Purpose = "MERCHANT_SETTLEMENT"
)

type Status string

const (
	StatusActive  Status = "ACTIVE"
	StatusBlocked Status = "BLOCKED"
	StatusClosed  Status = "CLOSED"
)

// Account is one row of accounts.accounts (00002).
type Account struct {
	ID            uuid.UUID
	AccountNumber string
	UserID        *uuid.UUID
	MerchantID    *uuid.UUID
	Purpose       Purpose
	Currency      money.Currency
	Status        Status
	Name          string
	Version       int64
	CreatedAt     time.Time
	UpdatedAt     time.Time
	ClosedAt      *time.Time
}

// OpenCommand is the input of opening a business account.
// Status is not a field: Open always creates ACTIVE.
// Account number format is Q-13 (unset); the caller supplies a non-empty number.
// Reason and ChangedBy are required (uuid.Nil is missing); they are stored
// as-is on opening history and are not generated or replaced.
type OpenCommand struct {
	ID            uuid.UUID // uuid.Nil → generate UUIDv7 (D-14)
	AccountNumber string
	UserID        *uuid.UUID
	MerchantID    *uuid.UUID
	Purpose       Purpose
	Currency      money.Currency
	Name          string
	Reason        string
	ChangedBy     uuid.UUID
}

func OwnerSet(id *uuid.UUID) bool {
	return id != nil && *id != uuid.Nil
}

// StatusCommand is Block, Unblock, or Close. ChangedBy is required (uuid.Nil
// is missing); it is stored as-is and is not generated or replaced.
type StatusCommand struct {
	ID        uuid.UUID
	Reason    string
	ChangedBy uuid.UUID
}

// StatusHistory is one row of accounts.account_status_history.
// From empty means SQL NULL (opening history; no previous status).
type StatusHistory struct {
	ID        uuid.UUID
	AccountID uuid.UUID
	From      Status
	To        Status
	Reason    string
	ChangedBy uuid.UUID
}
