// Package domain is the Identity/Auth business model (schema auth).
//
// It does not import pgx, HTTP, JWT, or Argon2. Password hashing and
// access-token signing live behind application ports.
package domain

import (
	"time"

	"github.com/google/uuid"
)

type Status string

const (
	StatusActive   Status = "ACTIVE"
	StatusLocked   Status = "LOCKED"
	StatusDisabled Status = "DISABLED"
)

type Role string

const (
	RoleCustomer    Role = "CUSTOMER"
	RoleMerchant    Role = "MERCHANT"
	RoleSupport     Role = "SUPPORT"
	RoleOperator    Role = "OPERATOR"
	RoleRiskAnalyst Role = "RISK_ANALYST"
	RoleAdmin       Role = "ADMIN"
)

func KnownRole(r Role) bool {
	switch r {
	case RoleCustomer, RoleMerchant, RoleSupport, RoleOperator, RoleRiskAnalyst, RoleAdmin:
		return true
	}
	return false
}

type RevokeReason string

const (
	RevokeLogout          RevokeReason = "LOGOUT"
	RevokeLogoutAll       RevokeReason = "LOGOUT_ALL"
	RevokePasswordChanged RevokeReason = "PASSWORD_CHANGED"
	RevokeTokenReuse      RevokeReason = "TOKEN_REUSE"
	RevokeUserLocked      RevokeReason = "USER_LOCKED"
	RevokeUserDisabled    RevokeReason = "USER_DISABLED"
	RevokeAdminRevoke     RevokeReason = "ADMIN_REVOKE"
)

type FailureReason string

const (
	FailureInvalidCredentials FailureReason = "INVALID_CREDENTIALS"
	FailureRateLimited        FailureReason = "RATE_LIMITED"
	FailureLocked             FailureReason = "LOCKED"
	FailureDisabled           FailureReason = "DISABLED"
	FailureInvalidRequest     FailureReason = "INVALID_REQUEST"
)

type RateBucket string

const (
	BucketLoginEmail RateBucket = "LOGIN_EMAIL"
	BucketLoginIP    RateBucket = "LOGIN_IP"
	BucketRegisterIP RateBucket = "REGISTER_IP"
	BucketRefreshIP  RateBucket = "REFRESH_IP"
)

const (
	MinPasswordRunes = 12
	MaxPasswordRunes = 128
	MinEmailLen      = 3
	MaxEmailLen      = 254
	MaxNameRunes     = 100

	AccessExpiresIn = 900
	AccessTTL       = 15 * time.Minute
	RefreshTTL      = 30 * 24 * time.Hour
	RateWindow      = 15 * time.Minute
	LockAfter       = 10
	LimitLoginEmail = 5
	LimitLoginIP    = 20
	LimitRegisterIP = 10
	LimitRefreshIP  = 60
)

// User is one row of auth.users. PasswordHash is the Argon2id PHC string;
// it must not appear in API results.
type User struct {
	ID               uuid.UUID
	Email            string
	PasswordHash     string
	FirstName        string
	LastName         string
	Status           Status
	FailedLoginCount int
	LockedAt         *time.Time
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

func (u User) Public() PublicUser {
	return PublicUser{
		ID:        u.ID,
		Email:     u.Email,
		FirstName: u.FirstName,
		LastName:  u.LastName,
		Status:    u.Status,
	}
}

// PublicUser is User without PasswordHash.
type PublicUser struct {
	ID        uuid.UUID
	Email     string
	FirstName string
	LastName  string
	Status    Status
	Roles     []Role
}

type Session struct {
	ID           uuid.UUID
	UserID       uuid.UUID
	UserAgent    string
	IP           string
	CreatedAt    time.Time
	LastSeenAt   time.Time
	RevokedAt    *time.Time
	RevokeReason RevokeReason
}

func (s Session) Live() bool { return s.RevokedAt == nil }

type RefreshToken struct {
	ID         uuid.UUID
	SessionID  uuid.UUID
	Hash       []byte
	ReplacedBy *uuid.UUID
	ExpiresAt  time.Time
	UsedAt     *time.Time
	RevokedAt  *time.Time
	CreatedAt  time.Time
}

func (t RefreshToken) Reused() bool {
	return t.UsedAt != nil || t.ReplacedBy != nil || t.RevokedAt != nil
}

func (t RefreshToken) Expired(now time.Time) bool {
	return !t.ExpiresAt.After(now)
}

type UserRole struct {
	UserID    uuid.UUID
	Role      Role
	GrantedBy *uuid.UUID
	GrantedAt time.Time
}

type LoginAttempt struct {
	ID            uuid.UUID
	Email         string
	UserID        *uuid.UUID
	IP            string
	Success       bool
	FailureReason FailureReason
	AttemptedAt   time.Time
}

// UserStatusHistory is one append-only row of auth.user_status_history.
// It records LockUser / UnlockUser / DisableUser only (D-43).
type UserStatusHistory struct {
	ID             uuid.UUID
	UserID         uuid.UUID
	ChangedBy      uuid.UUID
	PreviousStatus Status
	NewStatus      Status
	Reason         string
	CreatedAt      time.Time
}

// Principal is the authenticated actor (identity-auth.md §8).
type Principal struct {
	UserID    uuid.UUID
	SessionID uuid.UUID
	Roles     []Role
}

func (p Principal) Has(r Role) bool {
	for _, got := range p.Roles {
		if got == r {
			return true
		}
	}
	return false
}

func (p Principal) IsActiveCustomer() bool { return p.Has(RoleCustomer) }
