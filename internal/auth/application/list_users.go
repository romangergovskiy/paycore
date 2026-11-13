package application

import (
	"context"

	"github.com/google/uuid"

	"github.com/romangergovskiy/paycore/internal/auth/domain"
)

// ListUsers is GET /admin/users. There is no UnitOfWork and no row lock.
// Roles come from RoleStore for the page IDs, not JWT.claims. The HTTP
// layer has already required ADMIN; this method does not re-check the
// actor so it cannot invent a Principal from a query string.
func (s Service) ListUsers(ctx context.Context, q ListUsersQuery) (UserPage, error) {
	var zero UserPage
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	if s.Users == nil || s.Roles == nil {
		return zero, errNotWired
	}
	if err := validateListUsersQuery(q); err != nil {
		return zero, err
	}
	rows, err := s.Users.List(ctx, q)
	if err != nil {
		return zero, err
	}
	page := userPage(rows, q.Limit)
	for i := range page.Users {
		roles, err := s.Roles.List(ctx, page.Users[i].ID)
		if err != nil {
			return zero, err
		}
		if roles == nil {
			roles = []domain.Role{}
		}
		page.Users[i].Roles = append([]domain.Role(nil), roles...)
	}
	return page, nil
}

func validateListUsersQuery(q ListUsersQuery) error {
	if q.Limit < MinListUsersLimit || q.Limit > MaxListUsersLimit {
		return domain.ErrInvalidRequest
	}
	if q.Status != nil && !knownUserStatus(*q.Status) {
		return domain.ErrInvalidRequest
	}
	if q.Cursor != nil && (q.Cursor.ID == uuid.Nil || q.Cursor.CreatedAt.IsZero()) {
		return domain.ErrInvalidRequest
	}
	return nil
}

func knownUserStatus(s domain.Status) bool {
	switch s {
	case domain.StatusActive, domain.StatusLocked, domain.StatusDisabled:
		return true
	}
	return false
}

func userPage(rows []domain.User, limit int) UserPage {
	if rows == nil {
		rows = []domain.User{}
	}
	if len(rows) <= limit {
		return UserPage{Users: publicUsers(rows)}
	}
	kept := rows[:limit]
	last := kept[len(kept)-1]
	return UserPage{
		Users: publicUsers(kept),
		Next:  &UserCursor{CreatedAt: last.CreatedAt, ID: last.ID},
	}
}

func publicUsers(rows []domain.User) []domain.PublicUser {
	out := make([]domain.PublicUser, 0, len(rows))
	for _, u := range rows {
		pub := u.Public()
		pub.Roles = []domain.Role{}
		out = append(out, pub)
	}
	return out
}
