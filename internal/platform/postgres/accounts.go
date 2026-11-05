package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/romangergovskiy/paycore/internal/accounts/application"
	"github.com/romangergovskiy/paycore/internal/accounts/domain"
	"github.com/romangergovskiy/paycore/internal/platform/apperror"
	"github.com/romangergovskiy/paycore/pkg/money"
)

// execQueryer is satisfied by *pgxpool.Pool, pgx.Tx, and *pgx.Conn.
// Accounts does not Begin: Unit of Work (or a test) owns the transaction so
// Create, status changes, and ledger adapters share one Tx.
type execQueryer interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Accounts is the PostgreSQL Store for accounts.accounts. It performs no
// ledger DML; opening the AVAILABLE/HOLD pair is AccountOpener on the same Tx.
type Accounts struct {
	q execQueryer
}

func NewAccounts(q execQueryer) *Accounts {
	return &Accounts{q: q}
}

var _ application.Store = (*Accounts)(nil)

const insertAccountSQL = `
INSERT INTO accounts.accounts (
  id, account_number, user_id, merchant_id, account_purpose, currency,
  status, name, version, closed_at
) VALUES (
  $1, $2, $3, $4, $5, $6::char(3), $7, NULLIF($8, ''), $9, $10
)`

const selectAccountSQL = `
SELECT id, account_number, user_id, merchant_id, account_purpose,
       rtrim(currency::text), status, name, version, created_at, updated_at, closed_at
  FROM accounts.accounts
 WHERE id = $1`

const listByUserSQL = `
SELECT id, account_number, user_id, merchant_id, account_purpose,
       rtrim(currency::text), status, name, version, created_at, updated_at, closed_at
  FROM accounts.accounts
 WHERE user_id = $1 AND merchant_id IS NULL
 ORDER BY created_at ASC, id ASC`

const listByMerchantSQL = `
SELECT id, account_number, user_id, merchant_id, account_purpose,
       rtrim(currency::text), status, name, version, created_at, updated_at, closed_at
  FROM accounts.accounts
 WHERE merchant_id = $1 AND user_id IS NULL
 ORDER BY created_at ASC, id ASC`

const selectAccountByNumberSQL = `
SELECT id, account_number, user_id, merchant_id, account_purpose,
       rtrim(currency::text), status, name, version, created_at, updated_at, closed_at
  FROM accounts.accounts
 WHERE account_number = $1`

const selectAccountForUpdateSQL = selectAccountSQL + ` FOR NO KEY UPDATE`

const applyStatusSQL = `
UPDATE accounts.accounts
   SET status = $2, version = $3, updated_at = now(), closed_at = $4
 WHERE id = $1`

const insertStatusHistorySQL = `
INSERT INTO accounts.account_status_history (
  id, account_id, from_status, to_status, reason, changed_by
) VALUES ($1, $2, $3, $4, $5, $6)`

func (a *Accounts) Create(ctx context.Context, acc domain.Account) error {
	if acc.ID == uuid.Nil {
		return domain.ErrInvalidAccount
	}
	_, err := a.q.Exec(ctx, insertAccountSQL,
		acc.ID,
		acc.AccountNumber,
		ownerArg(acc.UserID),
		ownerArg(acc.MerchantID),
		string(acc.Purpose),
		string(acc.Currency),
		string(acc.Status),
		acc.Name,
		acc.Version,
		acc.ClosedAt,
	)
	if err != nil {
		return mapAccountSQLError("create", err)
	}
	return nil
}

func (a *Accounts) Get(ctx context.Context, id uuid.UUID) (domain.Account, error) {
	if id == uuid.Nil {
		return domain.Account{}, domain.ErrAccountNotFound
	}
	acc, err := scanAccount(a.q.QueryRow(ctx, selectAccountSQL, id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.Account{}, domain.ErrAccountNotFound
		}
		return domain.Account{}, mapAccountSQLError("get", err)
	}
	return acc, nil
}

func (a *Accounts) GetByNumber(ctx context.Context, number string) (domain.Account, error) {
	if number == "" {
		return domain.Account{}, domain.ErrInvalidAccount
	}
	acc, err := scanAccount(a.q.QueryRow(ctx, selectAccountByNumberSQL, number))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.Account{}, domain.ErrAccountNotFound
		}
		return domain.Account{}, mapAccountSQLError("get_by_number", err)
	}
	return acc, nil
}

func (a *Accounts) GetForUpdate(ctx context.Context, id uuid.UUID) (domain.Account, error) {
	if id == uuid.Nil {
		return domain.Account{}, domain.ErrAccountNotFound
	}
	acc, err := scanAccount(a.q.QueryRow(ctx, selectAccountForUpdateSQL, id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.Account{}, domain.ErrAccountNotFound
		}
		return domain.Account{}, mapAccountSQLError("get_for_update", err)
	}
	return acc, nil
}

func (a *Accounts) ApplyStatus(ctx context.Context, acc domain.Account) error {
	if acc.ID == uuid.Nil {
		return domain.ErrAccountNotFound
	}
	tag, err := a.q.Exec(ctx, applyStatusSQL, acc.ID, string(acc.Status), acc.Version, acc.ClosedAt)
	if err != nil {
		return mapAccountSQLError("apply_status", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrAccountNotFound
	}
	return nil
}

func (a *Accounts) AppendHistory(ctx context.Context, h domain.StatusHistory) error {
	_, err := a.q.Exec(ctx, insertStatusHistorySQL,
		h.ID, h.AccountID, string(h.From), string(h.To), h.Reason, h.ChangedBy,
	)
	if err != nil {
		return mapAccountSQLError("append_history", err)
	}
	return nil
}

func (a *Accounts) ListByOwner(ctx context.Context, userID, merchantID *uuid.UUID) ([]domain.Account, error) {
	if err := domain.ValidateOwner(userID, merchantID); err != nil {
		return nil, err
	}
	empty := make([]domain.Account, 0)
	var (
		rows pgx.Rows
		err  error
	)
	if domain.OwnerSet(userID) {
		rows, err = a.q.Query(ctx, listByUserSQL, *userID)
	} else {
		rows, err = a.q.Query(ctx, listByMerchantSQL, *merchantID)
	}
	if err != nil {
		return nil, mapAccountSQLError("list", err)
	}
	defer rows.Close()

	out := empty
	for rows.Next() {
		acc, err := scanAccount(rows)
		if err != nil {
			return nil, fmt.Errorf("accounts list: scan: %w", err)
		}
		out = append(out, acc)
	}
	if err := rows.Err(); err != nil {
		return nil, mapAccountSQLError("list", err)
	}
	return out, nil
}

func ownerArg(id *uuid.UUID) any {
	if !domain.OwnerSet(id) {
		return nil
	}
	return *id
}

type scanner interface {
	Scan(dest ...any) error
}

func scanAccount(row scanner) (domain.Account, error) {
	var (
		acc                  domain.Account
		purpose, status, cur string
		name                 *string
		userID, merchantID   *uuid.UUID
	)
	if err := row.Scan(
		&acc.ID, &acc.AccountNumber, &userID, &merchantID, &purpose,
		&cur, &status, &name, &acc.Version, &acc.CreatedAt, &acc.UpdatedAt, &acc.ClosedAt,
	); err != nil {
		return domain.Account{}, err
	}
	acc.UserID = userID
	acc.MerchantID = merchantID
	acc.Purpose = domain.Purpose(purpose)
	acc.Currency = money.Currency(strings.TrimSpace(cur))
	acc.Status = domain.Status(status)
	if name != nil {
		acc.Name = *name
	}
	return acc, nil
}

func mapAccountSQLError(op string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("accounts %s: %w", op, err)
	}
	var pe *pgconn.PgError
	if !errors.As(err, &pe) {
		return fmt.Errorf("accounts %s: %w", op, err)
	}
	switch pe.Code {
	case "23505":
		return wrapAccount(err, domain.ErrInvalidAccount, apperror.KindConflict)
	case "23502", "23503", "23514":
		return wrapAccount(err, domain.ErrInvalidAccount, apperror.KindInvalid)
	default:
		return fmt.Errorf("accounts %s: %w", op, err)
	}
}

func wrapAccount(cause error, sentinel *domain.Error, kind apperror.Kind) error {
	return apperror.Wrap(fmt.Errorf("%w: %w", sentinel, cause), kind, sentinel.Code, sentinel.Message)
}
