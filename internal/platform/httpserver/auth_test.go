package httpserver

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/romangergovskiy/paycore/internal/auth/domain"
	"github.com/romangergovskiy/paycore/internal/platform/apperror"
	"github.com/romangergovskiy/paycore/internal/platform/authcrypto"
)

const testJWTSecret = "0123456789abcdef0123456789abcdef"

type fakeSessions struct {
	mu   sync.Mutex
	byID map[uuid.UUID]domain.Principal
	err  error
}

func newFakeSessions(ps ...domain.Principal) *fakeSessions {
	f := &fakeSessions{byID: make(map[uuid.UUID]domain.Principal, len(ps))}
	for _, p := range ps {
		f.byID[p.SessionID] = p
	}
	return f
}

func (f *fakeSessions) ValidateSession(_ context.Context, sessionID uuid.UUID) (domain.Principal, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return domain.Principal{}, f.err
	}
	p, ok := f.byID[sessionID]
	if !ok {
		return domain.Principal{}, domain.ErrInvalidToken
	}
	return p, nil
}

func (f *fakeSessions) setRoles(userID uuid.UUID, roles []domain.Role) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for sid, p := range f.byID {
		if p.UserID == userID {
			cp := p
			cp.Roles = append([]domain.Role(nil), roles...)
			f.byID[sid] = cp
		}
	}
}

func (f *fakeSessions) revoke(sessionID uuid.UUID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.byID, sessionID)
}

func testPrincipal(roles ...domain.Role) domain.Principal {
	if len(roles) == 0 {
		roles = []domain.Role{domain.RoleCustomer}
	}
	return domain.Principal{
		UserID:    uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		SessionID: uuid.MustParse("22222222-2222-2222-2222-222222222222"),
		Roles:     roles,
	}
}

func testSigner(t *testing.T) *authcrypto.HS256 {
	t.Helper()
	s, err := authcrypto.NewHS256([]byte(testJWTSecret))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func signAccess(t *testing.T, s *authcrypto.HS256, p domain.Principal, exp time.Time) string {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	if exp.Before(now) {
		tok, err := s.Sign(p, exp.Add(-15*time.Minute), exp)
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	tok, err := s.Sign(p, now, exp)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func okHandler(t *testing.T) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := PrincipalFromContext(r.Context())
		if !ok {
			t.Error("handler ran without Principal")
			http.Error(w, "missing principal", http.StatusInternalServerError)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"user_id":    p.UserID.String(),
			"session_id": p.SessionID.String(),
			"roles":      p.Roles,
		})
	})
}

func serveBearer(t *testing.T, h http.Handler, auth string) *httptest.ResponseRecorder {
	t.Helper()
	s, _ := newTestServer(t, testConfig())
	s.Handle("GET /protected", h)
	hdr := http.Header{}
	if auth != "" {
		hdr.Set("Authorization", auth)
	}
	return serve(s, http.MethodGet, "/protected", nil, hdr)
}

func assertInvalidToken401(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	e := decodeError(t, rec)
	if rec.Code != http.StatusUnauthorized || e.Code != domain.CodeInvalidToken {
		t.Fatalf("got %d %+v body=%s", rec.Code, e, rec.Body)
	}
	if e.Message != domain.ErrInvalidToken.Message {
		t.Errorf("message = %q", e.Message)
	}
	if rec.Header().Get("WWW-Authenticate") != "Bearer" {
		t.Errorf("WWW-Authenticate = %q", rec.Header().Get("WWW-Authenticate"))
	}
	if strings.Contains(rec.Header().Get("WWW-Authenticate"), "error") ||
		strings.Contains(rec.Header().Get("WWW-Authenticate"), " ") {
		t.Errorf("WWW-Authenticate must be exactly Bearer: %q", rec.Header().Get("WWW-Authenticate"))
	}
	raw := rec.Body.String()
	for _, leak := range []string{"token is expired", "Token has expired.", "jwt:", "signature is invalid", "TOKEN_EXPIRED"} {
		if strings.Contains(raw, leak) {
			t.Errorf("JWT error leaked: %s", raw)
		}
	}
}

func TestBearerMissingAndBadAuthorization(t *testing.T) {
	s := testSigner(t)
	p := testPrincipal()
	sessions := newFakeSessions(p)
	h := BearerAuth(s, sessions)(okHandler(t))

	for _, auth := range []string{
		"",
		"Basic abc",
		"Bearer",
		"Bearer ",
		"Bearer  extra-space",
	} {
		rec := serveBearer(t, h, auth)
		assertInvalidToken401(t, rec)
	}
}

func TestBearerBadJWTWrongIssExpiredBadSigRevoked(t *testing.T) {
	s := testSigner(t)
	p := testPrincipal()
	sessions := newFakeSessions(p)
	h := BearerAuth(s, sessions)(okHandler(t))
	now := time.Now().UTC().Truncate(time.Second)

	badJWT := serveBearer(t, h, "Bearer not-a-jwt")
	assertInvalidToken401(t, badJWT)

	wrongIss := signRaw(t, []byte(testJWTSecret), map[string]any{
		"iss":   "other",
		"sub":   p.UserID.String(),
		"sid":   p.SessionID.String(),
		"roles": []string{"CUSTOMER"},
		"iat":   now.Unix(),
		"nbf":   now.Unix(),
		"exp":   now.Add(15 * time.Minute).Unix(),
		"jti":   uuid.MustParse("33333333-3333-3333-3333-333333333333").String(),
	})
	rec := serveBearer(t, h, "Bearer "+wrongIss)
	assertInvalidToken401(t, rec)

	expired := signAccess(t, s, p, now.Add(-time.Minute))
	rec = serveBearer(t, h, "Bearer "+expired)
	assertInvalidToken401(t, rec)

	live := signAccess(t, s, p, now.Add(15*time.Minute))
	other, err := authcrypto.NewHS256([]byte("ffffffffffffffffffffffffffffffff"))
	if err != nil {
		t.Fatal(err)
	}
	badSig, err := other.Sign(p, now, now.Add(15*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	rec = serveBearer(t, h, "Bearer "+badSig)
	assertInvalidToken401(t, rec)

	rec = serveBearer(t, h, "Bearer "+live)
	if rec.Code != http.StatusOK {
		t.Fatalf("live token before revoke: %d %s", rec.Code, rec.Body)
	}
	sessions.revoke(p.SessionID)
	rec = serveBearer(t, h, "Bearer "+live)
	assertInvalidToken401(t, rec)
}

func TestBearerSuccessPutsStorePrincipal(t *testing.T) {
	s := testSigner(t)
	store := testPrincipal(domain.RoleCustomer)
	jwtP := testPrincipal(domain.RoleAdmin)
	sessions := newFakeSessions(store)
	now := time.Now().UTC()
	tok, err := s.Sign(jwtP, now, now.Add(15*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	h := BearerAuth(s, sessions)(okHandler(t))
	rec := serveBearer(t, h, "Bearer "+tok)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), `"ADMIN"`) {
		t.Fatalf("JWT roles used as authorization: %s", rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"CUSTOMER"`) {
		t.Fatalf("store roles missing: %s", rec.Body)
	}
}

func TestRequireRoleNoPrincipal(t *testing.T) {
	s, _ := newTestServer(t, testConfig())
	s.Handle("GET /admin", RequireRole(domain.RoleAdmin)(okHandler(t)))
	rec := serve(s, http.MethodGet, "/admin", nil, nil)
	assertInvalidToken401(t, rec)
}

func TestRequireRoleCustomerForbiddenAdminAllowed(t *testing.T) {
	s := testSigner(t)
	now := time.Now().UTC()

	customer := testPrincipal(domain.RoleCustomer)
	custTok, err := s.Sign(customer, now, now.Add(15*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	h := BearerAuth(s, newFakeSessions(customer))(RequireRole(domain.RoleAdmin)(okHandler(t)))
	rec := serveBearer(t, h, "Bearer "+custTok)
	e := decodeError(t, rec)
	if rec.Code != http.StatusForbidden || e.Code != domain.CodeForbidden {
		t.Fatalf("customer vs ADMIN: %d %+v", rec.Code, e)
	}
	if rec.Header().Get("WWW-Authenticate") != "" {
		t.Errorf("403 must not send WWW-Authenticate: %q", rec.Header().Get("WWW-Authenticate"))
	}

	admin := testPrincipal(domain.RoleAdmin)
	adminTok, err := s.Sign(admin, now, now.Add(15*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	h = BearerAuth(s, newFakeSessions(admin))(RequireRole(domain.RoleAdmin)(okHandler(t)))
	rec = serveBearer(t, h, "Bearer "+adminTok)
	if rec.Code != http.StatusOK {
		t.Fatalf("ADMIN allowed: %d %s", rec.Code, rec.Body)
	}
}

func TestRequireRoleUsesStoreNotJWT(t *testing.T) {
	s := testSigner(t)
	now := time.Now().UTC()
	store := testPrincipal(domain.RoleCustomer)
	jwtAdmin := testPrincipal(domain.RoleAdmin)
	sessions := newFakeSessions(store)
	tok, err := s.Sign(jwtAdmin, now, now.Add(15*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	h := BearerAuth(s, sessions)(RequireRole(domain.RoleAdmin)(okHandler(t)))

	rec := serveBearer(t, h, "Bearer "+tok)
	e := decodeError(t, rec)
	if rec.Code != http.StatusForbidden || e.Code != domain.CodeForbidden {
		t.Fatalf("JWT ADMIN / store CUSTOMER: %d %+v", rec.Code, e)
	}

	sessions.setRoles(store.UserID, []domain.Role{domain.RoleCustomer, domain.RoleAdmin})
	rec = serveBearer(t, h, "Bearer "+tok)
	if rec.Code != http.StatusOK {
		t.Fatalf("after Grant ADMIN: %d %s", rec.Code, rec.Body)
	}

	sessions.setRoles(store.UserID, []domain.Role{domain.RoleCustomer})
	rec = serveBearer(t, h, "Bearer "+tok)
	e = decodeError(t, rec)
	if rec.Code != http.StatusForbidden || e.Code != domain.CodeForbidden {
		t.Fatalf("after Revoke ADMIN: %d %+v", rec.Code, e)
	}
}

func TestPrincipalFromContextMissing(t *testing.T) {
	p, ok := PrincipalFromContext(context.Background())
	if ok || p.UserID != uuid.Nil {
		t.Fatalf("empty context: %+v ok=%v", p, ok)
	}
}

func TestBearerValidateSessionDeadlineExceeded(t *testing.T) {
	s := testSigner(t)
	p := testPrincipal()
	sessions := newFakeSessions(p)
	sessions.err = context.DeadlineExceeded
	tok := signAccess(t, s, p, time.Now().UTC().Add(15*time.Minute))
	h := BearerAuth(s, sessions)(okHandler(t))
	rec := serveBearer(t, h, "Bearer "+tok)
	e := decodeError(t, rec)
	if rec.Code != http.StatusServiceUnavailable || e.Code != apperror.CodeRequestTimeout {
		t.Fatalf("got %d %+v body=%s", rec.Code, e, rec.Body)
	}
	if rec.Header().Get("WWW-Authenticate") != "" {
		t.Errorf("503 must not send WWW-Authenticate: %q", rec.Header().Get("WWW-Authenticate"))
	}
}

func TestBearerValidateSessionInfraErrorHidesCause(t *testing.T) {
	s := testSigner(t)
	p := testPrincipal()
	sessions := newFakeSessions(p)
	sessions.err = errors.New("connection reset by postgres host=db password=supersecret")
	tok := signAccess(t, s, p, time.Now().UTC().Add(15*time.Minute))
	h := BearerAuth(s, sessions)(okHandler(t))
	rec := serveBearer(t, h, "Bearer "+tok)
	e := decodeError(t, rec)
	if rec.Code != http.StatusInternalServerError || e.Code != apperror.CodeInternal {
		t.Fatalf("got %d %+v body=%s", rec.Code, e, rec.Body)
	}
	raw := rec.Body.String()
	for _, secret := range []string{"postgres", "supersecret", "connection reset", "password"} {
		if strings.Contains(raw, secret) {
			t.Errorf("infra error leaked %q: %s", secret, raw)
		}
	}
	if rec.Header().Get("WWW-Authenticate") != "" {
		t.Errorf("500 must not send WWW-Authenticate: %q", rec.Header().Get("WWW-Authenticate"))
	}
}

func signRaw(t *testing.T, secret []byte, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	header, _ := json.Marshal(map[string]string{"alg": "HS256", "typ": "JWT"})
	head := base64.RawURLEncoding.EncodeToString(header)
	body := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(head + "." + body))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return head + "." + body + "." + sig
}
