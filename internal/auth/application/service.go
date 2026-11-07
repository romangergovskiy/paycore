package application

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/romangergovskiy/paycore/internal/auth/domain"
)

var errNotWired = errors.New("auth: stores, hasher, signer, issuer, and clock are required")

// Service is Identity/Auth use cases. Ports must not Begin. Writes that
// need one database transaction run inside UnitOfWork.Do and return an
// explicit Decision so security-critical side effects are not rolled back
// just because the use case returned a business error.
type Service struct {
	Users    UserStore
	Sessions SessionStore
	Tokens   TokenStore
	Roles    RoleStore
	Attempts AttemptStore
	Limiter  RateLimiter
	Hasher   Hasher
	Issuer   RefreshIssuer
	Access   AccessSigner
	Clock    Clock
}

func (s Service) writeWired() error {
	if s.Users == nil || s.Sessions == nil || s.Tokens == nil || s.Roles == nil ||
		s.Attempts == nil || s.Limiter == nil || s.Hasher == nil || s.Issuer == nil ||
		s.Access == nil || s.Clock == nil {
		return errNotWired
	}
	return nil
}

func (s Service) readWired() error {
	if s.Users == nil || s.Sessions == nil || s.Roles == nil {
		return errNotWired
	}
	return nil
}

func (s Service) now() time.Time {
	return s.Clock.Now().UTC()
}

func newID() (uuid.UUID, error) { return uuid.NewV7() }

func copyHash(h []byte) []byte {
	if h == nil {
		return nil
	}
	out := make([]byte, len(h))
	copy(out, h)
	return out
}

// Register creates an ACTIVE user with role CUSTOMER only (D-42).
func (s Service) Register(ctx context.Context, cmd RegisterCommand) (domain.PublicUser, Decision, error) {
	var zero domain.PublicUser
	if err := ctx.Err(); err != nil {
		return zero, Rollback, err
	}
	if err := s.writeWired(); err != nil {
		return zero, Rollback, err
	}
	if len(cmd.Roles) > 0 {
		return zero, Rollback, domain.ErrInvalidRequest
	}
	email, err := domain.CanonicalEmail(cmd.Email)
	if err != nil {
		return zero, Rollback, err
	}
	if err := domain.ValidatePassword(cmd.Password); err != nil {
		return zero, Rollback, err
	}
	if err := domain.ValidateName(cmd.FirstName); err != nil {
		return zero, Rollback, err
	}
	if err := domain.ValidateName(cmd.LastName); err != nil {
		return zero, Rollback, err
	}
	if err := domain.ValidateIP(cmd.IP); err != nil {
		return zero, Rollback, err
	}

	now := s.now()
	n, err := s.Limiter.Hit(ctx, domain.BucketRegisterIP, cmd.IP, now)
	if err != nil {
		return zero, Rollback, err
	}
	if n > domain.LimitRegisterIP {
		return zero, Commit, domain.ErrRateLimited
	}

	if _, err := s.Users.GetByEmail(ctx, email); err == nil {
		return zero, Commit, domain.ErrEmailTaken
	} else if !errors.Is(err, domain.ErrNotFound) {
		return zero, Rollback, err
	}

	hash, err := s.Hasher.Hash(cmd.Password)
	if err != nil {
		return zero, Rollback, err
	}
	id, err := newID()
	if err != nil {
		return zero, Rollback, err
	}
	user := domain.User{
		ID:           id,
		Email:        email,
		PasswordHash: hash,
		FirstName:    cmd.FirstName,
		LastName:     cmd.LastName,
		Status:       domain.StatusActive,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := s.Users.Create(ctx, user); err != nil {
		if errors.Is(err, domain.ErrEmailTaken) {
			return zero, Commit, err
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
	pub := user.Public()
	pub.Roles = []domain.Role{domain.RoleCustomer}
	return pub, Commit, nil
}

// Login verifies the password through Hasher, opens a session, and issues
// tokens. Unknown email, wrong password, LOCKED, and DISABLED all return
// INVALID_CREDENTIALS with Commit so lockout and rate-limit writes persist.
func (s Service) Login(ctx context.Context, cmd LoginCommand) (Credentials, Decision, error) {
	var zero Credentials
	if err := ctx.Err(); err != nil {
		return zero, Rollback, err
	}
	if err := s.writeWired(); err != nil {
		return zero, Rollback, err
	}
	email, err := domain.CanonicalEmail(cmd.Email)
	if err != nil {
		return zero, Rollback, err
	}
	if err := domain.ValidatePassword(cmd.Password); err != nil {
		return zero, Rollback, err
	}
	if err := domain.ValidateIP(cmd.IP); err != nil {
		return zero, Rollback, err
	}

	now := s.now()
	n, err := s.Limiter.Hit(ctx, domain.BucketLoginIP, cmd.IP, now)
	if err != nil {
		return zero, Rollback, err
	}
	if n > domain.LimitLoginIP {
		if err := s.noteAttempt(ctx, email, nil, cmd.IP, false, domain.FailureRateLimited, now); err != nil {
			return zero, Rollback, err
		}
		return zero, Commit, domain.ErrRateLimited
	}

	u, err := s.Users.GetByEmail(ctx, email)
	found := err == nil
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return zero, Rollback, err
	}

	encoded := s.Hasher.DummyHash()
	if found {
		encoded = u.PasswordHash
	}
	ok, err := s.Hasher.Verify(cmd.Password, encoded)
	if err != nil {
		return zero, Rollback, err
	}

	if !found || !ok {
		dec, err := s.loginRejected(ctx, email, cmd.IP, u, found, now)
		return zero, dec, err
	}

	// Re-read under row lock so a concurrent lockout is not overwritten
	// by Update of the GetByEmail snapshot after verify.
	fresh, err := s.Users.GetForUpdate(ctx, u.ID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			dec, rerr := s.loginRejected(ctx, email, cmd.IP, u, false, now)
			return zero, dec, rerr
		}
		return zero, Rollback, err
	}
	if fresh.Status != domain.StatusActive {
		dec, err := s.rejectInactive(ctx, email, cmd.IP, fresh, now)
		return zero, dec, err
	}

	fresh.FailedLoginCount = 0
	fresh.LockedAt = nil
	fresh.UpdatedAt = now
	if err := s.Users.Update(ctx, fresh); err != nil {
		return zero, Rollback, err
	}
	roles, err := s.Roles.List(ctx, fresh.ID)
	if err != nil {
		return zero, Rollback, err
	}
	cred, err := s.issueSession(ctx, fresh, roles, cmd.IP, cmd.UserAgent, now)
	if err != nil {
		return zero, Rollback, err
	}
	if err := s.noteAttempt(ctx, email, &fresh.ID, cmd.IP, true, "", now); err != nil {
		return zero, Rollback, err
	}
	return cred, Commit, nil
}

func (s Service) loginRejected(ctx context.Context, email, ip string, u domain.User, found bool, now time.Time) (Decision, error) {
	n, err := s.Limiter.Hit(ctx, domain.BucketLoginEmail, email, now)
	if err != nil {
		return Rollback, err
	}

	reason := domain.FailureInvalidCredentials
	var uid *uuid.UUID
	if found {
		fresh, err := s.Users.GetForUpdate(ctx, u.ID)
		if err != nil {
			if !errors.Is(err, domain.ErrNotFound) {
				return Rollback, err
			}
		} else {
			uid = &fresh.ID
			if fresh.Status == domain.StatusActive {
				fresh, err = s.applyActiveFailure(ctx, fresh, now)
				if err != nil {
					return Rollback, err
				}
			}
			reason = failureReason(fresh.Status)
		}
	}

	if n > domain.LimitLoginEmail {
		if err := s.noteAttempt(ctx, email, uid, ip, false, domain.FailureRateLimited, now); err != nil {
			return Rollback, err
		}
		return Commit, domain.ErrRateLimited
	}
	if err := s.noteAttempt(ctx, email, uid, ip, false, reason, now); err != nil {
		return Rollback, err
	}
	return Commit, domain.ErrInvalidCredentials
}

func (s Service) rejectInactive(ctx context.Context, email, ip string, u domain.User, now time.Time) (Decision, error) {
	n, err := s.Limiter.Hit(ctx, domain.BucketLoginEmail, email, now)
	if err != nil {
		return Rollback, err
	}
	reason := domain.FailureLocked
	if u.Status == domain.StatusDisabled {
		reason = domain.FailureDisabled
	}
	if n > domain.LimitLoginEmail {
		if err := s.noteAttempt(ctx, email, &u.ID, ip, false, domain.FailureRateLimited, now); err != nil {
			return Rollback, err
		}
		return Commit, domain.ErrRateLimited
	}
	if err := s.noteAttempt(ctx, email, &u.ID, ip, false, reason, now); err != nil {
		return Rollback, err
	}
	return Commit, domain.ErrInvalidCredentials
}

func failureReason(st domain.Status) domain.FailureReason {
	switch st {
	case domain.StatusLocked:
		return domain.FailureLocked
	case domain.StatusDisabled:
		return domain.FailureDisabled
	default:
		return domain.FailureInvalidCredentials
	}
}

// applyActiveFailure increments the locked ACTIVE row. LOCKED/DISABLED
// callers must not invoke it. One failed login does not GetForUpdate again.
func (s Service) applyActiveFailure(ctx context.Context, u domain.User, now time.Time) (domain.User, error) {
	u.FailedLoginCount++
	u.UpdatedAt = now
	if u.FailedLoginCount >= domain.LockAfter {
		locked := now
		u.Status = domain.StatusLocked
		u.LockedAt = &locked
		if err := s.Users.Update(ctx, u); err != nil {
			return u, err
		}
		if err := s.Sessions.RevokeAllForUser(ctx, u.ID, domain.RevokeUserLocked, now); err != nil {
			return u, err
		}
		if err := s.Tokens.RevokeAllForUser(ctx, u.ID, now); err != nil {
			return u, err
		}
		return u, nil
	}
	if err := s.Users.Update(ctx, u); err != nil {
		return u, err
	}
	return u, nil
}

func (s Service) issueSession(ctx context.Context, u domain.User, roles []domain.Role, ip, ua string, now time.Time) (Credentials, error) {
	var zero Credentials
	sid, err := newID()
	if err != nil {
		return zero, err
	}
	sess := domain.Session{
		ID:         sid,
		UserID:     u.ID,
		UserAgent:  ua,
		IP:         ip,
		CreatedAt:  now,
		LastSeenAt: now,
	}
	if err := s.Sessions.Create(ctx, sess); err != nil {
		return zero, err
	}
	plain, hash, err := s.Issuer.Issue()
	if err != nil {
		return zero, err
	}
	tid, err := newID()
	if err != nil {
		return zero, err
	}
	tok := domain.RefreshToken{
		ID:        tid,
		SessionID: sid,
		Hash:      copyHash(hash),
		ExpiresAt: now.Add(domain.RefreshTTL),
		CreatedAt: now,
	}
	if err := s.Tokens.Create(ctx, tok); err != nil {
		return zero, err
	}
	p := domain.Principal{UserID: u.ID, SessionID: sid, Roles: roles}
	exp := now.Add(domain.AccessTTL)
	access, err := s.Access.Sign(p, now, exp)
	if err != nil {
		return zero, err
	}
	pub := u.Public()
	pub.Roles = append([]domain.Role(nil), roles...)
	return Credentials{
		SessionID:    sid,
		AccessToken:  access,
		RefreshToken: plain,
		TokenType:    tokenTypeBearer,
		ExpiresIn:    domain.AccessExpiresIn,
		User:         pub,
	}, nil
}

func (s Service) noteAttempt(ctx context.Context, email string, userID *uuid.UUID, ip string, success bool, reason domain.FailureReason, now time.Time) error {
	id, err := newID()
	if err != nil {
		return err
	}
	return s.Attempts.Append(ctx, domain.LoginAttempt{
		ID:            id,
		Email:         email,
		UserID:        userID,
		IP:            ip,
		Success:       success,
		FailureReason: reason,
		AttemptedAt:   now,
	})
}

// Refresh rotates the opaque refresh token (D-41). Reuse revokes the session.
func (s Service) Refresh(ctx context.Context, cmd RefreshCommand) (Credentials, Decision, error) {
	var zero Credentials
	if err := ctx.Err(); err != nil {
		return zero, Rollback, err
	}
	if err := s.writeWired(); err != nil {
		return zero, Rollback, err
	}
	if cmd.RefreshToken == "" {
		return zero, Rollback, domain.ErrInvalidRequest
	}
	if err := domain.ValidateIP(cmd.IP); err != nil {
		return zero, Rollback, err
	}

	now := s.now()
	n, err := s.Limiter.Hit(ctx, domain.BucketRefreshIP, cmd.IP, now)
	if err != nil {
		return zero, Rollback, err
	}
	if n > domain.LimitRefreshIP {
		return zero, Commit, domain.ErrRateLimited
	}

	hash, err := s.Issuer.Hash(cmd.RefreshToken)
	if err != nil {
		return zero, Rollback, err
	}
	tok, err := s.Tokens.GetForUpdate(ctx, hash)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return zero, Commit, domain.ErrInvalidToken
		}
		return zero, Rollback, err
	}
	if tok.Reused() {
		sess, serr := s.Sessions.Get(ctx, tok.SessionID)
		if serr != nil && !errors.Is(serr, domain.ErrNotFound) {
			return zero, Rollback, serr
		}
		if serr == nil && sess.Live() {
			if err := s.Sessions.Revoke(ctx, sess.ID, domain.RevokeTokenReuse, now); err != nil {
				return zero, Rollback, err
			}
		}
		if err := s.Tokens.RevokeBySession(ctx, tok.SessionID, now); err != nil {
			return zero, Rollback, err
		}
		return zero, Commit, domain.ErrTokenReused
	}
	if tok.Expired(now) {
		return zero, Commit, domain.ErrTokenExpired
	}

	sess, err := s.Sessions.Get(ctx, tok.SessionID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return zero, Commit, domain.ErrInvalidToken
		}
		return zero, Rollback, err
	}
	if !sess.Live() {
		return zero, Commit, domain.ErrInvalidToken
	}
	u, err := s.Users.Get(ctx, sess.UserID)
	if err != nil {
		return zero, Rollback, err
	}
	if u.Status != domain.StatusActive {
		return zero, Commit, domain.ErrInvalidToken
	}
	roles, err := s.Roles.List(ctx, u.ID)
	if err != nil {
		return zero, Rollback, err
	}

	plain, newHash, err := s.Issuer.Issue()
	if err != nil {
		return zero, Rollback, err
	}
	nid, err := newID()
	if err != nil {
		return zero, Rollback, err
	}
	next := domain.RefreshToken{
		ID:        nid,
		SessionID: sess.ID,
		Hash:      copyHash(newHash),
		ExpiresAt: now.Add(domain.RefreshTTL),
		CreatedAt: now,
	}
	if err := s.Tokens.Create(ctx, next); err != nil {
		return zero, Rollback, err
	}
	if err := s.Tokens.MarkUsed(ctx, tok.ID, nid, now); err != nil {
		return zero, Rollback, err
	}
	if err := s.Sessions.Touch(ctx, sess.ID, now); err != nil {
		return zero, Rollback, err
	}
	p := domain.Principal{UserID: u.ID, SessionID: sess.ID, Roles: roles}
	access, err := s.Access.Sign(p, now, now.Add(domain.AccessTTL))
	if err != nil {
		return zero, Rollback, err
	}
	pub := u.Public()
	pub.Roles = append([]domain.Role(nil), roles...)
	return Credentials{
		SessionID:    sess.ID,
		AccessToken:  access,
		RefreshToken: plain,
		TokenType:    tokenTypeBearer,
		ExpiresIn:    domain.AccessExpiresIn,
		User:         pub,
	}, Commit, nil
}

// Logout revokes the current session. A second logout is a no-op success.
func (s Service) Logout(ctx context.Context, p domain.Principal) (Decision, error) {
	if err := ctx.Err(); err != nil {
		return Rollback, err
	}
	if s.Sessions == nil || s.Tokens == nil || s.Clock == nil {
		return Rollback, errNotWired
	}
	if p.SessionID == uuid.Nil {
		return Rollback, domain.ErrInvalidToken
	}
	sess, err := s.Sessions.Get(ctx, p.SessionID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return Rollback, domain.ErrInvalidToken
		}
		return Rollback, err
	}
	if sess.UserID != p.UserID {
		return Rollback, domain.ErrInvalidToken
	}
	if !sess.Live() {
		return Commit, nil
	}
	now := s.now()
	if err := s.Sessions.Revoke(ctx, sess.ID, domain.RevokeLogout, now); err != nil {
		return Rollback, err
	}
	if err := s.Tokens.RevokeBySession(ctx, sess.ID, now); err != nil {
		return Rollback, err
	}
	return Commit, nil
}

// ValidateSession checks that the session is live and the user is ACTIVE.
// Roles are loaded from the store (variant A): JWT claims are a 15-minute
// cache. Grant/revoke does not revoke sessions; the next ValidateSession
// sees current roles.
func (s Service) ValidateSession(ctx context.Context, sessionID uuid.UUID) (domain.Principal, error) {
	var zero domain.Principal
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	if err := s.readWired(); err != nil {
		return zero, err
	}
	if sessionID == uuid.Nil {
		return zero, domain.ErrInvalidToken
	}
	sess, err := s.Sessions.Get(ctx, sessionID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return zero, domain.ErrInvalidToken
		}
		return zero, err
	}
	if !sess.Live() {
		return zero, domain.ErrInvalidToken
	}
	u, err := s.Users.Get(ctx, sess.UserID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return zero, domain.ErrInvalidToken
		}
		return zero, err
	}
	if u.Status != domain.StatusActive {
		return zero, domain.ErrInvalidToken
	}
	roles, err := s.Roles.List(ctx, u.ID)
	if err != nil {
		return zero, err
	}
	return domain.Principal{UserID: u.ID, SessionID: sess.ID, Roles: roles}, nil
}

// GetRoles returns the user's current roles.
func (s Service) GetRoles(ctx context.Context, userID uuid.UUID) ([]domain.Role, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.Users == nil || s.Roles == nil {
		return nil, errNotWired
	}
	if userID == uuid.Nil {
		return nil, domain.ErrNotFound
	}
	if _, err := s.Users.Get(ctx, userID); err != nil {
		return nil, err
	}
	return s.Roles.List(ctx, userID)
}

// GrantRole assigns a role. Only ADMIN may call it. Sessions stay live;
// ValidateSession loads current roles from the store (identity-auth §4.1).
func (s Service) GrantRole(ctx context.Context, actor domain.Principal, userID uuid.UUID, role domain.Role) (Decision, error) {
	if err := ctx.Err(); err != nil {
		return Rollback, err
	}
	if s.Users == nil || s.Roles == nil || s.Clock == nil {
		return Rollback, errNotWired
	}
	if !actor.Has(domain.RoleAdmin) {
		return Rollback, domain.ErrForbidden
	}
	if !domain.KnownRole(role) {
		return Rollback, domain.ErrInvalidRequest
	}
	if actor.UserID == userID && role != domain.RoleCustomer {
		return Rollback, domain.ErrForbidden
	}
	if role == domain.RoleAdmin {
		if err := s.lockAndRequireActiveAdmin(ctx, actor.UserID); err != nil {
			return Rollback, err
		}
	}
	if _, err := s.Users.Get(ctx, userID); err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return Rollback, err
		}
		return Rollback, err
	}
	has, err := s.Roles.Has(ctx, userID, role)
	if err != nil {
		return Rollback, err
	}
	if has {
		return Rollback, domain.ErrInvalidRequest
	}
	gid := actor.UserID
	if err := s.Roles.Grant(ctx, domain.UserRole{
		UserID:    userID,
		Role:      role,
		GrantedBy: &gid,
		GrantedAt: s.now(),
	}); err != nil {
		return Rollback, err
	}
	return Commit, nil
}

// RevokeRole removes a role. The last ACTIVE ADMIN cannot be revoked.
func (s Service) RevokeRole(ctx context.Context, actor domain.Principal, userID uuid.UUID, role domain.Role) (Decision, error) {
	if err := ctx.Err(); err != nil {
		return Rollback, err
	}
	if s.Users == nil || s.Roles == nil {
		return Rollback, errNotWired
	}
	if !actor.Has(domain.RoleAdmin) {
		return Rollback, domain.ErrForbidden
	}
	if !domain.KnownRole(role) {
		return Rollback, domain.ErrInvalidRequest
	}
	if actor.UserID == userID && role == domain.RoleAdmin {
		return Rollback, domain.ErrForbidden
	}
	if role == domain.RoleAdmin {
		if err := s.lockAndRequireActiveAdmin(ctx, actor.UserID); err != nil {
			return Rollback, err
		}
	}
	u, err := s.Users.Get(ctx, userID)
	if err != nil {
		return Rollback, err
	}
	has, err := s.Roles.Has(ctx, userID, role)
	if err != nil {
		return Rollback, err
	}
	if !has {
		return Rollback, domain.ErrInvalidRequest
	}
	if role == domain.RoleAdmin {
		n, err := s.Roles.CountActiveWithRole(ctx, domain.RoleAdmin)
		if err != nil {
			return Rollback, err
		}
		if u.Status == domain.StatusActive && n <= 1 {
			return Rollback, domain.ErrLastAdmin
		}
		if u.Status != domain.StatusActive && n == 0 {
			return Rollback, domain.ErrLastAdmin
		}
	}
	if err := s.Roles.Revoke(ctx, userID, role); err != nil {
		return Rollback, err
	}
	return Commit, nil
}

// lockAndRequireActiveAdmin takes the ADMIN-mutation lock in this Tx, then
// re-reads the initiator. A stale Principal that still Has(ADMIN) is
// Forbidden when the store row is not ACTIVE with role ADMIN.
func (s Service) lockAndRequireActiveAdmin(ctx context.Context, actorID uuid.UUID) error {
	if err := s.Roles.LockAdminRoleMutations(ctx); err != nil {
		return err
	}
	u, err := s.Users.Get(ctx, actorID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return domain.ErrForbidden
		}
		return err
	}
	if u.Status != domain.StatusActive {
		return domain.ErrForbidden
	}
	has, err := s.Roles.Has(ctx, actorID, domain.RoleAdmin)
	if err != nil {
		return err
	}
	if !has {
		return domain.ErrForbidden
	}
	return nil
}
