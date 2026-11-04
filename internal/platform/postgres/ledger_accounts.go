package postgres

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/romangergovskiy/paycore/internal/ledger"
)

const openLedgerAccountsSQL = `
SELECT kind, ledger_account_id
  FROM ledger.open_ledger_accounts($1::uuid, $2, $3::char(3))`

const lockAccountBalancesSQL = `
SELECT account_id, currency::text, available_amount, reserved_amount, version
  FROM ledger.lock_account_balances($1::uuid[])`

// Open calls ledger.open_ledger_accounts. It does not Begin.
func (l *Ledger) Open(ctx context.Context, accountID uuid.UUID, owner ledger.OwnerType, currency string) ([]ledger.OpenedLedger, error) {
	rows, err := l.q.Query(ctx, openLedgerAccountsSQL, accountID, string(owner), currency)
	if err != nil {
		return nil, mapLedgerSQLError("open_ledger_accounts", err)
	}
	defer rows.Close()

	var out []ledger.OpenedLedger
	for rows.Next() {
		var row ledger.OpenedLedger
		var kind string
		if err := rows.Scan(&kind, &row.ID); err != nil {
			return nil, fmt.Errorf("ledger open_ledger_accounts: scan: %w", err)
		}
		row.Kind = ledger.AccountKind(kind)
		out = append(out, row)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, mapLedgerSQLError("open_ledger_accounts", err)
	}
	return out, nil
}

// Lock calls ledger.lock_account_balances. Locks belong to the caller's Tx.
func (l *Ledger) Lock(ctx context.Context, accountIDs []uuid.UUID) ([]ledger.BalanceSnapshot, error) {
	rows, err := l.q.Query(ctx, lockAccountBalancesSQL, accountIDs)
	if err != nil {
		return nil, mapLedgerSQLError("lock_account_balances", err)
	}
	defer rows.Close()

	var out []ledger.BalanceSnapshot
	for rows.Next() {
		var row ledger.BalanceSnapshot
		if err := rows.Scan(&row.AccountID, &row.Currency, &row.Available, &row.Reserved, &row.Version); err != nil {
			return nil, fmt.Errorf("ledger lock_account_balances: scan: %w", err)
		}
		out = append(out, row)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, mapLedgerSQLError("lock_account_balances", err)
	}
	return out, nil
}
