package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/romangergovskiy/paycore/internal/accounts/application"
)

// beginner is *pgxpool.Pool or *pgx.Conn. Do not pass a pgx.Tx: Begin on a
// Tx is a savepoint, which would nest transactions.
type beginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

type accountUoW struct {
	db beginner
}

// NewAccountUoW owns Begin/Commit/Rollback for Accounts writes.
// Each Do starts one Tx and builds Accounts Store + Ledger opener on it.
func NewAccountUoW(db beginner) application.UnitOfWork {
	return accountUoW{db: db}
}

var _ application.UnitOfWork = accountUoW{}

func (u accountUoW) Do(ctx context.Context, fn func(context.Context, application.Deps) error) error {
	if fn == nil {
		return errors.New("accounts: unit of work callback is required")
	}
	tx, err := u.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("accounts tx begin: %w", err)
	}
	runErr := fn(ctx, application.Deps{
		Store:  NewAccounts(tx),
		Opener: NewLedger(tx),
	})
	return finishTx(ctx, tx, runErr)
}

type txFinisher interface {
	Commit(ctx context.Context) error
	Rollback(ctx context.Context) error
}

func finishTx(ctx context.Context, tx txFinisher, runErr error) error {
	if runErr != nil {
		return rollbackTx(ctx, tx, runErr)
	}
	if err := ctx.Err(); err != nil {
		return rollbackTx(ctx, tx, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("accounts tx commit: %w", err)
	}
	return nil
}

func rollbackTx(ctx context.Context, tx txFinisher, cause error) error {
	// Do not Rollback with a canceled/expired ctx: pgx can then mark the conn closed.
	if rbErr := tx.Rollback(context.WithoutCancel(ctx)); rbErr != nil && !txDone(rbErr) {
		return errors.Join(cause, fmt.Errorf("accounts tx rollback: %w", rbErr))
	}
	return cause
}

func txDone(err error) bool {
	return errors.Is(err, pgx.ErrTxClosed)
}
