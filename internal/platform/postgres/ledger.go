package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/romangergovskiy/paycore/internal/ledger"
	"github.com/romangergovskiy/paycore/internal/platform/apperror"
)

// queryer is satisfied by *pgxpool.Pool, pgx.Tx, and *pgx.Conn.
// Post does not Begin: the caller owns the transaction.
type queryer interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Ledger is the typed caller of ledger.post_journal. It performs no financial DML.
type Ledger struct {
	q queryer
}

func NewLedger(q queryer) *Ledger {
	return &Ledger{q: q}
}

var _ ledger.Poster = (*Ledger)(nil)

const postJournalSQL = `
SELECT posted_journal_id, already_posted, posted_entry_id, posted_ledger_account_id,
       posted_entry_type, posted_amount, posted_currency::text,
       posted_account_version, posted_balance_after
  FROM ledger.post_journal(
	$1::uuid, $2, $3, $4::uuid, $5::smallint, $6::uuid, $7::uuid,
	$8, $9::uuid, $10::date, $11::jsonb, $12::jsonb)`

type entryJSON struct {
	ID              uuid.UUID `json:"id"`
	LedgerAccountID uuid.UUID `json:"ledger_account_id"`
	EntryType       string    `json:"entry_type"`
	Amount          int64     `json:"amount"`
	Currency        string    `json:"currency"`
}

type holdJSON struct {
	Action        string     `json:"action"`
	HoldID        uuid.UUID  `json:"hold_id"`
	AccountID     uuid.UUID  `json:"account_id"`
	Amount        int64      `json:"amount"`
	Currency      string     `json:"currency"`
	ExpiresAt     *string    `json:"expires_at,omitempty"`
	ReferenceType string     `json:"reference_type,omitempty"`
	ReferenceID   *uuid.UUID `json:"reference_id,omitempty"`
}

func (l *Ledger) Post(ctx context.Context, cmd ledger.Command) (ledger.Result, error) {
	var zero ledger.Result
	entries, err := json.Marshal(toEntryJSON(cmd.Entries))
	if err != nil {
		return zero, fmt.Errorf("ledger post_journal: encode entries: %w", err)
	}

	var hold any
	if cmd.Hold != nil {
		raw, err := json.Marshal(toHoldJSON(cmd.Hold))
		if err != nil {
			return zero, fmt.Errorf("ledger post_journal: encode hold_action: %w", err)
		}
		hold = string(raw)
	}

	seq := cmd.OperationSeq
	if seq == 0 {
		seq = 1
	}
	var description any
	if cmd.Description != "" {
		description = cmd.Description
	}

	rows, err := l.q.Query(ctx, postJournalSQL,
		cmd.JournalID,
		string(cmd.Operation),
		cmd.ReferenceType,
		cmd.ReferenceID,
		seq,
		cmd.ReversesJournalID,
		cmd.AdjustmentID,
		description,
		cmd.InitiatedBy,
		cmd.AccountingDate,
		string(entries),
		hold,
	)
	if err != nil {
		return zero, mapPostJournalError(err)
	}
	defer rows.Close()

	var out ledger.Result
	for rows.Next() {
		var pe ledger.PostedEntry
		var entryType string
		if err := rows.Scan(
			&out.JournalID,
			&out.AlreadyPosted,
			&pe.ID,
			&pe.LedgerAccountID,
			&entryType,
			&pe.Amount,
			&pe.Currency,
			&pe.AccountVersion,
			&pe.BalanceAfter,
		); err != nil {
			return zero, fmt.Errorf("ledger post_journal: scan: %w", err)
		}
		pe.Type = ledger.EntryType(entryType)
		out.Entries = append(out.Entries, pe)
	}
	if err := rows.Err(); err != nil {
		return zero, mapPostJournalError(err)
	}
	return out, nil
}

func toEntryJSON(in []ledger.Entry) []entryJSON {
	out := make([]entryJSON, len(in))
	for i, e := range in {
		out[i] = entryJSON{
			ID: e.ID, LedgerAccountID: e.LedgerAccountID,
			EntryType: string(e.Type), Amount: e.Amount, Currency: e.Currency,
		}
	}
	return out
}

func toHoldJSON(h *ledger.HoldAction) holdJSON {
	j := holdJSON{
		Action:        string(h.Action),
		HoldID:        h.HoldID,
		AccountID:     h.AccountID,
		Amount:        h.Amount,
		Currency:      h.Currency,
		ReferenceType: h.ReferenceType,
		ReferenceID:   h.ReferenceID,
	}
	if h.ExpiresAt != nil {
		s := h.ExpiresAt.UTC().Format("2006-01-02T15:04:05.999999999Z07:00")
		j.ExpiresAt = &s
	}
	return j
}

func mapPostJournalError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("ledger post_journal: %w", err)
	}
	var pe *pgconn.PgError
	if !errors.As(err, &pe) {
		return fmt.Errorf("ledger post_journal: %w", err)
	}
	switch pe.Code {
	case "PC400":
		return apperror.Wrap(err, apperror.KindInvalid, ledger.ErrInvalidJournal.Code, pe.Message)
	case "PC402":
		if pe.Message == "SYSTEM_ACCOUNT_INSUFFICIENT" {
			return apperror.Wrap(err, apperror.KindUnprocessable, ledger.ErrSystemAccountInsufficient.Code, pe.Message)
		}
		return apperror.Wrap(err, apperror.KindUnprocessable, ledger.ErrInsufficientFunds.Code, pe.Message)
	case "PC403":
		return apperror.Wrap(err, apperror.KindUnprocessable, ledger.ErrAccountNotActive.Code, pe.Message)
	case "PC409":
		return apperror.Wrap(err, apperror.KindConflict, ledger.ErrConflict.Code, pe.Message)
	case "PC460":
		return apperror.Wrap(err, apperror.KindInternal, ledger.ErrLockProtocol.Code, pe.Message)
	default:
		// connect, serialization (40001), deadlock (40P01), lock_timeout, …
		return fmt.Errorf("ledger post_journal: %w", err)
	}
}
