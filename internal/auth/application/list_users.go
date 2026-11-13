package application

import (
	"context"

	"github.com/google/uuid"

	"github.com/romangergovskiy/paycore/internal/auth/domain"
)

// ListUsers is GET /admin/users. There is no UnitOfWork and no row lock.
// Users.List is one keyset query; roles are one RoleStore.ListByIDs call
// for the returned page IDs (not List per user). JWT.claims are not used.
// The HTTP layer has already required ADMIN.
//
// The page is not a snapshot. READ COMMITTED keyset pagination can see
// rows committed after a previous page: a user inserted with (created_at, id)
// greater than the cursor may appear on a later page; one at or before the
// cursor will not. This method does not freeze the user set.
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
	ids := make([]uuid.UUID, len(page.Users))
	for i := range page.Users {
		ids[i] = page.Users[i].ID
	}
	byUser, err := s.Roles.ListByIDs(ctx, ids)
	if err != nil {
		return zero, err
	}
	for i := range page.Users {
		roles := byUser[page.Users[i].ID]
		if roles == nil {
			roles = []domain.Role{}
		}
		page.Users[i].Roles = append([]domain.Role{}, roles...)
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
