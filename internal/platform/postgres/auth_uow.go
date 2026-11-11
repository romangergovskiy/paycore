package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/romangergovskiy/paycore/internal/auth/application"
)

type authUoW struct {
	db beginner
}

// NewAuthUoW owns Begin/Commit/Rollback for Identity/Auth writes.
// Each Do starts one pgx.Tx and builds every auth store on that Tx.
// Commit vs rollback is an explicit match on application.Decision, not
// err == nil and not error text or codes. An unknown Decision rolls back
// with errUnknownAuthDecision. Accounts finishTx must not be reused.
//
// Isolation is READ COMMITTED at BEGIN (pgx BeginTx options), not SET
// TRANSACTION after the first statement, not Open() RuntimeParams, and
// not the paycore_app role default. Lock* stabilization needs a new
// snapshot per statement so COUNT sees a Refresh insert that the
// previous FOR UPDATE missed.
func NewAuthUoW(db beginner) application.UnitOfWork {
	return authUoW{db: db}
}

var _ application.UnitOfWork = authUoW{}

// authTxOptions is BEGIN ISOLATION LEVEL read committed. Empty Begin()
// would inherit the session (REPEATABLE READ if the client set it).
var authTxOptions = pgx.TxOptions{IsoLevel: pgx.ReadCommitted}

type authTxBeginner interface {
	BeginTx(ctx context.Context, txOptions pgx.TxOptions) (pgx.Tx, error)
}

func beginAuthTx(ctx context.Context, db beginner) (pgx.Tx, error) {
	b, ok := db.(authTxBeginner)
	if !ok {
		return nil, errors.New("auth tx begin: BeginTx is required")
	}
	tx, err := b.BeginTx(ctx, authTxOptions)
	if err != nil {
		return nil, fmt.Errorf("auth tx begin: %w", err)
	}
	return tx, nil
}

func (u authUoW) Do(ctx context.Context, fn func(context.Context, application.Deps) (application.Decision, error)) error {
	if fn == nil {
		return errors.New("auth: unit of work callback is required")
	}
	tx, err := beginAuthTx(ctx, u.db)
	if err != nil {
		return err
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

// errUnknownAuthDecision is returned when finishAuthTx sees a Decision that
// is neither Commit nor Rollback. That is an adapter defect, not a business
// rejection: the transaction rolls back. finishAuthTx still does not inspect
// runErr text or codes to choose commit vs rollback.
var errUnknownAuthDecision = errors.New("auth: unknown unit of work decision")

func finishAuthTx(ctx context.Context, tx txFinisher, dec application.Decision, runErr error) error {
	if err := ctx.Err(); err != nil {
		cause := err
		if runErr != nil && !errors.Is(runErr, err) {
			cause = errors.Join(err, runErr)
		}
		return rollbackAuthTx(ctx, tx, cause)
	}
	switch dec {
	case application.Commit:
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("auth tx commit: %w", err)
		}
		return runErr
	case application.Rollback:
		return rollbackAuthTx(ctx, tx, runErr)
	default:
		cause := errUnknownAuthDecision
		if runErr != nil {
			cause = errors.Join(errUnknownAuthDecision, runErr)
		}
		return rollbackAuthTx(ctx, tx, cause)
	}
}

func rollbackAuthTx(ctx context.Context, tx txFinisher, cause error) error {
	if rbErr := rollbackCleanup(ctx, tx); rbErr != nil {
		return errors.Join(cause, fmt.Errorf("auth tx rollback: %w", rbErr))
	}
	return cause
}
