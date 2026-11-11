package authhttp

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/romangergovskiy/paycore/internal/auth/application"
	"github.com/romangergovskiy/paycore/internal/auth/domain"
	"github.com/romangergovskiy/paycore/internal/platform/config"
	"github.com/romangergovskiy/paycore/internal/platform/httpserver"
)

func mountSelfService(t *testing.T, sessions *logoutSessions) Handler {
	t.Helper()
	h := mountLogout(t, sessions)
	h.currentUser = func(_ *http.Request, p domain.Principal) (domain.PublicUser, error) {
		return domain.PublicUser{
			ID:        p.UserID,
			Email:     "ada@example.com",
			FirstName: "Ada",
			LastName:  "Lovelace",
			Status:    domain.StatusActive,
			Roles:     []domain.Role{domain.RoleCustomer},
		}, nil
	}
	h.logoutAll = sessions.handleLogout
	h.changePassword = func(*http.Request, application.ChangePasswordCommand) error { return nil }
	h.listSessions = func(_ *http.Request, p domain.Principal) ([]domain.Session, error) {
		now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
		other := uuid.MustParse("33333333-3333-3333-3333-333333333333")
		return []domain.Session{
			{
				ID:         p.SessionID,
				UserID:     p.UserID,
				UserAgent:  "PayCoreTest/1",
				IP:         "203.0.113.10",
				CreatedAt:  now,
				LastSeenAt: now,
			},
			{
				ID:         other,
				UserID:     p.UserID,
				UserAgent:  "other",
				IP:         "198.51.100.8",
				CreatedAt:  now.Add(-time.Hour),
				LastSeenAt: now.Add(-time.Minute),
			},
		}, nil
	}
	h.revokeSession = func(*http.Request, application.RevokeSessionCommand) error { return nil }
	return h
}

func bearerFor(t *testing.T, p domain.Principal) (Handler, *logoutSessions, string) {
	t.Helper()
	sessions := newLogoutSessions(p)
	h := mountSelfService(t, sessions)
	return h, sessions, signLogout(t, logoutSigner(t), p)
}

func TestMeSuccessShape(t *testing.T) {
	p := logoutPrincipal("11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222", domain.RoleAdmin)
	h, _, tok := bearerFor(t, p)
	h.currentUser = func(_ *http.Request, got domain.Principal) (domain.PublicUser, error) {
		if got.UserID != p.UserID || got.SessionID != p.SessionID {
			t.Fatalf("principal = %+v", got)
		}
		return domain.PublicUser{
			ID:        p.UserID,
			Email:     "ada@example.com",
			FirstName: "Ada",
			LastName:  "Lovelace",
			Status:    domain.StatusActive,
			Roles:     []domain.Role{domain.RoleCustomer},
		}, nil
	}

	rec := serveAuth(t, h, http.MethodGet, "/api/v1/auth/me", "", http.Header{
		"Authorization": {"Bearer " + tok},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s", rec.Code, rec.Body)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["id"] != p.UserID.String() || body["email"] != "ada@example.com" ||
		body["first_name"] != "Ada" || body["last_name"] != "Lovelace" || body["status"] != "ACTIVE" {
		t.Fatalf("me = %#v", body)
	}
	roles, _ := body["roles"].([]any)
	if len(roles) != 1 || roles[0] != "CUSTOMER" {
		t.Fatalf("roles = %#v", body["roles"])
	}
	for _, leak := range []string{"password", "password_hash", "session_id", "failed_login_count", "access_token", "refresh_token"} {
		if _, ok := body[leak]; ok {
			t.Errorf("me leaked %s", leak)
		}
		if strings.Contains(rec.Body.String(), leak) && leak != "session_id" {
			t.Errorf("body mentioned %s: %s", leak, rec.Body)
		}
	}
	if _, ok := body["user"]; ok {
		t.Fatal("/me must be the profile, not a login envelope")
	}
	if strings.Contains(rec.Body.String(), "ADMIN") {
		t.Fatal("/me must use store roles, not JWT/Principal.Roles")
	}
	if rec.Header().Get("WWW-Authenticate") != "" {
		t.Fatal("200 must not send WWW-Authenticate")
	}
}

func TestMeUnauthorized(t *testing.T) {
	p := logoutPrincipal("11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222")
	h, sessions, tok := bearerFor(t, p)

	missing := serveAuth(t, h, http.MethodGet, "/api/v1/auth/me", "", nil)
	assertLogout401(t, missing)

	sessions.revoke(p.SessionID)
	after := serveAuth(t, h, http.MethodGet, "/api/v1/auth/me", "", http.Header{
		"Authorization": {"Bearer " + tok},
	})
	assertLogout401(t, after)
}

func TestMeAfterLogoutIs401(t *testing.T) {
	p := logoutPrincipal("11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222")
	h, _, tok := bearerFor(t, p)
	hdr := http.Header{"Authorization": {"Bearer " + tok}}

	out := serveAuth(t, h, http.MethodPost, "/api/v1/auth/logout", "", hdr)
	if out.Code != http.StatusNoContent {
		t.Fatalf("logout = %d %s", out.Code, out.Body)
	}
	me := serveAuth(t, h, http.MethodGet, "/api/v1/auth/me", "", hdr)
	assertLogout401(t, me)
}

func TestLogoutAllSuccess(t *testing.T) {
	p := logoutPrincipal("11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222")
	h, sessions, tok := bearerFor(t, p)
	rec := serveAuth(t, h, http.MethodPost, "/api/v1/auth/logout-all", "", http.Header{
		"Authorization": {"Bearer " + tok},
	})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d %s", rec.Code, rec.Body)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("204 body must be empty: %q", rec.Body)
	}
	calls := sessions.principals()
	if len(calls) != 1 || calls[0].UserID != p.UserID || calls[0].SessionID != p.SessionID {
		t.Fatalf("logout-all calls = %+v", calls)
	}
}

func TestPasswordSuccessAndUnknownField(t *testing.T) {
	p := logoutPrincipal("11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222")
	h, _, tok := bearerFor(t, p)
	var got application.ChangePasswordCommand
	h.changePassword = func(_ *http.Request, cmd application.ChangePasswordCommand) error {
		got = cmd
		return nil
	}
	hdr := http.Header{"Authorization": {"Bearer " + tok}}

	ok := serveAuth(t, h, http.MethodPost, "/api/v1/auth/password",
		`{"old_password":"correct-horse","new_password":"new-correct-horse"}`, hdr)
	if ok.Code != http.StatusNoContent {
		t.Fatalf("status = %d %s", ok.Code, ok.Body)
	}
	if got.Actor.UserID != p.UserID || got.OldPassword != "correct-horse" || got.NewPassword != "new-correct-horse" {
		t.Fatalf("cmd = %+v", got)
	}

	h.changePassword = func(*http.Request, application.ChangePasswordCommand) error {
		t.Fatal("changePassword must not run")
		return nil
	}
	extra := serveAuth(t, h, http.MethodPost, "/api/v1/auth/password",
		`{"old_password":"correct-horse","new_password":"new-correct-horse","extra":true}`, hdr)
	e := decodeErr(t, extra)
	if extra.Code != http.StatusBadRequest || e.Error.Code != domain.CodeInvalidRequest {
		t.Fatalf("extra = %d %s", extra.Code, extra.Body)
	}
}

func TestPasswordInvalidCredentials(t *testing.T) {
	p := logoutPrincipal("11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222")
	h, _, tok := bearerFor(t, p)
	h.changePassword = func(*http.Request, application.ChangePasswordCommand) error {
		return domain.ErrInvalidCredentials
	}
	rec := serveAuth(t, h, http.MethodPost, "/api/v1/auth/password",
		`{"old_password":"wrong-password","new_password":"new-correct-horse"}`,
		http.Header{"Authorization": {"Bearer " + tok}})
	e := decodeErr(t, rec)
	if rec.Code != http.StatusUnauthorized || e.Error.Code != domain.CodeInvalidCredentials {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	if rec.Header().Get("WWW-Authenticate") != "Bearer" {
		t.Fatalf("WWW-Authenticate = %q", rec.Header().Get("WWW-Authenticate"))
	}
	if strings.Contains(rec.Header().Get("WWW-Authenticate"), "error") {
		t.Fatal("WWW-Authenticate must have no error=")
	}
	if strings.Contains(rec.Body.String(), "wrong-password") {
		t.Fatalf("password leaked: %s", rec.Body)
	}
}

func TestSessionsListMarksCurrent(t *testing.T) {
	p := logoutPrincipal("11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222")
	h, _, tok := bearerFor(t, p)
	rec := serveAuth(t, h, http.MethodGet, "/api/v1/auth/sessions", "", http.Header{
		"Authorization": {"Bearer " + tok},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s", rec.Code, rec.Body)
	}
	var env sessionsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if len(env.Sessions) != 2 {
		t.Fatalf("sessions = %#v", env.Sessions)
	}
	var current, other int
	for _, s := range env.Sessions {
		if s.Current {
			current++
			if s.ID != p.SessionID {
				t.Fatalf("current id = %s", s.ID)
			}
		} else {
			other++
		}
		if s.UserAgent == "" || s.IP == "" || s.CreatedAt.IsZero() {
			t.Fatalf("session = %+v", s)
		}
	}
	if current != 1 || other != 1 {
		t.Fatalf("current=%d other=%d", current, other)
	}
	raw := rec.Body.String()
	if strings.Contains(raw, "user_id") || strings.Contains(raw, "password") ||
		strings.Contains(raw, "revoke_reason") || strings.Contains(raw, "token") {
		t.Fatalf("leaked fields: %s", raw)
	}
}

func TestRevokeSessionHTTP(t *testing.T) {
	p := logoutPrincipal("11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222")
	h, _, tok := bearerFor(t, p)
	var got application.RevokeSessionCommand
	h.revokeSession = func(_ *http.Request, cmd application.RevokeSessionCommand) error {
		got = cmd
		return nil
	}
	hdr := http.Header{"Authorization": {"Bearer " + tok}}
	path := "/api/v1/auth/sessions/" + p.SessionID.String() + "/revoke"

	rec := serveAuth(t, h, http.MethodPost, path, "", hdr)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d %s", rec.Code, rec.Body)
	}
	if got.Actor.UserID != p.UserID || got.SessionID != p.SessionID {
		t.Fatalf("cmd = %+v", got)
	}

	bad := serveAuth(t, h, http.MethodPost, "/api/v1/auth/sessions/not-a-uuid/revoke", "", hdr)
	e := decodeErr(t, bad)
	if bad.Code != http.StatusBadRequest || e.Error.Code != domain.CodeInvalidRequest {
		t.Fatalf("invalid id = %d %s", bad.Code, bad.Body)
	}

	h.revokeSession = func(*http.Request, application.RevokeSessionCommand) error {
		return domain.ErrNotFound
	}
	foreign := serveAuth(t, h, http.MethodPost, "/api/v1/auth/sessions/aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa/revoke", "", hdr)
	fe := decodeErr(t, foreign)
	if foreign.Code != http.StatusNotFound || fe.Error.Code != domain.CodeNotFound {
		t.Fatalf("foreign = %d %s", foreign.Code, foreign.Body)
	}
}

func TestSelfService401WWWAuthenticateHasNoReason(t *testing.T) {
	p := logoutPrincipal("11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222")
	h := mountSelfService(t, newLogoutSessions(p))
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/auth/me"},
		{http.MethodPost, "/api/v1/auth/logout-all"},
		{http.MethodPost, "/api/v1/auth/password"},
		{http.MethodGet, "/api/v1/auth/sessions"},
		{http.MethodPost, "/api/v1/auth/sessions/" + p.SessionID.String() + "/revoke"},
	} {
		rec := serveAuth(t, h, tc.method, tc.path, `{"old_password":"x","new_password":"y"}`, nil)
		assertLogout401(t, rec)
		if strings.Contains(rec.Header().Get("WWW-Authenticate"), "error=") {
			t.Errorf("%s %s WWW-Authenticate has error=", tc.method, tc.path)
		}
	}
}

func TestSelfServiceLogsDoNotContainSecrets(t *testing.T) {
	p := logoutPrincipal("11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222")
	sessions := newLogoutSessions(p)
	h := mountSelfService(t, sessions)
	h.changePassword = func(*http.Request, application.ChangePasswordCommand) error {
		return domain.ErrInvalidCredentials
	}
	oldPassword := "correct-horse-secret"
	newPassword := "new-correct-horse-secret"
	tok := signLogout(t, logoutSigner(t), p)

	var logs bytes.Buffer
	cfg := config.HTTP{Addr: "127.0.0.1:0", MaxBodyBytes: 1 << 20}
	s := httpserver.New(cfg, slog.New(slog.NewJSONHandler(&logs, nil)))
	Mount(s, h)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/password", strings.NewReader(
		`{"old_password":"`+oldPassword+`","new_password":"`+newPassword+`"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok)
	req.RemoteAddr = "203.0.113.10:54321"
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d %s", rec.Code, rec.Body)
	}

	dump := logs.String() + rec.Body.String()
	for _, secret := range []string{oldPassword, newPassword, tok, "Bearer " + tok} {
		if strings.Contains(dump, secret) {
			t.Errorf("secret %q leaked", secret)
		}
	}
	if strings.Contains(logs.String(), `"Authorization"`) {
		t.Errorf("Authorization header logged: %s", logs.String())
	}
}

func TestComposeUsesRetryUoWOnlyForLogoutAndRefresh(t *testing.T) {
	var calls []string
	main := &namedUoW{name: "main", calls: &calls}
	retry := &namedUoW{name: "retry", calls: &calls}
	h := Compose(main, retry, application.Service{}, nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/", nil)

	_ = h.logout(req, domain.Principal{})
	_, _ = h.refresh(req, application.RefreshCommand{})
	_ = h.logoutAll(req, domain.Principal{})
	_ = h.changePassword(req, application.ChangePasswordCommand{})
	_ = h.revokeSession(req, application.RevokeSessionCommand{})
	_, _ = h.login(req, application.LoginCommand{})
	_, _ = h.register(req, application.RegisterCommand{})

	var retryN, mainN int
	for _, c := range calls {
		switch c {
		case "retry":
			retryN++
		case "main":
			mainN++
		}
	}
	if retryN != 2 {
		t.Fatalf("retry UoW calls = %d (%v), want Logout+Refresh", retryN, calls)
	}
	if mainN != 5 {
		t.Fatalf("main UoW calls = %d (%v), want register/login/logout-all/password/revoke", mainN, calls)
	}
}

type namedUoW struct {
	name  string
	calls *[]string
}

func (u *namedUoW) Do(ctx context.Context, fn func(context.Context, application.Deps) (application.Decision, error)) error {
	*u.calls = append(*u.calls, u.name)
	_, err := fn(ctx, application.Deps{})
	return err
}

func TestEmptySessionsJSONIsArray(t *testing.T) {
	p := logoutPrincipal("11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222")
	h, _, tok := bearerFor(t, p)
	h.listSessions = func(*http.Request, domain.Principal) ([]domain.Session, error) {
		return nil, nil
	}
	rec := serveAuth(t, h, http.MethodGet, "/api/v1/auth/sessions", "", http.Header{
		"Authorization": {"Bearer " + tok},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s", rec.Code, rec.Body)
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"sessions":[]`)) &&
		!strings.Contains(rec.Body.String(), `"sessions": []`) {
		t.Fatalf("want empty array, got %s", rec.Body)
	}
	if strings.Contains(rec.Body.String(), `"sessions":null`) {
		t.Fatal("sessions must not be null")
	}
}

func TestAdminRoutesNotMounted(t *testing.T) {
	p := logoutPrincipal("11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222")
	h, _, tok := bearerFor(t, p)
	rec := serveAuth(t, h, http.MethodGet, "/api/v1/admin/users", "", http.Header{
		"Authorization": {"Bearer " + tok},
	})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("admin = %d, want 404", rec.Code)
	}
}
