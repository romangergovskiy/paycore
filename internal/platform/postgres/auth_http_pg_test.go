package postgres

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/romangergovskiy/paycore/internal/auth/application"
	"github.com/romangergovskiy/paycore/internal/auth/domain"
	authhttp "github.com/romangergovskiy/paycore/internal/auth/http"
	"github.com/romangergovskiy/paycore/internal/platform/authcrypto"
	"github.com/romangergovskiy/paycore/internal/platform/config"
	"github.com/romangergovskiy/paycore/internal/platform/httpserver"
)

const (
	httpPGJWTSecret = "paycore-http-pg-test-secret-32b"
	httpPGPassword  = "correct-horse-pg"
	testAdminPath   = "/api/v1/__test/require-admin"
)

// TestAuthHTTPPostgres drives the real Auth HTTP handlers against PostgreSQL
// as paycore_app. Hasher, signer, refresh issuer, UoW, and adapters are the
// production types. There is no t.Skip for Docker: CI runs testcontainers.
func TestAuthHTTPPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	c := startCluster(t)
	if err := Bootstrap(ctx, c.admin, c.owner, c.app, c.readonly); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if err := Up(ctx, c.owner); err != nil {
		t.Fatalf("up: %v", err)
	}

	pool, err := Open(ctx, config.Postgres{
		DSN:              c.app,
		MaxConns:         8,
		MinConns:         2,
		MaxConnLifetime:  time.Minute,
		MaxConnIdleTime:  time.Minute,
		LockTimeout:      3 * time.Second,
		StatementTimeout: 15 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	app := mustConnect(t, ctx, c.app)
	t.Cleanup(func() { _ = app.Close(context.Background()) })

	env := newAuthHTTPEnv(t, ctx, pool)

	t.Run("full lifecycle", func(t *testing.T) {
		env.t = t
		email := pgEmail(t, "http-life")
		password := httpPGPassword
		env.trackSecret(password)

		reg := env.do(http.MethodPost, "/api/v1/auth/register", registerBody(email, password), "", nextTestIP())
		if reg.Code != http.StatusCreated {
			t.Fatalf("register = %d %s", reg.Code, reg.Body)
		}
		user := decodeUser(t, reg.Body)
		if user.Email != email || len(user.Roles) != 1 || user.Roles[0] != domain.RoleCustomer {
			t.Fatalf("register user = %+v", user)
		}
		raw := reg.Body.String()
		if strings.Contains(raw, "access_token") || strings.Contains(raw, "refresh_token") || strings.Contains(raw, password) {
			t.Fatalf("register leaked tokens or password: %s", raw)
		}
		phc := storedPasswordHash(t, ctx, app, email)
		env.trackSecret(phc)
		if strings.Contains(raw, phc) || strings.Contains(raw, "$argon2id$") {
			t.Fatalf("register leaked PHC: %s", raw)
		}

		login := env.do(http.MethodPost, "/api/v1/auth/login", loginBody(email, password), "", nextTestIP())
		if login.Code != http.StatusOK {
			t.Fatalf("login = %d %s", login.Code, login.Body)
		}
		first := decodeCreds(t, login.Body)
		if first.TokenType != "Bearer" || first.ExpiresIn != domain.AccessExpiresIn || first.User.ID != user.ID {
			t.Fatalf("login creds = %+v", first)
		}
		env.trackSecret(first.AccessToken, first.RefreshToken)

		ref := env.do(http.MethodPost, "/api/v1/auth/refresh", refreshBody(first.RefreshToken), "", nextTestIP())
		if ref.Code != http.StatusOK {
			t.Fatalf("refresh = %d %s", ref.Code, ref.Body)
		}
		second := decodeCreds(t, ref.Body)
		if second.RefreshToken == first.RefreshToken || second.AccessToken == first.AccessToken {
			t.Fatal("refresh did not rotate tokens")
		}
		if second.User.ID != user.ID {
			t.Fatalf("refresh user = %+v", second.User)
		}
		env.trackSecret(second.AccessToken, second.RefreshToken)
		assertRefreshRotated(t, ctx, app, env.issuer, first.RefreshToken, second.RefreshToken)

		old := env.do(http.MethodPost, "/api/v1/auth/refresh", refreshBody(first.RefreshToken), "", nextTestIP())
		assertAuthError(t, old, http.StatusUnauthorized, domain.CodeTokenReused)
		env.assertNoSecrets(old.Body.String())

		out := env.do(http.MethodPost, "/api/v1/auth/logout", "", "Bearer "+second.AccessToken, nextTestIP())
		if out.Code != http.StatusNoContent || out.Body.Len() != 0 {
			t.Fatalf("logout = %d %q", out.Code, out.Body)
		}
		assertSessionRevoked(t, ctx, app, firstSessionID(t, ctx, app, user.ID), domain.RevokeLogout)

		bearer := env.do(http.MethodGet, testAdminPath, "", "Bearer "+second.AccessToken, nextTestIP())
		assertAuthError(t, bearer, http.StatusUnauthorized, domain.CodeInvalidToken)

		ended := env.do(http.MethodPost, "/api/v1/auth/refresh", refreshBody(second.RefreshToken), "", nextTestIP())
		assertAuthError(t, ended, http.StatusUnauthorized, domain.CodeInvalidToken, domain.CodeTokenReused)

		repeat := env.do(http.MethodPost, "/api/v1/auth/logout", "", "Bearer "+second.AccessToken, nextTestIP())
		assertAuthError(t, repeat, http.StatusUnauthorized, domain.CodeInvalidToken)
		env.assertLogsClean()
	})

	t.Run("concurrent refresh", func(t *testing.T) {
		env.t = t
		email := pgEmail(t, "http-par")
		env.trackSecret(httpPGPassword)
		if env.do(http.MethodPost, "/api/v1/auth/register", registerBody(email, httpPGPassword), "", nextTestIP()).Code != http.StatusCreated {
			t.Fatal("register")
		}
		login := env.do(http.MethodPost, "/api/v1/auth/login", loginBody(email, httpPGPassword), "", nextTestIP())
		if login.Code != http.StatusOK {
			t.Fatalf("login = %d %s", login.Code, login.Body)
		}
		cred := decodeCreds(t, login.Body)
		env.trackSecret(cred.AccessToken, cred.RefreshToken)

		var ready, done sync.WaitGroup
		start := make(chan struct{})
		type outcome struct {
			rec *httptest.ResponseRecorder
		}
		out := make([]outcome, 2)
		ready.Add(2)
		done.Add(2)
		for i := 0; i < 2; i++ {
			go func(i int) {
				defer done.Done()
				ready.Done()
				<-start
				out[i].rec = env.do(http.MethodPost, "/api/v1/auth/refresh", refreshBody(cred.RefreshToken), "", nextTestIP())
			}(i)
		}
		ready.Wait()
		close(start)
		done.Wait()

		ok, fail := 0, 0
		for i, o := range out {
			switch o.rec.Code {
			case http.StatusOK:
				ok++
				got := decodeCreds(t, o.rec.Body)
				env.trackSecret(got.AccessToken, got.RefreshToken)
			case http.StatusUnauthorized:
				fail++
				assertAuthError(t, o.rec, http.StatusUnauthorized, domain.CodeTokenReused, domain.CodeInvalidToken)
				env.assertNoSecrets(o.rec.Body.String())
			default:
				t.Fatalf("refresh %d = %d %s", i, o.rec.Code, o.rec.Body)
			}
		}
		if ok != 1 || fail != 1 {
			t.Fatalf("success=%d fail=%d", ok, fail)
		}
		var live int
		if err := app.QueryRow(ctx, `
			SELECT count(*) FROM auth.refresh_tokens tok
			JOIN auth.sessions s ON s.id = tok.session_id
			 WHERE s.user_id = $1
			   AND tok.used_at IS NULL
			   AND tok.revoked_at IS NULL
			   AND s.revoked_at IS NULL`, cred.User.ID).Scan(&live); err != nil {
			t.Fatal(err)
		}
		if live > 1 {
			t.Fatalf("live refresh chains %d", live)
		}
		env.assertLogsClean()
	})

	t.Run("login rate limit", func(t *testing.T) {
		env.t = t
		email := pgEmail(t, "http-rl")
		wrong := "wrong-password-pg"
		env.trackSecret(httpPGPassword, wrong)
		if env.do(http.MethodPost, "/api/v1/auth/register", registerBody(email, httpPGPassword), "", nextTestIP()).Code != http.StatusCreated {
			t.Fatal("register")
		}

		unknown := pgEmail(t, "http-rl-miss")
		miss := env.do(http.MethodPost, "/api/v1/auth/login", loginBody(unknown, wrong), "", nextTestIP())
		bad := env.do(http.MethodPost, "/api/v1/auth/login", loginBody(email, wrong), "", nextTestIP())
		if miss.Code != http.StatusUnauthorized || bad.Code != http.StatusUnauthorized {
			t.Fatalf("unknown=%d known-wrong=%d", miss.Code, bad.Code)
		}
		me := decodeHTTPError(t, miss)
		ke := decodeHTTPError(t, bad)
		if me.Error.Code != domain.CodeInvalidCredentials || ke.Error.Code != domain.CodeInvalidCredentials {
			t.Fatalf("codes unknown=%s known=%s", me.Error.Code, ke.Error.Code)
		}
		if me.Error.Message != ke.Error.Message || me.Error.Message != domain.ErrInvalidCredentials.Message {
			t.Fatalf("login leaked user existence: unknown=%q known=%q", me.Error.Message, ke.Error.Message)
		}

		for i := 0; i < domain.LimitLoginEmail-1; i++ {
			rec := env.do(http.MethodPost, "/api/v1/auth/login", loginBody(email, wrong), "", nextTestIP())
			assertAuthError(t, rec, http.StatusUnauthorized, domain.CodeInvalidCredentials)
			env.assertNoSecrets(rec.Body.String())
		}
		limited := env.do(http.MethodPost, "/api/v1/auth/login", loginBody(email, wrong), "", nextTestIP())
		assertAuthError(t, limited, http.StatusTooManyRequests, domain.CodeRateLimited)
		env.assertNoSecrets(limited.Body.String())
		assertCounterAtLeast(t, ctx, app, domain.BucketLoginEmail, email, time.Now().UTC(), domain.LimitLoginEmail+1)
		env.assertLogsClean()
	})

	t.Run("rbac from postgres not jwt", func(t *testing.T) {
		env.t = t
		email := pgEmail(t, "http-rbac")
		env.trackSecret(httpPGPassword)
		if env.do(http.MethodPost, "/api/v1/auth/register", registerBody(email, httpPGPassword), "", nextTestIP()).Code != http.StatusCreated {
			t.Fatal("register")
		}
		login := env.do(http.MethodPost, "/api/v1/auth/login", loginBody(email, httpPGPassword), "", nextTestIP())
		if login.Code != http.StatusOK {
			t.Fatalf("login = %d %s", login.Code, login.Body)
		}
		cred := decodeCreds(t, login.Body)
		env.trackSecret(cred.AccessToken, cred.RefreshToken)
		if len(cred.User.Roles) != 1 || cred.User.Roles[0] != domain.RoleCustomer {
			t.Fatalf("jwt user roles = %v", cred.User.Roles)
		}

		denied := env.do(http.MethodGet, testAdminPath, "", "Bearer "+cred.AccessToken, nextTestIP())
		assertAuthError(t, denied, http.StatusForbidden, domain.CodeForbidden)

		actor := seedPGAdmin(t, ctx, app, &pgClock{t: time.Now().UTC()}, pgEmail(t, "http-actor"))
		if err := application.GrantRoleIn(ctx, env.uow, env.write, adminPrincipal(actor), cred.User.ID, domain.RoleAdmin); err != nil {
			t.Fatal(err)
		}
		allowed := env.do(http.MethodGet, testAdminPath, "", "Bearer "+cred.AccessToken, nextTestIP())
		if allowed.Code != http.StatusNoContent {
			t.Fatalf("after grant = %d %s", allowed.Code, allowed.Body)
		}

		if err := application.RevokeRoleIn(ctx, env.uow, env.write, adminPrincipal(actor), cred.User.ID, domain.RoleAdmin); err != nil {
			t.Fatal(err)
		}
		revoked := env.do(http.MethodGet, testAdminPath, "", "Bearer "+cred.AccessToken, nextTestIP())
		assertAuthError(t, revoked, http.StatusForbidden, domain.CodeForbidden)
		env.assertNoSecrets(revoked.Body.String())
		env.assertLogsClean()
	})

	t.Run("no secret leaks on error paths", func(t *testing.T) {
		env.t = t
		email := pgEmail(t, "http-leak")
		env.trackSecret(httpPGPassword, appPassword)
		if env.do(http.MethodPost, "/api/v1/auth/register", registerBody(email, httpPGPassword), "", nextTestIP()).Code != http.StatusCreated {
			t.Fatal("register")
		}
		login := env.do(http.MethodPost, "/api/v1/auth/login", loginBody(email, httpPGPassword), "", nextTestIP())
		cred := decodeCreds(t, login.Body)
		env.trackSecret(cred.AccessToken, cred.RefreshToken, storedPasswordHash(t, ctx, app, email))

		wrong := env.do(http.MethodPost, "/api/v1/auth/login", loginBody(email, "wrong-password-pg"), "", nextTestIP())
		assertAuthError(t, wrong, http.StatusUnauthorized, domain.CodeInvalidCredentials)

		env.do(http.MethodPost, "/api/v1/auth/refresh", refreshBody(cred.RefreshToken), "", nextTestIP())
		reuse := env.do(http.MethodPost, "/api/v1/auth/refresh", refreshBody(cred.RefreshToken), "", nextTestIP())
		assertAuthError(t, reuse, http.StatusUnauthorized, domain.CodeTokenReused, domain.CodeInvalidToken)

		missing := env.do(http.MethodPost, "/api/v1/auth/logout", "", "Bearer "+cred.AccessToken, nextTestIP())
		assertAuthError(t, missing, http.StatusUnauthorized, domain.CodeInvalidToken)

		canceled, stop := context.WithCancel(ctx)
		stop()
		infra := env.doCtx(canceled, http.MethodPost, "/api/v1/auth/login", loginBody(email, httpPGPassword), "", nextTestIP())
		if infra.Code != http.StatusInternalServerError {
			t.Fatalf("canceled login = %d %s", infra.Code, infra.Body)
		}
		raw := infra.Body.String()
		for _, n := range []string{"pgx", "SQLSTATE", "paycore_app", appPassword, "BEGIN"} {
			if strings.Contains(raw, n) {
				t.Errorf("infra response leaked %q: %s", n, raw)
			}
		}
		env.assertNoSecrets(wrong.Body.String(), reuse.Body.String(), missing.Body.String(), raw)
		env.assertLogsClean()
	})

	t.Run("auth http does not touch ledger", func(t *testing.T) {
		env.t = t
		var accounts, journals, balances, entries int
		if err := app.QueryRow(ctx, `SELECT count(*) FROM ledger.ledger_accounts`).Scan(&accounts); err != nil {
			t.Fatal(err)
		}
		if err := app.QueryRow(ctx, `SELECT count(*) FROM ledger.journal_transactions`).Scan(&journals); err != nil {
			t.Fatal(err)
		}
		if err := app.QueryRow(ctx, `SELECT count(*) FROM ledger.account_balances`).Scan(&balances); err != nil {
			t.Fatal(err)
		}
		if err := app.QueryRow(ctx, `SELECT count(*) FROM ledger.ledger_entries`).Scan(&entries); err != nil {
			t.Fatal(err)
		}
		if accounts != 0 || journals != 0 || balances != 0 || entries != 0 {
			t.Fatalf("ledger rows accounts=%d journals=%d balances=%d entries=%d", accounts, journals, balances, entries)
		}
	})
}

type authHTTPEnv struct {
	t      *testing.T
	ctx    context.Context
	logs   *syncBuf
	h      http.Handler
	uow    application.UnitOfWork
	write  application.Service
	issuer application.RefreshIssuer
	mu     sync.Mutex
	banned []string
}

func newAuthHTTPEnv(t *testing.T, ctx context.Context, pool *pgxpool.Pool) *authHTTPEnv {
	t.Helper()
	signer, err := authcrypto.NewHS256([]byte(httpPGJWTSecret))
	if err != nil {
		t.Fatal(err)
	}
	base := application.Service{
		Hasher: authcrypto.NewArgon2idHasher(),
		Issuer: authcrypto.NewOpaqueRefresh(),
		Access: signer,
		Clock:  authcrypto.NewSystemClock(),
	}
	read := base
	read.Users = NewAuthUsers(pool)
	read.Sessions = NewAuthSessions(pool)
	read.Roles = NewAuthRoles(pool)
	uow := NewAuthUoW(pool)

	logs := &syncBuf{}
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	srv := httpserver.New(config.HTTP{
		Addr:           "127.0.0.1:0",
		RequestTimeout: 60 * time.Second,
		MaxBodyBytes:   1 << 20,
	}, log)
	authhttp.Mount(srv, authhttp.New(uow, base, signer, read))
	srv.Handle("GET "+testAdminPath, httpserver.BearerAuth(signer, read)(
		httpserver.RequireRole(domain.RoleAdmin)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}))))

	env := &authHTTPEnv{
		t:      t,
		ctx:    ctx,
		logs:   logs,
		h:      srv.Handler(),
		uow:    uow,
		write:  base,
		issuer: base.Issuer,
		banned: []string{
			httpPGPassword, httpPGJWTSecret, appPassword, ownerPassword,
			"Authorization", "$argon2id$", "pgx",
		},
	}
	return env
}

func (e *authHTTPEnv) trackSecret(ss ...string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.banned = append(e.banned, ss...)
}

func (e *authHTTPEnv) do(method, path, body, auth, ip string) *httptest.ResponseRecorder {
	return e.doCtx(e.ctx, method, path, body, auth, ip)
}

func (e *authHTTPEnv) doCtx(ctx context.Context, method, path, body, auth, ip string) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(ctx)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	req.RemoteAddr = ip + ":54321"
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

func (e *authHTTPEnv) assertNoSecrets(haystacks ...string) {
	e.t.Helper()
	e.mu.Lock()
	banned := append([]string(nil), e.banned...)
	e.mu.Unlock()
	for _, hay := range haystacks {
		for _, s := range banned {
			if s == "" || s == "Authorization" {
				continue
			}
			if strings.Contains(hay, s) {
				e.t.Errorf("value leaked %q in %s", s, hay)
			}
		}
		if strings.Contains(hay, "Authorization") && strings.Contains(hay, "Bearer ") {
			e.t.Errorf("Authorization leaked: %s", hay)
		}
	}
}

func (e *authHTTPEnv) assertLogsClean() {
	e.t.Helper()
	dump := e.logs.String()
	e.assertNoSecrets(dump)
	if strings.Contains(dump, "Authorization") {
		e.t.Errorf("logs leaked Authorization: %s", dump)
	}
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type httpUser struct {
	ID    uuid.UUID     `json:"id"`
	Email string        `json:"email"`
	Roles []domain.Role `json:"roles"`
}

type httpCreds struct {
	AccessToken  string   `json:"access_token"`
	TokenType    string   `json:"token_type"`
	ExpiresIn    int      `json:"expires_in"`
	RefreshToken string   `json:"refresh_token"`
	User         httpUser `json:"user"`
}

type httpErr struct {
	Error struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		RequestID string `json:"request_id"`
	} `json:"error"`
}

func registerBody(email, password string) string {
	return `{"email":"` + email + `","password":"` + password + `","first_name":"Ada","last_name":"Lovelace"}`
}

func loginBody(email, password string) string {
	return `{"email":"` + email + `","password":"` + password + `"}`
}

func refreshBody(token string) string {
	return `{"refresh_token":"` + token + `"}`
}

func decodeUser(t *testing.T, body *bytes.Buffer) httpUser {
	t.Helper()
	var env struct {
		User httpUser `json:"user"`
	}
	if err := json.Unmarshal(body.Bytes(), &env); err != nil {
		t.Fatalf("user json: %v %s", err, body)
	}
	return env.User
}

func decodeCreds(t *testing.T, body *bytes.Buffer) httpCreds {
	t.Helper()
	var c httpCreds
	if err := json.Unmarshal(body.Bytes(), &c); err != nil {
		t.Fatalf("creds json: %v %s", err, body)
	}
	if c.AccessToken == "" || c.RefreshToken == "" {
		t.Fatalf("missing tokens: %+v", c)
	}
	return c
}

func decodeHTTPError(t *testing.T, rec *httptest.ResponseRecorder) httpErr {
	t.Helper()
	var env httpErr
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("error json: %v %s", err, rec.Body)
	}
	return env
}

func assertAuthError(t *testing.T, rec *httptest.ResponseRecorder, status int, codes ...string) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d %s, want %d", rec.Code, rec.Body, status)
	}
	env := decodeHTTPError(t, rec)
	ok := false
	for _, c := range codes {
		if env.Error.Code == c {
			ok = true
			break
		}
	}
	if !ok {
		t.Fatalf("code = %s, want one of %v body=%s", env.Error.Code, codes, rec.Body)
	}
	if status == http.StatusUnauthorized && rec.Header().Get("WWW-Authenticate") != "Bearer" {
		t.Fatal("missing WWW-Authenticate: Bearer")
	}
	if strings.Contains(rec.Header().Get("WWW-Authenticate"), "error=") {
		t.Fatalf("WWW-Authenticate leaked reason: %s", rec.Header().Get("WWW-Authenticate"))
	}
}

func storedPasswordHash(t *testing.T, ctx context.Context, conn *pgx.Conn, email string) string {
	t.Helper()
	var hash string
	if err := conn.QueryRow(ctx, `SELECT password_hash FROM auth.users WHERE email = $1`, email).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(hash, "$argon2id$") {
		t.Fatalf("hash = %q", hash)
	}
	return hash
}

func firstSessionID(t *testing.T, ctx context.Context, conn *pgx.Conn, userID uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := conn.QueryRow(ctx, `
		SELECT id FROM auth.sessions WHERE user_id = $1 ORDER BY created_at LIMIT 1`, userID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func assertSessionRevoked(t *testing.T, ctx context.Context, conn *pgx.Conn, id uuid.UUID, reason domain.RevokeReason) {
	t.Helper()
	var revoked bool
	var got string
	if err := conn.QueryRow(ctx, `
		SELECT revoked_at IS NOT NULL, COALESCE(revoke_reason, '')
		  FROM auth.sessions WHERE id = $1`, id).Scan(&revoked, &got); err != nil {
		t.Fatal(err)
	}
	if !revoked || got != string(reason) {
		t.Fatalf("session revoked=%v reason=%q want %s", revoked, got, reason)
	}
}

func assertRefreshRotated(t *testing.T, ctx context.Context, conn *pgx.Conn, issuer application.RefreshIssuer, oldPlain, newPlain string) {
	t.Helper()
	oldHash, err := issuer.Hash(oldPlain)
	if err != nil {
		t.Fatal(err)
	}
	newHash, err := issuer.Hash(newPlain)
	if err != nil {
		t.Fatal(err)
	}
	var oldUsed, oldLive, newUsed, newRevoked bool
	if err := conn.QueryRow(ctx, `
		SELECT used_at IS NOT NULL, revoked_at IS NULL
		  FROM auth.refresh_tokens WHERE token_hash = $1`, oldHash).Scan(&oldUsed, &oldLive); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(ctx, `
		SELECT used_at IS NOT NULL, revoked_at IS NOT NULL
		  FROM auth.refresh_tokens WHERE token_hash = $1`, newHash).Scan(&newUsed, &newRevoked); err != nil {
		t.Fatal(err)
	}
	if !oldUsed || !oldLive {
		t.Fatalf("old refresh used=%v live_row=%v", oldUsed, oldLive)
	}
	if newUsed || newRevoked {
		t.Fatalf("new refresh used=%v revoked=%v", newUsed, newRevoked)
	}
}
