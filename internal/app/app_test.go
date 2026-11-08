package app

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/romangergovskiy/paycore/internal/auth/domain"
	"github.com/romangergovskiy/paycore/internal/platform/authcrypto"
	"github.com/romangergovskiy/paycore/internal/platform/config"
)

const testJWTSecret = "0123456789abcdef0123456789abcdef"

func TestRunInvalidConfig(t *testing.T) {
	err := Run(context.Background(), []string{"PAYCORE_ENV=staging"}, io.Discard)
	if !errors.Is(err, config.ErrInvalid) {
		t.Fatalf("Run = %v, want config.ErrInvalid", err)
	}
}

func TestRunShortJWTSecret(t *testing.T) {
	err := Run(context.Background(), []string{
		"PAYCORE_AUTH_JWT_SECRET=0123456789abcdef0123456789abcde",
	}, io.Discard)
	if !errors.Is(err, config.ErrInvalid) {
		t.Fatalf("Run = %v, want config.ErrInvalid", err)
	}
}

func TestRunProductionRequiresPostgresDSN(t *testing.T) {
	err := Run(context.Background(), []string{
		"PAYCORE_ENV=production",
		"PAYCORE_AUTH_JWT_SECRET=" + testJWTSecret,
	}, io.Discard)
	if !errors.Is(err, config.ErrInvalid) {
		t.Fatalf("Run = %v, want config.ErrInvalid", err)
	}
	if !strings.Contains(err.Error(), "PAYCORE_POSTGRES_DSN") {
		t.Fatalf("Run = %v, want PAYCORE_POSTGRES_DSN in the error", err)
	}
}

func TestRunListenError(t *testing.T) {
	err := Run(context.Background(), []string{
		"PAYCORE_HTTP_ADDR=127.0.0.1:-1",
		"PAYCORE_AUTH_JWT_SECRET=" + testJWTSecret,
	}, io.Discard)
	if err == nil || errors.Is(err, config.ErrInvalid) {
		t.Fatalf("Run = %v, want listen error", err)
	}
}

func TestRunServesWithoutDSNLocal(t *testing.T) {
	runServesWithoutDSN(t, "local")
}

func TestRunServesWithoutDSNTest(t *testing.T) {
	runServesWithoutDSN(t, "test")
}

func runServesWithoutDSN(t *testing.T, env string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logR, logW := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, []string{
			"PAYCORE_ENV=" + env,
			"PAYCORE_LOG_FORMAT=json",
			"PAYCORE_HTTP_ADDR=127.0.0.1:0",
			"PAYCORE_SHUTDOWN_TIMEOUT=2s",
			"PAYCORE_AUTH_JWT_SECRET=" + testJWTSecret,
		}, logW)
		logW.Close()
	}()

	lines := make(chan map[string]any, 64)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(logR)
		for sc.Scan() {
			var rec map[string]any
			if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
				rec = map[string]any{"msg": "non-JSON log line: " + sc.Text()}
			}
			lines <- rec
		}
	}()
	addr := waitForLog(t, lines, "http server started")["addr"].(string)

	resp, err := http.Get("http://" + addr + "/livez")
	if err != nil {
		t.Fatalf("GET /livez: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/livez = %d", resp.StatusCode)
	}

	reg, err := http.Post("http://"+addr+"/api/v1/auth/register", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("POST /api/v1/auth/register: %v", err)
	}
	reg.Body.Close()
	if reg.StatusCode != http.StatusNotFound {
		t.Errorf("empty DSN must not mount auth: POST /register = %d", reg.StatusCode)
	}

	waitForLog(t, lines, "http request")

	cancel()
	waitForLog(t, lines, "shutdown complete")

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after cancel")
	}

	for rec := range lines {
		raw, _ := json.Marshal(rec)
		if strings.Contains(string(raw), testJWTSecret) {
			t.Errorf("JWT secret leaked in logs: %s", raw)
		}
		if rec["msg"] == "auth runtime ready" {
			t.Error("auth runtime wired without a postgres pool")
		}
	}
}

func TestNewAuthRuntimeWithoutPool(t *testing.T) {
	rt, err := newAuthRuntime(nil, testJWTSecret)
	if err != nil || rt != nil {
		t.Fatalf("nil pool: runtime=%v err=%v", rt, err)
	}
}

func TestNewAuthRuntimeWiresSigner(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, "postgres://paycore_app:x@127.0.0.1:1/paycore?sslmode=disable")
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	defer pool.Close()

	rt, err := newAuthRuntime(pool, testJWTSecret)
	if err != nil {
		t.Fatalf("newAuthRuntime: %v", err)
	}
	if rt == nil || rt.uow == nil || rt.svc.Hasher == nil || rt.svc.Issuer == nil ||
		rt.svc.Access == nil || rt.svc.Clock == nil || rt.read.Users == nil ||
		rt.read.Sessions == nil || rt.read.Roles == nil {
		t.Fatalf("incomplete runtime: %+v", rt)
	}

	p := domain.Principal{
		UserID:    uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		SessionID: uuid.MustParse("22222222-2222-2222-2222-222222222222"),
		Roles:     []domain.Role{domain.RoleCustomer},
	}
	now := time.Now().UTC().Truncate(time.Second)
	tok, err := rt.svc.Access.Sign(p, now, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	want, err := authcrypto.NewHS256([]byte(testJWTSecret))
	if err != nil {
		t.Fatal(err)
	}
	got, err := want.Parse(tok)
	if err != nil {
		t.Fatalf("Parse with configured secret: %v", err)
	}
	if got.UserID != p.UserID || got.SessionID != p.SessionID {
		t.Fatalf("claims: %+v", got)
	}

	other, err := authcrypto.NewHS256([]byte("ffffffffffffffffffffffffffffffff"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Parse(tok); err == nil {
		t.Fatal("Parse with a different secret succeeded")
	}
}

func TestNewAuthRuntimeShortSecret(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, "postgres://paycore_app:x@127.0.0.1:1/paycore?sslmode=disable")
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	defer pool.Close()

	short := testJWTSecret[:31]
	_, err = newAuthRuntime(pool, short)
	if err == nil {
		t.Fatal("short secret accepted")
	}
	if strings.Contains(err.Error(), short) {
		t.Errorf("secret leaked in error: %v", err)
	}
}

func waitForLog(t *testing.T, lines <-chan map[string]any, msg string) map[string]any {
	t.Helper()
	timeout := time.After(3 * time.Second)
	for {
		select {
		case rec, ok := <-lines:
			if !ok {
				t.Fatalf("log %q not found", msg)
			}
			if rec["msg"] == msg {
				return rec
			}
		case <-timeout:
			t.Fatalf("timed out waiting for log %q", msg)
		}
	}
}
