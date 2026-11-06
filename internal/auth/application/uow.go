package application

import (
	"context"

	"github.com/google/uuid"

	"github.com/romangergovskiy/paycore/internal/auth/domain"
)

// Deps are the transactional ports of one auth write. They share one
// database transaction; Hasher, AccessSigner, RefreshIssuer, and Clock do not.
type Deps struct {
	Users    UserStore
	Sessions SessionStore
	Tokens   TokenStore
	Roles    RoleStore
	Attempts AttemptStore
	Limiter  RateLimiter
}

// UnitOfWork runs fn in one transaction. Begin/Commit/Rollback belong to
// infrastructure, matching Accounts.
type UnitOfWork interface {
	Do(ctx context.Context, fn func(ctx context.Context, d Deps) error) error
}

func (s Service) withDeps(d Deps) Service {
	out := s
	out.Users = d.Users
	out.Sessions = d.Sessions
	out.Tokens = d.Tokens
	out.Roles = d.Roles
	out.Attempts = d.Attempts
	out.Limiter = d.Limiter
	return out
}

func RegisterIn(ctx context.Context, uow UnitOfWork, s Service, cmd RegisterCommand) (domain.PublicUser, error) {
	var out domain.PublicUser
	if uow == nil {
		return out, errNotWired
	}
	err := uow.Do(ctx, func(ctx context.Context, d Deps) error {
		var err error
		out, err = s.withDeps(d).Register(ctx, cmd)
		return err
	})
	return out, err
}

func LoginIn(ctx context.Context, uow UnitOfWork, s Service, cmd LoginCommand) (Credentials, error) {
	var out Credentials
	if uow == nil {
		return out, errNotWired
	}
	err := uow.Do(ctx, func(ctx context.Context, d Deps) error {
		var err error
		out, err = s.withDeps(d).Login(ctx, cmd)
		return err
	})
	return out, err
}

func RefreshIn(ctx context.Context, uow UnitOfWork, s Service, cmd RefreshCommand) (Credentials, error) {
	var out Credentials
	if uow == nil {
		return out, errNotWired
	}
	err := uow.Do(ctx, func(ctx context.Context, d Deps) error {
		var err error
		out, err = s.withDeps(d).Refresh(ctx, cmd)
		return err
	})
	return out, err
}

func LogoutIn(ctx context.Context, uow UnitOfWork, s Service, p domain.Principal) error {
	if uow == nil {
		return errNotWired
	}
	return uow.Do(ctx, func(ctx context.Context, d Deps) error {
		return s.withDeps(d).Logout(ctx, p)
	})
}

func GrantRoleIn(ctx context.Context, uow UnitOfWork, s Service, actor domain.Principal, userID uuid.UUID, role domain.Role) error {
	if uow == nil {
		return errNotWired
	}
	return uow.Do(ctx, func(ctx context.Context, d Deps) error {
		return s.withDeps(d).GrantRole(ctx, actor, userID, role)
	})
}

func RevokeRoleIn(ctx context.Context, uow UnitOfWork, s Service, actor domain.Principal, userID uuid.UUID, role domain.Role) error {
	if uow == nil {
		return errNotWired
	}
	return uow.Do(ctx, func(ctx context.Context, d Deps) error {
		return s.withDeps(d).RevokeRole(ctx, actor, userID, role)
	})
}
