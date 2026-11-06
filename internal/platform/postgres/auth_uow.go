package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/romangergovskiy/paycore/internal/auth/application"
)

type authUoW struct {
	db beginner
}

// NewAuthUoW owns Begin/Commit/Rollback for Identity/Auth writes.
// Each Do starts one pgx.Tx and builds every auth store on that Tx.
// Commit vs rollback comes from application.Decision, not from err == nil
// and not from error text or codes. Accounts finishTx must not be reused.
func NewAuthUoW(db beginner) application.UnitOfWork {
	return authUoW{db: db}
}

var _ application.UnitOfWork = authUoW{}

func (u authUoW) Do(ctx context.Context, fn func(context.Context, application.Deps) (application.Decision, error)) error {
	if fn == nil {
		return errors.New("auth: unit of work callback is required")
	}
	tx, err := u.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("auth tx begin: %w", err)
	}
	return runAuthTx(ctx, tx, func() (application.Decision, error) {
		return fn(ctx, authDeps(tx))
	})
}

func runAuthTx(ctx context.Context, tx txFinisher, fn func() (application.Decision, error)) (err error) {
	defer func() {
		if rec := recover(); rec != nil {
			func() {
				defer func() { _ = recover() }()
				_ = rollbackCleanup(ctx, tx)
			}()
			panic(rec)
		}
	}()
	dec, runErr := fn()
	return finishAuthTx(ctx, tx, dec, runErr)
}

func finishAuthTx(ctx context.Context, tx txFinisher, dec application.Decision, runErr error) error {
	if err := ctx.Err(); err != nil {
		cause := err
		if runErr != nil && !errors.Is(runErr, err) {
			cause = errors.Join(err, runErr)
		}
		return rollbackAuthTx(ctx, tx, cause)
	}
	if dec != application.Commit {
		return rollbackAuthTx(ctx, tx, runErr)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("auth tx commit: %w", err)
	}
	return runErr
}

func rollbackAuthTx(ctx context.Context, tx txFinisher, cause error) error {
	if rbErr := rollbackCleanup(ctx, tx); rbErr != nil {
		return errors.Join(cause, fmt.Errorf("auth tx rollback: %w", rbErr))
	}
	return cause
}
