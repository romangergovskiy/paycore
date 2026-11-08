package authcrypto

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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
	if err := requireJWTClaimJSONTypes(token); err != nil {
		return zero, err
	}
	claims, ok := parsed.Claims.(*accessClaims)
	if !ok || !parsed.Valid {
		return zero, domain.ErrInvalidToken
	}
	if claims.Issuer != accessIssuer || claims.Subject == "" || claims.SessionID == "" ||
		claims.ID == "" || claims.Roles == nil || claims.IssuedAt == nil ||
		claims.NotBefore == nil || claims.ExpiresAt == nil {
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
	jti, err := uuid.Parse(claims.ID)
	if err != nil || jti == uuid.Nil {
		return zero, domain.ErrInvalidToken
	}
	roles, err := parseJWTRoles(claims.Roles)
	if err != nil {
		return zero, err
	}
	iat := claims.IssuedAt.Time.UTC()
	nbf := claims.NotBefore.Time.UTC()
	exp := claims.ExpiresAt.Time.UTC()
	if !exp.After(iat) || !exp.After(nbf) || nbf.After(exp) {
		return zero, domain.ErrInvalidToken
	}
	return AccessToken{
		UserID:    uid,
		SessionID: sid,
		Roles:     roles,
		ExpiresAt: exp,
		TokenID:   claims.ID,
	}, nil
}

func requireJWTClaimJSONTypes(token string) error {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return domain.ErrInvalidToken
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return domain.ErrInvalidToken
	}
	var raw map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.UseNumber()
	if err := dec.Decode(&raw); err != nil {
		return domain.ErrInvalidToken
	}
	for _, k := range []string{"iss", "sub", "sid", "jti"} {
		if !jsonIsString(raw[k]) {
			return domain.ErrInvalidToken
		}
	}
	for _, k := range []string{"iat", "nbf", "exp"} {
		if !jsonIsNumber(raw[k]) {
			return domain.ErrInvalidToken
		}
	}
	if !jsonIsStringArray(raw["roles"]) {
		return domain.ErrInvalidToken
	}
	return nil
}

func jsonTrim(b json.RawMessage) []byte {
	return bytes.TrimSpace(b)
}

func jsonIsString(b json.RawMessage) bool {
	b = jsonTrim(b)
	return len(b) >= 2 && b[0] == '"' && b[len(b)-1] == '"'
}

func jsonIsNumber(b json.RawMessage) bool {
	b = jsonTrim(b)
	if len(b) == 0 || b[0] == '"' {
		return false
	}
	var n json.Number
	return json.Unmarshal(b, &n) == nil
}

func jsonIsStringArray(b json.RawMessage) bool {
	b = jsonTrim(b)
	if len(b) == 0 || b[0] != '[' {
		return false
	}
	var elems []json.RawMessage
	if err := json.Unmarshal(b, &elems); err != nil {
		return false
	}
	for _, e := range elems {
		if !jsonIsString(e) {
			return false
		}
	}
	return true
}

func parseJWTRoles(raw []string) ([]domain.Role, error) {
	roles := make([]domain.Role, 0, len(raw))
	seen := make(map[domain.Role]struct{}, len(raw))
	for _, r := range raw {
		role := domain.Role(r)
		if r == "" || !domain.KnownRole(role) {
			return nil, domain.ErrInvalidToken
		}
		if _, dup := seen[role]; dup {
			return nil, domain.ErrInvalidToken
		}
		seen[role] = struct{}{}
		roles = append(roles, role)
	}
	return roles, nil
}
