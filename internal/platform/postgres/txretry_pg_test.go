package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/romangergovskiy/paycore/internal/platform/apperror"
	"github.com/romangergovskiy/paycore/internal/platform/idempotency"
)

func TestRetryTxSQLSTATEPostgres(t *testing.T) {
	ctx, c := startMigrated(t)
	owner := mustConnect(t, ctx, c.owner)
	defer owner.Close(ctx)
	app := mustConnect(t, ctx, c.app)
	defer app.Close(ctx)
	createDemoTable(t, ctx, owner)

	t.Run("40001 retries then succeeds", func(t *testing.T) {
		n := 0
		err := RetryTx(ctx, app, func(context.Context, pgx.Tx) error {
			n++
			if n < 3 {
				return &pgconn.PgError{Code: "40001", Message: "could not serialize access"}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if n != 3 {
			t.Fatalf("attempts = %d", n)
		}
	})

	t.Run("40P01 retries then succeeds", func(t *testing.T) {
		n := 0
		err := RetryTx(ctx, app, func(context.Context, pgx.Tx) error {
			n++
			if n < 2 {
				return &pgconn.PgError{Code: "40P01", Message: "deadlock detected"}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if n != 2 {
			t.Fatalf("attempts = %d", n)
		}
	})

	t.Run("exhausted 40001 returns original cause", func(t *testing.T) {
		cause := &pgconn.PgError{Code: "40001", Message: "could not serialize access"}
		n := 0
		err := RetryTx(ctx, app, func(context.Context, pgx.Tx) error {
			n++
			return cause
		})
		if err == nil {
			t.Fatal("expected error")
		}
		if errors.Is(err, idempotency.ErrInProgress) {
			t.Fatal("40001 mapped to REQUEST_IN_PROGRESS")
		}
		var pe *pgconn.PgError
		if !errors.As(err, &pe) || pe.Code != "40001" || pe != cause {
			t.Fatalf("want original 40001, got %v", err)
		}
		if n != retryMaxAttempts {
			t.Fatalf("attempts = %d want %d", n, retryMaxAttempts)
		}
	})

	t.Run("arbitrary error is not retried", func(t *testing.T) {
		n := 0
		want := errors.New("boom")
		err := RetryTx(ctx, app, func(context.Context, pgx.Tx) error {
			n++
			return want
		})
		if !errors.Is(err, want) {
			t.Fatalf("got %v", err)
		}
		if n != 1 {
			t.Fatalf("retried arbitrary error: attempts = %d", n)
		}
	})

	t.Run("injected 55P03 is not REQUEST_IN_PROGRESS", func(t *testing.T) {
		err := RetryTx(ctx, app, func(context.Context, pgx.Tx) error {
			return &pgconn.PgError{Code: "55P03", Message: "canceling statement due to lock timeout"}
		})
		if errors.Is(err, idempotency.ErrInProgress) {
			t.Fatal("generic 55P03 mapped to REQUEST_IN_PROGRESS")
		}
		var later *retryLaterError
		if !errors.As(err, &later) {
			t.Fatalf("want 503 retry-later, got %v", err)
		}
		var ae *apperror.Error
		if !errors.As(err, &ae) || ae.Kind != apperror.KindUnavailable {
			t.Fatalf("want SERVICE_UNAVAILABLE, got %v", err)
		}
		var pe *pgconn.PgError
		if !errors.As(err, &pe) || pe.Code != "55P03" {
			t.Fatalf("lost original 55P03: %v", err)
		}
	})

	t.Run("55P03 on idempotency key is REQUEST_IN_PROGRESS", func(t *testing.T) {
		req := demoRequest(t, idempotency.UserScope(mustNewID(t)), "POST /api/v1/demo", "l1-lock", `{"n":1}`)
		holding := make(chan struct{})
		release := make(chan struct{})
		errA := make(chan error, 1)
		go func() {
			conn := mustConnect(t, ctx, c.app)
			defer conn.Close(ctx)
			tx, err := conn.Begin(ctx)
			if err != nil {
				errA <- err
				close(holding)
				return
			}
			_, err = ExecuteIdempotent(ctx, tx, req, func(context.Context) (idempotency.Response, error) {
				close(holding)
				<-release
				return idempotency.Response{Status: 201, Body: json.RawMessage(`{"ok":true}`)}, nil
			})
			if err != nil {
				_ = tx.Rollback(ctx)
				errA <- err
				return
			}
			errA <- tx.Commit(ctx)
		}()
		<-holding
		waiter := mustConnect(t, ctx, c.app)
		defer waiter.Close(ctx)
		if _, err := waiter.Exec(ctx, `SET lock_timeout = '200ms'`); err != nil {
			close(release)
			t.Fatal(err)
		}
		err := RetryTx(ctx, waiter, func(ctx context.Context, tx pgx.Tx) error {
			_, err := ExecuteIdempotent(ctx, tx, req, func(context.Context) (idempotency.Response, error) {
				t.Error("waiter op must not run")
				return idempotency.Response{}, errors.New("must not run")
			})
			return err
		})
		close(release)
		if aerr := <-errA; aerr != nil {
			t.Fatalf("holder: %v", aerr)
		}
		if !errors.Is(err, idempotency.ErrInProgress) {
			t.Fatalf("want REQUEST_IN_PROGRESS, got %v", err)
		}
		var pe *pgconn.PgError
		if !errors.As(err, &pe) || pe.Code != "55P03" {
			t.Fatalf("lost 55P03 cause: %v", err)
		}
	})

	t.Run("55P03 on another lock is not REQUEST_IN_PROGRESS", func(t *testing.T) {
		scope := idempotency.UserScope(mustNewID(t))
		id := insertDemoCommitted(t, ctx, app, scope, "lock-row", "held")
		holder := mustConnect(t, ctx, c.app)
		defer holder.Close(ctx)
		htx, err := holder.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = htx.Rollback(ctx) }()
		if _, err := htx.Exec(ctx, `
			SELECT id FROM platform.stage_a_demo_resources WHERE id = $1 FOR UPDATE`, id); err != nil {
			t.Fatal(err)
		}

		waiter := mustConnect(t, ctx, c.app)
		defer waiter.Close(ctx)
		if _, err := waiter.Exec(ctx, `SET lock_timeout = '200ms'`); err != nil {
			t.Fatal(err)
		}
		err = RetryTx(ctx, waiter, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `
				SELECT id FROM platform.stage_a_demo_resources WHERE id = $1 FOR UPDATE`, id)
			return err
		})
		if errors.Is(err, idempotency.ErrInProgress) {
			t.Fatal("demo-row lock_timeout mapped to REQUEST_IN_PROGRESS")
		}
		var later *retryLaterError
		if !errors.As(err, &later) || later.RetryAfter() != lockTimeoutRetryAfter {
			t.Fatalf("want 503 Retry-After, got %v", err)
		}
		var pe *pgconn.PgError
		if !errors.As(err, &pe) || pe.Code != "55P03" {
			t.Fatalf("want 55P03 cause, got %v", err)
		}
	})

	t.Run("cancel during backoff", func(t *testing.T) {
		cctx, cancel := context.WithCancel(ctx)
		defer cancel()
		n := 0
		err := RetryTx(cctx, app, func(context.Context, pgx.Tx) error {
			n++
			if n == 1 {
				cancel()
				return &pgconn.PgError{Code: "40001", Message: "could not serialize access"}
			}
			t.Error("retried after cancel")
			return nil
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v", err)
		}
		if n != 1 {
			t.Fatalf("attempts after cancel = %d", n)
		}
	})
}

func insertDemoCommitted(t *testing.T, ctx context.Context, conn *pgx.Conn, scope, key, payload string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := RetryTx(ctx, conn, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		id, err = insertDemo(ctx, tx, scope, key, payload)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}
