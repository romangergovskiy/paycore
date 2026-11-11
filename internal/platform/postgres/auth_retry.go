package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/romangergovskiy/paycore/internal/auth/application"
)

// LogoutRefreshRetry wraps uow so LogoutIn and RefreshIn retry once when
// the first Do callback returns SQLSTATE 40P01 or 40001 and that Tx rolled
// back successfully. It does not change NewAuthUoW.Do. It is not used for
// Login, LogoutAll, ChangePassword, admin writes, or unknown Commit.
func LogoutRefreshRetry(uow application.UnitOfWork) application.UnitOfWork {
	if uow == nil {
		return nil
	}
	return serializationRetryUoW{inner: uow}
}

type serializationRetryUoW struct {
	inner application.UnitOfWork
}

func (u serializationRetryUoW) Do(ctx context.Context, fn func(context.Context, application.Deps) (application.Decision, error)) error {
	var fromCallback error
	var dec application.Decision
	err := u.inner.Do(ctx, func(ctx context.Context, d application.Deps) (application.Decision, error) {
		var e error
		dec, e = fn(ctx, d)
		fromCallback = e
		return dec, e
	})
	if !retryLogoutRefresh(dec, fromCallback, err) {
		return err
	}
	return u.inner.Do(ctx, fn)
}

func retryLogoutRefresh(dec application.Decision, fromCallback, result error) bool {
	if result == nil || fromCallback == nil {
		return false
	}
	if dec != application.Rollback {
		return false
	}
	if !isSerializationFailure(fromCallback) {
		return false
	}
	if isJoined(result) {
		return false
	}
	return true
}

func isSerializationFailure(err error) bool {
	var pe *pgconn.PgError
	if !errors.As(err, &pe) {
		return false
	}
	return pe.Code == "40P01" || pe.Code == "40001"
}

func isJoined(err error) bool {
	type multi interface{ Unwrap() []error }
	u, ok := err.(multi)
	return ok && len(u.Unwrap()) > 1
}
