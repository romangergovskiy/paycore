package postgres

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/romangergovskiy/paycore/internal/auth/application"
	"github.com/romangergovskiy/paycore/internal/auth/domain"
)

func TestAuthAdminUserStatus(t *testing.T) {
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
	connD := mustConnect(t, ctx, c.app)
	defer connD.Close(ctx)

	clock := &pgClock{t: time.Date(2026, 11, 6, 14, 0, 0, 0, time.UTC)}
	svc := pgAuthService(clock)
	uow := NewAuthUoW(app)

	t.Run("last ADMIN lock and disable", func(t *testing.T) {
		clearActiveAdmins(t, ctx, app, clock)
		admin := seedPGAdmin(t, ctx, app, clock, pgEmail(t, "last"))
		cmd := pgStatus(adminPrincipal(admin), admin.ID)
		if err := application.LockUserIn(ctx, uow, svc, cmd); !errors.Is(err, domain.ErrLastAdmin) {
			t.Fatalf("lock: %v", err)
		}
		got, err := NewAuthUsers(app).Get(ctx, admin.ID)
		if err != nil || got.Status != domain.StatusActive {
			t.Fatalf("after lock %+v %v", got, err)
		}
		if err := application.DisableUserIn(ctx, uow, svc, cmd); !errors.Is(err, domain.ErrLastAdmin) {
			t.Fatalf("disable: %v", err)
		}
		got, err = NewAuthUsers(app).Get(ctx, admin.ID)
		if err != nil || got.Status != domain.StatusActive {
			t.Fatalf("after disable %+v %v", got, err)
		}
	})

	t.Run("one of two can lock", func(t *testing.T) {
		clearActiveAdmins(t, ctx, app, clock)
		a1 := seedPGAdmin(t, ctx, app, clock, pgEmail(t, "one-a"))
		a2 := seedPGAdmin(t, ctx, app, clock, pgEmail(t, "one-b"))
		if err := application.LockUserIn(ctx, uow, svc, pgStatus(adminPrincipal(a1), a2.ID)); err != nil {
			t.Fatal(err)
		}
		got, err := NewAuthUsers(app).Get(ctx, a2.ID)
		if err != nil || got.Status != domain.StatusLocked {
			t.Fatalf("locked %+v %v", got, err)
		}
		if err := application.LockUserIn(ctx, uow, svc, pgStatus(adminPrincipal(a1), a1.ID)); !errors.Is(err, domain.ErrLastAdmin) {
			t.Fatalf("last: %v", err)
		}
		left, err := NewAuthUsers(app).Get(ctx, a1.ID)
		if err != nil || left.Status != domain.StatusActive {
			t.Fatalf("remaining %+v %v", left, err)
		}
		if countActiveAdmins(t, ctx, app) < 1 {
			t.Fatal("no ACTIVE ADMIN")
		}
	})

	t.Run("parallel lockout cannot lock both last ADMINs", func(t *testing.T) {
		clearActiveAdmins(t, ctx, app, clock)
		clock.t = clock.t.Add(domain.RateWindow)
		a := seedPGAdmin(t, ctx, app, clock, pgEmail(t, "par-a"))
		b := seedPGAdmin(t, ctx, app, clock, pgEmail(t, "par-b"))
		setFailedLogins(t, ctx, app, a.ID, domain.LockAfter-1, clock.t)
		setFailedLogins(t, ctx, app, b.ID, domain.LockAfter-1, clock.t)

		errs := make([]error, 2)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			cmd := pgLogin(a.Email)
			cmd.Password = "wrongpassword!"
			_, errs[0] = application.LoginIn(ctx, NewAuthUoW(app), pgAuthService(clock), cmd)
		}()
		go func() {
			defer wg.Done()
			cmd := pgLogin(b.Email)
			cmd.Password = "wrongpassword!"
			_, errs[1] = application.LoginIn(ctx, NewAuthUoW(connB), pgAuthService(clock), cmd)
		}()
		wg.Wait()
		for i, err := range errs {
			if !errors.Is(err, domain.ErrInvalidCredentials) {
				t.Fatalf("login %d: %v", i, err)
			}
		}
		n := countActiveAdmins(t, ctx, app)
		if n < 1 {
			t.Fatalf("ACTIVE ADMIN = %d", n)
		}
		ua, _ := NewAuthUsers(app).Get(ctx, a.ID)
		ub, _ := NewAuthUsers(app).Get(ctx, b.ID)
		locked := 0
		if ua.Status == domain.StatusLocked {
			locked++
		}
		if ub.Status == domain.StatusLocked {
			locked++
		}
		if locked != 1 {
			t.Fatalf("locked=%d a=%s b=%s (want exactly one)", locked, ua.Status, ub.Status)
		}
		if ua.FailedLoginCount < domain.LockAfter && ub.FailedLoginCount < domain.LockAfter {
			t.Fatalf("fail counts a=%d b=%d", ua.FailedLoginCount, ub.FailedLoginCount)
		}
		if failAttempts(t, ctx, app, a.ID)+failAttempts(t, ctx, app, b.ID) < 2 {
			t.Fatal("attempts not persisted")
		}
	})

	t.Run("non-ADMIN login does not take advisory", func(t *testing.T) {
		email := pgEmail(t, "noadv")
		if _, err := application.RegisterIn(ctx, uow, svc, pgRegister(email)); err != nil {
			t.Fatal(err)
		}
		held, err := app.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = held.Rollback(ctx) }()
		if err := NewAuthRoles(held).LockAdminRoleMutations(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := connB.Exec(ctx, `SET lock_timeout = '5s'`); err != nil {
			t.Fatal(err)
		}
		if _, err := application.LoginIn(ctx, NewAuthUoW(connB), pgAuthService(clock), pgLogin(email)); err != nil {
			t.Fatalf("customer login blocked on ADMIN advisory: %v", err)
		}
		if _, err := connB.Exec(ctx, `SET lock_timeout = '0'`); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("ADMIN lockout waits on advisory", func(t *testing.T) {
		admin := seedPGAdmin(t, ctx, app, clock, pgEmail(t, "wait"))
		held, err := app.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := NewAuthRoles(held).LockAdminRoleMutations(ctx); err != nil {
			_ = held.Rollback(ctx)
			t.Fatal(err)
		}
		if _, err := connC.Exec(ctx, `SET lock_timeout = '5s'`); err != nil {
			_ = held.Rollback(ctx)
			t.Fatal(err)
		}
		cmd := pgLogin(admin.Email)
		cmd.Password = "wrongpassword!"
		_, err = application.LoginIn(ctx, NewAuthUoW(connC), pgAuthService(clock), cmd)
		if err == nil || errors.Is(err, domain.ErrInvalidCredentials) {
			_ = held.Rollback(ctx)
			t.Fatalf("ADMIN lockout ignored advisory: %v", err)
		}
		if !isLockTimeout(err) {
			_ = held.Rollback(ctx)
			t.Fatalf("want lock timeout, got %v", err)
		}
		if err := held.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := connC.Exec(ctx, `SET lock_timeout = '0'`); err != nil {
			t.Fatal(err)
		}
		_, err = application.LoginIn(ctx, NewAuthUoW(connC), pgAuthService(clock), cmd)
		if !errors.Is(err, domain.ErrInvalidCredentials) {
			t.Fatalf("after release: %v", err)
		}
	})

	t.Run("one of two can disable", func(t *testing.T) {
		clearActiveAdmins(t, ctx, app, clock)
		a1 := seedPGAdmin(t, ctx, app, clock, pgEmail(t, "dis-a"))
		a2 := seedPGAdmin(t, ctx, app, clock, pgEmail(t, "dis-b"))
		if err := application.DisableUserIn(ctx, uow, svc, pgStatus(adminPrincipal(a1), a2.ID)); err != nil {
			t.Fatal(err)
		}
		got, err := NewAuthUsers(app).Get(ctx, a2.ID)
		if err != nil || got.Status != domain.StatusDisabled {
			t.Fatalf("disabled %+v %v", got, err)
		}
		if err := application.DisableUserIn(ctx, uow, svc, pgStatus(adminPrincipal(a1), a1.ID)); !errors.Is(err, domain.ErrLastAdmin) {
			t.Fatalf("last: %v", err)
		}
	})

	t.Run("unlock locked user", func(t *testing.T) {
		clearActiveAdmins(t, ctx, app, clock)
		admin := seedPGAdmin(t, ctx, app, clock, pgEmail(t, "un-a"))
		email := pgEmail(t, "un-c")
		cust, err := application.RegisterIn(ctx, uow, svc, pgRegister(email))
		if err != nil {
			t.Fatal(err)
		}
		if err := application.LockUserIn(ctx, uow, svc, pgStatus(adminPrincipal(admin), cust.ID)); err != nil {
			t.Fatal(err)
		}
		if err := application.UnlockUserIn(ctx, uow, svc, pgStatus(adminPrincipal(admin), cust.ID)); err != nil {
			t.Fatal(err)
		}
		got, err := NewAuthUsers(app).Get(ctx, cust.ID)
		if err != nil || got.Status != domain.StatusActive || got.LockedAt != nil || got.FailedLoginCount != 0 {
			t.Fatalf("unlocked %+v %v", got, err)
		}
	})

	t.Run("rollback discards lock", func(t *testing.T) {
		clearActiveAdmins(t, ctx, app, clock)
		admin := seedPGAdmin(t, ctx, app, clock, pgEmail(t, "rb-a"))
		email := pgEmail(t, "rb-c")
		cust, err := application.RegisterIn(ctx, uow, svc, pgRegister(email))
		if err != nil {
			t.Fatal(err)
		}
		err = uow.Do(ctx, func(ctx context.Context, d application.Deps) (application.Decision, error) {
			s := pgAuthService(clock)
			s.Users, s.Sessions, s.Tokens, s.Roles = d.Users, d.Sessions, d.Tokens, d.Roles
			s.Attempts, s.Limiter = d.Attempts, d.Limiter
			dec, err := s.LockUser(ctx, pgStatus(adminPrincipal(admin), cust.ID))
			if err != nil {
				return dec, err
			}
			return application.Rollback, errors.New("forced")
		})
		if err == nil || err.Error() != "forced" {
			t.Fatalf("got %v", err)
		}
		got, err := NewAuthUsers(app).Get(ctx, cust.ID)
		if err != nil || got.Status != domain.StatusActive {
			t.Fatalf("partial lock %+v %v", got, err)
		}
	})

	t.Run("advisory released after commit and rollback", func(t *testing.T) {
		clearActiveAdmins(t, ctx, app, clock)
		a1 := seedPGAdmin(t, ctx, app, clock, pgEmail(t, "rel-a"))
		a2 := seedPGAdmin(t, ctx, app, clock, pgEmail(t, "rel-b"))
		if err := application.LockUserIn(ctx, uow, svc, pgStatus(adminPrincipal(a1), a2.ID)); err != nil {
			t.Fatal(err)
		}
		probeCommit, err := connB.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !tryAdminAdvisoryLock(t, ctx, probeCommit) {
			_ = probeCommit.Rollback(ctx)
			t.Fatal("lock held after LockUser commit")
		}
		if err := probeCommit.Rollback(ctx); err != nil {
			t.Fatal(err)
		}

		if err := application.LockUserIn(ctx, uow, svc, pgStatus(adminPrincipal(a1), a1.ID)); !errors.Is(err, domain.ErrLastAdmin) {
			t.Fatalf("last: %v", err)
		}
		probeRollback, err := connB.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !tryAdminAdvisoryLock(t, ctx, probeRollback) {
			_ = probeRollback.Rollback(ctx)
			t.Fatal("lock held after last-ADMIN rollback")
		}
		if err := probeRollback.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("no deadlock lockout and status", func(t *testing.T) {
		clearActiveAdmins(t, ctx, app, clock)
		clock.t = clock.t.Add(domain.RateWindow)
		a := seedPGAdmin(t, ctx, app, clock, pgEmail(t, "dl-a"))
		b := seedPGAdmin(t, ctx, app, clock, pgEmail(t, "dl-b"))
		setFailedLogins(t, ctx, app, a.ID, domain.LockAfter-1, clock.t)
		setFailedLogins(t, ctx, app, b.ID, domain.LockAfter-1, clock.t)
		email := pgEmail(t, "dl-c")
		cust, err := application.RegisterIn(ctx, uow, svc, pgRegister(email))
		if err != nil {
			t.Fatal(err)
		}

		errs := make([]error, 4)
		var wg sync.WaitGroup
		wg.Add(4)
		go func() {
			defer wg.Done()
			cmd := pgLogin(a.Email)
			cmd.Password = "wrongpassword!"
			_, errs[0] = application.LoginIn(ctx, NewAuthUoW(app), pgAuthService(clock), cmd)
		}()
		go func() {
			defer wg.Done()
			cmd := pgLogin(b.Email)
			cmd.Password = "wrongpassword!"
			_, errs[1] = application.LoginIn(ctx, NewAuthUoW(connB), pgAuthService(clock), cmd)
		}()
		go func() {
			defer wg.Done()
			errs[2] = application.LockUserIn(ctx, NewAuthUoW(connC), pgAuthService(clock), pgStatus(adminPrincipal(a), cust.ID))
		}()
		go func() {
			defer wg.Done()
			errs[3] = application.LockUserIn(ctx, NewAuthUoW(connD), pgAuthService(clock), pgStatus(adminPrincipal(a), b.ID))
		}()
		wg.Wait()

		if countActiveAdmins(t, ctx, app) < 1 {
			t.Fatalf("ACTIVE ADMIN lost after mixed writes (errs=%v)", errs)
		}
		for i, err := range errs {
			if err == nil || errors.Is(err, domain.ErrInvalidCredentials) || errors.Is(err, domain.ErrLastAdmin) || errors.Is(err, domain.ErrForbidden) {
				continue
			}
			t.Fatalf("op %d: %v (all %v)", i, err, errs)
		}
	})
}

func clearActiveAdmins(t *testing.T, ctx context.Context, conn *pgx.Conn, clock *pgClock) {
	t.Helper()
	rows, err := conn.Query(ctx, `
		SELECT u.id
		  FROM auth.users u
		  JOIN auth.user_roles ur ON ur.user_id = u.id AND ur.role_code = $1
		 WHERE u.status = 'ACTIVE'`, string(domain.RoleAdmin))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	users := NewAuthUsers(conn)
	for _, id := range ids {
		u, err := users.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		u.Status = domain.StatusDisabled
		u.LockedAt = nil
		u.UpdatedAt = clock.t
		if err := users.Update(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
}

func pgStatus(actor domain.Principal, userID uuid.UUID) application.StatusCommand {
	return application.StatusCommand{Actor: actor, UserID: userID, Reason: "policy"}
}

func setFailedLogins(t *testing.T, ctx context.Context, conn *pgx.Conn, id uuid.UUID, n int, at time.Time) {
	t.Helper()
	u, err := NewAuthUsers(conn).Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	u.FailedLoginCount = n
	u.UpdatedAt = at
	if err := NewAuthUsers(conn).Update(ctx, u); err != nil {
		t.Fatal(err)
	}
}

func failAttempts(t *testing.T, ctx context.Context, conn *pgx.Conn, id uuid.UUID) int {
	t.Helper()
	var n int
	if err := conn.QueryRow(ctx, `
		SELECT count(*) FROM auth.login_attempts
		 WHERE user_id = $1 AND success = false`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func isLockTimeout(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "lock timeout") || strings.Contains(s, "55p03")
}
