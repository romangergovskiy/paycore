package application

import (
	"context"
	"errors"

	"github.com/romangergovskiy/paycore/internal/auth/domain"
)

func (s Service) bootstrapWired() error {
	if s.Users == nil || s.Roles == nil || s.Hasher == nil || s.Clock == nil {
		return errNotWired
	}
	return nil
}

// BootstrapFirstAdmin creates the first ACTIVE ADMIN (CUSTOMER+ADMIN) in
// one Auth UoW transaction. It is not GrantRole: there is no actor
// Principal, so granted_by stays NULL. It does not open a session or
// issue tokens. An existing ACTIVE ADMIN is a Commit no-op even when
// email differs. Occupied email with no ACTIVE ADMIN is EMAIL_TAKEN and
// Rollback of the whole transaction.
func (s Service) BootstrapFirstAdmin(ctx context.Context, email, password string) (domain.PublicUser, Decision, error) {
	var zero domain.PublicUser
	if err := ctx.Err(); err != nil {
		return zero, Rollback, err
	}
	if err := s.bootstrapWired(); err != nil {
		return zero, Rollback, err
	}
	if err := s.Roles.LockAdminRoleMutations(ctx); err != nil {
		return zero, Rollback, err
	}
	n, err := s.Roles.CountActiveWithRole(ctx, domain.RoleAdmin)
	if err != nil {
		return zero, Rollback, err
	}
	if n >= 1 {
		return zero, Commit, nil
	}

	canon, err := domain.CanonicalEmail(email)
	if err != nil {
		return zero, Rollback, err
	}
	if err := domain.ValidatePassword(password); err != nil {
		return zero, Rollback, err
	}

	hash, err := s.Hasher.Hash(password)
	if err != nil {
		return zero, Rollback, err
	}
	id, err := newID()
	if err != nil {
		return zero, Rollback, err
	}
	now := s.now()
	user := domain.User{
		ID:           id,
		Email:        canon,
		PasswordHash: hash,
		Status:       domain.StatusActive,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := s.Users.Create(ctx, user); err != nil {
		if errors.Is(err, domain.ErrEmailTaken) {
			return zero, Rollback, err
		}
		return zero, Rollback, err
	}
	if err := s.Roles.Grant(ctx, domain.UserRole{
		UserID:    id,
		Role:      domain.RoleCustomer,
		GrantedAt: now,
	}); err != nil {
		return zero, Rollback, err
	}
	if err := s.Roles.Grant(ctx, domain.UserRole{
		UserID:    id,
		Role:      domain.RoleAdmin,
		GrantedAt: now,
	}); err != nil {
		return zero, Rollback, err
	}
	pub := user.Public()
	pub.Roles = []domain.Role{domain.RoleCustomer, domain.RoleAdmin}
	return pub, Commit, nil
}
