// Package application is Accounts use cases. Store and AccountOpener must
// not Begin. Writes that need both ports run inside UnitOfWork.Do so
// infrastructure owns Begin/Commit/Rollback.
package application

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/romangergovskiy/paycore/internal/accounts/domain"
	"github.com/romangergovskiy/paycore/internal/ledger"
)

var (
	errNotWired      = errors.New("accounts: store and opener are required")
	errCloseSnapshot = errors.New("accounts: close requires one balance snapshot for the account")
)

// Store persists business accounts. The implementation must not Begin
// and must not write ledger tables (use AccountOpener).
type Store interface {
	Create(ctx context.Context, acc domain.Account) error
	Get(ctx context.Context, id uuid.UUID) (domain.Account, error)
	GetByNumber(ctx context.Context, number string) (domain.Account, error)
	ListByOwner(ctx context.Context, userID, merchantID *uuid.UUID) ([]domain.Account, error)
	GetForUpdate(ctx context.Context, id uuid.UUID) (domain.Account, error)
	ApplyStatus(ctx context.Context, acc domain.Account) error
	AppendHistory(ctx context.Context, h domain.StatusHistory) error
}

// BalanceReader reads ledger.account_balances without locking (not L5).
// Amounts are int64 minor units, matching ledger.BalanceSnapshot fields.
type BalanceReader interface {
	Get(ctx context.Context, accountID uuid.UUID) (available, reserved, version int64, err error)
}

// MaxStatementLimit is the maximum StatementQuery.Limit.
const MaxStatementLimit = 100

// StatementCursor is a keyset position (created_at, id). It is not a SQL
// fragment and is not encoded by infrastructure.
type StatementCursor struct {
	CreatedAt time.Time
	ID        uuid.UUID
}

// StatementQuery pages a statement. Order is created_at ASC, id ASC.
// A nil Cursor is the first page. Limit is 1…MaxStatementLimit.
type StatementQuery struct {
	Limit  int
	Cursor *StatementCursor
}

// StatementPage is one page of statement lines. NextCursor is set from the
// last returned line when another row exists; it is nil on the last page.
type StatementPage struct {
	Lines      []StatementLine
	NextCursor *StatementCursor
}

// StatementLine is one POSTED ledger_entries row on this account's
// AVAILABLE or HOLD ledger account. Amount is a positive int64; Side is
// the stored DEBIT/CREDIT (ТЗ §4.4). BalanceAfter is nil on system rows.
type StatementLine struct {
	ID              uuid.UUID
	JournalID       uuid.UUID
	LedgerAccountID uuid.UUID
	Kind            ledger.AccountKind
	Side            ledger.EntryType
	Amount          int64
	Currency        string
	AccountVersion  *int64
	BalanceAfter    *int64
	CreatedAt       time.Time
	PostedAt        time.Time
}

// StatementReader lists POSTED ledger_entries for a business account.
// The implementation must not Begin and must not lock.
type StatementReader interface {
	List(ctx context.Context, accountID uuid.UUID, query StatementQuery) ([]StatementLine, error)
}

type Service struct {
	Store      Store
	Opener     ledger.AccountOpener
	Locker     ledger.BalanceLocker
	Balances   BalanceReader
	Statements StatementReader
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

	hid, err := uuid.NewV7()
	if err != nil {
		return zero, err
	}
	if err := s.Store.AppendHistory(ctx, domain.StatusHistory{
		ID:        hid,
		AccountID: acc.ID,
		To:        domain.StatusActive,
		Reason:    cmd.Reason,
		ChangedBy: cmd.ChangedBy,
	}); err != nil {
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

func (s Service) GetByNumber(ctx context.Context, number string) (domain.Account, error) {
	if s.Store == nil {
		return domain.Account{}, errNotWired
	}
	if number == "" {
		return domain.Account{}, domain.ErrInvalidAccount
	}
	return s.Store.GetByNumber(ctx, number)
}

func (s Service) Balance(ctx context.Context, id uuid.UUID) (available, reserved, version int64, err error) {
	if s.Store == nil || s.Balances == nil {
		return 0, 0, 0, errNotWired
	}
	if id == uuid.Nil {
		return 0, 0, 0, domain.ErrAccountNotFound
	}
	if _, err := s.Store.Get(ctx, id); err != nil {
		return 0, 0, 0, err
	}
	return s.Balances.Get(ctx, id)
}

func (s Service) Statement(ctx context.Context, accountID uuid.UUID, query StatementQuery) (StatementPage, error) {
	var zero StatementPage
	if s.Store == nil || s.Statements == nil {
		return zero, errNotWired
	}
	if accountID == uuid.Nil {
		return zero, domain.ErrAccountNotFound
	}
	if err := validateStatementQuery(query); err != nil {
		return zero, err
	}
	if _, err := s.Store.Get(ctx, accountID); err != nil {
		return zero, err
	}
	list, err := s.Statements.List(ctx, accountID, query)
	if err != nil {
		return zero, err
	}
	return statementPage(list, query.Limit), nil
}

func validateStatementQuery(query StatementQuery) error {
	if query.Limit < 1 || query.Limit > MaxStatementLimit {
		return domain.ErrInvalidAccount
	}
	if query.Cursor != nil && (query.Cursor.ID == uuid.Nil || query.Cursor.CreatedAt.IsZero()) {
		return domain.ErrInvalidAccount
	}
	return nil
}

func statementPage(lines []StatementLine, limit int) StatementPage {
	if lines == nil {
		lines = []StatementLine{}
	}
	if len(lines) <= limit {
		return StatementPage{Lines: lines}
	}
	out := append([]StatementLine(nil), lines[:limit]...)
	last := out[len(out)-1]
	return StatementPage{
		Lines:      out,
		NextCursor: &StatementCursor{CreatedAt: last.CreatedAt, ID: last.ID},
	}
}

func (s Service) ListByOwner(ctx context.Context, userID, merchantID *uuid.UUID) ([]domain.Account, error) {
	if err := domain.ValidateOwner(userID, merchantID); err != nil {
		return nil, err
	}
	if s.Store == nil {
		return nil, errNotWired
	}
	list, err := s.Store.ListByOwner(ctx, userID, merchantID)
	if err != nil {
		return nil, err
	}
	if list == nil {
		list = []domain.Account{}
	}
	return list, nil
}

func (s Service) Block(ctx context.Context, cmd domain.StatusCommand) (domain.Account, error) {
	return s.changeStatus(ctx, cmd, domain.StatusBlocked)
}

func (s Service) Unblock(ctx context.Context, cmd domain.StatusCommand) (domain.Account, error) {
	return s.changeStatus(ctx, cmd, domain.StatusActive)
}

func (s Service) Close(ctx context.Context, cmd domain.StatusCommand) (domain.Account, error) {
	return s.changeStatus(ctx, cmd, domain.StatusClosed)
}

func (s Service) changeStatus(ctx context.Context, cmd domain.StatusCommand, to domain.Status) (domain.Account, error) {
	var zero domain.Account
	if err := domain.ValidateStatusCommand(cmd); err != nil {
		return zero, err
	}
	if s.Store == nil {
		return zero, errNotWired
	}

	acc, err := s.Store.GetForUpdate(ctx, cmd.ID)
	if err != nil {
		return zero, err
	}
	if err := domain.ValidateTransition(acc.Status, to); err != nil {
		return zero, err
	}

	if to == domain.StatusClosed {
		if s.Locker == nil {
			return zero, errNotWired
		}
		snaps, err := s.Locker.Lock(ctx, []uuid.UUID{acc.ID})
		if err != nil {
			return zero, err
		}
		if err := requireEmptySnapshot(acc.ID, snaps); err != nil {
			return zero, err
		}
		now := time.Now().UTC()
		acc.ClosedAt = &now
	}

	from := acc.Status
	acc.Status = to
	acc.Version++
	if err := s.Store.ApplyStatus(ctx, acc); err != nil {
		return zero, err
	}

	hid, err := uuid.NewV7()
	if err != nil {
		return zero, err
	}
	if err := s.Store.AppendHistory(ctx, domain.StatusHistory{
		ID:        hid,
		AccountID: acc.ID,
		From:      from,
		To:        to,
		Reason:    cmd.Reason,
		ChangedBy: cmd.ChangedBy,
	}); err != nil {
		return zero, err
	}
	return acc, nil
}

func requireEmptySnapshot(id uuid.UUID, snaps []ledger.BalanceSnapshot) error {
	if len(snaps) != 1 || snaps[0].AccountID != id {
		return errCloseSnapshot
	}
	if snaps[0].Available != 0 || snaps[0].Reserved != 0 {
		return domain.ErrAccountNotEmpty
	}
	return nil
}

func ledgerOwner(acc domain.Account) ledger.OwnerType {
	if domain.OwnerSet(acc.MerchantID) {
		return ledger.OwnerMerchant
	}
	return ledger.OwnerCustomer
}
