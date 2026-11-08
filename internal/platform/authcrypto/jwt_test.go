package authcrypto

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/romangergovskiy/paycore/internal/auth/domain"
)

func testSigner(t *testing.T) *HS256 {
	t.Helper()
	s, err := NewHS256([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func testPrincipal() domain.Principal {
	return domain.Principal{
		UserID:    uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		SessionID: uuid.MustParse("22222222-2222-2222-2222-222222222222"),
		Roles:     []domain.Role{domain.RoleCustomer},
	}
}

func TestHS256SignParse(t *testing.T) {
	s := testSigner(t)
	now := time.Now().UTC().Truncate(time.Second)
	exp := now.Add(15 * time.Minute)
	tok, err := s.Sign(testPrincipal(), now, exp)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Parse(tok)
	if err != nil {
		t.Fatal(err)
	}
	p := testPrincipal()
	if got.UserID != p.UserID || got.SessionID != p.SessionID {
		t.Fatalf("ids: %+v", got)
	}
	if len(got.Roles) != 1 || got.Roles[0] != domain.RoleCustomer {
		t.Fatalf("roles: %v", got.Roles)
	}
	if got.ExpiresAt.UTC() != exp {
		t.Fatalf("exp: %v", got.ExpiresAt)
	}
	if got.TokenID == "" {
		t.Fatal("missing jti")
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("jwt parts: %d", len(parts))
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"sub", "sid", "roles", "iat", "nbf", "exp", "jti", "iss"} {
		if _, ok := claims[k]; !ok {
			t.Fatalf("missing claim %s", k)
		}
	}
	if claims["iss"] != "paycore" {
		t.Fatalf("iss=%v", claims["iss"])
	}
}

func TestHS256RejectsShortSecret(t *testing.T) {
	if _, err := NewHS256([]byte("short")); err == nil {
		t.Fatal("expected error")
	}
}

func TestHS256WrongSecret(t *testing.T) {
	s := testSigner(t)
	now := time.Now().UTC()
	tok, err := s.Sign(testPrincipal(), now, now.Add(15*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	other, err := NewHS256([]byte("ffffffffffffffffffffffffffffffff"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Parse(tok); !errors.Is(err, domain.ErrInvalidToken) {
		t.Fatalf("got %v", err)
	}
}

func TestHS256WrongIssuer(t *testing.T) {
	s := testSigner(t)
	now := time.Now().UTC().Truncate(time.Second)
	exp := now.Add(15 * time.Minute)
	payload := jwtPayload(map[string]any{
		"iss":   "other",
		"sub":   testPrincipal().UserID.String(),
		"sid":   testPrincipal().SessionID.String(),
		"roles": []string{"CUSTOMER"},
		"iat":   now.Unix(),
		"nbf":   now.Unix(),
		"exp":   exp.Unix(),
		"jti":   uuid.MustParse("33333333-3333-3333-3333-333333333333").String(),
	})
	tok := signHS256(t, payload, s.secret)
	if _, err := s.Parse(tok); !errors.Is(err, domain.ErrInvalidToken) {
		t.Fatalf("got %v", err)
	}
}

func TestHS256RejectsOtherAlg(t *testing.T) {
	s := testSigner(t)
	now := time.Now().UTC().Truncate(time.Second)
	claims := map[string]any{
		"iss":   "paycore",
		"sub":   testPrincipal().UserID.String(),
		"sid":   testPrincipal().SessionID.String(),
		"roles": []string{"CUSTOMER"},
		"iat":   now.Unix(),
		"nbf":   now.Unix(),
		"exp":   now.Add(15 * time.Minute).Unix(),
		"jti":   "jti",
	}
	payload := jwtPayload(claims)
	noneHeader, _ := json.Marshal(map[string]string{"alg": "none", "typ": "JWT"})
	noneTok := base64.RawURLEncoding.EncodeToString(noneHeader) + "." + payload + "."
	if _, err := s.Parse(noneTok); !errors.Is(err, domain.ErrInvalidToken) {
		t.Fatalf("alg none: %v", err)
	}
	hs384Header, _ := json.Marshal(map[string]string{"alg": "HS384", "typ": "JWT"})
	head := base64.RawURLEncoding.EncodeToString(hs384Header)
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(head + "." + payload))
	hs384Tok := head + "." + payload + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if _, err := s.Parse(hs384Tok); !errors.Is(err, domain.ErrInvalidToken) {
		t.Fatalf("alg HS384: %v", err)
	}
}

func TestHS256ExpiredAndNBF(t *testing.T) {
	s := testSigner(t)
	p := testPrincipal()
	now := time.Now().UTC()
	tok, err := s.Sign(p, now.Add(-30*time.Minute), now.Add(-15*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Parse(tok); !errors.Is(err, domain.ErrTokenExpired) {
		t.Fatalf("expired: %v", err)
	}
	future := now.Add(time.Hour)
	tok, err = s.Sign(p, future, future.Add(15*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Parse(tok); !errors.Is(err, domain.ErrInvalidToken) {
		t.Fatalf("nbf: %v", err)
	}
}

func TestHS256Malformed(t *testing.T) {
	s := testSigner(t)
	for _, tok := range []string{"", "a.b", "not-a-jwt", "a.b.c.d"} {
		if _, err := s.Parse(tok); !errors.Is(err, domain.ErrInvalidToken) {
			t.Fatalf("%q: %v", tok, err)
		}
	}
}

func jwtPayload(claims map[string]any) string {
	b, err := json.Marshal(claims)
	if err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func signHS256(t *testing.T, payloadB64 string, secret []byte) string {
	t.Helper()
	header, _ := json.Marshal(map[string]string{"alg": "HS256", "typ": "JWT"})
	head := base64.RawURLEncoding.EncodeToString(header)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(head + "." + payloadB64))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return head + "." + payloadB64 + "." + sig
}
