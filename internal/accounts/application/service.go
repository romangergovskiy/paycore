// Package application is Accounts use cases. Persistence is a Store port;
// there is no accounts SQL gateway yet. The postgres adapter is the next stage.
// Caller owns the transaction so Store.Create and AccountOpener.Open are atomic.
package application

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/romangergovskiy/paycore/internal/accounts/domain"
	"github.com/romangergovskiy/paycore/internal/ledger"
)

var errNotWired = errors.New("accounts: store and opener are required")

// Store persists business accounts. The implementation must not Begin
// and must not write ledger tables (use AccountOpener).
type Store interface {
	Create(ctx context.Context, acc domain.Account) error
	Get(ctx context.Context, id uuid.UUID) (domain.Account, error)
	ListByOwner(ctx context.Context, userID, merchantID *uuid.UUID) ([]domain.Account, error)
}

type Service struct {
	Store  Store
	Opener ledger.AccountOpener
}

type Opened struct {
	Account domain.Account
	Ledgers []ledger.OpenedLedger
}

func (s Service) Open(ctx context.Context, cmd domain.OpenCommand) (Opened, error) {
	var zero Opened
	if err := domain.ValidateOpen(cmd); err != nil {
		return zero, err
	}
	if s.Store == nil || s.Opener == nil {
		return zero, errNotWired
	}

	id := cmd.ID
	if id == uuid.Nil {
		generated, err := uuid.NewV7()
		if err != nil {
			return zero, err
		}
		id = generated
	}

	acc := domain.Account{
		ID:            id,
		AccountNumber: cmd.AccountNumber,
		UserID:        cmd.UserID,
		MerchantID:    cmd.MerchantID,
		Purpose:       cmd.Purpose,
		Currency:      cmd.Currency,
		Status:        domain.StatusActive,
		Name:          cmd.Name,
		Version:       0,
	}
	if !domain.OwnerSet(acc.UserID) {
		acc.UserID = nil
	}
	if !domain.OwnerSet(acc.MerchantID) {
		acc.MerchantID = nil
	}

	if err := s.Store.Create(ctx, acc); err != nil {
		return zero, err
	}

	// Ledger API is int64/string. Convert currency at this boundary.
	ledgers, err := s.Opener.Open(ctx, acc.ID, ledgerOwner(acc), string(acc.Currency))
	if err != nil {
		return zero, err
	}
	return Opened{Account: acc, Ledgers: ledgers}, nil
}

func (s Service) Get(ctx context.Context, id uuid.UUID) (domain.Account, error) {
	if s.Store == nil {
		return domain.Account{}, errNotWired
	}
	if id == uuid.Nil {
		return domain.Account{}, domain.ErrAccountNotFound
	}
	return s.Store.Get(ctx, id)
}

func (s Service) ListByOwner(ctx context.Context, userID, merchantID *uuid.UUID) ([]domain.Account, error) {
	if err := domain.ValidateOwner(userID, merchantID); err != nil {
		return nil, err
	}
	if s.Store == nil {
		return nil, errNotWired
	}
	return s.Store.ListByOwner(ctx, userID, merchantID)
}

func ledgerOwner(acc domain.Account) ledger.OwnerType {
	if domain.OwnerSet(acc.MerchantID) {
		return ledger.OwnerMerchant
	}
	return ledger.OwnerCustomer
}
