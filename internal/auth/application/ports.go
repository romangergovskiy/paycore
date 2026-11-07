package application

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/romangergovskiy/paycore/internal/auth/domain"
)

// UserStore persists auth.users. The implementation must not Begin.
type UserStore interface {
	Create(ctx context.Context, u domain.User) error
	Get(ctx context.Context, id uuid.UUID) (domain.User, error)
	GetByEmail(ctx context.Context, email string) (domain.User, error)
	// GetForUpdate locks the user row until the transaction ends (FOR UPDATE).
	// Login re-reads with this after password verify so a concurrent lockout
	// is not overwritten by Update of a stale GetByEmail copy.
	GetForUpdate(ctx context.Context, id uuid.UUID) (domain.User, error)
	Update(ctx context.Context, u domain.User) error
}

// SessionStore persists auth.sessions. The implementation must not Begin.
type SessionStore interface {
	Create(ctx context.Context, s domain.Session) error
	Get(ctx context.Context, id uuid.UUID) (domain.Session, error)
	Revoke(ctx context.Context, id uuid.UUID, reason domain.RevokeReason, at time.Time) error
	RevokeAllForUser(ctx context.Context, userID uuid.UUID, reason domain.RevokeReason, at time.Time) error
	Touch(ctx context.Context, id uuid.UUID, at time.Time) error
}

// TokenStore persists hashed refresh tokens. The implementation must not Begin
// and must not store plaintext.
type TokenStore interface {
	Create(ctx context.Context, tok domain.RefreshToken) error
	GetByHash(ctx context.Context, hash []byte) (domain.RefreshToken, error)
	// GetForUpdate is SELECT … FOR UPDATE on token_hash, held until the
	// transaction ends. Two concurrent Refresh calls must not both see an
	// unused token. Unit tests serialize a fake transaction; they are not
	// a PostgreSQL isolation proof.
	GetForUpdate(ctx context.Context, hash []byte) (domain.RefreshToken, error)
	MarkUsed(ctx context.Context, id, replacedBy uuid.UUID, at time.Time) error
	RevokeBySession(ctx context.Context, sessionID uuid.UUID, at time.Time) error
	RevokeAllForUser(ctx context.Context, userID uuid.UUID, at time.Time) error
}

// RoleStore persists auth.user_roles.
type RoleStore interface {
	Grant(ctx context.Context, ur domain.UserRole) error
	Revoke(ctx context.Context, userID uuid.UUID, role domain.Role) error
	List(ctx context.Context, userID uuid.UUID) ([]domain.Role, error)
	Has(ctx context.Context, userID uuid.UUID, role domain.Role) (bool, error)
	CountActiveWithRole(ctx context.Context, role domain.Role) (int, error)
	// LockAdminRoleMutations serializes Grant/Revoke of ADMIN in this
	// transaction (pg_advisory_xact_lock, not a session lock). Count and
	// the initiator re-check run after the lock is held.
	LockAdminRoleMutations(ctx context.Context) error
}

// AttemptStore appends auth.login_attempts.
type AttemptStore interface {
	Append(ctx context.Context, a domain.LoginAttempt) error
}

// RateLimiter increments a PostgreSQL window counter (not Redis).
// Hit is a security-critical write: the use case returns Commit after a
// business rejection so the adapter does not roll the counter back.
type RateLimiter interface {
	Hit(ctx context.Context, bucket domain.RateBucket, key string, at time.Time) (int, error)
}

// Hasher is Argon2id (D-41). Domain never sees the algorithm parameters.
type Hasher interface {
	Hash(password string) (string, error)
	Verify(password, encoded string) (bool, error)
	DummyHash() string
}

// RefreshIssuer creates opaque refresh tokens. Hash is SHA-256 of the raw
// bytes; plaintext is shown once. Application does not hash.
type RefreshIssuer interface {
	Issue() (plaintext string, hash []byte, err error)
	Hash(plaintext string) ([]byte, error)
}

// AccessSigner issues the JWT access token (HS256, 15 min). Parsing the
// compact JWT is HTTP middleware, not this package.
type AccessSigner interface {
	Sign(p domain.Principal, now, exp time.Time) (token string, err error)
}

// Clock is UTC time for TTLs and windows (D-29).
type Clock interface {
	Now() time.Time
}
