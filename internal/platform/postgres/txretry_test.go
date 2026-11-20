package postgres

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/romangergovskiy/paycore/internal/platform/idempotency"
)

func TestSQLStateAndRetryable(t *testing.T) {
	cases := []struct {
		err  error
		code string
		ok   bool
	}{
		{&pgconn.PgError{Code: "40001"}, "40001", true},
		{&pgconn.PgError{Code: "40P01"}, "40P01", true},
		{&pgconn.PgError{Code: "55P03"}, "55P03", true},
		{fmt.Errorf("wrap: %w", &pgconn.PgError{Code: "40001"}), "40001", true},
		{&pgconn.PgError{Code: "23505"}, "23505", false},
		{&pgconn.PgError{Code: "57014"}, "57014", false},
		{errors.New("nope"), "", false},
	}
	for _, tc := range cases {
		if got := sqlState(tc.err); got != tc.code {
			t.Errorf("sqlState(%v) = %q want %q", tc.err, got, tc.code)
		}
		if got := retryable(tc.err); got != tc.ok {
			t.Errorf("retryable(%v) = %v want %v", tc.err, got, tc.ok)
		}
	}
}

func TestRetryTxNilCallback(t *testing.T) {
	err := RetryTx(context.Background(), nil, nil)
	if err == nil {
		t.Fatal("nil callback")
	}
}

func TestWrapRetryInProgress(t *testing.T) {
	cause := &pgconn.PgError{Code: "55P03", Message: "lock timeout"}
	err := wrapRetry(idempotency.ErrInProgress, cause)
	if !errors.Is(err, idempotency.ErrInProgress) {
		t.Fatalf("Is InProgress: %v", err)
	}
	var pe *pgconn.PgError
	if !errors.As(err, &pe) || pe.Code != "55P03" {
		t.Fatalf("lost PgError: %v", err)
	}
}
