package postgres

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/romangergovskiy/paycore/internal/platform/apperror"
	"github.com/romangergovskiy/paycore/internal/platform/idempotency"
)

const (
	retryMaxAttempts      = 5
	retryLockTimeoutExtra = 2 // additional attempts after the first 55P03
	retryBaseDelay        = 10 * time.Millisecond
)

// errIdempotencyLock marks 55P03 that happened while capturing the
// idempotency key (L1). Generic RetryTx must not treat every lock_timeout
// as REQUEST_IN_PROGRESS (concurrency.md §6).
var errIdempotencyLock = errors.New("idempotency key lock timeout")

const lockTimeoutRetryAfter = time.Second

// RetryTx runs fn in one PostgreSQL transaction and retries the whole
// closure on SQLSTATE 40001, 40P01, and 55P03 (concurrency.md §6).
// Other errors are not retried. Nested transactions are not used: db must
// be a pool or connection, not a Tx. fn must not perform irreversible
// effects outside PostgreSQL (those go through the outbox after COMMIT).
//
// After lock_timeout retries are exhausted:
//   - L1 (idempotency key capture) → idempotency.ErrInProgress (409)
//   - any other lock → 503 SERVICE_UNAVAILABLE with Retry-After 1s
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
				return mapLockTimeout(last)
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
		return mapLockTimeout(last)
	}
	return last
}

func mapLockTimeout(err error) error {
	if errors.Is(err, errIdempotencyLock) {
		return wrapRetry(idempotency.ErrInProgress, err)
	}
	return &retryLaterError{
		after: lockTimeoutRetryAfter,
		err:   apperror.Wrap(err, apperror.KindUnavailable, apperror.CodeUnavailable, "Service busy. Retry later."),
	}
}

// retryLaterError is exhausted non-L1 lock_timeout. Transport maps it to
// 503 and Retry-After (concurrency.md §6).
type retryLaterError struct {
	after time.Duration
	err   error
}

func (e *retryLaterError) Error() string { return e.err.Error() }
func (e *retryLaterError) Unwrap() error { return e.err }
func (e *retryLaterError) RetryAfter() time.Duration {
	return e.after
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
