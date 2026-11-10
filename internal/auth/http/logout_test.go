package authhttp

import (
	"context"
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

const logoutJWTSecret = "0123456789abcdef0123456789abcdef"

type logoutSessions struct {
	mu     sync.Mutex
	live   map[uuid.UUID]domain.Principal
	calls  []domain.Principal
	logout func(domain.Principal) error
}

func newLogoutSessions(ps ...domain.Principal) *logoutSessions {
	s := &logoutSessions{live: make(map[uuid.UUID]domain.Principal, len(ps))}
	for _, p := range ps {
		s.live[p.SessionID] = p
	}
	return s
}

func (s *logoutSessions) ValidateSession(_ context.Context, sessionID uuid.UUID) (domain.Principal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.live[sessionID]
	if !ok {
		return domain.Principal{}, domain.ErrInvalidToken
	}
	return p, nil
}

func (s *logoutSessions) revoke(sessionID uuid.UUID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.live, sessionID)
}

func (s *logoutSessions) handleLogout(_ *http.Request, p domain.Principal) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, p)
	if s.logout != nil {
		return s.logout(p)
	}
	cur, ok := s.live[p.SessionID]
	if !ok || cur.UserID != p.UserID {
		// Already revoked: application Logout commits success.
		if !ok {
			return nil
		}
		return domain.ErrInvalidToken
	}
	delete(s.live, p.SessionID)
	return nil
}

func (s *logoutSessions) principals() []domain.Principal {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]domain.Principal, len(s.calls))
	copy(out, s.calls)
	return out
}

func logoutPrincipal(user, session string, roles ...domain.Role) domain.Principal {
	if len(roles) == 0 {
		roles = []domain.Role{domain.RoleCustomer}
	}
	return domain.Principal{
		UserID:    uuid.MustParse(user),
		SessionID: uuid.MustParse(session),
		Roles:     roles,
	}
}

func logoutSigner(t *testing.T) *authcrypto.HS256 {
	t.Helper()
	s, err := authcrypto.NewHS256([]byte(logoutJWTSecret))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func signLogout(t *testing.T, s *authcrypto.HS256, p domain.Principal) string {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	tok, err := s.Sign(p, now, now.Add(15*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func mountLogout(t *testing.T, sessions *logoutSessions) Handler {
	t.Helper()
	h := newTestHandler()
	h.parser = logoutSigner(t)
	h.sessions = sessions
	h.logout = sessions.handleLogout
	return h
}

func postLogout(t *testing.T, h Handler, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	hdr := http.Header{}
	if token != "" {
		hdr.Set("Authorization", "Bearer "+token)
	}
	return serveAuth(t, h, http.MethodPost, "/api/v1/auth/logout", body, hdr)
}

func assertLogout401(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	e := decodeErr(t, rec)
	if rec.Code != http.StatusUnauthorized || e.Error.Code != domain.CodeInvalidToken {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	if rec.Header().Get("WWW-Authenticate") != "Bearer" {
		t.Fatalf("WWW-Authenticate = %q", rec.Header().Get("WWW-Authenticate"))
	}
	raw := rec.Body.String()
	for _, leak := range []string{"pgx", "sql:", "session_id", "jwt:", "TOKEN_EXPIRED", "panic"} {
		if strings.Contains(raw, leak) {
			t.Errorf("response leaked %q: %s", leak, raw)
		}
	}
}

func TestLogoutSuccess(t *testing.T) {
	p := logoutPrincipal("11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222")
	sessions := newLogoutSessions(p)
	h := mountLogout(t, sessions)
	tok := signLogout(t, logoutSigner(t), p)

	rec := postLogout(t, h, tok, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d %s", rec.Code, rec.Body)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("204 body must be empty: %q", rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "" {
		t.Errorf("Content-Type = %q, want empty", ct)
	}
	calls := sessions.principals()
	if len(calls) != 1 || calls[0].UserID != p.UserID || calls[0].SessionID != p.SessionID {
		t.Fatalf("logout calls = %+v", calls)
	}
}

func TestLogoutMissingBearer(t *testing.T) {
	p := logoutPrincipal("11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222")
	h := mountLogout(t, newLogoutSessions(p))
	rec := postLogout(t, h, "", "")
	assertLogout401(t, rec)
	if len(h.sessions.(*logoutSessions).principals()) != 0 {
		t.Fatal("logout must not run without Bearer")
	}
}

func TestLogoutInvalidBearer(t *testing.T) {
	p := logoutPrincipal("11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222")
	sessions := newLogoutSessions(p)
	h := mountLogout(t, sessions)
	rec := serveAuth(t, h, http.MethodPost, "/api/v1/auth/logout", "", http.Header{
		"Authorization": {"Bearer not-a-jwt"},
	})
	assertLogout401(t, rec)
	if len(sessions.principals()) != 0 {
		t.Fatal("logout must not run for invalid Bearer")
	}
}

func TestLogoutRevokedSession(t *testing.T) {
	p := logoutPrincipal("11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222")
	sessions := newLogoutSessions(p)
	sessions.revoke(p.SessionID)
	h := mountLogout(t, sessions)
	tok := signLogout(t, logoutSigner(t), p)
	rec := postLogout(t, h, tok, "")
	assertLogout401(t, rec)
	if len(sessions.principals()) != 0 {
		t.Fatal("logout must not run for revoked session")
	}
}

func TestLogoutUsesPrincipalNotBody(t *testing.T) {
	p := logoutPrincipal("11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222")
	sessions := newLogoutSessions(p)
	h := mountLogout(t, sessions)
	tok := signLogout(t, logoutSigner(t), p)
	body := `{"user_id":"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa","session_id":"bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"}`
	rec := postLogout(t, h, tok, body)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d %s", rec.Code, rec.Body)
	}
	calls := sessions.principals()
	if len(calls) != 1 {
		t.Fatalf("calls = %d", len(calls))
	}
	if calls[0].UserID != p.UserID || calls[0].SessionID != p.SessionID {
		t.Fatalf("handler used body IDs: %+v", calls[0])
	}
}

func TestLogoutRepeatMatchesApplication(t *testing.T) {
	p := logoutPrincipal("11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222")
	sessions := newLogoutSessions(p)
	// Keep ValidateSession succeeding so the handler can run twice, matching
	// application Logout (repeat is Commit/nil once the session is not live).
	sessions.logout = func(domain.Principal) error { return nil }
	h := mountLogout(t, sessions)
	tok := signLogout(t, logoutSigner(t), p)

	first := postLogout(t, h, tok, "")
	second := postLogout(t, h, tok, "")
	if first.Code != http.StatusNoContent || second.Code != http.StatusNoContent {
		t.Fatalf("repeat HTTP = %d / %d", first.Code, second.Code)
	}
	if first.Body.Len() != 0 || second.Body.Len() != 0 {
		t.Fatalf("repeat bodies = %q / %q", first.Body, second.Body)
	}
	if n := len(sessions.principals()); n != 2 {
		t.Fatalf("logout calls = %d, want 2", n)
	}
}

func TestLogoutRepeatAfterRevokeIs401(t *testing.T) {
	p := logoutPrincipal("11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222")
	sessions := newLogoutSessions(p)
	h := mountLogout(t, sessions)
	tok := signLogout(t, logoutSigner(t), p)

	first := postLogout(t, h, tok, "")
	if first.Code != http.StatusNoContent {
		t.Fatalf("first = %d %s", first.Code, first.Body)
	}
	second := postLogout(t, h, tok, "")
	assertLogout401(t, second)
	if n := len(sessions.principals()); n != 1 {
		t.Fatalf("logout calls = %d, want 1 (BearerAuth stops the repeat)", n)
	}
}

func TestLogoutDoesNotAffectOtherSessions(t *testing.T) {
	a := logoutPrincipal("11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222")
	b := logoutPrincipal("11111111-1111-1111-1111-111111111111", "33333333-3333-3333-3333-333333333333")
	sessions := newLogoutSessions(a, b)
	h := mountLogout(t, sessions)
	signer := logoutSigner(t)

	rec := postLogout(t, h, signLogout(t, signer, a), "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("logout A = %d %s", rec.Code, rec.Body)
	}
	rec = postLogout(t, h, signLogout(t, signer, b), "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("logout B after A = %d %s", rec.Code, rec.Body)
	}
	calls := sessions.principals()
	if len(calls) != 2 || calls[0].SessionID != a.SessionID || calls[1].SessionID != b.SessionID {
		t.Fatalf("calls = %+v", calls)
	}
}

func TestLogoutNotMountedWithoutBearerDeps(t *testing.T) {
	rec := serveAuth(t, newTestHandler(), http.MethodPost, "/api/v1/auth/logout", "", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestLogoutInternalErrorNotLeaked(t *testing.T) {
	p := logoutPrincipal("11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222")
	sessions := newLogoutSessions(p)
	sessions.logout = func(domain.Principal) error {
		return errors.New("pgx: session hash=deadbeef")
	}
	h := mountLogout(t, sessions)
	rec := postLogout(t, h, signLogout(t, logoutSigner(t), p), "")
	e := decodeErr(t, rec)
	if rec.Code != http.StatusInternalServerError || e.Error.Code != apperror.CodeInternal {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	raw := rec.Body.String()
	if strings.Contains(raw, "pgx") || strings.Contains(raw, "deadbeef") || strings.Contains(raw, "hash=") {
		t.Fatalf("internal error leaked: %s", raw)
	}
}
