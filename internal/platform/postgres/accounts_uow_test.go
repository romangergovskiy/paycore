package postgres

import (
	"context"
	"errors"
	"testing"
)

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

type fakeFinisher struct {
	commits, rollbacks int
	commitErr          error
	rollbackErr        error
}

func (f *fakeFinisher) Commit(context.Context) error {
	f.commits++
	return f.commitErr
}

func (f *fakeFinisher) Rollback(context.Context) error {
	f.rollbacks++
	return f.rollbackErr
}
