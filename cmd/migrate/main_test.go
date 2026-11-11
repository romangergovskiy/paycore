package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/romangergovskiy/paycore/internal/auth/domain"
	"github.com/romangergovskiy/paycore/internal/platform/config"
	"github.com/romangergovskiy/paycore/internal/platform/postgres"
)

func TestRunUsage(t *testing.T) {
	if err := run(nil, nil); !errors.Is(err, errUsage) {
		t.Fatalf("got %v", err)
	}
	if err := run([]string{"bootstrap-admin", "a@b.c", "secret-password"}, nil); !errors.Is(err, errUsage) {
		t.Fatalf("argv secrets: %v", err)
	}
	if err := run([]string{"nope"}, nil); !errors.Is(err, errUsage) {
		t.Fatalf("got %v", err)
	}
	if exitStatus(errUsage) != 2 {
		t.Fatalf("usage exit %d", exitStatus(errUsage))
	}
}

func TestParseBootstrapAdminEnv(t *testing.T) {
	password := "twelvechars!"
	_, err := parseBootstrapAdminEnv(nil)
	if !errors.Is(err, config.ErrInvalid) {
		t.Fatalf("got %v", err)
	}
	if !strings.Contains(err.Error(), envBootstrapAdminEmail) || !strings.Contains(err.Error(), envBootstrapAdminPassword) {
		t.Fatalf("missing keys: %v", err)
	}
	if strings.Contains(err.Error(), password) {
		t.Fatal("error leaked password")
	}
	if exitStatus(err) != 2 {
		t.Fatalf("exit %d", exitStatus(err))
	}

	_, err = parseBootstrapAdminEnv([]string{
		envPostgresDSN + "=postgres://paycore_app:p@127.0.0.1:5432/paycore",
		envBootstrapAdminEmail + "=a@b.c",
		envBootstrapAdminPassword + "=" + password,
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = parseBootstrapAdminEnv([]string{
		envPostgresDSN + "=localhost:5432",
		envBootstrapAdminEmail + "=a@b.c",
		envBootstrapAdminPassword + "=" + password,
	})
	if !errors.Is(err, config.ErrInvalid) || !strings.Contains(err.Error(), envPostgresDSN) {
		t.Fatalf("bad DSN: %v", err)
	}
	if strings.Contains(err.Error(), password) {
		t.Fatal("DSN error leaked password")
	}
}

func TestWithoutBootstrapAdminVars(t *testing.T) {
	password := "twelvechars!"
	environ := []string{
		"PAYCORE_AUTH_JWT_SECRET=0123456789abcdef0123456789abcdef",
		envBootstrapAdminEmail + "=a@b.c",
		envBootstrapAdminPassword + "=" + password,
	}
	filtered := withoutBootstrapAdminVars(environ)
	for _, kv := range filtered {
		if strings.HasPrefix(kv, envBootstrapAdminEmail) || strings.HasPrefix(kv, envBootstrapAdminPassword) {
			t.Fatalf("bootstrap var leaked into Load input: %s", kv)
		}
	}
	if _, err := config.Load(filtered); err != nil {
		t.Fatal(err)
	}
	_, err := config.Load(environ)
	if !errors.Is(err, config.ErrInvalid) {
		t.Fatalf("API Load must reject bootstrap vars: %v", err)
	}
}

func TestEmailTakenExitStatus(t *testing.T) {
	if exitStatus(domain.ErrEmailTaken) != 1 {
		t.Fatalf("EMAIL_TAKEN exit %d", exitStatus(domain.ErrEmailTaken))
	}
}

func TestMigrateBootstrapAdminCLI(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	c := startMigrateCluster(t, ctx)
	if err := postgres.Bootstrap(ctx, c.admin, c.owner, c.app, c.readonly); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if err := postgres.Up(ctx, c.owner); err != nil {
		t.Fatalf("up: %v", err)
	}

	password := "twelvechars!"
	email := "admin@pay.example"
	env := func(e, p string) []string {
		return []string{
			envPostgresDSN + "=" + c.app,
			envBootstrapAdminEmail + "=" + e,
			envBootstrapAdminPassword + "=" + p,
		}
	}

	t.Run("first ADMIN", func(t *testing.T) {
		var logs bytes.Buffer
		prev := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
		t.Cleanup(func() { slog.SetDefault(prev) })
		err := run([]string{"bootstrap-admin"}, env(email, password))
		if err != nil {
			t.Fatal(err)
		}
		if exitStatus(err) != 0 {
			t.Fatalf("exit %d", exitStatus(err))
		}
		dump := logs.String()
		if strings.Contains(dump, password) || strings.Contains(dump, "$argon2id$") {
			t.Fatalf("log leaked secret: %s", dump)
		}
		pool := mustAppPool(t, ctx, c.app)
		defer pool.Close()
		var n, admins int
		var hash string
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM auth.users`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if err := pool.QueryRow(ctx, `
			SELECT count(*) FROM auth.user_roles ur
			  JOIN auth.users u ON u.id = ur.user_id
			 WHERE ur.role_code = 'ADMIN' AND u.status = 'ACTIVE'`).Scan(&admins); err != nil {
			t.Fatal(err)
		}
		if n != 1 || admins != 1 {
			t.Fatalf("users=%d admins=%d", n, admins)
		}
		if err := pool.QueryRow(ctx, `SELECT password_hash FROM auth.users`).Scan(&hash); err != nil {
			t.Fatal(err)
		}
		if hash == password || !strings.HasPrefix(hash, "$argon2id$") {
			t.Fatalf("hash %q", hash)
		}
		var sessions, tokens int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM auth.sessions`).Scan(&sessions); err != nil {
			t.Fatal(err)
		}
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM auth.refresh_tokens`).Scan(&tokens); err != nil {
			t.Fatal(err)
		}
		if sessions != 0 || tokens != 0 {
			t.Fatalf("sessions=%d tokens=%d", sessions, tokens)
		}
	})

	t.Run("repeat no-op", func(t *testing.T) {
		err := run([]string{"bootstrap-admin"}, env("other@pay.example", "otherpassword1"))
		if err != nil || exitStatus(err) != 0 {
			t.Fatalf("err=%v exit=%d", err, exitStatus(err))
		}
		pool := mustAppPool(t, ctx, c.app)
		defer pool.Close()
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM auth.users`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("users %d", n)
		}
	})

	t.Run("occupied email exit 1", func(t *testing.T) {
		pool := mustAppPool(t, ctx, c.app)
		defer pool.Close()
		if _, err := pool.Exec(ctx, `DELETE FROM auth.user_roles`); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE auth.users SET status = 'DISABLED'`); err != nil {
			t.Fatal(err)
		}
		id, err := uuid.NewV7()
		if err != nil {
			t.Fatal(err)
		}
		taken := "taken@pay.example"
		if _, err := pool.Exec(ctx, `
			INSERT INTO auth.users (id, email, password_hash, status)
			VALUES ($1, $2, $3, 'ACTIVE')`,
			id, taken, "$argon2id$v=19$m=19456,t=2,p=1$Zml4dHVyZXNhbHQxMjM0$Zml4dHVyZWhhc2hmaXh0dXJlaGFzaGZpeHR1cmVoYXNo",
		); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO auth.user_roles (user_id, role_code, granted_at)
			VALUES ($1, 'CUSTOMER', now())`, id); err != nil {
			t.Fatal(err)
		}
		var usersBefore int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM auth.users`).Scan(&usersBefore); err != nil {
			t.Fatal(err)
		}
		err = run([]string{"bootstrap-admin"}, env(taken, password))
		if !errors.Is(err, domain.ErrEmailTaken) {
			t.Fatalf("got %v", err)
		}
		if exitStatus(err) != 1 {
			t.Fatalf("exit %d", exitStatus(err))
		}
		if strings.Contains(err.Error(), password) || strings.Contains(err.Error(), "$argon2id$") {
			t.Fatalf("error leaked secret: %v", err)
		}
		var usersAfter, admins int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM auth.users`).Scan(&usersAfter); err != nil {
			t.Fatal(err)
		}
		if err := pool.QueryRow(ctx, `
			SELECT count(*) FROM auth.user_roles WHERE role_code = 'ADMIN'`).Scan(&admins); err != nil {
			t.Fatal(err)
		}
		if usersAfter != usersBefore || admins != 0 {
			t.Fatalf("partial write users=%d→%d admins=%d", usersBefore, usersAfter, admins)
		}
	})
}

type migrateCluster struct {
	admin, owner, app, readonly string
}

func startMigrateCluster(t *testing.T, ctx context.Context) migrateCluster {
	t.Helper()
	ctr, err := tpostgres.Run(ctx, "postgres:16-alpine",
		tpostgres.WithDatabase("paycore"),
		tpostgres.WithUsername("postgres"),
		tpostgres.WithPassword("postgres"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(time.Minute),
		),
	)
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	t.Cleanup(func() {
		_ = ctr.Terminate(context.Background())
	})
	admin, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	base, err := url.Parse(admin)
	if err != nil {
		t.Fatal(err)
	}
	return migrateCluster{
		admin:    admin,
		owner:    withUser(base, postgres.RoleOwner, "owner-pass"),
		app:      withUser(base, postgres.RoleApp, "app-pass"),
		readonly: withUser(base, postgres.RoleReadonly, "ro-pass"),
	}
}

func mustAppPool(t *testing.T, ctx context.Context, dsn string) *pgxpool.Pool {
	t.Helper()
	pool, err := postgres.Open(ctx, config.Postgres{
		DSN:              dsn,
		MaxConns:         4,
		MinConns:         0,
		MaxConnLifetime:  30 * time.Minute,
		MaxConnIdleTime:  5 * time.Minute,
		LockTimeout:      3 * time.Second,
		StatementTimeout: 5 * time.Second,
	})
	if err != nil || pool == nil {
		t.Fatalf("open app pool: %v", err)
	}
	return pool
}

func withUser(u *url.URL, user, password string) string {
	cp := *u
	cp.User = url.UserPassword(user, password)
	return cp.String()
}
