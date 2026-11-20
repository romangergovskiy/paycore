package postgres

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/romangergovskiy/paycore/internal/platform/idempotency"
)

const (
	retryMaxAttempts      = 5
	retryLockTimeoutExtra = 2 // additional attempts after the first 55P03
	retryBaseDelay        = 10 * time.Millisecond
)

// RetryTx runs fn in one PostgreSQL transaction and retries the whole
// closure on SQLSTATE 40001, 40P01, and 55P03 (concurrency.md §6).
// Other errors are not retried. Nested transactions are not used: db must
// be a pool or connection, not a Tx. fn must not perform irreversible
// effects outside PostgreSQL (those go through the outbox after COMMIT).
//
// After lock_timeout retries are exhausted, RetryTx returns
// idempotency.ErrInProgress (L1 / 409 REQUEST_IN_PROGRESS).
func RetryTx(ctx context.Context, db beginner, fn func(context.Context, pgx.Tx) error) error {
	if fn == nil {
		return errors.New("retry: callback is required")
	}
	var last error
	lockTries := 0
	for attempt := 0; attempt < retryMaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		last = doTx(ctx, db, fn)
		if last == nil {
			return nil
		}
		code := sqlState(last)
		switch code {
		case "40001", "40P01":
			// retry whole Tx; 40P01 also wants a metric/alert (caller logs)
		case "55P03":
			if lockTries >= retryLockTimeoutExtra {
				return wrapRetry(idempotency.ErrInProgress, last)
			}
			lockTries++
		default:
			return last
		}
		if err := sleepBackoff(ctx, attempt); err != nil {
			return err
		}
	}
	if sqlState(last) == "55P03" {
		return wrapRetry(idempotency.ErrInProgress, last)
	}
	return last
}

func doTx(ctx context.Context, db beginner, fn func(context.Context, pgx.Tx) error) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("tx begin: %w", err)
	}
	return runTx(ctx, tx, func() error {
		return fn(ctx, tx)
	})
}

func sqlState(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

func retryable(err error) bool {
	switch sqlState(err) {
	case "40001", "40P01", "55P03":
		return true
	default:
		return false
	}
}

func sleepBackoff(ctx context.Context, attempt int) error {
	base := retryBaseDelay << attempt // 10, 20, 40, 80, 160 ms
	if base > 40*time.Millisecond {
		base = 40 * time.Millisecond
	}
	// jitter in [base/2, base]
	delay := base/2 + time.Duration(rand.Int64N(int64(base/2)+1))
	t := time.NewTimer(delay)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func wrapRetry(sentinel, cause error) error {
	return fmt.Errorf("%w: %w", sentinel, cause)
}
