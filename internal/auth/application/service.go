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
// need one database transaction run inside UnitOfWork.Do.
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
func (s Service) Register(ctx context.Context, cmd RegisterCommand) (domain.PublicUser, error) {
	var zero domain.PublicUser
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	if err := s.writeWired(); err != nil {
		return zero, err
	}
	if len(cmd.Roles) > 0 {
		return zero, domain.ErrInvalidRequest
	}
	email, err := domain.CanonicalEmail(cmd.Email)
	if err != nil {
		return zero, err
	}
	if err := domain.ValidatePassword(cmd.Password); err != nil {
		return zero, err
	}
	if err := domain.ValidateName(cmd.FirstName); err != nil {
		return zero, err
	}
	if err := domain.ValidateName(cmd.LastName); err != nil {
		return zero, err
	}
	if err := domain.ValidateIP(cmd.IP); err != nil {
		return zero, err
	}

	now := s.now()
	n, err := s.Limiter.Hit(ctx, domain.BucketRegisterIP, cmd.IP, now)
	if err != nil {
		return zero, err
	}
	if n > domain.LimitRegisterIP {
		return zero, domain.ErrRateLimited
	}

	if _, err := s.Users.GetByEmail(ctx, email); err == nil {
		return zero, domain.ErrEmailTaken
	} else if !errors.Is(err, domain.ErrNotFound) {
		return zero, err
	}

	hash, err := s.Hasher.Hash(cmd.Password)
	if err != nil {
		return zero, err
	}
	id, err := newID()
	if err != nil {
		return zero, err
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
		return zero, err
	}
	if err := s.Roles.Grant(ctx, domain.UserRole{
		UserID:    id,
		Role:      domain.RoleCustomer,
		GrantedAt: now,
	}); err != nil {
		return zero, err
	}
	pub := user.Public()
	pub.Roles = []domain.Role{domain.RoleCustomer}
	return pub, nil
}

// Login verifies the password through Hasher, opens a session, and issues
// tokens. Unknown email, wrong password, LOCKED, and DISABLED all return
// INVALID_CREDENTIALS. Rate-limit hits persist even when this returns an error.
func (s Service) Login(ctx context.Context, cmd LoginCommand) (Credentials, error) {
	var zero Credentials
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	if err := s.writeWired(); err != nil {
		return zero, err
	}
	email, err := domain.CanonicalEmail(cmd.Email)
	if err != nil {
		return zero, err
	}
	if err := domain.ValidatePassword(cmd.Password); err != nil {
		return zero, err
	}
	if err := domain.ValidateIP(cmd.IP); err != nil {
		return zero, err
	}

	now := s.now()
	n, err := s.Limiter.Hit(ctx, domain.BucketLoginIP, cmd.IP, now)
	if err != nil {
		return zero, err
	}
	if n > domain.LimitLoginIP {
		s.noteAttempt(ctx, email, nil, cmd.IP, false, domain.FailureRateLimited, now)
		return zero, domain.ErrRateLimited
	}

	u, err := s.Users.GetByEmail(ctx, email)
	found := err == nil
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return zero, err
	}

	encoded := s.Hasher.DummyHash()
	if found {
		encoded = u.PasswordHash
	}
	ok, err := s.Hasher.Verify(cmd.Password, encoded)
	if err != nil {
		return zero, err
	}

	if !found || !ok || u.Status != domain.StatusActive {
		return zero, s.loginRejected(ctx, email, cmd.IP, u, found, now)
	}

	u.FailedLoginCount = 0
	u.LockedAt = nil
	u.UpdatedAt = now
	if err := s.Users.Update(ctx, u); err != nil {
		return zero, err
	}
	roles, err := s.Roles.List(ctx, u.ID)
	if err != nil {
		return zero, err
	}
	cred, err := s.issueSession(ctx, u, roles, cmd.IP, cmd.UserAgent, now)
	if err != nil {
		return zero, err
	}
	s.noteAttempt(ctx, email, &u.ID, cmd.IP, true, "", now)
	return cred, nil
}

func (s Service) loginRejected(ctx context.Context, email, ip string, u domain.User, found bool, now time.Time) error {
	n, err := s.Limiter.Hit(ctx, domain.BucketLoginEmail, email, now)
	if err != nil {
		return err
	}

	reason := domain.FailureInvalidCredentials
	var uid *uuid.UUID
	if found {
		uid = &u.ID
		switch u.Status {
		case domain.StatusLocked:
			reason = domain.FailureLocked
		case domain.StatusDisabled:
			reason = domain.FailureDisabled
		}
		if u.Status == domain.StatusActive {
			if err := s.bumpFailure(ctx, u.ID, now); err != nil {
				return err
			}
		}
	}

	if n > domain.LimitLoginEmail {
		s.noteAttempt(ctx, email, uid, ip, false, domain.FailureRateLimited, now)
		return domain.ErrRateLimited
	}
	s.noteAttempt(ctx, email, uid, ip, false, reason, now)
	return domain.ErrInvalidCredentials
}

func (s Service) bumpFailure(ctx context.Context, userID uuid.UUID, now time.Time) error {
	u, err := s.Users.GetForUpdate(ctx, userID)
	if err != nil {
		return err
	}
	u.FailedLoginCount++
	u.UpdatedAt = now
	if u.FailedLoginCount >= domain.LockAfter && u.Status == domain.StatusActive {
		locked := now
		u.Status = domain.StatusLocked
		u.LockedAt = &locked
		if err := s.Users.Update(ctx, u); err != nil {
			return err
		}
		if err := s.Sessions.RevokeAllForUser(ctx, userID, domain.RevokeUserLocked, now); err != nil {
			return err
		}
		return s.Tokens.RevokeAllForUser(ctx, userID, now)
	}
	return s.Users.Update(ctx, u)
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

func (s Service) noteAttempt(ctx context.Context, email string, userID *uuid.UUID, ip string, success bool, reason domain.FailureReason, now time.Time) {
	id, err := newID()
	if err != nil {
		return
	}
	_ = s.Attempts.Append(ctx, domain.LoginAttempt{
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
func (s Service) Refresh(ctx context.Context, cmd RefreshCommand) (Credentials, error) {
	var zero Credentials
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	if err := s.writeWired(); err != nil {
		return zero, err
	}
	if cmd.RefreshToken == "" {
		return zero, domain.ErrInvalidRequest
	}
	if err := domain.ValidateIP(cmd.IP); err != nil {
		return zero, err
	}

	now := s.now()
	n, err := s.Limiter.Hit(ctx, domain.BucketRefreshIP, cmd.IP, now)
	if err != nil {
		return zero, err
	}
	if n > domain.LimitRefreshIP {
		return zero, domain.ErrRateLimited
	}

	hash, err := s.Issuer.Hash(cmd.RefreshToken)
	if err != nil {
		return zero, err
	}
	tok, err := s.Tokens.GetForUpdate(ctx, hash)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return zero, domain.ErrInvalidToken
		}
		return zero, err
	}
	if tok.Reused() {
		sess, serr := s.Sessions.Get(ctx, tok.SessionID)
		if serr != nil && !errors.Is(serr, domain.ErrNotFound) {
			return zero, serr
		}
		if serr == nil && sess.Live() {
			if err := s.Sessions.Revoke(ctx, sess.ID, domain.RevokeTokenReuse, now); err != nil {
				return zero, err
			}
		}
		if err := s.Tokens.RevokeBySession(ctx, tok.SessionID, now); err != nil {
			return zero, err
		}
		return zero, domain.ErrTokenReused
	}
	if tok.Expired(now) {
		return zero, domain.ErrTokenExpired
	}

	sess, err := s.Sessions.Get(ctx, tok.SessionID)
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
		return zero, err
	}
	if u.Status != domain.StatusActive {
		return zero, domain.ErrInvalidToken
	}
	roles, err := s.Roles.List(ctx, u.ID)
	if err != nil {
		return zero, err
	}

	plain, newHash, err := s.Issuer.Issue()
	if err != nil {
		return zero, err
	}
	nid, err := newID()
	if err != nil {
		return zero, err
	}
	next := domain.RefreshToken{
		ID:        nid,
		SessionID: sess.ID,
		Hash:      copyHash(newHash),
		ExpiresAt: now.Add(domain.RefreshTTL),
		CreatedAt: now,
	}
	if err := s.Tokens.Create(ctx, next); err != nil {
		return zero, err
	}
	if err := s.Tokens.MarkUsed(ctx, tok.ID, nid, now); err != nil {
		return zero, err
	}
	if err := s.Sessions.Touch(ctx, sess.ID, now); err != nil {
		return zero, err
	}
	p := domain.Principal{UserID: u.ID, SessionID: sess.ID, Roles: roles}
	access, err := s.Access.Sign(p, now, now.Add(domain.AccessTTL))
	if err != nil {
		return zero, err
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
	}, nil
}

// Logout revokes the current session. A second logout is a no-op success.
func (s Service) Logout(ctx context.Context, p domain.Principal) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.Sessions == nil || s.Tokens == nil || s.Clock == nil {
		return errNotWired
	}
	if p.SessionID == uuid.Nil {
		return domain.ErrInvalidToken
	}
	sess, err := s.Sessions.Get(ctx, p.SessionID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return domain.ErrInvalidToken
		}
		return err
	}
	if sess.UserID != p.UserID {
		return domain.ErrInvalidToken
	}
	if !sess.Live() {
		return nil
	}
	now := s.now()
	if err := s.Sessions.Revoke(ctx, sess.ID, domain.RevokeLogout, now); err != nil {
		return err
	}
	return s.Tokens.RevokeBySession(ctx, sess.ID, now)
}

// ValidateSession checks that the session is live and the user is ACTIVE.
// Roles are loaded from the store (D-42): grant/revoke is visible here
// without revoking existing sessions.
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
// ValidateSession and the next refresh load current roles (identity-auth §4.1).
func (s Service) GrantRole(ctx context.Context, actor domain.Principal, userID uuid.UUID, role domain.Role) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.Users == nil || s.Roles == nil || s.Clock == nil {
		return errNotWired
	}
	if !actor.Has(domain.RoleAdmin) {
		return domain.ErrForbidden
	}
	if !domain.KnownRole(role) {
		return domain.ErrInvalidRequest
	}
	if actor.UserID == userID && role != domain.RoleCustomer {
		return domain.ErrForbidden
	}
	if _, err := s.Users.Get(ctx, userID); err != nil {
		return err
	}
	has, err := s.Roles.Has(ctx, userID, role)
	if err != nil {
		return err
	}
	if has {
		return domain.ErrInvalidRequest
	}
	gid := actor.UserID
	return s.Roles.Grant(ctx, domain.UserRole{
		UserID:    userID,
		Role:      role,
		GrantedBy: &gid,
		GrantedAt: s.now(),
	})
}

// RevokeRole removes a role. The last ACTIVE ADMIN cannot be revoked.
func (s Service) RevokeRole(ctx context.Context, actor domain.Principal, userID uuid.UUID, role domain.Role) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.Users == nil || s.Roles == nil {
		return errNotWired
	}
	if !actor.Has(domain.RoleAdmin) {
		return domain.ErrForbidden
	}
	if !domain.KnownRole(role) {
		return domain.ErrInvalidRequest
	}
	if actor.UserID == userID && role == domain.RoleAdmin {
		return domain.ErrForbidden
	}
	u, err := s.Users.Get(ctx, userID)
	if err != nil {
		return err
	}
	has, err := s.Roles.Has(ctx, userID, role)
	if err != nil {
		return err
	}
	if !has {
		return domain.ErrInvalidRequest
	}
	if role == domain.RoleAdmin {
		n, err := s.Roles.CountActiveWithRole(ctx, domain.RoleAdmin)
		if err != nil {
			return err
		}
		if u.Status == domain.StatusActive && n <= 1 {
			return domain.ErrLastAdmin
		}
		if u.Status != domain.StatusActive && n == 0 {
			return domain.ErrLastAdmin
		}
	}
	return s.Roles.Revoke(ctx, userID, role)
}
