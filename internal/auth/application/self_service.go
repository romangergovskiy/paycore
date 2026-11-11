package application

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/romangergovskiy/paycore/internal/auth/domain"
)

func (s Service) revokeSessionAfterTokenLock(ctx context.Context, sessionID uuid.UUID, reason domain.RevokeReason, at time.Time) error {
	if err := s.Tokens.LockBySession(ctx, sessionID); err != nil {
		return err
	}
	if err := s.Sessions.Revoke(ctx, sessionID, reason, at); err != nil {
		return err
	}
	return s.Tokens.RevokeBySession(ctx, sessionID, at)
}

func (s Service) revokeUserAfterTokenLock(ctx context.Context, userID uuid.UUID, reason domain.RevokeReason, at time.Time) error {
	if s.Users == nil {
		return errNotWired
	}
	// User row first so a concurrent Login cannot INSERT a session after
	// LockAllForUser's token set looks stable. Same Tx; order is user →
	// tokens → sessions. A second GetForUpdate of a row already held is a
	// no-op. Login that waited behind this lock runs after Commit and may
	// create a new session (logout-all then login, not a leak).
	if _, err := s.Users.GetForUpdate(ctx, userID); err != nil {
		return err
	}
	if err := s.Tokens.LockAllForUser(ctx, userID); err != nil {
		return err
	}
	if err := s.Sessions.RevokeAllForUser(ctx, userID, reason, at); err != nil {
		return err
	}
	return s.Tokens.RevokeAllForUser(ctx, userID, at)
}

// CurrentUser returns the actor's profile from the store. Roles come from
// RoleStore, not Principal.Roles. There is no UnitOfWork: the Bearer path
// already proved a live session; this re-reads user+roles on the pool.
func (s Service) CurrentUser(ctx context.Context, p domain.Principal) (domain.PublicUser, error) {
	var zero domain.PublicUser
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	if s.Users == nil || s.Roles == nil {
		return zero, errNotWired
	}
	if p.UserID == uuid.Nil {
		return zero, domain.ErrInvalidToken
	}
	u, err := s.Users.Get(ctx, p.UserID)
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
	pub := u.Public()
	pub.Roles = append([]domain.Role(nil), roles...)
	return pub, nil
}

// LogoutAll revokes every live session of the actor with LOGOUT_ALL.
// A repeat is a no-op Commit, like Logout. The user row is locked in
// the same Tx before tokens so a Login that already holds GetForUpdate
// can insert, then this waits and still revokes that session. A Login
// that waits behind this lock runs after Commit and may create a new
// session.
func (s Service) LogoutAll(ctx context.Context, p domain.Principal) (Decision, error) {
	if err := ctx.Err(); err != nil {
		return Rollback, err
	}
	if s.Users == nil || s.Sessions == nil || s.Tokens == nil || s.Clock == nil {
		return Rollback, errNotWired
	}
	if p.UserID == uuid.Nil || p.SessionID == uuid.Nil {
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
	now := s.now()
	if err := s.revokeUserAfterTokenLock(ctx, p.UserID, domain.RevokeLogoutAll, now); err != nil {
		return Rollback, err
	}
	return Commit, nil
}

// ChangePassword order in Tx: GetForUpdate → Verify current PHC →
// ValidatePassword(new) → Hash → Update → revoke all PASSWORD_CHANGED.
// A stale old password after a concurrent winner is INVALID_CREDENTIALS + Rollback.
func (s Service) ChangePassword(ctx context.Context, cmd ChangePasswordCommand) (Decision, error) {
	if err := ctx.Err(); err != nil {
		return Rollback, err
	}
	if s.Users == nil || s.Sessions == nil || s.Tokens == nil || s.Hasher == nil || s.Clock == nil {
		return Rollback, errNotWired
	}
	if cmd.Actor.UserID == uuid.Nil {
		return Rollback, domain.ErrInvalidToken
	}

	u, err := s.Users.GetForUpdate(ctx, cmd.Actor.UserID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return Rollback, domain.ErrInvalidToken
		}
		return Rollback, err
	}
	if u.Status != domain.StatusActive {
		return Rollback, domain.ErrInvalidToken
	}
	ok, err := s.Hasher.Verify(cmd.OldPassword, u.PasswordHash)
	if err != nil {
		return Rollback, err
	}
	if !ok {
		return Rollback, domain.ErrInvalidCredentials
	}
	if cmd.OldPassword == cmd.NewPassword {
		return Rollback, domain.ErrInvalidRequest
	}
	if err := domain.ValidatePassword(cmd.NewPassword); err != nil {
		return Rollback, err
	}
	hash, err := s.Hasher.Hash(cmd.NewPassword)
	if err != nil {
		return Rollback, err
	}
	now := s.now()
	u.PasswordHash = hash
	u.UpdatedAt = now
	if err := s.Users.Update(ctx, u); err != nil {
		return Rollback, err
	}
	if err := s.revokeUserAfterTokenLock(ctx, u.ID, domain.RevokePasswordChanged, now); err != nil {
		return Rollback, err
	}
	return Commit, nil
}

// ListSessions returns the actor's live sessions. There is no UnitOfWork.
func (s Service) ListSessions(ctx context.Context, p domain.Principal) ([]domain.Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.Sessions == nil {
		return nil, errNotWired
	}
	if p.UserID == uuid.Nil {
		return nil, domain.ErrInvalidToken
	}
	return s.Sessions.ListByUser(ctx, p.UserID, true)
}

// RevokeSession revokes one of the actor's sessions with LOGOUT. A foreign
// or missing id is NOT_FOUND. An already revoked own session is Commit.
func (s Service) RevokeSession(ctx context.Context, cmd RevokeSessionCommand) (Decision, error) {
	if err := ctx.Err(); err != nil {
		return Rollback, err
	}
	if s.Sessions == nil || s.Tokens == nil || s.Clock == nil {
		return Rollback, errNotWired
	}
	if cmd.Actor.UserID == uuid.Nil || cmd.SessionID == uuid.Nil {
		return Rollback, domain.ErrInvalidRequest
	}
	sess, err := s.Sessions.Get(ctx, cmd.SessionID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return Rollback, domain.ErrNotFound
		}
		return Rollback, err
	}
	if sess.UserID != cmd.Actor.UserID {
		return Rollback, domain.ErrNotFound
	}
	if !sess.Live() {
		return Commit, nil
	}
	now := s.now()
	if err := s.revokeSessionAfterTokenLock(ctx, sess.ID, domain.RevokeLogout, now); err != nil {
		return Rollback, err
	}
	return Commit, nil
}

// EnableUser sets DISABLED → ACTIVE. Already ACTIVE is a no-op Commit
// without history. LOCKED is INVALID_REQUEST (UnlockUser). Advisory lock,
// actor re-check, then GetForUpdate — same order as Lock/Unlock/Disable.
func (s Service) EnableUser(ctx context.Context, cmd StatusCommand) (Decision, error) {
	if err := ctx.Err(); err != nil {
		return Rollback, err
	}
	if err := s.statusWired(); err != nil {
		return Rollback, err
	}
	if err := s.validateStatusCommand(cmd); err != nil {
		return Rollback, err
	}
	u, err := s.prepareStatusTarget(ctx, cmd.Actor, cmd.UserID)
	if err != nil {
		return Rollback, err
	}
	if u.Status == domain.StatusActive {
		return Commit, nil
	}
	if u.Status != domain.StatusDisabled {
		return Rollback, domain.ErrInvalidRequest
	}
	now := s.now()
	u.Status = domain.StatusActive
	u.LockedAt = nil
	u.FailedLoginCount = 0
	u.UpdatedAt = now
	if err := s.Users.Update(ctx, u); err != nil {
		return Rollback, err
	}
	if err := s.recordStatusChange(ctx, cmd, domain.StatusDisabled, domain.StatusActive, now); err != nil {
		return Rollback, err
	}
	return Commit, nil
}
