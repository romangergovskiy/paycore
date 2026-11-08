package authcrypto

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/romangergovskiy/paycore/internal/auth/application"
	"github.com/romangergovskiy/paycore/internal/auth/domain"
)

var _ application.AccessSigner = (*HS256)(nil)

const (
	accessIssuer   = "paycore"
	minSecretBytes = 32
)

// HS256 signs and verifies access JWTs (D-41). Parse is for future HTTP
// middleware; application only uses Sign. Roles in the token are a TTL
// cache — ValidateSession reloads roles from PostgreSQL.
type HS256 struct {
	secret []byte
}

func NewHS256(secret []byte) (*HS256, error) {
	if len(secret) < minSecretBytes {
		return nil, fmt.Errorf("auth: JWT secret must be at least %d bytes", minSecretBytes)
	}
	out := make([]byte, len(secret))
	copy(out, secret)
	return &HS256{secret: out}, nil
}

type accessClaims struct {
	SessionID string   `json:"sid"`
	Roles     []string `json:"roles"`
	jwt.RegisteredClaims
}

func (s *HS256) Sign(p domain.Principal, now, exp time.Time) (string, error) {
	if s == nil || len(s.secret) < minSecretBytes {
		return "", errors.New("auth: JWT signer is not configured")
	}
	if p.UserID == uuid.Nil || p.SessionID == uuid.Nil {
		return "", domain.ErrInvalidRequest
	}
	now = now.UTC()
	exp = exp.UTC()
	jti, err := uuid.NewV7()
	if err != nil {
		jti, err = uuid.NewRandom()
		if err != nil {
			return "", fmt.Errorf("auth: jwt id: %w", err)
		}
	}
	roles := make([]string, 0, len(p.Roles))
	for _, r := range p.Roles {
		roles = append(roles, string(r))
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, accessClaims{
		SessionID: p.SessionID.String(),
		Roles:     roles,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    accessIssuer,
			Subject:   p.UserID.String(),
			ID:        jti.String(),
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(exp),
		},
	})
	signed, err := tok.SignedString(s.secret)
	if err != nil {
		return "", fmt.Errorf("auth: sign access jwt: %w", err)
	}
	return signed, nil
}

// AccessToken is the verified JWT payload. Roles are not authorization.
type AccessToken struct {
	UserID    uuid.UUID
	SessionID uuid.UUID
	Roles     []domain.Role
	ExpiresAt time.Time
	TokenID   string
}

func (s *HS256) Parse(token string) (AccessToken, error) {
	var zero AccessToken
	if s == nil || len(s.secret) < minSecretBytes {
		return zero, errors.New("auth: JWT signer is not configured")
	}
	parsed, err := jwt.ParseWithClaims(token, &accessClaims{}, func(t *jwt.Token) (any, error) {
		if t.Method != jwt.SigningMethodHS256 {
			return nil, domain.ErrInvalidToken
		}
		return s.secret, nil
	},
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(accessIssuer),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
	)
	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return zero, domain.ErrTokenExpired
		}
		return zero, domain.ErrInvalidToken
	}
	claims, ok := parsed.Claims.(*accessClaims)
	if !ok || !parsed.Valid {
		return zero, domain.ErrInvalidToken
	}
	uid, err := uuid.Parse(claims.Subject)
	if err != nil || uid == uuid.Nil {
		return zero, domain.ErrInvalidToken
	}
	sid, err := uuid.Parse(claims.SessionID)
	if err != nil || sid == uuid.Nil {
		return zero, domain.ErrInvalidToken
	}
	roles := make([]domain.Role, 0, len(claims.Roles))
	for _, r := range claims.Roles {
		roles = append(roles, domain.Role(r))
	}
	exp := time.Time{}
	if claims.ExpiresAt != nil {
		exp = claims.ExpiresAt.Time.UTC()
	}
	return AccessToken{
		UserID:    uid,
		SessionID: sid,
		Roles:     roles,
		ExpiresAt: exp,
		TokenID:   claims.ID,
	}, nil
}
