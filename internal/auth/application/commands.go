package application

import (
	"time"

	"github.com/google/uuid"

	"github.com/romangergovskiy/paycore/internal/auth/domain"
)

// RegisterCommand is POST /auth/register. Roles must be empty (D-42).
type RegisterCommand struct {
	Email     string
	Password  string
	FirstName string
	LastName  string
	IP        string
	Roles     []domain.Role
}

// LoginCommand is POST /auth/login.
type LoginCommand struct {
	Email     string
	Password  string
	IP        string
	UserAgent string
}

// RefreshCommand is POST /auth/refresh. RefreshToken is the opaque plaintext.
type RefreshCommand struct {
	RefreshToken string
	IP           string
}

// Credentials is the login/refresh result. RefreshToken is shown once.
// AccessToken is an opaque string from AccessSigner; this package does not parse JWT.
type Credentials struct {
	SessionID    uuid.UUID
	AccessToken  string
	RefreshToken string
	TokenType    string
	ExpiresIn    int
	User         domain.PublicUser
}

const tokenTypeBearer = "Bearer"

// StatusCommand is LockUser / UnlockUser / DisableUser / EnableUser.
type StatusCommand struct {
	Actor  domain.Principal
	UserID uuid.UUID
	Reason string
}

// ChangePasswordCommand is POST /auth/password. It does not issue tokens.
type ChangePasswordCommand struct {
	Actor       domain.Principal
	OldPassword string
	NewPassword string
}

// RevokeSessionCommand is POST /auth/sessions/{id}/revoke.
type RevokeSessionCommand struct {
	Actor     domain.Principal
	SessionID uuid.UUID
}

const (
	DefaultListUsersLimit = 50
	MinListUsersLimit     = 1
	MaxListUsersLimit     = 100
)

// UserCursor is a keyset position (created_at, id). It is not a SQL
// fragment. HTTP encodes it as opaque Base64; this package never sees
// that string.
type UserCursor struct {
	CreatedAt time.Time
	ID        uuid.UUID
}

// ListUsersQuery pages GET /admin/users. A nil Status is every status.
// Limit is 1…100; HTTP applies the default 50 before calling.
type ListUsersQuery struct {
	Status *domain.Status
	Limit  int
	Cursor *UserCursor
}

// UserPage is one admin-list page. Next is set from the last returned
// user when another row exists; it is nil on the last page.
type UserPage struct {
	Users []domain.PublicUser
	Next  *UserCursor
}
