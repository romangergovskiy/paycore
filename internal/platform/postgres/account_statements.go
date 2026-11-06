package postgres

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/romangergovskiy/paycore/internal/accounts/application"
	"github.com/romangergovskiy/paycore/internal/accounts/domain"
	"github.com/romangergovskiy/paycore/internal/ledger"
)

const selectAccountStatementSQL = `
SELECT e.id, e.journal_transaction_id, e.ledger_account_id, la.kind,
       e.entry_type, e.amount, rtrim(e.currency::text),
       e.account_version, e.balance_after, e.created_at, j.posted_at
  FROM ledger.ledger_accounts AS la
  JOIN ledger.ledger_entries AS e ON e.ledger_account_id = la.id
  JOIN ledger.journal_transactions AS j ON j.id = e.journal_transaction_id
 WHERE la.account_id = $1
   AND la.kind IN ('AVAILABLE', 'HOLD')
   AND j.status = 'POSTED'
   AND ($3::timestamptz IS NULL OR (e.created_at, e.id) > ($3::timestamptz, $4::uuid))
 ORDER BY e.created_at ASC, e.id ASC
 LIMIT $2`

// AccountStatements is the read adapter for a business-account statement.
// It does not Begin, lock, or call lock_account_balances.
type AccountStatements struct {
	q execQueryer
}

func NewAccountStatements(q execQueryer) *AccountStatements {
	return &AccountStatements{q: q}
}

var _ application.StatementReader = (*AccountStatements)(nil)

func (s *AccountStatements) List(ctx context.Context, accountID uuid.UUID, query application.StatementQuery) ([]application.StatementLine, error) {
	empty := make([]application.StatementLine, 0)
	if accountID == uuid.Nil {
		return nil, domain.ErrAccountNotFound
	}
	if query.Limit < 1 || query.Limit > application.MaxStatementLimit {
		return nil, domain.ErrInvalidAccount
	}
	if query.Cursor != nil && (query.Cursor.ID == uuid.Nil || query.Cursor.CreatedAt.IsZero()) {
		return nil, domain.ErrInvalidAccount
	}

	var (
		cursorAt any
		cursorID any
	)
	if query.Cursor != nil {
		cursorAt = query.Cursor.CreatedAt
		cursorID = query.Cursor.ID
	}

	rows, err := s.q.Query(ctx, selectAccountStatementSQL, accountID, query.Limit+1, cursorAt, cursorID)
	if err != nil {
		return nil, mapLedgerSQLError("list_statement", err)
	}
	defer rows.Close()

	out := empty
	for rows.Next() {
		var (
			line       application.StatementLine
			kind, side string
			currency   string
		)
		if err := rows.Scan(
			&line.ID, &line.JournalID, &line.LedgerAccountID, &kind,
			&side, &line.Amount, &currency,
			&line.AccountVersion, &line.BalanceAfter, &line.CreatedAt, &line.PostedAt,
		); err != nil {
			return nil, fmt.Errorf("accounts list_statement: scan: %w", err)
		}
		line.Kind = ledger.AccountKind(kind)
		line.Side = ledger.EntryType(side)
		line.Currency = strings.TrimSpace(currency)
		out = append(out, line)
	}
	if err := rows.Err(); err != nil {
		return nil, mapLedgerSQLError("list_statement", err)
	}
	return out, nil
}
