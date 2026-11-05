package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestRunTxSuccessCommits(t *testing.T) {
	tx := &fakeFinisher{}
	if err := runTx(context.Background(), tx, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if tx.commits != 1 || tx.rollbacks != 0 {
		t.Fatalf("commit=%d rollback=%d", tx.commits, tx.rollbacks)
	}
}

func TestRunTxCallbackErrorRollsBack(t *testing.T) {
	tx := &fakeFinisher{}
	want := errors.New("step failed")
	err := runTx(context.Background(), tx, func() error { return want })
	if !errors.Is(err, want) {
		t.Fatalf("got %v", err)
	}
	if tx.commits != 0 || tx.rollbacks != 1 {
		t.Fatalf("commit=%d rollback=%d", tx.commits, tx.rollbacks)
	}
	assertCleanupCtx(t, tx)
}

func TestRunTxPanicRollsBackAndPropagates(t *testing.T) {
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
	_ = runTx(context.Background(), tx, func() error {
		panic("boom")
	})
	t.Fatal("expected panic")
}

func TestRunTxPanicOnCanceledParentStillRollsBack(t *testing.T) {
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
	_ = runTx(ctx, tx, func() error {
		panic("boom")
	})
	t.Fatal("expected panic")
}

func TestFinishTxSuccessCommits(t *testing.T) {
	tx := &fakeFinisher{}
	if err := finishTx(context.Background(), tx, nil); err != nil {
		t.Fatal(err)
	}
	if tx.commits != 1 || tx.rollbacks != 0 {
		t.Fatalf("commit=%d rollback=%d", tx.commits, tx.rollbacks)
	}
}

func TestFinishTxCallbackErrorRollsBack(t *testing.T) {
	tx := &fakeFinisher{}
	want := errors.New("step failed")
	err := finishTx(context.Background(), tx, want)
	if !errors.Is(err, want) {
		t.Fatalf("got %v", err)
	}
	if tx.commits != 0 || tx.rollbacks != 1 {
		t.Fatalf("commit=%d rollback=%d", tx.commits, tx.rollbacks)
	}
}

func TestFinishTxKeepsCallbackErrorIfRollbackFails(t *testing.T) {
	rb := errors.New("rollback failed")
	tx := &fakeFinisher{rollbackErr: rb}
	want := errors.New("step failed")
	err := finishTx(context.Background(), tx, want)
	if !errors.Is(err, want) || !errors.Is(err, rb) {
		t.Fatalf("got %v", err)
	}
	if tx.commits != 0 {
		t.Fatal("committed")
	}
}

func TestFinishTxCanceledAfterSuccessRollsBack(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tx := &fakeFinisher{}
	err := finishTx(ctx, tx, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	if tx.commits != 0 || tx.rollbacks != 1 {
		t.Fatalf("commit=%d rollback=%d", tx.commits, tx.rollbacks)
	}
	assertCleanupCtx(t, tx)
}

func TestFinishTxCommitError(t *testing.T) {
	want := errors.New("commit failed")
	tx := &fakeFinisher{commitErr: want}
	err := finishTx(context.Background(), tx, nil)
	if !errors.Is(err, want) {
		t.Fatalf("got %v", err)
	}
	if tx.rollbacks != 0 {
		t.Fatal("rollback after commit attempt is finishTx's caller's concern")
	}
}

func TestRollbackCleanupIgnoresClosedTx(t *testing.T) {
	tx := &fakeFinisher{rollbackErr: pgx.ErrTxClosed}
	if err := rollbackCleanup(context.Background(), tx); err != nil {
		t.Fatal(err)
	}
	if err := rollbackCleanup(context.Background(), tx); err != nil {
		t.Fatal(err)
	}
	if tx.rollbacks != 2 || tx.commits != 0 {
		t.Fatalf("commit=%d rollback=%d", tx.commits, tx.rollbacks)
	}
}

func assertCleanupCtx(t *testing.T, tx *fakeFinisher) {
	t.Helper()
	if !tx.rollbackLive {
		t.Fatal("rollback used a dead context")
	}
	if !tx.rollbackHasDeadline {
		t.Fatal("rollback context has no timeout")
	}
	if d, ok := tx.rollbackDeadline.Deadline(); !ok || d.Before(time.Now()) {
		t.Fatal("rollback deadline already expired")
	}
}

type fakeFinisher struct {
	commits, rollbacks  int
	commitErr           error
	rollbackErr         error
	rollbackLive        bool
	rollbackHasDeadline bool
	rollbackDeadline    context.Context
}

func (f *fakeFinisher) Commit(context.Context) error {
	f.commits++
	return f.commitErr
}

func (f *fakeFinisher) Rollback(ctx context.Context) error {
	f.rollbacks++
	f.rollbackLive = ctx.Err() == nil
	_, f.rollbackHasDeadline = ctx.Deadline()
	f.rollbackDeadline = ctx
	return f.rollbackErr
}
