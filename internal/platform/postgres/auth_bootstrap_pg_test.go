package postgres

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/romangergovskiy/paycore/internal/auth/application"
	"github.com/romangergovskiy/paycore/internal/auth/domain"
	"github.com/romangergovskiy/paycore/internal/platform/authcrypto"
)

func bootstrapAdminService(clock *pgClock) application.Service {
	return application.Service{
		Hasher: authcrypto.NewArgon2idHasher(),
		Issuer: &pgIssuer{},
		Access: pgSigner{},
		Clock:  clock,
	}
}

func TestAuthBootstrapFirstAdminPG(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	c := startCluster(t)
	if err := Bootstrap(ctx, c.admin, c.owner, c.app, c.readonly); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if err := Up(ctx, c.owner); err != nil {
		t.Fatalf("up: %v", err)
	}

	app := mustConnect(t, ctx, c.app)
	defer app.Close(ctx)
	connB := mustConnect(t, ctx, c.app)
	defer connB.Close(ctx)

	clock := &pgClock{t: time.Date(2026, 11, 11, 15, 0, 0, 0, time.UTC)}
	svc := bootstrapAdminService(clock)
	const password = "twelvechars!"

	t.Run("first ADMIN is CUSTOMER+ADMIN with Argon2id", func(t *testing.T) {
		email := pgEmail(t, "boot")
		got, err := application.BootstrapFirstAdminIn(ctx, NewAuthUoW(app), svc, email, password)
		if err != nil {
			t.Fatal(err)
		}
		if got.Email != email || got.Status != domain.StatusActive {
			t.Fatalf("user %+v", got)
		}
		if !containsRole(got.Roles, domain.RoleCustomer) || !containsRole(got.Roles, domain.RoleAdmin) {
			t.Fatalf("roles %v", got.Roles)
		}
		var hash, status string
		var failed int
		if err := app.QueryRow(ctx, `
			SELECT password_hash, status, failed_login_count
			  FROM auth.users WHERE id = $1`, got.ID).Scan(&hash, &status, &failed); err != nil {
			t.Fatal(err)
		}
		if status != "ACTIVE" || failed != 0 {
			t.Fatalf("status=%s failed=%d", status, failed)
		}
		if hash == password || !strings.HasPrefix(hash, "$argon2id$") {
			t.Fatalf("stored hash %q", hash)
		}
		ok, err := authcrypto.NewArgon2idHasher().Verify(password, hash)
		if err != nil || !ok {
			t.Fatalf("verify stored PHC: ok=%v err=%v", ok, err)
		}
		var grantedBy *string
		var n int
		if err := app.QueryRow(ctx, `
			SELECT count(*), min(granted_by::text)
			  FROM auth.user_roles WHERE user_id = $1`, got.ID).Scan(&n, &grantedBy); err != nil {
			t.Fatal(err)
		}
		if n != 2 || grantedBy != nil {
			t.Fatalf("roles n=%d granted_by=%v", n, grantedBy)
		}
		assertEmptyAuthSideTables(t, ctx, app)
		var journals int
		if err := app.QueryRow(ctx, `SELECT count(*) FROM ledger.journal_transactions`).Scan(&journals); err != nil {
			t.Fatal(err)
		}
		if journals != 0 {
			t.Fatalf("ledger journal count %d", journals)
		}
	})

	t.Run("repeat is no-op regardless of email", func(t *testing.T) {
		before := countActiveAdmins(t, ctx, app)
		usersBefore := countUsers(t, ctx, app)
		got, err := application.BootstrapFirstAdminIn(ctx, NewAuthUoW(app), svc, pgEmail(t, "other"), "otherpassword1")
		if err != nil {
			t.Fatal(err)
		}
		if got.ID.String() != "00000000-0000-0000-0000-000000000000" {
			t.Fatalf("no-op user %+v", got)
		}
		if countActiveAdmins(t, ctx, app) != before || countUsers(t, ctx, app) != usersBefore {
			t.Fatal("no-op inserted a user")
		}
	})

	t.Run("occupied email without ADMIN rolls back", func(t *testing.T) {
		clearActiveAdmins(t, ctx, app, clock)
		email := pgEmail(t, "taken")
		cust, err := application.RegisterIn(ctx, NewAuthUoW(app), pgAuthService(clock), pgRegister(email))
		if err != nil {
			t.Fatal(err)
		}
		usersBefore := countUsers(t, ctx, app)
		_, err = application.BootstrapFirstAdminIn(ctx, NewAuthUoW(app), svc, email, password)
		if !errors.Is(err, domain.ErrEmailTaken) {
			t.Fatalf("got %v", err)
		}
		if countUsers(t, ctx, app) != usersBefore {
			t.Fatal("partial user on EMAIL_TAKEN")
		}
		if countActiveAdmins(t, ctx, app) != 0 {
			t.Fatal("ADMIN granted to occupied email")
		}
		roles, err := NewAuthRoles(app).List(ctx, cust.ID)
		if err != nil {
			t.Fatal(err)
		}
		if containsRole(roles, domain.RoleAdmin) {
			t.Fatalf("roles %v", roles)
		}
		assertEmptyAuthSideTables(t, ctx, app)
	})

	t.Run("concurrent bootstraps create one ADMIN", func(t *testing.T) {
		clearActiveAdmins(t, ctx, app, clock)
		usersBefore := countUsers(t, ctx, app)
		emailA := pgEmail(t, "par-a")
		emailB := pgEmail(t, "par-b")
		errs := make([]error, 2)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, errs[0] = application.BootstrapFirstAdminIn(ctx, NewAuthUoW(app), bootstrapAdminService(clock), emailA, password)
		}()
		go func() {
			defer wg.Done()
			_, errs[1] = application.BootstrapFirstAdminIn(ctx, NewAuthUoW(connB), bootstrapAdminService(clock), emailB, password)
		}()
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("goroutine %d: %v", i, err)
			}
		}
		if n := countActiveAdmins(t, ctx, app); n != 1 {
			t.Fatalf("ACTIVE ADMIN count %d", n)
		}
		if countUsers(t, ctx, app) != usersBefore+1 {
			t.Fatalf("users after concurrent bootstrap")
		}
		assertEmptyAuthSideTables(t, ctx, app)
	})

	t.Run("register cannot assign ADMIN", func(t *testing.T) {
		if countActiveAdmins(t, ctx, app) == 0 {
			if _, err := application.BootstrapFirstAdminIn(ctx, NewAuthUoW(app), svc, pgEmail(t, "reg-admin"), password); err != nil {
				t.Fatal(err)
			}
		}
		cmd := pgRegister(pgEmail(t, "reg-cust"))
		cmd.Roles = []domain.Role{domain.RoleAdmin}
		if _, err := application.RegisterIn(ctx, NewAuthUoW(app), pgAuthService(clock), cmd); !errors.Is(err, domain.ErrInvalidRequest) {
			t.Fatalf("register roles: %v", err)
		}
		got, err := application.RegisterIn(ctx, NewAuthUoW(app), pgAuthService(clock), pgRegister(pgEmail(t, "reg-ok")))
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Roles) != 1 || got.Roles[0] != domain.RoleCustomer {
			t.Fatalf("roles %v", got.Roles)
		}
	})
}

func countUsers(t *testing.T, ctx context.Context, conn *pgx.Conn) int {
	t.Helper()
	var n int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM auth.users`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func assertEmptyAuthSideTables(t *testing.T, ctx context.Context, conn *pgx.Conn) {
	t.Helper()
	for _, q := range []string{
		`SELECT count(*) FROM auth.sessions`,
		`SELECT count(*) FROM auth.refresh_tokens`,
		`SELECT count(*) FROM auth.login_attempts`,
		`SELECT count(*) FROM auth.user_status_history`,
	} {
		var n int
		if err := conn.QueryRow(ctx, q).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%s = %d %v", q, n, err)
		}
	}
}

func containsRole(roles []domain.Role, want domain.Role) bool {
	for _, r := range roles {
		if r == want {
			return true
		}
	}
	return false
}
