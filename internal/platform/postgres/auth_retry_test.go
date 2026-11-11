package postgres

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/romangergovskiy/paycore/internal/auth/application"
	"github.com/romangergovskiy/paycore/internal/auth/domain"
)

func pgErr(code string) error {
	return &pgconn.PgError{Code: code, Message: "postgres " + code}
}

type scriptedUoW struct {
	n      int
	script []scriptedDo
}

type scriptedDo struct {
	after func(application.Decision, error) error
}

func (u *scriptedUoW) Do(ctx context.Context, fn func(context.Context, application.Deps) (application.Decision, error)) error {
	if u.n >= len(u.script) {
		return errors.New("unexpected extra Do")
	}
	step := u.script[u.n]
	u.n++
	dec, err := fn(ctx, application.Deps{})
	if step.after != nil {
		return step.after(dec, err)
	}
	return err
}

func TestLogoutRefreshRetryCallbackDeadlockRetriesOnce(t *testing.T) {
	dead := pgErr("40P01")
	uow := &scriptedUoW{script: []scriptedDo{
		{after: func(dec application.Decision, err error) error {
			if dec != application.Rollback || !errors.Is(err, dead) {
				t.Fatalf("first callback: dec=%v err=%v", dec, err)
			}
			return err
		}},
		{after: func(dec application.Decision, err error) error {
			if dec != application.Commit || err != nil {
				t.Fatalf("retry callback: dec=%v err=%v", dec, err)
			}
			return err
		}},
	}}
	calls := 0
	err := LogoutRefreshRetry(uow).Do(context.Background(), func(context.Context, application.Deps) (application.Decision, error) {
		calls++
		if calls == 1 {
			return application.Rollback, dead
		}
		return application.Commit, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || uow.n != 2 {
		t.Fatalf("calls=%d dos=%d", calls, uow.n)
	}
}

func TestLogoutRefreshRetrySerializationFailureRetriesOnce(t *testing.T) {
	ser := pgErr("40001")
	uow := &scriptedUoW{script: []scriptedDo{{}, {}}}
	calls := 0
	err := LogoutRefreshRetry(uow).Do(context.Background(), func(context.Context, application.Deps) (application.Decision, error) {
		calls++
		if calls == 1 {
			return application.Rollback, ser
		}
		return application.Commit, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("calls=%d", calls)
	}
}

func TestLogoutRefreshRetrySecondDeadlockIsNotRetried(t *testing.T) {
	dead := pgErr("40P01")
	uow := &scriptedUoW{script: []scriptedDo{{}, {}}}
	calls := 0
	err := LogoutRefreshRetry(uow).Do(context.Background(), func(context.Context, application.Deps) (application.Decision, error) {
		calls++
		return application.Rollback, dead
	})
	if !errors.As(err, new(*pgconn.PgError)) {
		t.Fatalf("got %v", err)
	}
	if calls != 2 || uow.n != 2 {
		t.Fatalf("calls=%d dos=%d, want 2 (no third Do)", calls, uow.n)
	}
}

func TestLogoutRefreshRetryRollbackFailureDoesNotRetry(t *testing.T) {
	dead := pgErr("40P01")
	rb := errors.New("auth tx rollback: boom")
	uow := &scriptedUoW{script: []scriptedDo{
		{after: func(_ application.Decision, err error) error {
			return errors.Join(err, rb)
		}},
	}}
	calls := 0
	err := LogoutRefreshRetry(uow).Do(context.Background(), func(context.Context, application.Deps) (application.Decision, error) {
		calls++
		return application.Rollback, dead
	})
	if !errors.Is(err, dead) || !errors.Is(err, rb) {
		t.Fatalf("got %v", err)
	}
	if calls != 1 || uow.n != 1 {
		t.Fatalf("calls=%d dos=%d, want no retry", calls, uow.n)
	}
}

func TestLogoutRefreshRetryCommitErrorDoesNotRetry(t *testing.T) {
	dead := pgErr("40P01")
	uow := &scriptedUoW{script: []scriptedDo{
		{after: func(dec application.Decision, err error) error {
			if dec != application.Commit {
				t.Fatalf("dec=%v", dec)
			}
			return fmt.Errorf("auth tx commit: %w", dead)
		}},
	}}
	calls := 0
	err := LogoutRefreshRetry(uow).Do(context.Background(), func(context.Context, application.Deps) (application.Decision, error) {
		calls++
		return application.Commit, nil
	})
	if !errors.As(err, new(*pgconn.PgError)) {
		t.Fatalf("got %v", err)
	}
	if calls != 1 || uow.n != 1 {
		t.Fatalf("calls=%d dos=%d", calls, uow.n)
	}
}

func TestLogoutRefreshRetryUnknownDecisionDoesNotRetry(t *testing.T) {
	dead := pgErr("40P01")
	uow := &scriptedUoW{script: []scriptedDo{
		{after: func(dec application.Decision, err error) error {
			return errors.Join(errors.New("auth: unknown unit of work decision"), err)
		}},
	}}
	calls := 0
	err := LogoutRefreshRetry(uow).Do(context.Background(), func(context.Context, application.Deps) (application.Decision, error) {
		calls++
		return application.Decision(99), dead
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if calls != 1 || uow.n != 1 {
		t.Fatalf("calls=%d dos=%d", calls, uow.n)
	}
}

func TestLogoutRefreshRetryIgnoresOtherSQLSTATE(t *testing.T) {
	for _, code := range []string{"55P03", "57014", "23505"} {
		uow := &scriptedUoW{script: []scriptedDo{{}}}
		calls := 0
		err := LogoutRefreshRetry(uow).Do(context.Background(), func(context.Context, application.Deps) (application.Decision, error) {
			calls++
			return application.Rollback, pgErr(code)
		})
		if !errors.As(err, new(*pgconn.PgError)) {
			t.Fatalf("%s: %v", code, err)
		}
		if calls != 1 || uow.n != 1 {
			t.Fatalf("%s retried: calls=%d dos=%d", code, calls, uow.n)
		}
	}
}

func TestLogoutRefreshRetryDomainErrorDoesNotRetry(t *testing.T) {
	uow := &scriptedUoW{script: []scriptedDo{{}}}
	calls := 0
	err := LogoutRefreshRetry(uow).Do(context.Background(), func(context.Context, application.Deps) (application.Decision, error) {
		calls++
		return application.Rollback, domain.ErrInvalidToken
	})
	if !errors.Is(err, domain.ErrInvalidToken) {
		t.Fatalf("got %v", err)
	}
	if calls != 1 {
		t.Fatalf("calls=%d", calls)
	}
}

func TestLogoutRefreshRetryNilUoW(t *testing.T) {
	if LogoutRefreshRetry(nil) != nil {
		t.Fatal("nil uow must stay nil")
	}
}

func TestNewAuthUoWTypeIsNotRetryWrapper(t *testing.T) {
	uow := NewAuthUoW(nil)
	if _, ok := uow.(serializationRetryUoW); ok {
		t.Fatal("NewAuthUoW.Do must not grow retry")
	}
	if _, ok := LogoutRefreshRetry(uow).(serializationRetryUoW); !ok {
		t.Fatal("LogoutRefreshRetry must wrap without changing NewAuthUoW")
	}
}
