package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/romangergovskiy/paycore/internal/auth/application"
	"github.com/romangergovskiy/paycore/internal/auth/domain"
	"github.com/romangergovskiy/paycore/internal/platform/apperror"
)

// Auth adapters persist Identity/Auth on the execQueryer they are given.
// They do not Begin, Commit, or Rollback. AuthUoW owns the transaction and
// constructs every store on that one pgx.Tx.

type AuthUsers struct{ q execQueryer }

func NewAuthUsers(q execQueryer) *AuthUsers { return &AuthUsers{q: q} }

var _ application.UserStore = (*AuthUsers)(nil)

type AuthSessions struct{ q execQueryer }

func NewAuthSessions(q execQueryer) *AuthSessions { return &AuthSessions{q: q} }

var _ application.SessionStore = (*AuthSessions)(nil)

type AuthTokens struct{ q execQueryer }

func NewAuthTokens(q execQueryer) *AuthTokens { return &AuthTokens{q: q} }

var _ application.TokenStore = (*AuthTokens)(nil)

type AuthRoles struct{ q execQueryer }

func NewAuthRoles(q execQueryer) *AuthRoles { return &AuthRoles{q: q} }

var _ application.RoleStore = (*AuthRoles)(nil)

type AuthAttempts struct{ q execQueryer }

func NewAuthAttempts(q execQueryer) *AuthAttempts { return &AuthAttempts{q: q} }

var _ application.AttemptStore = (*AuthAttempts)(nil)

type AuthLimiter struct{ q execQueryer }

func NewAuthLimiter(q execQueryer) *AuthLimiter { return &AuthLimiter{q: q} }

var _ application.RateLimiter = (*AuthLimiter)(nil)

type AuthStatusHistory struct{ q execQueryer }

func NewAuthStatusHistory(q execQueryer) *AuthStatusHistory { return &AuthStatusHistory{q: q} }

var _ application.StatusHistoryStore = (*AuthStatusHistory)(nil)

func authDeps(tx execQueryer) application.Deps {
	return application.Deps{
		Users:    NewAuthUsers(tx),
		Sessions: NewAuthSessions(tx),
		Tokens:   NewAuthTokens(tx),
		Roles:    NewAuthRoles(tx),
		Attempts: NewAuthAttempts(tx),
		Limiter:  NewAuthLimiter(tx),
		History:  NewAuthStatusHistory(tx),
	}
}

const (
	uqUsersEmail = "uq_users_email"
	pkUserRoles  = "user_roles_pkey"
)

const selectUserSQL = `
SELECT id, email, password_hash, COALESCE(first_name, ''), COALESCE(last_name, ''),
       status, failed_login_count, locked_at, created_at, updated_at
  FROM auth.users
 WHERE id = $1`

const selectUserByEmailSQL = `
SELECT id, email, password_hash, COALESCE(first_name, ''), COALESCE(last_name, ''),
       status, failed_login_count, locked_at, created_at, updated_at
  FROM auth.users
 WHERE email = $1`

const selectUserForUpdateSQL = selectUserSQL + ` FOR UPDATE`

const insertUserSQL = `
INSERT INTO auth.users (
  id, email, password_hash, first_name, last_name, status,
  failed_login_count, locked_at, created_at, updated_at
) VALUES (
  $1, $2, $3, NULLIF($4, ''), NULLIF($5, ''), $6, $7, $8, $9, $10
)`

const updateUserSQL = `
UPDATE auth.users
   SET email = $2,
       password_hash = $3,
       first_name = NULLIF($4, ''),
       last_name = NULLIF($5, ''),
       status = $6,
       failed_login_count = $7,
       locked_at = $8,
       updated_at = $9
 WHERE id = $1`

const selectSessionSQL = `
SELECT id, user_id, COALESCE(user_agent, ''),
       CASE WHEN ip IS NULL THEN '' ELSE host(ip) END,
       created_at, last_seen_at, revoked_at, COALESCE(revoke_reason, '')
  FROM auth.sessions
 WHERE id = $1`

const insertSessionSQL = `
INSERT INTO auth.sessions (
  id, user_id, user_agent, ip, created_at, last_seen_at, revoked_at, revoke_reason
) VALUES (
  $1, $2, NULLIF($3, ''), $4::inet, $5, $6, $7, NULLIF($8, '')
)`

const revokeSessionSQL = `
UPDATE auth.sessions
   SET revoked_at = COALESCE(revoked_at, $2),
       revoke_reason = COALESCE(revoke_reason, NULLIF($3, ''))
 WHERE id = $1`

const revokeAllSessionsSQL = `
UPDATE auth.sessions
   SET revoked_at = $2,
       revoke_reason = $3
 WHERE user_id = $1 AND revoked_at IS NULL`

const touchSessionSQL = `
UPDATE auth.sessions
   SET last_seen_at = $2
 WHERE id = $1`

const selectTokenSQL = `
SELECT id, session_id, token_hash, replaced_by, expires_at, used_at, revoked_at, created_at
  FROM auth.refresh_tokens
 WHERE token_hash = $1`

const selectTokenForUpdateSQL = selectTokenSQL + ` FOR UPDATE`

const insertTokenSQL = `
INSERT INTO auth.refresh_tokens (
  id, session_id, token_hash, replaced_by, expires_at, used_at, revoked_at, created_at
) VALUES (
  $1, $2, $3, $4, $5, $6, $7, $8
)`

const markUsedSQL = `
UPDATE auth.refresh_tokens
   SET used_at = $2, replaced_by = $3
 WHERE id = $1`

const revokeTokensBySessionSQL = `
UPDATE auth.refresh_tokens
   SET revoked_at = $2
 WHERE session_id = $1 AND revoked_at IS NULL`

const revokeTokensForUserSQL = `
UPDATE auth.refresh_tokens AS t
   SET revoked_at = $2
  FROM auth.sessions AS s
 WHERE t.session_id = s.id
   AND s.user_id = $1
   AND t.revoked_at IS NULL`

const grantRoleSQL = `
INSERT INTO auth.user_roles (user_id, role_code, granted_by, granted_at)
VALUES ($1, $2, $3, $4)`

const revokeRoleSQL = `
DELETE FROM auth.user_roles
 WHERE user_id = $1 AND role_code = $2`

const listRolesSQL = `
SELECT role_code
  FROM auth.user_roles
 WHERE user_id = $1
 ORDER BY role_code`

const hasRoleSQL = `
SELECT EXISTS (
  SELECT 1 FROM auth.user_roles WHERE user_id = $1 AND role_code = $2
)`

const countActiveRoleSQL = `
SELECT COUNT(*)::int
  FROM auth.user_roles ur
  JOIN auth.users u ON u.id = ur.user_id
 WHERE ur.role_code = $1 AND u.status = 'ACTIVE'`

// adminRoleMutationLockKey is the single bigint passed to
// pg_advisory_xact_lock for GrantRole/RevokeRole of ADMIN.
// Transaction-scoped: released on Commit or Rollback of this Tx.
// Not pg_advisory_lock (session). Not a process mutex. Not FOR UPDATE
// on auth.roles (paycore_app has SELECT only). See identity-auth.md §10.
//
// 0x5041590000000001 = PayCore namespace 0x504159 ("PAY") + object 1
// (ADMIN role mutations). Do not reuse this value for other locks.
const adminRoleMutationLockKey int64 = 0x5041590000000001

const lockAdminRoleMutationsSQL = `SELECT pg_advisory_xact_lock($1)`

const insertAttemptSQL = `
INSERT INTO auth.login_attempts (
  id, email, user_id, ip, success, failure_reason, attempted_at
) VALUES (
  $1, $2, $3, $4::inet, $5, $6, $7
)`

const insertUserStatusHistorySQL = `
INSERT INTO auth.user_status_history (
  id, user_id, changed_by, previous_status, new_status, reason, created_at
) VALUES (
  $1, $2, $3, $4, $5, $6, $7
)`

const hitLimiterSQL = `
INSERT INTO auth.rate_limit_counters (bucket, counter_key, window_start, count, updated_at)
VALUES (
  $1, $2,
  date_bin('15 minutes', $3::timestamptz, TIMESTAMPTZ '1970-01-01 00:00:00+00'),
  1, $3::timestamptz
)
ON CONFLICT (bucket, counter_key, window_start)
DO UPDATE SET
  count = auth.rate_limit_counters.count + 1,
  updated_at = EXCLUDED.updated_at
RETURNING count`

func (s *AuthUsers) Create(ctx context.Context, u domain.User) error {
	_, err := s.q.Exec(ctx, insertUserSQL,
		u.ID, u.Email, u.PasswordHash, u.FirstName, u.LastName, string(u.Status),
		u.FailedLoginCount, u.LockedAt, u.CreatedAt, u.UpdatedAt,
	)
	return mapAuthSQLError("users create", err)
}

func (s *AuthUsers) Get(ctx context.Context, id uuid.UUID) (domain.User, error) {
	return scanUser(s.q.QueryRow(ctx, selectUserSQL, id), "users get")
}

func (s *AuthUsers) GetByEmail(ctx context.Context, email string) (domain.User, error) {
	return scanUser(s.q.QueryRow(ctx, selectUserByEmailSQL, email), "users get by email")
}

func (s *AuthUsers) GetForUpdate(ctx context.Context, id uuid.UUID) (domain.User, error) {
	return scanUser(s.q.QueryRow(ctx, selectUserForUpdateSQL, id), "users get for update")
}

func (s *AuthUsers) Update(ctx context.Context, u domain.User) error {
	tag, err := s.q.Exec(ctx, updateUserSQL,
		u.ID, u.Email, u.PasswordHash, u.FirstName, u.LastName, string(u.Status),
		u.FailedLoginCount, u.LockedAt, u.UpdatedAt,
	)
	if err != nil {
		return mapAuthSQLError("users update", err)
	}
	if tag.RowsAffected() == 0 {
		return wrapAuth(pgx.ErrNoRows, domain.ErrNotFound, apperror.KindNotFound)
	}
	return nil
}

// List satisfies UserStore. SQL keyset paging is stage 3.27.2.
func (s *AuthUsers) List(ctx context.Context, q application.ListUsersQuery) ([]domain.User, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("auth users list: not implemented")
}

func (s *AuthSessions) Create(ctx context.Context, sess domain.Session) error {
	_, err := s.q.Exec(ctx, insertSessionSQL,
		sess.ID, sess.UserID, sess.UserAgent, inetArg(sess.IP),
		sess.CreatedAt, sess.LastSeenAt, sess.RevokedAt, string(sess.RevokeReason),
	)
	return mapAuthSQLError("sessions create", err)
}

func (s *AuthSessions) Get(ctx context.Context, id uuid.UUID) (domain.Session, error) {
	return scanSession(s.q.QueryRow(ctx, selectSessionSQL, id), "sessions get")
}

func (s *AuthSessions) Revoke(ctx context.Context, id uuid.UUID, reason domain.RevokeReason, at time.Time) error {
	tag, err := s.q.Exec(ctx, revokeSessionSQL, id, at, string(reason))
	if err != nil {
		return mapAuthSQLError("sessions revoke", err)
	}
	if tag.RowsAffected() == 0 {
		return wrapAuth(pgx.ErrNoRows, domain.ErrNotFound, apperror.KindNotFound)
	}
	return nil
}

func (s *AuthSessions) RevokeAllForUser(ctx context.Context, userID uuid.UUID, reason domain.RevokeReason, at time.Time) error {
	_, err := s.q.Exec(ctx, revokeAllSessionsSQL, userID, at, string(reason))
	return mapAuthSQLError("sessions revoke all", err)
}

// ListByUser satisfies SessionStore. SQL is stage 3.27.2.
func (s *AuthSessions) ListByUser(ctx context.Context, userID uuid.UUID, liveOnly bool) ([]domain.Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("auth sessions list by user: not implemented")
}

func (s *AuthSessions) Touch(ctx context.Context, id uuid.UUID, at time.Time) error {
	tag, err := s.q.Exec(ctx, touchSessionSQL, id, at)
	if err != nil {
		return mapAuthSQLError("sessions touch", err)
	}
	if tag.RowsAffected() == 0 {
		return wrapAuth(pgx.ErrNoRows, domain.ErrNotFound, apperror.KindNotFound)
	}
	return nil
}

func (s *AuthTokens) Create(ctx context.Context, tok domain.RefreshToken) error {
	_, err := s.q.Exec(ctx, insertTokenSQL,
		tok.ID, tok.SessionID, copyBytes(tok.Hash), tok.ReplacedBy,
		tok.ExpiresAt, tok.UsedAt, tok.RevokedAt, tok.CreatedAt,
	)
	return mapAuthSQLError("tokens create", err)
}

func (s *AuthTokens) GetByHash(ctx context.Context, hash []byte) (domain.RefreshToken, error) {
	return scanToken(s.q.QueryRow(ctx, selectTokenSQL, hash), "tokens get")
}

func (s *AuthTokens) GetForUpdate(ctx context.Context, hash []byte) (domain.RefreshToken, error) {
	return scanToken(s.q.QueryRow(ctx, selectTokenForUpdateSQL, hash), "tokens get for update")
}

func (s *AuthTokens) MarkUsed(ctx context.Context, id, replacedBy uuid.UUID, at time.Time) error {
	tag, err := s.q.Exec(ctx, markUsedSQL, id, at, replacedBy)
	if err != nil {
		return mapAuthSQLError("tokens mark used", err)
	}
	if tag.RowsAffected() == 0 {
		return wrapAuth(pgx.ErrNoRows, domain.ErrNotFound, apperror.KindNotFound)
	}
	return nil
}

// LockBySession and LockAllForUser satisfy TokenStore. SELECT … FOR UPDATE
// ORDER BY id is stage 3.27.2. Returning nil keeps existing Logout tests
// compiling: application still revokes the session then the tokens.
func (s *AuthTokens) LockBySession(ctx context.Context, sessionID uuid.UUID) error {
	return ctx.Err()
}

func (s *AuthTokens) LockAllForUser(ctx context.Context, userID uuid.UUID) error {
	return ctx.Err()
}

func (s *AuthTokens) RevokeBySession(ctx context.Context, sessionID uuid.UUID, at time.Time) error {
	_, err := s.q.Exec(ctx, revokeTokensBySessionSQL, sessionID, at)
	return mapAuthSQLError("tokens revoke by session", err)
}

func (s *AuthTokens) RevokeAllForUser(ctx context.Context, userID uuid.UUID, at time.Time) error {
	_, err := s.q.Exec(ctx, revokeTokensForUserSQL, userID, at)
	return mapAuthSQLError("tokens revoke all", err)
}

func (s *AuthRoles) Grant(ctx context.Context, ur domain.UserRole) error {
	_, err := s.q.Exec(ctx, grantRoleSQL, ur.UserID, string(ur.Role), uuidPtrArg(ur.GrantedBy), ur.GrantedAt)
	return mapAuthSQLError("roles grant", err)
}

func (s *AuthRoles) Revoke(ctx context.Context, userID uuid.UUID, role domain.Role) error {
	_, err := s.q.Exec(ctx, revokeRoleSQL, userID, string(role))
	return mapAuthSQLError("roles revoke", err)
}

func (s *AuthRoles) List(ctx context.Context, userID uuid.UUID) ([]domain.Role, error) {
	rows, err := s.q.Query(ctx, listRolesSQL, userID)
	if err != nil {
		return nil, mapAuthSQLError("roles list", err)
	}
	defer rows.Close()
	out := make([]domain.Role, 0)
	for rows.Next() {
		var code string
		if err := rows.Scan(&code); err != nil {
			return nil, fmt.Errorf("auth roles list: scan: %w", err)
		}
		out = append(out, domain.Role(code))
	}
	if err := rows.Err(); err != nil {
		return nil, mapAuthSQLError("roles list", err)
	}
	return out, nil
}

func (s *AuthRoles) Has(ctx context.Context, userID uuid.UUID, role domain.Role) (bool, error) {
	var ok bool
	if err := s.q.QueryRow(ctx, hasRoleSQL, userID, string(role)).Scan(&ok); err != nil {
		return false, mapAuthSQLError("roles has", err)
	}
	return ok, nil
}

func (s *AuthRoles) CountActiveWithRole(ctx context.Context, role domain.Role) (int, error) {
	var n int
	if err := s.q.QueryRow(ctx, countActiveRoleSQL, string(role)).Scan(&n); err != nil {
		return 0, mapAuthSQLError("roles count active", err)
	}
	return n, nil
}

func (s *AuthRoles) LockAdminRoleMutations(ctx context.Context) error {
	if _, err := s.q.Exec(ctx, lockAdminRoleMutationsSQL, adminRoleMutationLockKey); err != nil {
		return mapAuthSQLError("roles lock admin mutations", err)
	}
	return nil
}

func (s *AuthAttempts) Append(ctx context.Context, a domain.LoginAttempt) error {
	_, err := s.q.Exec(ctx, insertAttemptSQL,
		a.ID, a.Email, uuidPtrArg(a.UserID), inetArg(a.IP), a.Success,
		failureReasonArg(a.Success, a.FailureReason), a.AttemptedAt,
	)
	return mapAuthSQLError("attempts append", err)
}

func (s *AuthStatusHistory) Append(ctx context.Context, h domain.UserStatusHistory) error {
	_, err := s.q.Exec(ctx, insertUserStatusHistorySQL,
		h.ID, h.UserID, h.ChangedBy, string(h.PreviousStatus), string(h.NewStatus),
		h.Reason, h.CreatedAt,
	)
	return mapAuthSQLError("status history append", err)
}

func (s *AuthLimiter) Hit(ctx context.Context, bucket domain.RateBucket, key string, at time.Time) (int, error) {
	var n int
	err := s.q.QueryRow(ctx, hitLimiterSQL, string(bucket), key, at.UTC()).Scan(&n)
	if err != nil {
		return 0, mapAuthSQLError("limiter hit", err)
	}
	return n, nil
}

func scanUser(row scanner, op string) (domain.User, error) {
	var (
		u      domain.User
		status string
	)
	err := row.Scan(
		&u.ID, &u.Email, &u.PasswordHash, &u.FirstName, &u.LastName,
		&status, &u.FailedLoginCount, &u.LockedAt, &u.CreatedAt, &u.UpdatedAt,
	)
	if err != nil {
		return domain.User{}, mapAuthSQLError(op, err)
	}
	u.Status = domain.Status(status)
	return u, nil
}

func scanSession(row scanner, op string) (domain.Session, error) {
	var (
		s      domain.Session
		reason string
	)
	err := row.Scan(
		&s.ID, &s.UserID, &s.UserAgent, &s.IP,
		&s.CreatedAt, &s.LastSeenAt, &s.RevokedAt, &reason,
	)
	if err != nil {
		return domain.Session{}, mapAuthSQLError(op, err)
	}
	s.RevokeReason = domain.RevokeReason(reason)
	return s, nil
}

func scanToken(row scanner, op string) (domain.RefreshToken, error) {
	var tok domain.RefreshToken
	err := row.Scan(
		&tok.ID, &tok.SessionID, &tok.Hash, &tok.ReplacedBy,
		&tok.ExpiresAt, &tok.UsedAt, &tok.RevokedAt, &tok.CreatedAt,
	)
	if err != nil {
		return domain.RefreshToken{}, mapAuthSQLError(op, err)
	}
	tok.Hash = copyBytes(tok.Hash)
	return tok, nil
}

func mapAuthSQLError(op string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return wrapAuth(err, domain.ErrNotFound, apperror.KindNotFound)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("auth %s: %w", op, err)
	}
	var pe *pgconn.PgError
	if !errors.As(err, &pe) {
		return fmt.Errorf("auth %s: %w", op, err)
	}
	if pe.Code == "23505" {
		switch pe.ConstraintName {
		case uqUsersEmail:
			return wrapAuth(err, domain.ErrEmailTaken, apperror.KindConflict)
		case pkUserRoles:
			return wrapAuth(err, domain.ErrInvalidRequest, apperror.KindInvalid)
		default:
			return fmt.Errorf("auth %s: %w", op, err)
		}
	}
	return fmt.Errorf("auth %s: %w", op, err)
}

func wrapAuth(cause error, sentinel *domain.Error, kind apperror.Kind) error {
	return apperror.Wrap(fmt.Errorf("%w: %w", sentinel, cause), kind, sentinel.Code, sentinel.Message)
}

func inetArg(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}

func uuidPtrArg(id *uuid.UUID) any {
	if id == nil || *id == uuid.Nil {
		return nil
	}
	return *id
}

func failureReasonArg(success bool, r domain.FailureReason) any {
	if success || r == "" {
		return nil
	}
	return string(r)
}

func copyBytes(b []byte) []byte {
	if b == nil {
		return nil
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out
}
