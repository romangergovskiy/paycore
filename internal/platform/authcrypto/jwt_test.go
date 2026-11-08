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
		"jti":   uuid.MustParse("33333333-3333-3333-3333-333333333333").String(),
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
	hs512Header, _ := json.Marshal(map[string]string{"alg": "HS512", "typ": "JWT"})
	hs512Head := base64.RawURLEncoding.EncodeToString(hs512Header)
	mac512 := hmac.New(sha256.New, s.secret)
	mac512.Write([]byte(hs512Head + "." + payload))
	hs512Tok := hs512Head + "." + payload + "." + base64.RawURLEncoding.EncodeToString(mac512.Sum(nil))
	if _, err := s.Parse(hs512Tok); !errors.Is(err, domain.ErrInvalidToken) {
		t.Fatalf("alg HS512: %v", err)
	}
}

func TestHS256RequiredClaims(t *testing.T) {
	s := testSigner(t)
	now := time.Now().UTC().Truncate(time.Second)
	valid := map[string]any{
		"iss":   "paycore",
		"sub":   testPrincipal().UserID.String(),
		"sid":   testPrincipal().SessionID.String(),
		"roles": []string{"CUSTOMER"},
		"iat":   now.Unix(),
		"nbf":   now.Unix(),
		"exp":   now.Add(15 * time.Minute).Unix(),
		"jti":   uuid.MustParse("33333333-3333-3333-3333-333333333333").String(),
	}
	tok := signHS256(t, jwtPayload(valid), s.secret)
	if _, err := s.Parse(tok); err != nil {
		t.Fatalf("valid: %v", err)
	}

	omit := func(key string) {
		t.Helper()
		c := cloneClaims(valid)
		delete(c, key)
		if _, err := s.Parse(signHS256(t, jwtPayload(c), s.secret)); !errors.Is(err, domain.ErrInvalidToken) {
			t.Fatalf("missing %s: %v", key, err)
		}
	}
	for _, key := range []string{"iss", "sub", "sid", "roles", "iat", "nbf", "exp", "jti"} {
		omit(key)
	}

	bad := []map[string]any{
		{"sub": "not-a-uuid"},
		{"sid": "not-a-uuid"},
		{"jti": "not-a-uuid"},
		{"sub": uuid.Nil.String()},
		{"sid": uuid.Nil.String()},
		{"jti": uuid.Nil.String()},
		{"roles": []string{""}},
		{"roles": []string{"HACKER"}},
		{"roles": []string{"CUSTOMER", "CUSTOMER"}},
		{"roles": "CUSTOMER"},
		{"sub": 123},
		{"iat": "1762570000"},
		{"iat": now.Add(time.Hour).Unix()},
		{"nbf": now.Add(20 * time.Minute).Unix()},
	}
	for i, patch := range bad {
		c := cloneClaims(valid)
		for k, v := range patch {
			c[k] = v
		}
		if _, err := s.Parse(signHS256(t, jwtPayload(c), s.secret)); !errors.Is(err, domain.ErrInvalidToken) {
			t.Fatalf("case %d %+v: %v", i, patch, err)
		}
	}

	emptyRoles := cloneClaims(valid)
	emptyRoles["roles"] = []string{}
	if _, err := s.Parse(signHS256(t, jwtPayload(emptyRoles), s.secret)); err != nil {
		t.Fatalf("empty roles: %v", err)
	}
}

func cloneClaims(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
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
