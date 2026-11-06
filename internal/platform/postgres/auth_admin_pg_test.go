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
)

func TestAuthAdminRoleSerialization(t *testing.T) {
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
	connC := mustConnect(t, ctx, c.app)
	defer connC.Close(ctx)

	clock := &pgClock{t: time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)}
	svc := application.Service{Clock: clock}

	t.Run("advisory xact lock without UPDATE on roles", func(t *testing.T) {
		var canUpdate bool
		if err := app.QueryRow(ctx, `SELECT has_table_privilege(current_user, 'auth.roles', 'UPDATE')`).Scan(&canUpdate); err != nil {
			t.Fatal(err)
		}
		if canUpdate {
			t.Fatal("paycore_app has UPDATE on auth.roles")
		}
		tx, err := app.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		_, err = tx.Exec(ctx, `SELECT code FROM auth.roles WHERE code = 'ADMIN' FOR UPDATE`)
		if err == nil {
			t.Fatal("FOR UPDATE on auth.roles succeeded")
		}
		if !strings.Contains(strings.ToLower(err.Error()), "permission denied") {
			t.Fatalf("FOR UPDATE error = %v", err)
		}
		if _, err := tx.Exec(ctx, lockAdminRoleMutationsSQL, adminRoleMutationLockKey); err != nil {
			t.Fatalf("pg_advisory_xact_lock: %v", err)
		}
	})

	t.Run("cycle revoke keeps at least one ACTIVE ADMIN", func(t *testing.T) {
		a := seedPGAdmin(t, ctx, app, clock, pgEmail(t, "aa"))
		b := seedPGAdmin(t, ctx, app, clock, pgEmail(t, "ab"))
		cUser := seedPGAdmin(t, ctx, app, clock, pgEmail(t, "ac"))
		pa, pb, pc := adminPrincipal(a), adminPrincipal(b), adminPrincipal(cUser)

		uowA := NewAuthUoW(app)
		uowB := NewAuthUoW(connB)
		uowC := NewAuthUoW(connC)
		errs := make([]error, 3)
		var wg sync.WaitGroup
		wg.Add(3)
		go func() {
			defer wg.Done()
			errs[0] = application.RevokeRoleIn(ctx, uowA, svc, pa, b.ID, domain.RoleAdmin)
		}()
		go func() {
			defer wg.Done()
			errs[1] = application.RevokeRoleIn(ctx, uowB, svc, pb, cUser.ID, domain.RoleAdmin)
		}()
		go func() {
			defer wg.Done()
			errs[2] = application.RevokeRoleIn(ctx, uowC, svc, pc, a.ID, domain.RoleAdmin)
		}()
		wg.Wait()

		n := countActiveAdmins(t, ctx, app)
		if n < 1 {
			t.Fatalf("ACTIVE ADMIN = %d after cycle (errs=%v)", n, errs)
		}

		for _, u := range []domain.User{a, b, cUser} {
			has, err := NewAuthRoles(app).Has(ctx, u.ID, domain.RoleAdmin)
			if err != nil {
				t.Fatal(err)
			}
			if has {
				continue
			}
			target := a.ID
			if u.ID == a.ID {
				target = b.ID
			}
			err = application.RevokeRoleIn(ctx, NewAuthUoW(app), svc, adminPrincipal(u), target, domain.RoleAdmin)
			if !errors.Is(err, domain.ErrForbidden) {
				t.Fatalf("lost ADMIN %s still mutated: %v (cycle errs %v)", u.Email, err, errs)
			}
		}
		if n != countActiveAdmins(t, ctx, app) {
			t.Fatal("loser stripped another ADMIN")
		}
	})
}

func seedPGAdmin(t *testing.T, ctx context.Context, conn *pgx.Conn, clock *pgClock, email string) domain.User {
	t.Helper()
	u := domain.User{
		ID:           mustNewID(t),
		Email:        email,
		PasswordHash: testArgon2idPHC,
		FirstName:    "Ad",
		LastName:     "Min",
		Status:       domain.StatusActive,
		CreatedAt:    clock.t,
		UpdatedAt:    clock.t,
	}
	if err := NewAuthUsers(conn).Create(ctx, u); err != nil {
		t.Fatal(err)
	}
	roles := NewAuthRoles(conn)
	if err := roles.Grant(ctx, domain.UserRole{UserID: u.ID, Role: domain.RoleCustomer, GrantedAt: clock.t}); err != nil {
		t.Fatal(err)
	}
	if err := roles.Grant(ctx, domain.UserRole{UserID: u.ID, Role: domain.RoleAdmin, GrantedAt: clock.t}); err != nil {
		t.Fatal(err)
	}
	return u
}

func adminPrincipal(u domain.User) domain.Principal {
	return domain.Principal{UserID: u.ID, Roles: []domain.Role{domain.RoleAdmin, domain.RoleCustomer}}
}

func countActiveAdmins(t *testing.T, ctx context.Context, conn *pgx.Conn) int {
	t.Helper()
	var n int
	if err := conn.QueryRow(ctx, countActiveRoleSQL, string(domain.RoleAdmin)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestAdminRoleMutationLockKeyStable(t *testing.T) {
	if adminRoleMutationLockKey != 0x5041590000000001 {
		t.Fatalf("key changed: 0x%x", adminRoleMutationLockKey)
	}
	if lockAdminRoleMutationsSQL != `SELECT pg_advisory_xact_lock($1)` {
		t.Fatal(lockAdminRoleMutationsSQL)
	}
}
