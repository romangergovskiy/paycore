package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/romangergovskiy/paycore/internal/auth/application"
	"github.com/romangergovskiy/paycore/internal/auth/domain"
)

// UoW finish cases 1–12: Decision is the commit signal. These use fakeFinisher
// and do not need Docker.

func TestAuthUoW1CommitNilCommits(t *testing.T) {
	tx := &fakeFinisher{}
	if err := runAuthTx(context.Background(), tx, func() (application.Decision, error) {
		return application.Commit, nil
	}); err != nil {
		t.Fatal(err)
	}
	if tx.commits != 1 || tx.rollbacks != 0 {
		t.Fatalf("commit=%d rollback=%d", tx.commits, tx.rollbacks)
	}
}

func TestAuthUoW2CommitDomainErrorCommits(t *testing.T) {
	tx := &fakeFinisher{}
	err := runAuthTx(context.Background(), tx, func() (application.Decision, error) {
		return application.Commit, domain.ErrInvalidCredentials
	})
	if !errors.Is(err, domain.ErrInvalidCredentials) {
		t.Fatalf("got %v", err)
	}
	if tx.commits != 1 || tx.rollbacks != 0 {
		t.Fatalf("commit=%d rollback=%d (must not use err==nil)", tx.commits, tx.rollbacks)
	}
}

func TestAuthUoW3RollbackErrorRollsBack(t *testing.T) {
	tx := &fakeFinisher{}
	want := errors.New("store failed")
	err := runAuthTx(context.Background(), tx, func() (application.Decision, error) {
		return application.Rollback, want
	})
	if !errors.Is(err, want) {
		t.Fatalf("got %v", err)
	}
	if tx.commits != 0 || tx.rollbacks != 1 {
		t.Fatalf("commit=%d rollback=%d", tx.commits, tx.rollbacks)
	}
	assertCleanupCtx(t, tx)
}

func TestAuthUoW4RollbackNilRollsBack(t *testing.T) {
	tx := &fakeFinisher{}
	if err := runAuthTx(context.Background(), tx, func() (application.Decision, error) {
		return application.Rollback, nil
	}); err != nil {
		t.Fatal(err)
	}
	if tx.commits != 0 || tx.rollbacks != 1 {
		t.Fatalf("commit=%d rollback=%d", tx.commits, tx.rollbacks)
	}
}

func TestAuthUoW5CanceledAfterCommitDoesNotCommit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tx := &fakeFinisher{}
	err := finishAuthTx(ctx, tx, application.Commit, domain.ErrEmailTaken)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	if !errors.Is(err, domain.ErrEmailTaken) {
		t.Fatalf("lost business error: %v", err)
	}
	if tx.commits != 0 || tx.rollbacks != 1 {
		t.Fatalf("commit=%d rollback=%d", tx.commits, tx.rollbacks)
	}
	assertCleanupCtx(t, tx)
}

func TestAuthUoW6PanicRollsBackAndPropagates(t *testing.T) {
	tx := &fakeFinisher{}
	defer func() {
		rec := recover()
		if rec != "boom" {
			t.Fatalf("recovered %#v", rec)
		}
		if tx.commits != 0 || tx.rollbacks != 1 {
			t.Fatalf("commit=%d rollback=%d", tx.commits, tx.rollbacks)
		}
		assertCleanupCtx(t, tx)
	}()
	_ = runAuthTx(context.Background(), tx, func() (application.Decision, error) {
		panic("boom")
	})
	t.Fatal("expected panic")
}

func TestAuthUoW7PanicOnCanceledParentStillRollsBack(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tx := &fakeFinisher{}
	defer func() {
		rec := recover()
		if rec != "boom" {
			t.Fatalf("recovered %#v", rec)
		}
		if tx.commits != 0 || tx.rollbacks != 1 {
			t.Fatalf("commit=%d rollback=%d", tx.commits, tx.rollbacks)
		}
		assertCleanupCtx(t, tx)
	}()
	_ = runAuthTx(ctx, tx, func() (application.Decision, error) {
		panic("boom")
	})
	t.Fatal("expected panic")
}

func TestAuthUoW8RollbackJoinsCleanupError(t *testing.T) {
	rb := errors.New("rollback failed")
	tx := &fakeFinisher{rollbackErr: rb}
	want := errors.New("hasher failed")
	err := finishAuthTx(context.Background(), tx, application.Rollback, want)
	if !errors.Is(err, want) || !errors.Is(err, rb) {
		t.Fatalf("got %v", err)
	}
	if tx.commits != 0 {
		t.Fatal("committed")
	}
}

func TestAuthUoW9CommitError(t *testing.T) {
	want := errors.New("commit failed")
	tx := &fakeFinisher{commitErr: want}
	err := finishAuthTx(context.Background(), tx, application.Commit, domain.ErrRateLimited)
	if !errors.Is(err, want) {
		t.Fatalf("got %v", err)
	}
	if tx.rollbacks != 0 {
		t.Fatal("rollback after commit attempt")
	}
}

func TestAuthUoW10ClosedTxRollbackIgnored(t *testing.T) {
	tx := &fakeFinisher{rollbackErr: pgx.ErrTxClosed}
	err := finishAuthTx(context.Background(), tx, application.Rollback, domain.ErrForbidden)
	if !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("got %v", err)
	}
	if tx.commits != 0 || tx.rollbacks != 1 {
		t.Fatalf("commit=%d rollback=%d", tx.commits, tx.rollbacks)
	}
}

func TestAuthUoW11NilCallback(t *testing.T) {
	uow := NewAuthUoW(panicBeginner{})
	err := uow.Do(context.Background(), nil)
	if err == nil || err.Error() != "auth: unit of work callback is required" {
		t.Fatalf("got %v", err)
	}
}

func TestAuthUoW12DecisionNotErrNil(t *testing.T) {
	t.Run("commit with error", func(t *testing.T) {
		tx := &fakeFinisher{}
		err := finishAuthTx(context.Background(), tx, application.Commit, domain.ErrTokenReused)
		if !errors.Is(err, domain.ErrTokenReused) {
			t.Fatalf("got %v", err)
		}
		if tx.commits != 1 || tx.rollbacks != 0 {
			t.Fatalf("commit=%d rollback=%d", tx.commits, tx.rollbacks)
		}
	})
	t.Run("rollback with nil error", func(t *testing.T) {
		tx := &fakeFinisher{}
		if err := finishAuthTx(context.Background(), tx, application.Rollback, nil); err != nil {
			t.Fatal(err)
		}
		if tx.commits != 0 || tx.rollbacks != 1 {
			t.Fatalf("commit=%d rollback=%d", tx.commits, tx.rollbacks)
		}
	})
	t.Run("does not inspect error text", func(t *testing.T) {
		tx := &fakeFinisher{}
		looksInfra := errors.New("auth users create: SQLSTATE 23505")
		err := finishAuthTx(context.Background(), tx, application.Commit, looksInfra)
		if !errors.Is(err, looksInfra) {
			t.Fatalf("got %v", err)
		}
		if tx.commits != 1 || tx.rollbacks != 0 {
			t.Fatalf("parsed error text: commit=%d rollback=%d", tx.commits, tx.rollbacks)
		}
	})
}

func TestAuthUoWUnknownDecisionNilRollsBack(t *testing.T) {
	tx := &fakeFinisher{}
	err := finishAuthTx(context.Background(), tx, application.Decision(99), nil)
	if !errors.Is(err, errUnknownAuthDecision) {
		t.Fatalf("got %v", err)
	}
	if tx.commits != 0 || tx.rollbacks != 1 {
		t.Fatalf("commit=%d rollback=%d", tx.commits, tx.rollbacks)
	}
}

func TestAuthUoWUnknownDecisionJoinsBusiness(t *testing.T) {
	tx := &fakeFinisher{}
	err := finishAuthTx(context.Background(), tx, application.Decision(2), domain.ErrInvalidCredentials)
	if !errors.Is(err, errUnknownAuthDecision) || !errors.Is(err, domain.ErrInvalidCredentials) {
		t.Fatalf("got %v", err)
	}
	if tx.commits != 0 || tx.rollbacks != 1 {
		t.Fatalf("commit=%d rollback=%d", tx.commits, tx.rollbacks)
	}
}

func TestAuthUoWKnownDecisionsUnchanged(t *testing.T) {
	t.Run("commit with business error still commits", func(t *testing.T) {
		tx := &fakeFinisher{}
		err := finishAuthTx(context.Background(), tx, application.Commit, domain.ErrEmailTaken)
		if !errors.Is(err, domain.ErrEmailTaken) {
			t.Fatalf("got %v", err)
		}
		if errors.Is(err, errUnknownAuthDecision) {
			t.Fatal("sentinel on a known Decision")
		}
		if tx.commits != 1 || tx.rollbacks != 0 {
			t.Fatalf("commit=%d rollback=%d", tx.commits, tx.rollbacks)
		}
	})
	t.Run("rollback with nil still rolls back", func(t *testing.T) {
		tx := &fakeFinisher{}
		if err := finishAuthTx(context.Background(), tx, application.Rollback, nil); err != nil {
			t.Fatal(err)
		}
		if tx.commits != 0 || tx.rollbacks != 1 {
			t.Fatalf("commit=%d rollback=%d", tx.commits, tx.rollbacks)
		}
	})
}

type panicBeginner struct{}

func (panicBeginner) Begin(context.Context) (pgx.Tx, error) {
	panic("Begin must not run for a nil callback")
}

func (panicBeginner) BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) {
	panic("BeginTx must not run for a nil callback")
}

func TestAuthUoWBeginCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	uow := NewAuthUoW(errBeginner{err: context.Canceled})
	err := uow.Do(ctx, func(context.Context, application.Deps) (application.Decision, error) {
		t.Fatal("callback ran")
		return application.Commit, nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

type errBeginner struct{ err error }

func (b errBeginner) Begin(context.Context) (pgx.Tx, error) {
	return nil, b.err
}

func (b errBeginner) BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) {
	return nil, b.err
}

type beginOnly struct{}

func (beginOnly) Begin(context.Context) (pgx.Tx, error) {
	return nil, errors.New("Begin must not be used")
}

func TestAuthUoWRequiresBeginTx(t *testing.T) {
	uow := NewAuthUoW(beginOnly{})
	err := uow.Do(context.Background(), func(context.Context, application.Deps) (application.Decision, error) {
		t.Fatal("callback ran")
		return application.Commit, nil
	})
	if err == nil || err.Error() != "auth tx begin: BeginTx is required" {
		t.Fatalf("got %v", err)
	}
}

func TestAuthUoWCleanupDeadline(t *testing.T) {
	if d := txCleanupTimeout; d != 5*time.Second {
		t.Fatalf("cleanup timeout %s", d)
	}
}
