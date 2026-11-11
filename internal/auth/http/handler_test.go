package authhttp

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/romangergovskiy/paycore/internal/auth/application"
	"github.com/romangergovskiy/paycore/internal/auth/domain"
	"github.com/romangergovskiy/paycore/internal/platform/config"
	"github.com/romangergovskiy/paycore/internal/platform/httpserver"
)

func testUser() domain.PublicUser {
	return domain.PublicUser{
		ID:        uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		Email:     "ada@example.com",
		FirstName: "Ada",
		LastName:  "Lovelace",
		Status:    domain.StatusActive,
		Roles:     []domain.Role{domain.RoleCustomer},
	}
}

func testCreds() application.Credentials {
	return application.Credentials{
		SessionID:    uuid.MustParse("22222222-2222-2222-2222-222222222222"),
		AccessToken:  "header.payload.sig",
		RefreshToken: "opaque-refresh-secret",
		TokenType:    "Bearer",
		ExpiresIn:    900,
		User:         testUser(),
	}
}

func opaqueToken() string {
	raw := bytes.Repeat([]byte{0xab}, 32)
	return base64.RawURLEncoding.EncodeToString(raw)
}

func newTestHandler() Handler {
	u := testUser()
	c := testCreds()
	return Handler{
		register: func(*http.Request, application.RegisterCommand) (domain.PublicUser, error) {
			return u, nil
		},
		login: func(*http.Request, application.LoginCommand) (application.Credentials, error) {
			return c, nil
		},
		refresh: func(*http.Request, application.RefreshCommand) (application.Credentials, error) {
			return c, nil
		},
	}
}

func serveAuth(t *testing.T, h Handler, method, target, body string, header http.Header) *httptest.ResponseRecorder {
	t.Helper()
	cfg := config.HTTP{
		Addr:         "127.0.0.1:0",
		MaxBodyBytes: 1 << 20,
	}
	s := httpserver.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	Mount(s, h)
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "203.0.113.10:54321"
	for k, v := range header {
		req.Header[k] = v
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

type errorEnvelope struct {
	Error struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		RequestID string `json:"request_id"`
	} `json:"error"`
}

func decodeErr(t *testing.T, rec *httptest.ResponseRecorder) errorEnvelope {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q body = %s", ct, rec.Body)
	}
	var env errorEnvelope
	dec := json.NewDecoder(rec.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&env); err != nil {
		t.Fatalf("decode error: %v body=%s", err, rec.Body)
	}
	if env.Error.Code == "" || env.Error.Message == "" || env.Error.RequestID == "" {
		t.Fatalf("incomplete D-39 body: %+v", env.Error)
	}
	return env
}

func TestRegisterSuccess(t *testing.T) {
	var got application.RegisterCommand
	h := newTestHandler()
	h.register = func(_ *http.Request, cmd application.RegisterCommand) (domain.PublicUser, error) {
		got = cmd
		return testUser(), nil
	}

	body := `{"email":"ada@example.com","password":"correct-horse","first_name":"Ada","last_name":"Lovelace"}`
	rec := serveAuth(t, h, http.MethodPost, "/api/v1/auth/register", body, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d %s", rec.Code, rec.Body)
	}
	if !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("Content-Type = %q", rec.Header().Get("Content-Type"))
	}
	var env map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	user, ok := env["user"].(map[string]any)
	if !ok {
		t.Fatalf("body = %s", rec.Body)
	}
	if user["id"] != "11111111-1111-1111-1111-111111111111" || user["email"] != "ada@example.com" {
		t.Fatalf("user = %#v", user)
	}
	roles, _ := user["roles"].([]any)
	if len(roles) != 1 || roles[0] != "CUSTOMER" {
		t.Fatalf("roles = %#v", user["roles"])
	}
	for _, leak := range []string{"first_name", "last_name", "status", "password", "password_hash", "session_id"} {
		if _, ok := user[leak]; ok {
			t.Errorf("user leaked %s", leak)
		}
		if strings.Contains(rec.Body.String(), `"Ada"`) || strings.Contains(rec.Body.String(), "correct-horse") {
			t.Fatalf("response leaked private fields: %s", rec.Body)
		}
	}
	if got.Email != "ada@example.com" || got.Password != "correct-horse" || got.FirstName != "Ada" || got.LastName != "Lovelace" {
		t.Fatalf("command = %+v", got)
	}
	if got.IP != "203.0.113.10" {
		t.Fatalf("IP = %q, want RemoteAddr host", got.IP)
	}
	if len(got.Roles) != 0 {
		t.Fatalf("handler must not pass client roles: %v", got.Roles)
	}
}

func TestNewWiresUseCases(t *testing.T) {
	h := New(nil, application.Service{}, nil, nil)
	if h.register == nil || h.login == nil || h.refresh == nil || h.logout == nil ||
		h.logoutAll == nil || h.changePassword == nil || h.revokeSession == nil {
		t.Fatal("New left a use case unset")
	}
	if h.currentUser != nil || h.listSessions != nil {
		t.Fatal("nil sessions must not wire CurrentUser/ListSessions")
	}
	wired := New(nil, application.Service{}, nil, application.Service{})
	if wired.currentUser == nil || wired.listSessions == nil {
		t.Fatal("read Service must wire CurrentUser/ListSessions")
	}
}

func TestRegisterUnknownFieldsAndRoles(t *testing.T) {
	h := newTestHandler()
	h.register = func(*http.Request, application.RegisterCommand) (domain.PublicUser, error) {
		t.Fatal("register must not run")
		return domain.PublicUser{}, nil
	}
	for _, body := range []string{
		`{"email":"ada@example.com","password":"x","roles":["ADMIN"]}`,
		`{"email":"ada@example.com","password":"x","unknown":true}`,
	} {
		rec := serveAuth(t, h, http.MethodPost, "/api/v1/auth/register", body, nil)
		e := decodeErr(t, rec)
		if rec.Code != http.StatusBadRequest || e.Error.Code != domain.CodeInvalidRequest {
			t.Errorf("body %s -> %d %s", body, rec.Code, rec.Body)
		}
	}
}

func TestRegisterEmailTaken(t *testing.T) {
	h := newTestHandler()
	h.register = func(*http.Request, application.RegisterCommand) (domain.PublicUser, error) {
		return domain.PublicUser{}, domain.ErrEmailTaken
	}
	rec := serveAuth(t, h, http.MethodPost, "/api/v1/auth/register",
		`{"email":"ada@example.com","password":"x"}`, nil)
	e := decodeErr(t, rec)
	if rec.Code != http.StatusConflict || e.Error.Code != domain.CodeEmailTaken {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	if rec.Header().Get("WWW-Authenticate") != "" {
		t.Errorf("409 must not send WWW-Authenticate")
	}
}

func TestJSONErrors(t *testing.T) {
	h := newTestHandler()
	h.login = func(*http.Request, application.LoginCommand) (application.Credentials, error) {
		t.Fatal("login must not run")
		return application.Credentials{}, nil
	}
	cases := []string{
		`{`,
		`{"email":`,
		`[]`,
		``,
	}
	for _, body := range cases {
		rec := serveAuth(t, h, http.MethodPost, "/api/v1/auth/login", body, nil)
		e := decodeErr(t, rec)
		if rec.Code != http.StatusBadRequest || e.Error.Code != domain.CodeInvalidRequest {
			t.Errorf("%q -> %d %s", body, rec.Code, rec.Body)
		}
		if strings.Contains(rec.Body.String(), "invalid character") ||
			strings.Contains(rec.Body.String(), "unexpected") {
			t.Errorf("decoder text leaked: %s", rec.Body)
		}
	}
}

func TestDecodeJSONOneObjectAndExtra(t *testing.T) {
	ok := `{"email":"ada@example.com","password":"x"}`
	h := newTestHandler()
	called := 0
	h.login = func(*http.Request, application.LoginCommand) (application.Credentials, error) {
		called++
		return testCreds(), nil
	}

	for _, body := range []string{
		ok,
		ok + "   \t\n\r\n",
	} {
		called = 0
		rec := serveAuth(t, h, http.MethodPost, "/api/v1/auth/login", body, nil)
		if rec.Code != http.StatusOK {
			t.Errorf("ok body %q -> %d %s", body, rec.Code, rec.Body)
		}
		if called != 1 {
			t.Errorf("login calls = %d, want 1 for %q", called, body)
		}
	}

	h.login = func(*http.Request, application.LoginCommand) (application.Credentials, error) {
		t.Error("login must not run for extra JSON")
		return application.Credentials{}, nil
	}
	for _, body := range []string{
		ok + `{"extra":true}`,
		ok + `null`,
		ok + ` not-json`,
		ok + `}`,
		ok + `]`,
	} {
		rec := serveAuth(t, h, http.MethodPost, "/api/v1/auth/login", body, nil)
		e := decodeErr(t, rec)
		if rec.Code != http.StatusBadRequest || e.Error.Code != domain.CodeInvalidRequest {
			t.Errorf("extra %q -> %d %s", body, rec.Code, rec.Body)
		}
		raw := rec.Body.String()
		if strings.Contains(raw, "invalid character") || strings.Contains(raw, "unexpected") ||
			strings.Contains(raw, "looking for beginning") {
			t.Errorf("decoder text leaked: %s", raw)
		}
	}
}

func TestLoginSuccess(t *testing.T) {
	var got application.LoginCommand
	h := newTestHandler()
	h.login = func(r *http.Request, cmd application.LoginCommand) (application.Credentials, error) {
		got = cmd
		if r.Header.Get("X-Forwarded-For") != "1.2.3.4" {
			t.Errorf("X-Forwarded-For should still be on the request")
		}
		return testCreds(), nil
	}
	rec := serveAuth(t, h, http.MethodPost, "/api/v1/auth/login",
		`{"email":"ada@example.com","password":"secret-pass"}`,
		http.Header{"X-Forwarded-For": {"1.2.3.4"}, "User-Agent": {"PayCoreTest/1"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s", rec.Code, rec.Body)
	}
	var env credentialsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.AccessToken != "header.payload.sig" || env.TokenType != "Bearer" || env.ExpiresIn != 900 ||
		env.RefreshToken != "opaque-refresh-secret" || env.User.Email != "ada@example.com" {
		t.Fatalf("creds = %+v", env)
	}
	if env.User.ID != testUser().ID || len(env.User.Roles) != 1 {
		t.Fatalf("user = %+v", env.User)
	}
	raw := rec.Body.String()
	if strings.Contains(raw, "session_id") || strings.Contains(raw, "22222222") ||
		strings.Contains(raw, "Ada") || strings.Contains(raw, "password_hash") {
		t.Fatalf("leaked fields: %s", raw)
	}
	if got.IP != "203.0.113.10" {
		t.Fatalf("IP used X-Forwarded-For: %q", got.IP)
	}
	if got.UserAgent != "PayCoreTest/1" || got.Email != "ada@example.com" || got.Password != "secret-pass" {
		t.Fatalf("login cmd = %+v", got)
	}
}

func TestLoginInvalidCredentialsNoEnumeration(t *testing.T) {
	h := newTestHandler()
	h.login = func(_ *http.Request, cmd application.LoginCommand) (application.Credentials, error) {
		return application.Credentials{}, domain.ErrInvalidCredentials
	}
	unknown := serveAuth(t, h, http.MethodPost, "/api/v1/auth/login",
		`{"email":"nobody@example.com","password":"x"}`, nil)
	wrong := serveAuth(t, h, http.MethodPost, "/api/v1/auth/login",
		`{"email":"ada@example.com","password":"wrong"}`, nil)
	eu, ew := decodeErr(t, unknown), decodeErr(t, wrong)
	if unknown.Code != http.StatusUnauthorized || wrong.Code != http.StatusUnauthorized {
		t.Fatalf("status unknown=%d wrong=%d", unknown.Code, wrong.Code)
	}
	if eu.Error.Code != domain.CodeInvalidCredentials || eu.Error.Code != ew.Error.Code ||
		eu.Error.Message != ew.Error.Message {
		t.Fatalf("enumeration: %+v vs %+v", eu.Error, ew.Error)
	}
	if unknown.Header().Get("WWW-Authenticate") != "Bearer" ||
		wrong.Header().Get("WWW-Authenticate") != "Bearer" {
		t.Fatal("401 must send WWW-Authenticate: Bearer")
	}
	if strings.Contains(unknown.Header().Get("WWW-Authenticate"), "error") ||
		strings.Contains(unknown.Header().Get("WWW-Authenticate"), "invalid") {
		t.Fatal("WWW-Authenticate must have no reason")
	}
}

func TestAuthRateLimited(t *testing.T) {
	h := newTestHandler()
	h.login = func(*http.Request, application.LoginCommand) (application.Credentials, error) {
		return application.Credentials{}, domain.ErrRateLimited
	}
	rec := serveAuth(t, h, http.MethodPost, "/api/v1/auth/login",
		`{"email":"ada@example.com","password":"x"}`, nil)
	e := decodeErr(t, rec)
	if rec.Code != http.StatusTooManyRequests || e.Error.Code != domain.CodeRateLimited {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	if rec.Header().Get("WWW-Authenticate") != "" {
		t.Errorf("429 must not send WWW-Authenticate: %q", rec.Header().Get("WWW-Authenticate"))
	}
}

func TestRefreshSuccessJSONBody(t *testing.T) {
	var got application.RefreshCommand
	h := newTestHandler()
	h.refresh = func(_ *http.Request, cmd application.RefreshCommand) (application.Credentials, error) {
		got = cmd
		return testCreds(), nil
	}
	tok := opaqueToken()
	rec := serveAuth(t, h, http.MethodPost, "/api/v1/auth/refresh",
		`{"refresh_token":"`+tok+`"}`,
		http.Header{"Cookie": {"refresh_token=from-cookie"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s", rec.Code, rec.Body)
	}
	if got.RefreshToken != tok || got.IP != "203.0.113.10" {
		t.Fatalf("refresh cmd = %+v", got)
	}
}

func TestRefreshEmptyAndMalformed(t *testing.T) {
	h := newTestHandler()
	h.refresh = func(*http.Request, application.RefreshCommand) (application.Credentials, error) {
		t.Fatal("refresh must not run")
		return application.Credentials{}, nil
	}
	empty := serveAuth(t, h, http.MethodPost, "/api/v1/auth/refresh", `{"refresh_token":""}`, nil)
	ee := decodeErr(t, empty)
	if empty.Code != http.StatusBadRequest || ee.Error.Code != domain.CodeInvalidRequest {
		t.Fatalf("empty = %d %s", empty.Code, empty.Body)
	}

	bad := serveAuth(t, h, http.MethodPost, "/api/v1/auth/refresh", `{"refresh_token":"not-32-bytes"}`, nil)
	eb := decodeErr(t, bad)
	if bad.Code != http.StatusUnauthorized || eb.Error.Code != domain.CodeInvalidToken {
		t.Fatalf("malformed = %d %s", bad.Code, bad.Body)
	}
	if bad.Header().Get("WWW-Authenticate") != "Bearer" {
		t.Fatal("401 must send WWW-Authenticate: Bearer")
	}
}

func TestRefreshTokenReusedAndExpired(t *testing.T) {
	h := newTestHandler()
	h.refresh = func(*http.Request, application.RefreshCommand) (application.Credentials, error) {
		return application.Credentials{}, domain.ErrTokenReused
	}
	rec := serveAuth(t, h, http.MethodPost, "/api/v1/auth/refresh",
		`{"refresh_token":"`+opaqueToken()+`"}`, nil)
	e := decodeErr(t, rec)
	if rec.Code != http.StatusUnauthorized || e.Error.Code != domain.CodeTokenReused {
		t.Fatalf("reuse = %d %s", rec.Code, rec.Body)
	}
	if rec.Header().Get("WWW-Authenticate") != "Bearer" {
		t.Fatal("missing WWW-Authenticate")
	}

	h.refresh = func(*http.Request, application.RefreshCommand) (application.Credentials, error) {
		return application.Credentials{}, domain.ErrTokenExpired
	}
	rec = serveAuth(t, h, http.MethodPost, "/api/v1/auth/refresh",
		`{"refresh_token":"`+opaqueToken()+`"}`, nil)
	e = decodeErr(t, rec)
	if rec.Code != http.StatusUnauthorized || e.Error.Code != domain.CodeTokenExpired {
		t.Fatalf("expired = %d %s", rec.Code, rec.Body)
	}
}

func TestUnknownInternalErrorHidesCause(t *testing.T) {
	h := newTestHandler()
	h.login = func(*http.Request, application.LoginCommand) (application.Credentials, error) {
		return application.Credentials{}, errors.New("connection reset by postgres host=db password=supersecret")
	}
	rec := serveAuth(t, h, http.MethodPost, "/api/v1/auth/login",
		`{"email":"ada@example.com","password":"x"}`, nil)
	e := decodeErr(t, rec)
	if rec.Code != http.StatusInternalServerError || e.Error.Code != "INTERNAL_ERROR" {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	if e.Error.Message != "Internal server error." {
		t.Fatalf("message = %q", e.Error.Message)
	}
	raw := rec.Body.String()
	for _, secret := range []string{"postgres", "supersecret", "connection reset", "password"} {
		if strings.Contains(raw, secret) {
			t.Errorf("secret %q leaked: %s", secret, raw)
		}
	}
}

func TestErrorJSONShapeHasNoDetails(t *testing.T) {
	h := newTestHandler()
	h.register = func(*http.Request, application.RegisterCommand) (domain.PublicUser, error) {
		return domain.PublicUser{}, domain.ErrEmailTaken
	}
	rec := serveAuth(t, h, http.MethodPost, "/api/v1/auth/register",
		`{"email":"ada@example.com","password":"pw"}`, nil)
	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	errObj, ok := raw["error"].(map[string]any)
	if !ok || len(raw) != 1 {
		t.Fatalf("envelope = %#v", raw)
	}
	if _, ok := errObj["code"].(string); !ok {
		t.Fatal("missing code")
	}
	if _, ok := errObj["message"].(string); !ok {
		t.Fatal("missing message")
	}
	if _, ok := errObj["request_id"].(string); !ok {
		t.Fatal("missing request_id")
	}
	if _, ok := errObj["details"]; ok {
		t.Fatal("details must not be present")
	}
}

func TestNoSecretsInResponses(t *testing.T) {
	password := "hunter2"
	refreshToken := opaqueToken()
	bodies := []struct {
		path, body string
		setup      func(*Handler)
	}{
		{"/api/v1/auth/register", `{"email":"ada@example.com","password":"` + password + `"}`, func(h *Handler) {
			h.register = func(*http.Request, application.RegisterCommand) (domain.PublicUser, error) {
				return domain.PublicUser{}, domain.ErrEmailTaken
			}
		}},
		{"/api/v1/auth/login", `{"email":"ada@example.com","password":"` + password + `"}`, func(h *Handler) {
			h.login = func(*http.Request, application.LoginCommand) (application.Credentials, error) {
				return application.Credentials{}, domain.ErrInvalidCredentials
			}
		}},
		{"/api/v1/auth/refresh", `{"refresh_token":"` + refreshToken + `"}`, func(h *Handler) {
			h.refresh = func(*http.Request, application.RefreshCommand) (application.Credentials, error) {
				return application.Credentials{}, domain.ErrInvalidToken
			}
		}},
	}
	for _, tc := range bodies {
		hh := newTestHandler()
		tc.setup(&hh)
		rec := serveAuth(t, hh, http.MethodPost, tc.path, tc.body, nil)
		raw := rec.Body.String()
		if strings.Contains(raw, password) {
			t.Errorf("%s leaked request password: %s", tc.path, raw)
		}
		if strings.Contains(raw, refreshToken) {
			t.Errorf("%s leaked request refresh token: %s", tc.path, raw)
		}
	}
}

func TestLogoutAndMeNotMounted(t *testing.T) {
	s := httpserver.New(config.HTTP{MaxBodyBytes: 1024}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	Mount(s, newTestHandler())
	for _, path := range []string{
		"/api/v1/auth/logout",
		"/api/v1/auth/me",
		"/api/v1/auth/logout-all",
		"/api/v1/auth/password",
		"/api/v1/auth/sessions",
		"/api/v1/admin/users",
	} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s = %d, want 404", path, rec.Code)
		}
	}
}

func TestRegisterTooLarge(t *testing.T) {
	cfg := config.HTTP{MaxBodyBytes: 16}
	s := httpserver.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	Mount(s, newTestHandler())
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/register", strings.NewReader(`{"email":"ada@example.com","password":"long-password"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d %s", rec.Code, rec.Body)
	}
}
