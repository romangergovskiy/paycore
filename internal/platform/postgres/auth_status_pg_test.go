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
	"github.com/jackc/pgx/v5/pgconn"

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
			s.Attempts, s.Limiter, s.History = d.Attempts, d.Limiter, d.History
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
		if n := countStatusHistory(t, ctx, app, cust.ID); n != 0 {
			t.Fatalf("history after rollback = %d", n)
		}
	})

	t.Run("status history append-only and reason", func(t *testing.T) {
		clearActiveAdmins(t, ctx, app, clock)
		admin := seedPGAdmin(t, ctx, app, clock, pgEmail(t, "hist-a"))
		cust, err := application.RegisterIn(ctx, uow, svc, pgRegister(pgEmail(t, "hist-c")))
		if err != nil {
			t.Fatal(err)
		}
		cmd := pgStatus(adminPrincipal(admin), cust.ID)
		cmd.Reason = "aml review"
		if err := application.LockUserIn(ctx, uow, svc, cmd); err != nil {
			t.Fatal(err)
		}
		assertLatestHistory(t, ctx, app, cust.ID, admin.ID, domain.StatusActive, domain.StatusLocked, "aml review")
		if err := application.LockUserIn(ctx, uow, svc, cmd); err != nil {
			t.Fatal(err)
		}
		if n := countStatusHistory(t, ctx, app, cust.ID); n != 1 {
			t.Fatalf("idempotent history = %d", n)
		}
		if err := application.UnlockUserIn(ctx, uow, svc, cmd); err != nil {
			t.Fatal(err)
		}
		assertLatestHistory(t, ctx, app, cust.ID, admin.ID, domain.StatusLocked, domain.StatusActive, "aml review")
		if err := application.DisableUserIn(ctx, uow, svc, cmd); err != nil {
			t.Fatal(err)
		}
		assertLatestHistory(t, ctx, app, cust.ID, admin.ID, domain.StatusActive, domain.StatusDisabled, "aml review")

		_, err = app.Exec(ctx, `UPDATE auth.user_status_history SET reason = 'x' WHERE user_id = $1`, cust.ID)
		if err == nil {
			t.Fatal("UPDATE history succeeded")
		}
		_, err = app.Exec(ctx, `DELETE FROM auth.user_status_history WHERE user_id = $1`, cust.ID)
		if err == nil {
			t.Fatal("DELETE history succeeded")
		}

		before := countStatusHistory(t, ctx, app, admin.ID)
		if err := application.LockUserIn(ctx, uow, svc, pgStatus(adminPrincipal(admin), admin.ID)); !errors.Is(err, domain.ErrLastAdmin) {
			t.Fatalf("last: %v", err)
		}
		if n := countStatusHistory(t, ctx, app, admin.ID); n != before {
			t.Fatalf("last-admin history %d -> %d", before, n)
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

func isDeadlock(err error) bool {
	if err == nil {
		return false
	}
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "40P01" {
		return true
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "deadlock detected")
}

func TestAuthAdminMembershipRaces(t *testing.T) {
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

	clock := &pgClock{t: time.Date(2026, 11, 8, 5, 0, 0, 0, time.UTC)}
	svc := pgAuthService(clock)
	uow := NewAuthUoW(app)

	t.Run("A grant ADMIN and LockUser serialize", func(t *testing.T) {
		clearActiveAdmins(t, ctx, app, clock)
		actor := seedPGAdmin(t, ctx, app, clock, pgEmail(t, "ra-a"))
		cust, err := application.RegisterIn(ctx, NewAuthUoW(app), svc, pgRegister(pgEmail(t, "ra-b")))
		if err != nil {
			t.Fatal(err)
		}
		held := beginAdminAdvisory(t, ctx, app)
		errs := make([]error, 2)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			errs[0] = application.GrantRoleIn(ctx, NewAuthUoW(connB), pgAuthService(clock), adminPrincipal(actor), cust.ID, domain.RoleAdmin)
		}()
		go func() {
			defer wg.Done()
			errs[1] = application.LockUserIn(ctx, NewAuthUoW(connC), pgAuthService(clock), pgStatus(adminPrincipal(actor), cust.ID))
		}()
		waitAdvisoryWaiters(t, ctx, held, 2)
		if err := held.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("op %d: %v (all %v)", i, err, errs)
			}
		}
		assertActorActiveAdmin(t, ctx, app, actor.ID)
		got, err := NewAuthUsers(app).Get(ctx, cust.ID)
		if err != nil {
			t.Fatal(err)
		}
		has, err := NewAuthRoles(app).Has(ctx, cust.ID, domain.RoleAdmin)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status == domain.StatusActive && !has {
			t.Fatalf("neither grant nor lock applied: %+v hasADMIN=%v", got, has)
		}
		assertAdvisoryReleased(t, ctx, app)
	})

	t.Run("B grant ADMIN and lockout keep last ADMIN", func(t *testing.T) {
		clearActiveAdmins(t, ctx, app, clock)
		clock.t = clock.t.Add(domain.RateWindow)
		actor := seedPGAdmin(t, ctx, app, clock, pgEmail(t, "rb-a"))
		cust, err := application.RegisterIn(ctx, NewAuthUoW(app), svc, pgRegister(pgEmail(t, "rb-b")))
		if err != nil {
			t.Fatal(err)
		}
		setFailedLogins(t, ctx, app, cust.ID, domain.LockAfter-1, clock.t)
		held := beginAdminAdvisory(t, ctx, app)
		grantErr := make(chan error, 1)
		go func() {
			grantErr <- application.GrantRoleIn(ctx, NewAuthUoW(connB), pgAuthService(clock), adminPrincipal(actor), cust.ID, domain.RoleAdmin)
		}()
		waitAdvisoryWaiters(t, ctx, held, 1)
		cmd := pgLogin(cust.Email)
		cmd.Password = "wrongpassword!"
		_, loginErr := application.LoginIn(ctx, NewAuthUoW(connC), pgAuthService(clock), cmd)
		if err := held.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
		if err := <-grantErr; err != nil {
			t.Fatalf("grant: %v", err)
		}
		if !errors.Is(loginErr, domain.ErrInvalidCredentials) {
			t.Fatalf("login: %v", loginErr)
		}
		assertActorActiveAdmin(t, ctx, app, actor.ID)
		assertAdvisoryReleased(t, ctx, app)
	})

	t.Run("C revoke ADMIN and LockUser serialize", func(t *testing.T) {
		clearActiveAdmins(t, ctx, app, clock)
		actor := seedPGAdmin(t, ctx, app, clock, pgEmail(t, "rc-a"))
		target := seedPGAdmin(t, ctx, app, clock, pgEmail(t, "rc-b"))
		held := beginAdminAdvisory(t, ctx, app)
		errs := make([]error, 2)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			errs[0] = application.RevokeRoleIn(ctx, NewAuthUoW(connB), pgAuthService(clock), adminPrincipal(actor), target.ID, domain.RoleAdmin)
		}()
		go func() {
			defer wg.Done()
			errs[1] = application.LockUserIn(ctx, NewAuthUoW(connC), pgAuthService(clock), pgStatus(adminPrincipal(actor), target.ID))
		}()
		waitAdvisoryWaiters(t, ctx, held, 2)
		if err := held.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
		wg.Wait()
		for i, err := range errs {
			if err != nil && !errors.Is(err, domain.ErrLastAdmin) {
				t.Fatalf("op %d: %v (all %v)", i, err, errs)
			}
		}
		assertActorActiveAdmin(t, ctx, app, actor.ID)
		got, err := NewAuthUsers(app).Get(ctx, target.ID)
		if err != nil {
			t.Fatal(err)
		}
		has, err := NewAuthRoles(app).Has(ctx, target.ID, domain.RoleAdmin)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status == domain.StatusActive && has {
			t.Fatalf("neither revoke nor lock applied: %+v", got)
		}
		assertAdvisoryReleased(t, ctx, app)
	})

	t.Run("C revoke ADMIN and DisableUser serialize", func(t *testing.T) {
		clearActiveAdmins(t, ctx, app, clock)
		actor := seedPGAdmin(t, ctx, app, clock, pgEmail(t, "rdc-a"))
		target := seedPGAdmin(t, ctx, app, clock, pgEmail(t, "rdc-b"))
		held := beginAdminAdvisory(t, ctx, app)
		errs := make([]error, 2)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			errs[0] = application.RevokeRoleIn(ctx, NewAuthUoW(connB), pgAuthService(clock), adminPrincipal(actor), target.ID, domain.RoleAdmin)
		}()
		go func() {
			defer wg.Done()
			errs[1] = application.DisableUserIn(ctx, NewAuthUoW(connC), pgAuthService(clock), pgStatus(adminPrincipal(actor), target.ID))
		}()
		waitAdvisoryWaiters(t, ctx, held, 2)
		if err := held.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
		wg.Wait()
		for i, err := range errs {
			if err != nil && !errors.Is(err, domain.ErrLastAdmin) {
				t.Fatalf("op %d: %v (all %v)", i, err, errs)
			}
		}
		assertActorActiveAdmin(t, ctx, app, actor.ID)
		assertAdvisoryReleased(t, ctx, app)
	})

	t.Run("D customer login does not wait on advisory", func(t *testing.T) {
		email := pgEmail(t, "rd")
		if _, err := application.RegisterIn(ctx, NewAuthUoW(app), svc, pgRegister(email)); err != nil {
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

	t.Run("LockUser waits on advisory even for CUSTOMER", func(t *testing.T) {
		clearActiveAdmins(t, ctx, app, clock)
		actor := seedPGAdmin(t, ctx, app, clock, pgEmail(t, "rw-a"))
		cust, err := application.RegisterIn(ctx, NewAuthUoW(app), svc, pgRegister(pgEmail(t, "rw-b")))
		if err != nil {
			t.Fatal(err)
		}
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
		err = application.LockUserIn(ctx, NewAuthUoW(connC), pgAuthService(clock), pgStatus(adminPrincipal(actor), cust.ID))
		if err == nil || errors.Is(err, domain.ErrLastAdmin) {
			_ = held.Rollback(ctx)
			t.Fatalf("LockUser skipped advisory: %v", err)
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
		if err := application.LockUserIn(ctx, NewAuthUoW(connC), pgAuthService(clock), pgStatus(adminPrincipal(actor), cust.ID)); err != nil {
			t.Fatalf("after release: %v", err)
		}
		got, err := NewAuthUsers(app).Get(ctx, cust.ID)
		if err != nil || got.Status != domain.StatusLocked {
			t.Fatalf("locked %+v %v", got, err)
		}
	})

	t.Run("Grant ADMIN waits on user FOR UPDATE", func(t *testing.T) {
		clearActiveAdmins(t, ctx, app, clock)
		actor := seedPGAdmin(t, ctx, app, clock, pgEmail(t, "rg-a"))
		cust, err := application.RegisterIn(ctx, NewAuthUoW(app), svc, pgRegister(pgEmail(t, "rg-b")))
		if err != nil {
			t.Fatal(err)
		}
		held, err := app.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := NewAuthUsers(held).GetForUpdate(ctx, cust.ID); err != nil {
			_ = held.Rollback(ctx)
			t.Fatal(err)
		}
		if _, err := connC.Exec(ctx, `SET lock_timeout = '5s'`); err != nil {
			_ = held.Rollback(ctx)
			t.Fatal(err)
		}
		err = application.GrantRoleIn(ctx, NewAuthUoW(connC), pgAuthService(clock), adminPrincipal(actor), cust.ID, domain.RoleAdmin)
		if err == nil {
			_ = held.Rollback(ctx)
			t.Fatal("Grant ADMIN ignored user FOR UPDATE")
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
		if err := application.GrantRoleIn(ctx, NewAuthUoW(connC), pgAuthService(clock), adminPrincipal(actor), cust.ID, domain.RoleAdmin); err != nil {
			t.Fatalf("after release: %v", err)
		}
	})

	t.Run("EnableUser history only DISABLED to ACTIVE", func(t *testing.T) {
		clearActiveAdmins(t, ctx, app, clock)
		admin := seedPGAdmin(t, ctx, app, clock, pgEmail(t, "en-a"))
		cust, err := application.RegisterIn(ctx, uow, svc, pgRegister(pgEmail(t, "en-c")))
		if err != nil {
			t.Fatal(err)
		}
		cmd := pgStatus(adminPrincipal(admin), cust.ID)
		if err := application.EnableUserIn(ctx, uow, svc, cmd); err != nil {
			t.Fatal(err)
		}
		if n := countStatusHistory(t, ctx, app, cust.ID); n != 0 {
			t.Fatalf("ACTIVE no-op history = %d", n)
		}
		if err := application.LockUserIn(ctx, uow, svc, cmd); err != nil {
			t.Fatal(err)
		}
		beforeLock := countStatusHistory(t, ctx, app, cust.ID)
		if err := application.EnableUserIn(ctx, uow, svc, cmd); !errors.Is(err, domain.ErrInvalidRequest) {
			t.Fatalf("enable LOCKED: %v", err)
		}
		if n := countStatusHistory(t, ctx, app, cust.ID); n != beforeLock {
			t.Fatalf("LOCKED enable history %d -> %d", beforeLock, n)
		}
		got, err := NewAuthUsers(app).Get(ctx, cust.ID)
		if err != nil || got.Status != domain.StatusLocked {
			t.Fatalf("still locked %+v %v", got, err)
		}
		if err := application.UnlockUserIn(ctx, uow, svc, cmd); err != nil {
			t.Fatal(err)
		}
		if err := application.DisableUserIn(ctx, uow, svc, cmd); err != nil {
			t.Fatal(err)
		}
		assertLatestHistory(t, ctx, app, cust.ID, admin.ID, domain.StatusActive, domain.StatusDisabled, "policy")
		beforeEnable := countStatusHistory(t, ctx, app, cust.ID)
		if err := application.UnlockUserIn(ctx, uow, svc, cmd); !errors.Is(err, domain.ErrInvalidRequest) {
			t.Fatalf("unlock DISABLED: %v", err)
		}
		if n := countStatusHistory(t, ctx, app, cust.ID); n != beforeEnable {
			t.Fatalf("unlock DISABLED history %d -> %d", beforeEnable, n)
		}
		if err := application.EnableUserIn(ctx, uow, svc, cmd); err != nil {
			t.Fatal(err)
		}
		if n := countStatusHistory(t, ctx, app, cust.ID); n != beforeEnable+1 {
			t.Fatalf("enable history %d want %d", n, beforeEnable+1)
		}
		assertLatestHistory(t, ctx, app, cust.ID, admin.ID, domain.StatusDisabled, domain.StatusActive, "policy")
		got, err = NewAuthUsers(app).Get(ctx, cust.ID)
		if err != nil || got.Status != domain.StatusActive || got.LockedAt != nil || got.FailedLoginCount != 0 {
			t.Fatalf("enabled %+v %v", got, err)
		}
		if err := application.EnableUserIn(ctx, uow, svc, cmd); err != nil {
			t.Fatal(err)
		}
		if n := countStatusHistory(t, ctx, app, cust.ID); n != beforeEnable+1 {
			t.Fatalf("second enable history %d", n)
		}
	})

	t.Run("EnableUser last-ADMIN disable still has no history", func(t *testing.T) {
		clearActiveAdmins(t, ctx, app, clock)
		admin := seedPGAdmin(t, ctx, app, clock, pgEmail(t, "en-last"))
		before := countStatusHistory(t, ctx, app, admin.ID)
		if err := application.DisableUserIn(ctx, uow, svc, pgStatus(adminPrincipal(admin), admin.ID)); !errors.Is(err, domain.ErrLastAdmin) {
			t.Fatalf("last disable: %v", err)
		}
		if n := countStatusHistory(t, ctx, app, admin.ID); n != before {
			t.Fatalf("ErrLastAdmin history %d -> %d", before, n)
		}
	})

	t.Run("EnableUser rollback discards history", func(t *testing.T) {
		clearActiveAdmins(t, ctx, app, clock)
		admin := seedPGAdmin(t, ctx, app, clock, pgEmail(t, "en-rb-a"))
		cust, err := application.RegisterIn(ctx, uow, svc, pgRegister(pgEmail(t, "en-rb-c")))
		if err != nil {
			t.Fatal(err)
		}
		cmd := pgStatus(adminPrincipal(admin), cust.ID)
		if err := application.DisableUserIn(ctx, uow, svc, cmd); err != nil {
			t.Fatal(err)
		}
		before := countStatusHistory(t, ctx, app, cust.ID)
		err = uow.Do(ctx, func(ctx context.Context, d application.Deps) (application.Decision, error) {
			s := pgAuthService(clock)
			s.Users, s.Sessions, s.Tokens, s.Roles = d.Users, d.Sessions, d.Tokens, d.Roles
			s.Attempts, s.Limiter, s.History = d.Attempts, d.Limiter, d.History
			dec, err := s.EnableUser(ctx, cmd)
			if err != nil {
				return dec, err
			}
			return application.Rollback, errors.New("forced")
		})
		if err == nil || err.Error() != "forced" {
			t.Fatalf("got %v", err)
		}
		got, err := NewAuthUsers(app).Get(ctx, cust.ID)
		if err != nil || got.Status != domain.StatusDisabled {
			t.Fatalf("partial enable %+v %v", got, err)
		}
		if n := countStatusHistory(t, ctx, app, cust.ID); n != before {
			t.Fatalf("history after rollback %d want %d", n, before)
		}
	})

	t.Run("EnableUser waits on advisory even for CUSTOMER", func(t *testing.T) {
		clearActiveAdmins(t, ctx, app, clock)
		actor := seedPGAdmin(t, ctx, app, clock, pgEmail(t, "en-w-a"))
		cust, err := application.RegisterIn(ctx, NewAuthUoW(app), svc, pgRegister(pgEmail(t, "en-w-b")))
		if err != nil {
			t.Fatal(err)
		}
		cmd := pgStatus(adminPrincipal(actor), cust.ID)
		if err := application.DisableUserIn(ctx, NewAuthUoW(app), svc, cmd); err != nil {
			t.Fatal(err)
		}
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
		err = application.EnableUserIn(ctx, NewAuthUoW(connC), pgAuthService(clock), cmd)
		if err == nil {
			_ = held.Rollback(ctx)
			t.Fatal("EnableUser skipped advisory")
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
		if err := application.EnableUserIn(ctx, NewAuthUoW(connC), pgAuthService(clock), cmd); err != nil {
			t.Fatalf("after release: %v", err)
		}
		got, err := NewAuthUsers(app).Get(ctx, cust.ID)
		if err != nil || got.Status != domain.StatusActive {
			t.Fatalf("enabled %+v %v", got, err)
		}
	})

	t.Run("EnableUser takes advisory before user FOR UPDATE", func(t *testing.T) {
		clearActiveAdmins(t, ctx, app, clock)
		actor := seedPGAdmin(t, ctx, app, clock, pgEmail(t, "en-ord-a"))
		cust, err := application.RegisterIn(ctx, NewAuthUoW(app), svc, pgRegister(pgEmail(t, "en-ord-b")))
		if err != nil {
			t.Fatal(err)
		}
		cmd := pgStatus(adminPrincipal(actor), cust.ID)
		if err := application.DisableUserIn(ctx, NewAuthUoW(app), svc, cmd); err != nil {
			t.Fatal(err)
		}
		held, err := app.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := NewAuthUsers(held).GetForUpdate(ctx, cust.ID); err != nil {
			_ = held.Rollback(ctx)
			t.Fatal(err)
		}
		const waiterName = "paycore-enable-order"
		setAppName(t, ctx, connC, waiterName)
		enableErr := make(chan error, 1)
		go func() {
			enableErr <- application.EnableUserIn(ctx, NewAuthUoW(connC), pgAuthService(clock), cmd)
		}()
		waitBackendLockWait(t, ctx, app, waiterName)
		probe, err := connB.Begin(ctx)
		if err != nil {
			_ = held.Rollback(ctx)
			t.Fatal(err)
		}
		if tryAdminAdvisoryLock(t, ctx, probe) {
			_ = probe.Rollback(ctx)
			_ = held.Rollback(ctx)
			t.Fatal("EnableUser waited on user without holding ADMIN advisory")
		}
		if err := probe.Rollback(ctx); err != nil {
			_ = held.Rollback(ctx)
			t.Fatal(err)
		}
		if err := held.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
		if err := <-enableErr; err != nil {
			t.Fatalf("enable after user release: %v", err)
		}
	})

	t.Run("Unlock Grant Lock Disable Enable wait on one advisory", func(t *testing.T) {
		clearActiveAdmins(t, ctx, app, clock)
		actor := seedPGAdmin(t, ctx, app, clock, pgEmail(t, "mix-a"))
		cust, err := application.RegisterIn(ctx, NewAuthUoW(app), svc, pgRegister(pgEmail(t, "mix-b")))
		if err != nil {
			t.Fatal(err)
		}
		held := beginAdminAdvisory(t, ctx, app)
		errs := make([]error, 3)
		var wg sync.WaitGroup
		wg.Add(3)
		go func() {
			defer wg.Done()
			errs[0] = application.GrantRoleIn(ctx, NewAuthUoW(connB), pgAuthService(clock), adminPrincipal(actor), cust.ID, domain.RoleAdmin)
		}()
		go func() {
			defer wg.Done()
			errs[1] = application.LockUserIn(ctx, NewAuthUoW(connC), pgAuthService(clock), pgStatus(adminPrincipal(actor), cust.ID))
		}()
		go func() {
			defer wg.Done()
			errs[2] = application.EnableUserIn(ctx, NewAuthUoW(connD), pgAuthService(clock), pgStatus(adminPrincipal(actor), cust.ID))
		}()
		waitAdvisoryWaiters(t, ctx, held, 3)
		if err := held.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
		wg.Wait()
		for i, err := range errs {
			if err != nil && !errors.Is(err, domain.ErrInvalidRequest) && !errors.Is(err, domain.ErrLastAdmin) {
				t.Fatalf("op %d: %v (all %v)", i, err, errs)
			}
		}
		assertAdvisoryReleased(t, ctx, app)
	})

	t.Run("advisory released after membership commit", func(t *testing.T) {
		clearActiveAdmins(t, ctx, app, clock)
		actor := seedPGAdmin(t, ctx, app, clock, pgEmail(t, "rr-a"))
		cust, err := application.RegisterIn(ctx, NewAuthUoW(app), svc, pgRegister(pgEmail(t, "rr-b")))
		if err != nil {
			t.Fatal(err)
		}
		if err := application.LockUserIn(ctx, NewAuthUoW(app), svc, pgStatus(adminPrincipal(actor), cust.ID)); err != nil {
			t.Fatal(err)
		}
		probe, err := connB.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !tryAdminAdvisoryLock(t, ctx, probe) {
			_ = probe.Rollback(ctx)
			t.Fatal("lock held after LockUser commit")
		}
		if err := probe.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
	})
}

func countStatusHistory(t *testing.T, ctx context.Context, conn *pgx.Conn, userID uuid.UUID) int {
	t.Helper()
	var n int
	if err := conn.QueryRow(ctx, `
		SELECT count(*) FROM auth.user_status_history WHERE user_id = $1`, userID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func assertLatestHistory(t *testing.T, ctx context.Context, conn *pgx.Conn, userID, actor uuid.UUID, from, to domain.Status, reason string) {
	t.Helper()
	var gotActor uuid.UUID
	var prev, next, gotReason string
	err := conn.QueryRow(ctx, `
		SELECT changed_by, previous_status, new_status, reason
		  FROM auth.user_status_history
		 WHERE user_id = $1
		 ORDER BY created_at DESC, id DESC
		 LIMIT 1`, userID).Scan(&gotActor, &prev, &next, &gotReason)
	if err != nil {
		t.Fatal(err)
	}
	if gotActor != actor || prev != string(from) || next != string(to) || gotReason != reason {
		t.Fatalf("history actor=%s %s->%s %q", gotActor, prev, next, gotReason)
	}
}

func beginAdminAdvisory(t *testing.T, ctx context.Context, conn *pgx.Conn) pgx.Tx {
	t.Helper()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := NewAuthRoles(tx).LockAdminRoleMutations(ctx); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	return tx
}

func waitAdvisoryWaiters(t *testing.T, ctx context.Context, q interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}, n int) {
	t.Helper()
	deadline, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		var got int
		err := q.QueryRow(deadline, `
			SELECT count(*)::int
			  FROM pg_locks
			 WHERE locktype = 'advisory'
			   AND classid = (($1::bigint >> 32) & 4294967295)::oid
			   AND objid = ($1::bigint & 4294967295)::oid
			   AND objsubid = 1
			   AND NOT granted`, adminRoleMutationLockKey).Scan(&got)
		if err != nil {
			t.Fatal(err)
		}
		if got >= n {
			return
		}
		select {
		case <-deadline.Done():
			t.Fatalf("waiting for %d advisory waiters, have %d: %v", n, got, deadline.Err())
		case <-ticker.C:
		}
	}
}

func assertAdvisoryReleased(t *testing.T, ctx context.Context, conn *pgx.Conn) {
	t.Helper()
	probe, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !tryAdminAdvisoryLock(t, ctx, probe) {
		_ = probe.Rollback(ctx)
		t.Fatal("ADMIN advisory still held")
	}
	if err := probe.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
}

func assertActorActiveAdmin(t *testing.T, ctx context.Context, conn *pgx.Conn, id uuid.UUID) {
	t.Helper()
	if countActiveAdmins(t, ctx, conn) < 1 {
		t.Fatal("ACTIVE ADMIN invariant broken")
	}
	got, err := NewAuthUsers(conn).Get(ctx, id)
	if err != nil || got.Status != domain.StatusActive {
		t.Fatalf("actor %+v %v", got, err)
	}
	has, err := NewAuthRoles(conn).Has(ctx, id, domain.RoleAdmin)
	if err != nil || !has {
		t.Fatalf("actor ADMIN %v %v", has, err)
	}
}
