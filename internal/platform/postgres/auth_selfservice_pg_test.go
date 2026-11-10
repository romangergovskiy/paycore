package postgres

import (
	"context"
	"crypto/sha256"
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

func TestAuthSelfServiceSQL(t *testing.T) {
	if strings.Contains(listSessionsByUserSQL, "password_hash") || strings.Contains(listSessionsByUserSQL, "token_hash") {
		t.Fatal("ListByUser SELECT includes a secret column")
	}
	if !strings.Contains(listSessionsByUserSQL, "ORDER BY created_at DESC, id DESC") {
		t.Fatal("ListByUser sort")
	}
	if strings.Contains(listSessionsByUserSQL, "LIMIT") || strings.Contains(listSessionsByUserSQL, "OFFSET") {
		t.Fatal("ListByUser paginates")
	}
	if !strings.Contains(lockTokensBySessionSQL, "ORDER BY id") || !strings.Contains(lockTokensBySessionSQL, "FOR UPDATE") {
		t.Fatal("LockBySession SQL")
	}
	if !strings.Contains(lockTokensForUserSQL, "ORDER BY t.id") || !strings.Contains(lockTokensForUserSQL, "FOR UPDATE OF t") {
		t.Fatal("LockAllForUser must lock tokens in id order without locking sessions")
	}
}

func TestAuthSelfServicePostgres(t *testing.T) {
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

	clock := &pgClock{t: time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC)}
	svc := pgAuthService(clock)
	uow := NewAuthUoW(app)
	read := pgReadService(clock, app)

	t.Run("GetForUpdate holds until Tx ends", func(t *testing.T) {
		email := pgEmail(t, "gfu")
		got, err := application.RegisterIn(ctx, uow, svc, pgRegister(email))
		if err != nil {
			t.Fatal(err)
		}
		held, err := app.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := NewAuthUsers(held).GetForUpdate(ctx, got.ID); err != nil {
			_ = held.Rollback(ctx)
			t.Fatal(err)
		}
		waiter, err := connB.Begin(ctx)
		if err != nil {
			_ = held.Rollback(ctx)
			t.Fatal(err)
		}
		if _, err := waiter.Exec(ctx, `SET LOCAL lock_timeout = '5s'`); err != nil {
			_ = waiter.Rollback(ctx)
			_ = held.Rollback(ctx)
			t.Fatal(err)
		}
		_, err = NewAuthUsers(waiter).GetForUpdate(ctx, got.ID)
		if !isLockTimeout(err) {
			_ = waiter.Rollback(ctx)
			_ = held.Rollback(ctx)
			t.Fatalf("want lock timeout, got %v", err)
		}
		if err := waiter.Rollback(ctx); err != nil {
			_ = held.Rollback(ctx)
			t.Fatal(err)
		}
		if err := held.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
		probe, err := connB.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = probe.Rollback(ctx) }()
		if _, err := NewAuthUsers(probe).GetForUpdate(ctx, got.ID); err != nil {
			t.Fatalf("after release: %v", err)
		}
	})

	t.Run("LockBySession is FOR UPDATE", func(t *testing.T) {
		email := pgEmail(t, "lbs")
		if _, err := application.RegisterIn(ctx, uow, svc, pgRegister(email)); err != nil {
			t.Fatal(err)
		}
		cred, err := application.LoginIn(ctx, uow, svc, pgLogin(email))
		if err != nil {
			t.Fatal(err)
		}
		hash, err := svc.Issuer.Hash(cred.RefreshToken)
		if err != nil {
			t.Fatal(err)
		}
		held, err := app.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := NewAuthTokens(held).LockBySession(ctx, cred.SessionID); err != nil {
			_ = held.Rollback(ctx)
			t.Fatal(err)
		}
		waiter, err := connB.Begin(ctx)
		if err != nil {
			_ = held.Rollback(ctx)
			t.Fatal(err)
		}
		if _, err := waiter.Exec(ctx, `SET LOCAL lock_timeout = '5s'`); err != nil {
			_ = waiter.Rollback(ctx)
			_ = held.Rollback(ctx)
			t.Fatal(err)
		}
		_, err = NewAuthTokens(waiter).GetForUpdate(ctx, hash)
		if !isLockTimeout(err) {
			_ = waiter.Rollback(ctx)
			_ = held.Rollback(ctx)
			t.Fatalf("want lock timeout, got %v", err)
		}
		if err := waiter.Rollback(ctx); err != nil {
			_ = held.Rollback(ctx)
			t.Fatal(err)
		}
		if err := held.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("LockAllForUser order and session rows stay free", func(t *testing.T) {
		u, sess, lowID, highID := seedTokenPair(t, ctx, app, clock)
		heldHigh, err := app.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := heldHigh.Exec(ctx, `SELECT id FROM auth.refresh_tokens WHERE id = $1 FOR UPDATE`, highID); err != nil {
			_ = heldHigh.Rollback(ctx)
			t.Fatal(err)
		}

		const waiterName = "paycore-lock-order-b"
		setAppName(t, ctx, connB, waiterName)
		heldAll, err := connB.Begin(ctx)
		if err != nil {
			_ = heldHigh.Rollback(ctx)
			t.Fatal(err)
		}
		lockErr := make(chan error, 1)
		go func() {
			lockErr <- NewAuthTokens(heldAll).LockAllForUser(ctx, u.ID)
		}()
		waitBackendLockWait(t, ctx, app, waiterName)

		probe, err := connC.Begin(ctx)
		if err != nil {
			_ = heldAll.Rollback(ctx)
			_ = heldHigh.Rollback(ctx)
			t.Fatal(err)
		}
		if _, err := probe.Exec(ctx, `SET LOCAL lock_timeout = '5s'`); err != nil {
			_ = probe.Rollback(ctx)
			_ = heldAll.Rollback(ctx)
			_ = heldHigh.Rollback(ctx)
			t.Fatal(err)
		}
		_, err = probe.Exec(ctx, `SELECT id FROM auth.refresh_tokens WHERE id = $1 FOR UPDATE`, lowID)
		if !isLockTimeout(err) {
			_ = probe.Rollback(ctx)
			_ = heldAll.Rollback(ctx)
			_ = heldHigh.Rollback(ctx)
			t.Fatalf("LockAllForUser did not hold lower id first: %v", err)
		}
		if err := probe.Rollback(ctx); err != nil {
			_ = heldAll.Rollback(ctx)
			_ = heldHigh.Rollback(ctx)
			t.Fatal(err)
		}

		sessProbe, err := connC.Begin(ctx)
		if err != nil {
			_ = heldAll.Rollback(ctx)
			_ = heldHigh.Rollback(ctx)
			t.Fatal(err)
		}
		if _, err := sessProbe.Exec(ctx, `SET LOCAL lock_timeout = '5s'`); err != nil {
			_ = sessProbe.Rollback(ctx)
			_ = heldAll.Rollback(ctx)
			_ = heldHigh.Rollback(ctx)
			t.Fatal(err)
		}
		if _, err := sessProbe.Exec(ctx, `SELECT id FROM auth.sessions WHERE id = $1 FOR UPDATE`, sess.ID); err != nil {
			_ = sessProbe.Rollback(ctx)
			_ = heldAll.Rollback(ctx)
			_ = heldHigh.Rollback(ctx)
			t.Fatalf("session row locked during token lock: %v", err)
		}
		if err := sessProbe.Rollback(ctx); err != nil {
			_ = heldAll.Rollback(ctx)
			_ = heldHigh.Rollback(ctx)
			t.Fatal(err)
		}

		if err := heldHigh.Rollback(ctx); err != nil {
			_ = heldAll.Rollback(ctx)
			t.Fatal(err)
		}
		if err := <-lockErr; err != nil {
			_ = heldAll.Rollback(ctx)
			t.Fatalf("LockAllForUser: %v", err)
		}
		if err := heldAll.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("LockBySession empty is nil", func(t *testing.T) {
		if err := NewAuthTokens(app).LockBySession(ctx, mustNewID(t)); err != nil {
			t.Fatalf("empty session lock: %v", err)
		}
		if err := NewAuthTokens(app).LockAllForUser(ctx, mustNewID(t)); err != nil {
			t.Fatalf("empty user lock: %v", err)
		}
	})

	t.Run("ListByUser live only stable sort no secrets", func(t *testing.T) {
		owner := seedAuthUser(t, ctx, app, clock, pgEmail(t, "list-a"))
		other := seedAuthUser(t, ctx, app, clock, pgEmail(t, "list-b"))
		t0 := clock.t
		t1 := clock.t.Add(time.Second)
		low := uuid.MustParse("00000000-0000-4000-8000-000000000010")
		high := uuid.MustParse("00000000-0000-4000-8000-0000000000ff")
		newest := uuid.MustParse("00000000-0000-4000-8000-000000000020")
		revoked := uuid.MustParse("00000000-0000-4000-8000-000000000030")
		foreign := uuid.MustParse("00000000-0000-4000-8000-000000000040")
		mustCreateSession(t, ctx, app, domain.Session{
			ID: low, UserID: owner.ID, UserAgent: "ua-low", IP: "203.0.113.10",
			CreatedAt: t0, LastSeenAt: t0,
		})
		mustCreateSession(t, ctx, app, domain.Session{
			ID: high, UserID: owner.ID, UserAgent: "ua-high", IP: "203.0.113.11",
			CreatedAt: t0, LastSeenAt: t0,
		})
		mustCreateSession(t, ctx, app, domain.Session{
			ID: newest, UserID: owner.ID, UserAgent: "ua-new", IP: "203.0.113.12",
			CreatedAt: t1, LastSeenAt: t1,
		})
		revAt := t1
		mustCreateSession(t, ctx, app, domain.Session{
			ID: revoked, UserID: owner.ID, UserAgent: "ua-rev", IP: "203.0.113.13",
			CreatedAt: t1, LastSeenAt: t1, RevokedAt: &revAt, RevokeReason: domain.RevokeLogout,
		})
		mustCreateSession(t, ctx, app, domain.Session{
			ID: foreign, UserID: other.ID, UserAgent: "ua-other", IP: "203.0.113.14",
			CreatedAt: t1, LastSeenAt: t1,
		})

		live, err := NewAuthSessions(app).ListByUser(ctx, owner.ID, true)
		if err != nil {
			t.Fatal(err)
		}
		if len(live) != 3 {
			t.Fatalf("live %d", len(live))
		}
		if live[0].ID != newest || live[1].ID != high || live[2].ID != low {
			t.Fatalf("order %s %s %s", live[0].ID, live[1].ID, live[2].ID)
		}
		for _, sess := range live {
			if sess.UserID != owner.ID || !sess.Live() || sess.RevokeReason != "" {
				t.Fatalf("leaked session %+v", sess)
			}
		}
		all, err := NewAuthSessions(app).ListByUser(ctx, owner.ID, false)
		if err != nil {
			t.Fatal(err)
		}
		if len(all) != 4 {
			t.Fatalf("all %d", len(all))
		}
		empty, err := NewAuthSessions(app).ListByUser(ctx, mustNewID(t), true)
		if err != nil || len(empty) != 0 {
			t.Fatalf("empty %+v %v", empty, err)
		}
	})

	t.Run("LogoutAll revokes every live session", func(t *testing.T) {
		email := pgEmail(t, "loall")
		if _, err := application.RegisterIn(ctx, uow, svc, pgRegister(email)); err != nil {
			t.Fatal(err)
		}
		first, err := application.LoginIn(ctx, uow, svc, pgLogin(email))
		if err != nil {
			t.Fatal(err)
		}
		second, err := application.LoginIn(ctx, uow, svc, pgLogin(email))
		if err != nil {
			t.Fatal(err)
		}
		actor := domain.Principal{UserID: first.User.ID, SessionID: first.SessionID, Roles: first.User.Roles}
		if err := application.LogoutAllIn(ctx, uow, svc, actor); err != nil {
			t.Fatal(err)
		}
		s1, err := NewAuthSessions(app).Get(ctx, first.SessionID)
		if err != nil || s1.Live() || s1.RevokeReason != domain.RevokeLogoutAll {
			t.Fatalf("first %+v %v", s1, err)
		}
		s2, err := NewAuthSessions(app).Get(ctx, second.SessionID)
		if err != nil || s2.Live() || s2.RevokeReason != domain.RevokeLogoutAll {
			t.Fatalf("second %+v %v", s2, err)
		}
		if _, err := read.ValidateSession(ctx, first.SessionID); !errors.Is(err, domain.ErrInvalidToken) {
			t.Fatalf("validate first: %v", err)
		}
		_, err = application.RefreshIn(ctx, uow, svc, application.RefreshCommand{RefreshToken: second.RefreshToken, IP: nextTestIP()})
		if !errors.Is(err, domain.ErrInvalidToken) && !errors.Is(err, domain.ErrTokenReused) {
			t.Fatalf("refresh after logout-all: %v", err)
		}
		if err := application.LogoutAllIn(ctx, uow, svc, actor); err != nil {
			t.Fatalf("repeat: %v", err)
		}
		live, err := NewAuthSessions(app).ListByUser(ctx, first.User.ID, true)
		if err != nil || len(live) != 0 {
			t.Fatalf("live after repeat %+v %v", live, err)
		}
	})

	t.Run("RevokeSession own live then repeat", func(t *testing.T) {
		email := pgEmail(t, "rev")
		if _, err := application.RegisterIn(ctx, uow, svc, pgRegister(email)); err != nil {
			t.Fatal(err)
		}
		keep, err := application.LoginIn(ctx, uow, svc, pgLogin(email))
		if err != nil {
			t.Fatal(err)
		}
		drop, err := application.LoginIn(ctx, uow, svc, pgLogin(email))
		if err != nil {
			t.Fatal(err)
		}
		actor := domain.Principal{UserID: keep.User.ID, SessionID: keep.SessionID, Roles: keep.User.Roles}
		if err := application.RevokeSessionIn(ctx, uow, svc, application.RevokeSessionCommand{Actor: actor, SessionID: drop.SessionID}); err != nil {
			t.Fatal(err)
		}
		got, err := NewAuthSessions(app).Get(ctx, drop.SessionID)
		if err != nil || got.Live() || got.RevokeReason != domain.RevokeLogout {
			t.Fatalf("revoked %+v %v", got, err)
		}
		kept, err := NewAuthSessions(app).Get(ctx, keep.SessionID)
		if err != nil || !kept.Live() {
			t.Fatalf("kept %+v %v", kept, err)
		}
		if err := application.RevokeSessionIn(ctx, uow, svc, application.RevokeSessionCommand{Actor: actor, SessionID: drop.SessionID}); err != nil {
			t.Fatalf("repeat: %v", err)
		}
		foreign, err := application.RegisterIn(ctx, uow, svc, pgRegister(pgEmail(t, "rev-x")))
		if err != nil {
			t.Fatal(err)
		}
		otherCred, err := application.LoginIn(ctx, uow, svc, pgLogin(foreign.Email))
		if err != nil {
			t.Fatal(err)
		}
		err = application.RevokeSessionIn(ctx, uow, svc, application.RevokeSessionCommand{Actor: actor, SessionID: otherCred.SessionID})
		if !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("foreign: %v", err)
		}
		still, err := NewAuthSessions(app).Get(ctx, otherCred.SessionID)
		if err != nil || !still.Live() {
			t.Fatalf("foreign touched %+v %v", still, err)
		}
	})

	t.Run("ChangePassword rotates hash and revokes sessions", func(t *testing.T) {
		email := pgEmail(t, "pw")
		if _, err := application.RegisterIn(ctx, uow, svc, pgRegister(email)); err != nil {
			t.Fatal(err)
		}
		first, err := application.LoginIn(ctx, uow, svc, pgLogin(email))
		if err != nil {
			t.Fatal(err)
		}
		second, err := application.LoginIn(ctx, uow, svc, pgLogin(email))
		if err != nil {
			t.Fatal(err)
		}
		oldHash := mustUserHash(t, ctx, app, first.User.ID)
		err = application.ChangePasswordIn(ctx, uow, svc, application.ChangePasswordCommand{
			Actor:       domain.Principal{UserID: first.User.ID, SessionID: first.SessionID, Roles: first.User.Roles},
			OldPassword: "twelvechars!",
			NewPassword: "newpassword!!",
		})
		if err != nil {
			t.Fatal(err)
		}
		stored, err := NewAuthUsers(app).Get(ctx, first.User.ID)
		if err != nil {
			t.Fatal(err)
		}
		want, err := pgHasher{}.Hash("newpassword!!")
		if err != nil || stored.PasswordHash != want || stored.PasswordHash == oldHash {
			t.Fatalf("hash %q want %q old %q err %v", stored.PasswordHash, want, oldHash, err)
		}
		s1, _ := NewAuthSessions(app).Get(ctx, first.SessionID)
		s2, _ := NewAuthSessions(app).Get(ctx, second.SessionID)
		if s1.Live() || s1.RevokeReason != domain.RevokePasswordChanged || s2.Live() || s2.RevokeReason != domain.RevokePasswordChanged {
			t.Fatalf("sessions %+v %+v", s1, s2)
		}
		if _, err := read.ValidateSession(ctx, first.SessionID); !errors.Is(err, domain.ErrInvalidToken) {
			t.Fatalf("old session: %v", err)
		}
		_, err = application.LoginIn(ctx, uow, svc, pgLogin(email))
		if !errors.Is(err, domain.ErrInvalidCredentials) {
			t.Fatalf("old password: %v", err)
		}
		cmd := pgLogin(email)
		cmd.Password = "newpassword!!"
		if _, err := application.LoginIn(ctx, uow, svc, cmd); err != nil {
			t.Fatalf("new password: %v", err)
		}
	})

	t.Run("ChangePassword wrong old rolls back", func(t *testing.T) {
		email := pgEmail(t, "pw-rb")
		if _, err := application.RegisterIn(ctx, uow, svc, pgRegister(email)); err != nil {
			t.Fatal(err)
		}
		cred, err := application.LoginIn(ctx, uow, svc, pgLogin(email))
		if err != nil {
			t.Fatal(err)
		}
		before := mustUserHash(t, ctx, app, cred.User.ID)
		err = application.ChangePasswordIn(ctx, uow, svc, application.ChangePasswordCommand{
			Actor:       domain.Principal{UserID: cred.User.ID, SessionID: cred.SessionID},
			OldPassword: "wrongpassword!",
			NewPassword: "newpassword!!",
		})
		if !errors.Is(err, domain.ErrInvalidCredentials) {
			t.Fatalf("got %v", err)
		}
		if mustUserHash(t, ctx, app, cred.User.ID) != before {
			t.Fatal("hash changed on rollback")
		}
		sess, err := NewAuthSessions(app).Get(ctx, cred.SessionID)
		if err != nil || !sess.Live() {
			t.Fatalf("session revoked on rollback %+v %v", sess, err)
		}
	})

	t.Run("Refresh and Logout still rotate and revoke", func(t *testing.T) {
		email := pgEmail(t, "reg")
		if _, err := application.RegisterIn(ctx, uow, svc, pgRegister(email)); err != nil {
			t.Fatal(err)
		}
		cred, err := application.LoginIn(ctx, uow, svc, pgLogin(email))
		if err != nil {
			t.Fatal(err)
		}
		next, err := application.RefreshIn(ctx, uow, svc, application.RefreshCommand{RefreshToken: cred.RefreshToken, IP: nextTestIP()})
		if err != nil {
			t.Fatal(err)
		}
		if next.SessionID != cred.SessionID || next.RefreshToken == cred.RefreshToken {
			t.Fatalf("rotation %+v", next)
		}
		if err := application.LogoutIn(ctx, uow, svc, domain.Principal{UserID: cred.User.ID, SessionID: cred.SessionID}); err != nil {
			t.Fatal(err)
		}
		sess, err := NewAuthSessions(app).Get(ctx, cred.SessionID)
		if err != nil || sess.Live() || sess.RevokeReason != domain.RevokeLogout {
			t.Fatalf("logout %+v %v", sess, err)
		}
		_, err = application.RefreshIn(ctx, uow, svc, application.RefreshCommand{RefreshToken: next.RefreshToken, IP: nextTestIP()})
		if !errors.Is(err, domain.ErrInvalidToken) && !errors.Is(err, domain.ErrTokenReused) {
			t.Fatalf("refresh after logout: %v", err)
		}
	})

	t.Run("LockUser and DisableUser still revoke through token locks", func(t *testing.T) {
		clearActiveAdmins(t, ctx, app, clock)
		admin := seedPGAdmin(t, ctx, app, clock, pgEmail(t, "st-a"))
		email := pgEmail(t, "st-c")
		cust, err := application.RegisterIn(ctx, uow, svc, pgRegister(email))
		if err != nil {
			t.Fatal(err)
		}
		cred, err := application.LoginIn(ctx, uow, svc, pgLogin(email))
		if err != nil {
			t.Fatal(err)
		}
		if err := application.LockUserIn(ctx, uow, svc, pgStatus(adminPrincipal(admin), cust.ID)); err != nil {
			t.Fatal(err)
		}
		got, err := NewAuthUsers(app).Get(ctx, cust.ID)
		if err != nil || got.Status != domain.StatusLocked {
			t.Fatalf("locked %+v %v", got, err)
		}
		sess, err := NewAuthSessions(app).Get(ctx, cred.SessionID)
		if err != nil || sess.Live() || sess.RevokeReason != domain.RevokeUserLocked {
			t.Fatalf("lock session %+v %v", sess, err)
		}
		if err := application.UnlockUserIn(ctx, uow, svc, pgStatus(adminPrincipal(admin), cust.ID)); err != nil {
			t.Fatal(err)
		}
		again, err := application.LoginIn(ctx, uow, svc, pgLogin(email))
		if err != nil {
			t.Fatal(err)
		}
		if err := application.DisableUserIn(ctx, uow, svc, pgStatus(adminPrincipal(admin), cust.ID)); err != nil {
			t.Fatal(err)
		}
		got, err = NewAuthUsers(app).Get(ctx, cust.ID)
		if err != nil || got.Status != domain.StatusDisabled {
			t.Fatalf("disabled %+v %v", got, err)
		}
		sess, err = NewAuthSessions(app).Get(ctx, again.SessionID)
		if err != nil || sess.Live() || sess.RevokeReason != domain.RevokeUserDisabled {
			t.Fatalf("disable session %+v %v", sess, err)
		}
	})

	t.Run("LogoutAll and login lockout do not deadlock", func(t *testing.T) {
		email := pgEmail(t, "dl-lo")
		if _, err := application.RegisterIn(ctx, uow, svc, pgRegister(email)); err != nil {
			t.Fatal(err)
		}
		cred, err := application.LoginIn(ctx, uow, svc, pgLogin(email))
		if err != nil {
			t.Fatal(err)
		}
		u, err := NewAuthUsers(app).Get(ctx, cred.User.ID)
		if err != nil {
			t.Fatal(err)
		}
		clock.t = clock.t.Add(domain.RateWindow)
		u.FailedLoginCount = domain.LockAfter - 1
		u.UpdatedAt = clock.t
		if err := NewAuthUsers(app).Update(ctx, u); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		errs := make([]error, 2)
		wg.Add(2)
		go func() {
			defer wg.Done()
			errs[0] = application.LogoutAllIn(ctx, NewAuthUoW(app), pgAuthService(clock), domain.Principal{
				UserID: cred.User.ID, SessionID: cred.SessionID,
			})
		}()
		go func() {
			defer wg.Done()
			cmd := pgLogin(email)
			cmd.Password = "wrongpassword!"
			_, errs[1] = application.LoginIn(ctx, NewAuthUoW(connB), pgAuthService(clock), cmd)
		}()
		wg.Wait()
		if errs[0] != nil {
			t.Fatalf("logout-all: %v", errs[0])
		}
		if !errors.Is(errs[1], domain.ErrInvalidCredentials) {
			t.Fatalf("lockout: %v", errs[1])
		}
	})

	t.Run("Refresh LogoutAll and ChangePassword do not deadlock", func(t *testing.T) {
		email := pgEmail(t, "dl-mix")
		if _, err := application.RegisterIn(ctx, uow, svc, pgRegister(email)); err != nil {
			t.Fatal(err)
		}
		a, err := application.LoginIn(ctx, uow, svc, pgLogin(email))
		if err != nil {
			t.Fatal(err)
		}
		b, err := application.LoginIn(ctx, uow, svc, pgLogin(email))
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		errs := make([]error, 3)
		wg.Add(3)
		go func() {
			defer wg.Done()
			_, errs[0] = application.RefreshIn(ctx, NewAuthUoW(app), pgAuthService(clock), application.RefreshCommand{
				RefreshToken: a.RefreshToken, IP: nextTestIP(),
			})
		}()
		go func() {
			defer wg.Done()
			errs[1] = application.LogoutAllIn(ctx, NewAuthUoW(connB), pgAuthService(clock), domain.Principal{
				UserID: a.User.ID, SessionID: a.SessionID,
			})
		}()
		go func() {
			defer wg.Done()
			errs[2] = application.ChangePasswordIn(ctx, NewAuthUoW(connC), pgAuthService(clock), application.ChangePasswordCommand{
				Actor:       domain.Principal{UserID: b.User.ID, SessionID: b.SessionID},
				OldPassword: "twelvechars!",
				NewPassword: "otherpassword!",
			})
		}()
		wg.Wait()
		for i, err := range errs {
			if err == nil || errors.Is(err, domain.ErrInvalidToken) || errors.Is(err, domain.ErrTokenReused) || errors.Is(err, domain.ErrInvalidCredentials) {
				continue
			}
			t.Fatalf("op %d: %v (all %v)", i, err, errs)
		}
	})

	t.Run("auth self-service does not write ledger", func(t *testing.T) {
		var n int
		if err := app.QueryRow(ctx, `SELECT count(*) FROM ledger.journal_transactions`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("journal_transactions %d", n)
		}
	})
}

func pgReadService(clock *pgClock, conn *pgx.Conn) application.Service {
	s := pgAuthService(clock)
	s.Users = NewAuthUsers(conn)
	s.Sessions = NewAuthSessions(conn)
	s.Roles = NewAuthRoles(conn)
	s.Tokens = NewAuthTokens(conn)
	return s
}

func seedAuthUser(t *testing.T, ctx context.Context, conn *pgx.Conn, clock *pgClock, email string) domain.User {
	t.Helper()
	u := domain.User{
		ID:           mustNewID(t),
		Email:        email,
		PasswordHash: testArgon2idPHC,
		Status:       domain.StatusActive,
		CreatedAt:    clock.t,
		UpdatedAt:    clock.t,
	}
	if err := NewAuthUsers(conn).Create(ctx, u); err != nil {
		t.Fatal(err)
	}
	return u
}

func mustCreateSession(t *testing.T, ctx context.Context, conn *pgx.Conn, sess domain.Session) {
	t.Helper()
	if err := NewAuthSessions(conn).Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
}

func seedTokenPair(t *testing.T, ctx context.Context, conn *pgx.Conn, clock *pgClock) (domain.User, domain.Session, uuid.UUID, uuid.UUID) {
	t.Helper()
	u := seedAuthUser(t, ctx, conn, clock, pgEmail(t, "tok"))
	sess := domain.Session{
		ID: mustNewID(t), UserID: u.ID, UserAgent: "ua", IP: "203.0.113.50",
		CreatedAt: clock.t, LastSeenAt: clock.t,
	}
	mustCreateSession(t, ctx, conn, sess)
	lowID := uuid.MustParse("00000000-0000-4000-8000-000000000001")
	highID := uuid.MustParse("ffffffff-ffff-4fff-bfff-ffffffffffff")
	lowHash := sha256.Sum256([]byte("low-" + u.ID.String()))
	highHash := sha256.Sum256([]byte("high-" + u.ID.String()))
	exp := clock.t.Add(24 * time.Hour)
	if err := NewAuthTokens(conn).Create(ctx, domain.RefreshToken{
		ID: lowID, SessionID: sess.ID, Hash: lowHash[:], ExpiresAt: exp, CreatedAt: clock.t,
	}); err != nil {
		t.Fatal(err)
	}
	if err := NewAuthTokens(conn).Create(ctx, domain.RefreshToken{
		ID: highID, SessionID: sess.ID, Hash: highHash[:], ExpiresAt: exp, CreatedAt: clock.t,
	}); err != nil {
		t.Fatal(err)
	}
	return u, sess, lowID, highID
}

func mustUserHash(t *testing.T, ctx context.Context, conn *pgx.Conn, id uuid.UUID) string {
	t.Helper()
	u, err := NewAuthUsers(conn).Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return u.PasswordHash
}

func setAppName(t *testing.T, ctx context.Context, conn *pgx.Conn, name string) {
	t.Helper()
	if _, err := conn.Exec(ctx, `SELECT set_config('application_name', $1, false)`, name); err != nil {
		t.Fatal(err)
	}
}

func waitBackendLockWait(t *testing.T, ctx context.Context, q interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}, name string) {
	t.Helper()
	deadline, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		var n int
		err := q.QueryRow(deadline, `
			SELECT count(*)::int
			  FROM pg_stat_activity
			 WHERE application_name = $1
			   AND wait_event_type = 'Lock'`, name).Scan(&n)
		if err != nil {
			t.Fatal(err)
		}
		if n >= 1 {
			return
		}
		select {
		case <-deadline.Done():
			t.Fatalf("waiting for lock wait on %s: %v", name, deadline.Err())
		case <-ticker.C:
		}
	}
}
