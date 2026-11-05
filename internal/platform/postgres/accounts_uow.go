package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

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

const txCleanupTimeout = 5 * time.Second

func (u accountUoW) Do(ctx context.Context, fn func(context.Context, application.Deps) error) error {
	if fn == nil {
		return errors.New("accounts: unit of work callback is required")
	}
	tx, err := u.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("accounts tx begin: %w", err)
	}
	return runTx(ctx, tx, func() error {
		return fn(ctx, application.Deps{
			Store:  NewAccounts(tx),
			Opener: NewLedger(tx),
		})
	})
}

type txFinisher interface {
	Commit(ctx context.Context) error
	Rollback(ctx context.Context) error
}

func runTx(ctx context.Context, tx txFinisher, fn func() error) (err error) {
	defer func() {
		if rec := recover(); rec != nil {
			func() {
				defer func() { _ = recover() }()
				_ = rollbackCleanup(ctx, tx)
			}()
			panic(rec)
		}
	}()
	return finishTx(ctx, tx, fn())
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
	if rbErr := rollbackCleanup(ctx, tx); rbErr != nil {
		return errors.Join(cause, fmt.Errorf("accounts tx rollback: %w", rbErr))
	}
	return cause
}

func rollbackCleanup(ctx context.Context, tx txFinisher) error {
	cctx, cancel := cleanupCtx(ctx)
	defer cancel()
	if rbErr := tx.Rollback(cctx); rbErr != nil && !txDone(rbErr) {
		return rbErr
	}
	return nil
}

func cleanupCtx(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), txCleanupTimeout)
}

func txDone(err error) bool {
	return errors.Is(err, pgx.ErrTxClosed)
}
