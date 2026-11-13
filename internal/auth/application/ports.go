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
	// Bulk session revoke (LogoutAll, ChangePassword, LockUser, DisableUser,
	// login lockout) takes it in the same Tx before tokens so Login cannot
	// insert a session after the token set is frozen. Order: user → tokens
	// → sessions. Signature unchanged.
	GetForUpdate(ctx context.Context, id uuid.UUID) (domain.User, error)
	Update(ctx context.Context, u domain.User) error
	// List is the admin keyset. It does not Begin. The SELECT must not
	// include password_hash. Callers pass Limit 1…100; the adapter
	// returns at most Limit+1 rows ordered (created_at, id) ASC so the
	// use case can set Next. Cursor values are bound as $created_at
	// and $id, never as a decoded SQL string.
	List(ctx context.Context, q ListUsersQuery) ([]domain.User, error)
}

// SessionStore persists auth.sessions. The implementation must not Begin.
type SessionStore interface {
	Create(ctx context.Context, s domain.Session) error
	Get(ctx context.Context, id uuid.UUID) (domain.Session, error)
	Revoke(ctx context.Context, id uuid.UUID, reason domain.RevokeReason, at time.Time) error
	RevokeAllForUser(ctx context.Context, userID uuid.UUID, reason domain.RevokeReason, at time.Time) error
	Touch(ctx context.Context, id uuid.UUID, at time.Time) error
	// ListByUser returns the user's sessions. liveOnly=true is the MVP
	// default (revoked_at IS NULL), matching the partial index.
	ListByUser(ctx context.Context, userID uuid.UUID, liveOnly bool) ([]domain.Session, error)
}

// TokenStore persists hashed refresh tokens. The implementation must not Begin
// and must not store plaintext.
type TokenStore interface {
	Create(ctx context.Context, tok domain.RefreshToken) error
	GetByHash(ctx context.Context, hash []byte) (domain.RefreshToken, error)
	// GetForUpdate is SELECT … FOR UPDATE on token_hash, held until the
	// transaction ends. Refresh re-reads with this after LockBySession so
	// two concurrent callers cannot both observe an unused token. Unit
	// tests serialize a fake transaction; they are not a PostgreSQL
	// isolation proof.
	GetForUpdate(ctx context.Context, hash []byte) (domain.RefreshToken, error)
	MarkUsed(ctx context.Context, id, replacedBy uuid.UUID, at time.Time) error
	// LockBySession is the application lock point for the session's
	// refresh tokens, taken before updating the session row. The
	// PostgreSQL adapter must SELECT … FOR UPDATE ORDER BY id and
	// repeat until every matching row is locked, including rows a
	// concurrent Refresh inserted after this Tx's first snapshot.
	// One SELECT is not enough under READ COMMITTED.
	LockBySession(ctx context.Context, sessionID uuid.UUID) error
	// LockAllForUser is the application lock point for the user's
	// refresh tokens, taken before updating session rows. Same
	// SELECT … FOR UPDATE ORDER BY id until the locked set is complete.
	LockAllForUser(ctx context.Context, userID uuid.UUID) error
	RevokeBySession(ctx context.Context, sessionID uuid.UUID, at time.Time) error
	RevokeAllForUser(ctx context.Context, userID uuid.UUID, at time.Time) error
}

// RoleStore persists auth.user_roles.
type RoleStore interface {
	Grant(ctx context.Context, ur domain.UserRole) error
	Revoke(ctx context.Context, userID uuid.UUID, role domain.Role) error
	List(ctx context.Context, userID uuid.UUID) ([]domain.Role, error)
	// ListByIDs returns roles for many users in one operation. Missing
	// users and users with no roles are absent from the map. Role slices
	// are ordered by role code. An empty or nil ID list must not query
	// and returns an empty map.
	ListByIDs(ctx context.Context, userIDs []uuid.UUID) (map[uuid.UUID][]domain.Role, error)
	Has(ctx context.Context, userID uuid.UUID, role domain.Role) (bool, error)
	CountActiveWithRole(ctx context.Context, role domain.Role) (int, error)
	// LockAdminRoleMutations serializes every use-case write that can
	// change the ACTIVE ADMIN set in this transaction (pg_advisory_xact_lock,
	// not a session lock): Grant/Revoke of ADMIN, Lock/Unlock/Disable/EnableUser,
	// login lockout of a user who has (or just gained) ADMIN, and
	// BootstrapFirstAdmin. Count and the initiator re-check run after the
	// lock is held. Order is always this lock, then count or user FOR UPDATE.
	LockAdminRoleMutations(ctx context.Context) error
}

// AttemptStore appends auth.login_attempts.
type AttemptStore interface {
	Append(ctx context.Context, a domain.LoginAttempt) error
}

// StatusHistoryStore appends auth.user_status_history. Implementations
// must INSERT only; UPDATE/DELETE are forbidden by the schema.
type StatusHistoryStore interface {
	Append(ctx context.Context, h domain.UserStatusHistory) error
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
