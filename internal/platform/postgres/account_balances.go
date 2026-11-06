package postgres

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/romangergovskiy/paycore/internal/accounts/application"
	"github.com/romangergovskiy/paycore/internal/accounts/domain"
)

const selectAccountBalanceSQL = `
SELECT available_amount, reserved_amount, version
  FROM ledger.account_balances
 WHERE account_id = $1`

// AccountBalances is the read adapter for ledger.account_balances.
// It does not Begin, lock, or call lock_account_balances.
type AccountBalances struct {
	q execQueryer
}

func NewAccountBalances(q execQueryer) *AccountBalances {
	return &AccountBalances{q: q}
}

var _ application.BalanceReader = (*AccountBalances)(nil)

func (b *AccountBalances) Get(ctx context.Context, accountID uuid.UUID) (int64, int64, int64, error) {
	if accountID == uuid.Nil {
		return 0, 0, 0, domain.ErrAccountNotFound
	}
	var available, reserved, version int64
	err := b.q.QueryRow(ctx, selectAccountBalanceSQL, accountID).Scan(&available, &reserved, &version)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, 0, 0, domain.ErrAccountNotFound
		}
		return 0, 0, 0, mapLedgerSQLError("get_balance", err)
	}
	return available, reserved, version, nil
}
