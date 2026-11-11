package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/romangergovskiy/paycore/internal/auth/application"
	"github.com/romangergovskiy/paycore/internal/auth/domain"
)

func TestAuthRefreshReuseVsRotate(t *testing.T) {
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

	clock := &pgClock{t: time.Date(2026, 11, 11, 16, 0, 0, 0, time.UTC)}
	svc := pgAuthService(clock)
	uow := NewAuthUoW(app)

	t.Run("rotation first then reuse revokes the new token", func(t *testing.T) {
		first, live := mustLoginRotated(t, ctx, uow, svc)
		rotateHold := newPark()
		defer rotateHold.release()
		const reuseName = "paycore-reuse-after-rotate"
		setAppName(t, ctx, connC, reuseName)

		rotateDone := make(chan credErr, 1)
		go func() {
			out, err := application.RefreshIn(ctx, afterGetForUpdateUoW(connB, rotateHold.hold), pgAuthService(clock), application.RefreshCommand{
				RefreshToken: live.RefreshToken, IP: nextTestIP(),
			})
			rotateDone <- credErr{out, err}
		}()
		if err := rotateHold.waitArrived(ctx); err != nil {
			t.Fatal(err)
		}

		reuseDone := make(chan error, 1)
		go func() {
			_, err := application.RefreshIn(ctx, NewAuthUoW(connC), pgAuthService(clock), application.RefreshCommand{
				RefreshToken: first.RefreshToken, IP: nextTestIP(),
			})
			reuseDone <- err
		}()
		waitBackendLockWait(t, ctx, app, reuseName)

		if n := countLiveSessions(t, ctx, app, first.User.ID); n != 1 {
			t.Fatalf("session should stay live while rotation holds tokens: %d", n)
		}

		rotateHold.release()
		rot := <-rotateDone
		reuseErr := <-reuseDone
		if rot.err != nil {
			t.Fatalf("rotation: %v", rot.err)
		}
		if !errors.Is(reuseErr, domain.ErrTokenReused) {
			t.Fatalf("reuse: %v", reuseErr)
		}
		assertReuseRevoked(t, ctx, app, first.SessionID, domain.RevokeTokenReuse)
	})

	t.Run("reuse first then rotation issues no credentials", func(t *testing.T) {
		first, live := mustLoginRotated(t, ctx, uow, svc)
		reuseHold := newPark()
		defer reuseHold.release()
		const rotateName = "paycore-rotate-after-reuse"
		setAppName(t, ctx, connC, rotateName)

		reuseDone := make(chan error, 1)
		go func() {
			_, err := application.RefreshIn(ctx, afterLockUoW(connB, reuseHold.hold), pgAuthService(clock), application.RefreshCommand{
				RefreshToken: first.RefreshToken, IP: nextTestIP(),
			})
			reuseDone <- err
		}()
		if err := reuseHold.waitArrived(ctx); err != nil {
			t.Fatal(err)
		}

		rotateDone := make(chan credErr, 1)
		go func() {
			out, err := application.RefreshIn(ctx, NewAuthUoW(connC), pgAuthService(clock), application.RefreshCommand{
				RefreshToken: live.RefreshToken, IP: nextTestIP(),
			})
			rotateDone <- credErr{out, err}
		}()
		waitBackendLockWait(t, ctx, app, rotateName)

		sess, err := NewAuthSessions(app).Get(ctx, first.SessionID)
		if err != nil || !sess.Live() {
			t.Fatalf("session should stay live while reuse holds tokens only: %+v %v", sess, err)
		}

		reuseHold.release()
		reuseErr := <-reuseDone
		rot := <-rotateDone
		if !errors.Is(reuseErr, domain.ErrTokenReused) {
			t.Fatalf("reuse: %v", reuseErr)
		}
		if rot.err == nil {
			t.Fatal("rotation issued credentials after reuse")
		}
		if !errors.Is(rot.err, domain.ErrTokenReused) && !errors.Is(rot.err, domain.ErrInvalidToken) {
			t.Fatalf("rotation: %v", rot.err)
		}
		assertReuseRevoked(t, ctx, app, first.SessionID, domain.RevokeTokenReuse)
	})

	t.Run("two concurrent used-token requests", func(t *testing.T) {
		first, _ := mustLoginRotated(t, ctx, uow, svc)
		aHold := newPark()
		defer aHold.release()
		const bName = "paycore-reuse-second"
		setAppName(t, ctx, connC, bName)

		aDone := make(chan error, 1)
		go func() {
			_, err := application.RefreshIn(ctx, afterLockUoW(connB, aHold.hold), pgAuthService(clock), application.RefreshCommand{
				RefreshToken: first.RefreshToken, IP: nextTestIP(),
			})
			aDone <- err
		}()
		if err := aHold.waitArrived(ctx); err != nil {
			t.Fatal(err)
		}

		bDone := make(chan error, 1)
		go func() {
			_, err := application.RefreshIn(ctx, NewAuthUoW(connC), pgAuthService(clock), application.RefreshCommand{
				RefreshToken: first.RefreshToken, IP: nextTestIP(),
			})
			bDone <- err
		}()
		waitBackendLockWait(t, ctx, app, bName)
		aHold.release()
		aErr, bErr := <-aDone, <-bDone
		if !errors.Is(aErr, domain.ErrTokenReused) {
			t.Fatalf("first reuse: %v", aErr)
		}
		if !errors.Is(bErr, domain.ErrTokenReused) && !errors.Is(bErr, domain.ErrInvalidToken) {
			t.Fatalf("second reuse: %v", bErr)
		}
		assertReuseRevoked(t, ctx, app, first.SessionID, domain.RevokeTokenReuse)
	})
}

func TestAuthRefreshReuseVsRevoke(t *testing.T) {
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

	clock := &pgClock{t: time.Date(2026, 11, 11, 17, 0, 0, 0, time.UTC)}
	svc := pgAuthService(clock)
	uow := NewAuthUoW(app)
	read := pgReadService(clock, app)

	logoutAll := func(uow application.UnitOfWork, first, _ application.Credentials, _ domain.Principal) error {
		return application.LogoutAllIn(ctx, uow, pgAuthService(clock), domain.Principal{
			UserID: first.User.ID, SessionID: first.SessionID, Roles: first.User.Roles,
		})
	}
	changePassword := func(uow application.UnitOfWork, first, _ application.Credentials, _ domain.Principal) error {
		return application.ChangePasswordIn(ctx, uow, pgAuthService(clock), application.ChangePasswordCommand{
			Actor:       domain.Principal{UserID: first.User.ID, SessionID: first.SessionID, Roles: first.User.Roles},
			OldPassword: "twelvechars!",
			NewPassword: "newpassword!!",
		})
	}
	for _, tc := range []struct {
		name  string
		reuse bool
		hold  string
		admin bool
		run   func(uow application.UnitOfWork, first, live application.Credentials, actor domain.Principal) error
	}{
		{name: "Refresh vs LogoutAll refresh holds tokens", hold: "refresh", run: logoutAll},
		{name: "Refresh vs LogoutAll logout-all holds tokens", hold: "other", run: logoutAll},
		{name: "reuse vs LogoutAll reuse holds tokens", reuse: true, hold: "refresh", run: logoutAll},
		{name: "reuse vs LogoutAll logout-all holds tokens", reuse: true, hold: "other", run: logoutAll},
		{name: "Refresh vs ChangePassword refresh holds tokens", hold: "refresh", run: changePassword},
		{name: "Refresh vs ChangePassword change-password holds tokens", hold: "other", run: changePassword},
		{name: "reuse vs ChangePassword reuse holds tokens", reuse: true, hold: "refresh", run: changePassword},
		{name: "reuse vs ChangePassword change-password holds tokens", reuse: true, hold: "other", run: changePassword},
		{
			name: "Refresh vs Logout refresh holds tokens",
			hold: "refresh",
			run: func(uow application.UnitOfWork, first, _ application.Credentials, _ domain.Principal) error {
				return application.LogoutIn(ctx, uow, pgAuthService(clock), domain.Principal{
					UserID: first.User.ID, SessionID: first.SessionID, Roles: first.User.Roles,
				})
			},
		},
		{
			name:  "Refresh vs LockUser refresh holds tokens",
			admin: true,
			hold:  "refresh",
			run: func(uow application.UnitOfWork, first, _ application.Credentials, actor domain.Principal) error {
				return application.LockUserIn(ctx, uow, pgAuthService(clock), pgStatus(actor, first.User.ID))
			},
		},
		{
			name:  "Refresh vs DisableUser refresh holds tokens",
			admin: true,
			hold:  "refresh",
			run: func(uow application.UnitOfWork, first, _ application.Credentials, actor domain.Principal) error {
				return application.DisableUserIn(ctx, uow, pgAuthService(clock), pgStatus(actor, first.User.ID))
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock.t = clock.t.Add(domain.RateWindow)
			first, live := mustLoginRotated(t, ctx, uow, svc)
			var actor domain.Principal
			if tc.admin {
				clearActiveAdmins(t, ctx, app, clock)
				admin := seedPGAdmin(t, ctx, app, clock, pgEmail(t, "rr-a"))
				actor = adminPrincipal(admin)
			}
			hold := newPark()
			defer hold.release()
			const waiterName = "paycore-reuse-waiter"
			refreshTok := live.RefreshToken
			if tc.reuse {
				refreshTok = first.RefreshToken
			}

			var refreshErr, otherErr error
			var cred application.Credentials
			if tc.hold == "refresh" {
				setAppName(t, ctx, connC, waiterName)
				refreshDone := make(chan credErr, 1)
				go func() {
					out, err := application.RefreshIn(ctx, afterLockUoW(connB, hold.hold), pgAuthService(clock), application.RefreshCommand{
						RefreshToken: refreshTok, IP: nextTestIP(),
					})
					refreshDone <- credErr{out, err}
				}()
				if err := hold.waitArrived(ctx); err != nil {
					t.Fatal(err)
				}
				otherDone := make(chan error, 1)
				go func() { otherDone <- tc.run(NewAuthUoW(connC), first, live, actor) }()
				waitBackendLockWait(t, ctx, app, waiterName)
				sess, err := NewAuthSessions(app).Get(ctx, first.SessionID)
				if err != nil || !sess.Live() {
					t.Fatalf("session should stay live while refresh holds tokens: %+v %v", sess, err)
				}
				hold.release()
				res := <-refreshDone
				cred, refreshErr = res.cred, res.err
				otherErr = <-otherDone
			} else {
				setAppName(t, ctx, connC, waiterName)
				otherDone := make(chan error, 1)
				go func() {
					otherDone <- tc.run(afterLockUoW(connB, hold.hold), first, live, actor)
				}()
				if err := hold.waitArrived(ctx); err != nil {
					t.Fatal(err)
				}
				refreshDone := make(chan credErr, 1)
				go func() {
					out, err := application.RefreshIn(ctx, NewAuthUoW(connC), pgAuthService(clock), application.RefreshCommand{
						RefreshToken: refreshTok, IP: nextTestIP(),
					})
					refreshDone <- credErr{out, err}
				}()
				waitBackendLockWait(t, ctx, app, waiterName)
				hold.release()
				otherErr = <-otherDone
				res := <-refreshDone
				cred, refreshErr = res.cred, res.err
			}

			if isDeadlock(refreshErr) || isDeadlock(otherErr) {
				t.Fatalf("deadlock refresh=%v other=%v cred=%v", refreshErr, otherErr, cred.SessionID)
			}
			if otherErr != nil {
				t.Fatalf("other: %v", otherErr)
			}
			if tc.reuse {
				if !errors.Is(refreshErr, domain.ErrTokenReused) && !errors.Is(refreshErr, domain.ErrInvalidToken) {
					t.Fatalf("reuse: %v", refreshErr)
				}
			} else if refreshErr != nil && !errors.Is(refreshErr, domain.ErrInvalidToken) && !errors.Is(refreshErr, domain.ErrTokenReused) {
				t.Fatalf("refresh: %v", refreshErr)
			}
			sess, err := NewAuthSessions(app).Get(ctx, first.SessionID)
			if err != nil || sess.Live() {
				t.Fatalf("session %+v %v", sess, err)
			}
			assertLiveUnusedTokens(t, ctx, app, first.SessionID, 0)
			if _, err := read.ValidateSession(ctx, first.SessionID); !errors.Is(err, domain.ErrInvalidToken) {
				t.Fatalf("validate: %v", err)
			}
		})
	}
}

type credErr struct {
	cred application.Credentials
	err  error
}

func afterLockUoW(db beginner, hold func(context.Context) error) hookAuthUoW {
	return hookAuthUoW{db: db, wrapTokens: func(base application.TokenStore) application.TokenStore {
		return hookTokens{TokenStore: base, afterLock: hold}
	}}
}

func afterGetForUpdateUoW(db beginner, hold func(context.Context) error) hookAuthUoW {
	return hookAuthUoW{db: db, wrapTokens: func(base application.TokenStore) application.TokenStore {
		return hookTokens{TokenStore: base, afterGetForUpdate: hold}
	}}
}

func mustLoginRotated(t *testing.T, ctx context.Context, uow application.UnitOfWork, svc application.Service) (first, live application.Credentials) {
	t.Helper()
	email := pgEmail(t, "rr")
	if _, err := application.RegisterIn(ctx, uow, svc, pgRegister(email)); err != nil {
		t.Fatal(err)
	}
	var err error
	first, err = application.LoginIn(ctx, uow, svc, pgLogin(email))
	if err != nil {
		t.Fatal(err)
	}
	live, err = application.RefreshIn(ctx, uow, svc, application.RefreshCommand{
		RefreshToken: first.RefreshToken, IP: nextTestIP(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return first, live
}

func assertReuseRevoked(t *testing.T, ctx context.Context, conn *pgx.Conn, sessionID uuid.UUID, reason domain.RevokeReason) {
	t.Helper()
	sess, err := NewAuthSessions(conn).Get(ctx, sessionID)
	if err != nil || sess.Live() || sess.RevokeReason != reason {
		t.Fatalf("session %+v %v want %s", sess, err, reason)
	}
	var unused, unrevoked int
	if err := conn.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE used_at IS NULL AND revoked_at IS NULL),
		       count(*) FILTER (WHERE revoked_at IS NULL)
		  FROM auth.refresh_tokens WHERE session_id = $1`, sessionID).Scan(&unused, &unrevoked); err != nil {
		t.Fatal(err)
	}
	if unused != 0 || unrevoked != 0 {
		t.Fatalf("tokens unused=%d unrevoked=%d", unused, unrevoked)
	}
}
