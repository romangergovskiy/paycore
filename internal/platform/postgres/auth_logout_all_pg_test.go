package postgres

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/romangergovskiy/paycore/internal/auth/application"
	"github.com/romangergovskiy/paycore/internal/auth/domain"
)

// TestAuthLoginVsLogoutAll forces Login vs LogoutAll on Users.GetForUpdate.
//
//	A: Login holds the user row, LogoutAll waits, Login commits a new
//	   session+token, LogoutAll continues and revokes that session too.
//	B: LogoutAll holds the user row, Login waits, LogoutAll revokes and
//	   commits, Login continues and creates a new live session
//	   (logout-all then login, not a leak of a session that existed
//	   during LogoutAll).
//
// Barriers are channels + pg_stat_activity; no time.Sleep.
func TestAuthLoginVsLogoutAll(t *testing.T) {
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

	clock := &pgClock{t: time.Date(2026, 11, 10, 14, 0, 0, 0, time.UTC)}
	svc := pgAuthService(clock)
	uow := NewAuthUoW(app)

	t.Run("A Login first then LogoutAll revokes the inserted session", func(t *testing.T) {
		email := pgEmail(t, "lo-a")
		if _, err := application.RegisterIn(ctx, uow, svc, pgRegister(email)); err != nil {
			t.Fatal(err)
		}
		first, err := application.LoginIn(ctx, uow, svc, pgLogin(email))
		if err != nil {
			t.Fatal(err)
		}

		loginHold := newPark()
		defer loginHold.release()
		loginUoW := hookAuthUoW{db: connB, wrapUsers: func(base application.UserStore) application.UserStore {
			return hookUsers{UserStore: base, afterGetForUpdate: loginHold.hold}
		}}
		const logoutName = "paycore-logoutall-a"
		setAppName(t, ctx, connC, logoutName)

		type credErr struct {
			cred application.Credentials
			err  error
		}
		loginDone := make(chan credErr, 1)
		go func() {
			out, err := application.LoginIn(ctx, loginUoW, pgAuthService(clock), pgLogin(email))
			loginDone <- credErr{out, err}
		}()
		if err := loginHold.waitArrived(ctx); err != nil {
			t.Fatal(err)
		}
		if n := countLiveSessions(t, ctx, app, first.User.ID); n != 1 {
			t.Fatalf("live sessions while Login holds user: %d", n)
		}

		logoutDone := make(chan error, 1)
		go func() {
			logoutDone <- application.LogoutAllIn(ctx, NewAuthUoW(connC), pgAuthService(clock), domain.Principal{
				UserID: first.User.ID, SessionID: first.SessionID, Roles: first.User.Roles,
			})
		}()
		waitBackendLockWait(t, ctx, app, logoutName)

		loginHold.release()
		loginRes := <-loginDone
		logoutErr := <-logoutDone
		if loginRes.err != nil {
			t.Fatalf("login: %v", loginRes.err)
		}
		if logoutErr != nil {
			t.Fatalf("logout-all: %v", logoutErr)
		}
		if loginRes.cred.SessionID == first.SessionID {
			t.Fatal("second login reused session")
		}
		assertUserFullyRevoked(t, ctx, app, first.User.ID)
		for _, sid := range []uuid.UUID{first.SessionID, loginRes.cred.SessionID} {
			sess, err := NewAuthSessions(app).Get(ctx, sid)
			if err != nil || sess.Live() || sess.RevokeReason != domain.RevokeLogoutAll {
				t.Fatalf("session %s %+v %v", sid, sess, err)
			}
		}
	})

	t.Run("B LogoutAll first then Login creates a new live session", func(t *testing.T) {
		email := pgEmail(t, "lo-b")
		if _, err := application.RegisterIn(ctx, uow, svc, pgRegister(email)); err != nil {
			t.Fatal(err)
		}
		first, err := application.LoginIn(ctx, uow, svc, pgLogin(email))
		if err != nil {
			t.Fatal(err)
		}

		logoutHold := newPark()
		defer logoutHold.release()
		logoutUoW := hookAuthUoW{db: connB, wrapUsers: func(base application.UserStore) application.UserStore {
			return hookUsers{UserStore: base, afterGetForUpdate: logoutHold.hold}
		}}
		const loginName = "paycore-login-b"
		setAppName(t, ctx, connC, loginName)

		logoutDone := make(chan error, 1)
		go func() {
			logoutDone <- application.LogoutAllIn(ctx, logoutUoW, pgAuthService(clock), domain.Principal{
				UserID: first.User.ID, SessionID: first.SessionID, Roles: first.User.Roles,
			})
		}()
		if err := logoutHold.waitArrived(ctx); err != nil {
			t.Fatal(err)
		}

		type credErr struct {
			cred application.Credentials
			err  error
		}
		loginDone := make(chan credErr, 1)
		go func() {
			out, err := application.LoginIn(ctx, NewAuthUoW(connC), pgAuthService(clock), pgLogin(email))
			loginDone <- credErr{out, err}
		}()
		waitBackendLockWait(t, ctx, app, loginName)

		logoutHold.release()
		logoutErr := <-logoutDone
		loginRes := <-loginDone
		if logoutErr != nil {
			t.Fatalf("logout-all: %v", logoutErr)
		}
		if loginRes.err != nil {
			t.Fatalf("login after logout-all: %v", loginRes.err)
		}
		old, err := NewAuthSessions(app).Get(ctx, first.SessionID)
		if err != nil || old.Live() || old.RevokeReason != domain.RevokeLogoutAll {
			t.Fatalf("old session %+v %v", old, err)
		}
		got, err := NewAuthSessions(app).Get(ctx, loginRes.cred.SessionID)
		if err != nil || !got.Live() {
			t.Fatalf("new login session %+v %v", got, err)
		}
		if countLiveSessions(t, ctx, app, first.User.ID) != 1 {
			t.Fatal("want exactly one live session after login-behind-logout-all")
		}
	})
}

func TestAuthLogoutAllConcurrency(t *testing.T) {
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

	clock := &pgClock{t: time.Date(2026, 11, 10, 15, 0, 0, 0, time.UTC)}
	svc := pgAuthService(clock)
	uow := NewAuthUoW(app)
	read := pgReadService(clock, app)

	t.Run("forced token revoke rolls back sessions", func(t *testing.T) {
		email := pgEmail(t, "lo-rb")
		if _, err := application.RegisterIn(ctx, uow, svc, pgRegister(email)); err != nil {
			t.Fatal(err)
		}
		cred, err := application.LoginIn(ctx, uow, svc, pgLogin(email))
		if err != nil {
			t.Fatal(err)
		}
		failUoW := hookAuthUoW{db: connB, wrapTokens: func(base application.TokenStore) application.TokenStore {
			return failRevokeAllTokens{TokenStore: base}
		}}
		err = application.LogoutAllIn(ctx, failUoW, pgAuthService(clock), domain.Principal{
			UserID: cred.User.ID, SessionID: cred.SessionID, Roles: cred.User.Roles,
		})
		if err == nil {
			t.Fatal("expected rollback")
		}
		if isDeadlock(err) {
			t.Fatalf("deadlock: %v", err)
		}
		sess, err := NewAuthSessions(app).Get(ctx, cred.SessionID)
		if err != nil || !sess.Live() {
			t.Fatalf("session after rollback %+v %v", sess, err)
		}
		assertLiveUnusedTokens(t, ctx, app, cred.SessionID, 1)
		if _, err := read.ValidateSession(ctx, cred.SessionID); err != nil {
			t.Fatalf("validate after rollback: %v", err)
		}
	})

	t.Run("parallel LogoutAll", func(t *testing.T) {
		email := pgEmail(t, "lo-par")
		if _, err := application.RegisterIn(ctx, uow, svc, pgRegister(email)); err != nil {
			t.Fatal(err)
		}
		cred, err := application.LoginIn(ctx, uow, svc, pgLogin(email))
		if err != nil {
			t.Fatal(err)
		}
		p := domain.Principal{UserID: cred.User.ID, SessionID: cred.SessionID, Roles: cred.User.Roles}
		errs := make([]error, 2)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			errs[0] = application.LogoutAllIn(ctx, NewAuthUoW(connB), pgAuthService(clock), p)
		}()
		go func() {
			defer wg.Done()
			errs[1] = application.LogoutAllIn(ctx, NewAuthUoW(connC), pgAuthService(clock), p)
		}()
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("logout-all %d: %v", i, err)
			}
		}
		assertUserFullyRevoked(t, ctx, app, cred.User.ID)
	})

	for _, tc := range []struct {
		name  string
		admin bool
		run   func(cred application.Credentials, actor domain.Principal) error
	}{
		{
			name: "LogoutAll vs Refresh",
			run: func(cred application.Credentials, _ domain.Principal) error {
				_, err := application.RefreshIn(ctx, NewAuthUoW(connC), pgAuthService(clock), application.RefreshCommand{
					RefreshToken: cred.RefreshToken, IP: nextTestIP(),
				})
				return err
			},
		},
		{
			name: "LogoutAll vs ChangePassword",
			run: func(cred application.Credentials, _ domain.Principal) error {
				return application.ChangePasswordIn(ctx, NewAuthUoW(connC), pgAuthService(clock), application.ChangePasswordCommand{
					Actor:       domain.Principal{UserID: cred.User.ID, SessionID: cred.SessionID, Roles: cred.User.Roles},
					OldPassword: "twelvechars!",
					NewPassword: "newpassword!!",
				})
			},
		},
		{
			name:  "LogoutAll vs LockUser",
			admin: true,
			run: func(cred application.Credentials, actor domain.Principal) error {
				return application.LockUserIn(ctx, NewAuthUoW(connC), pgAuthService(clock), pgStatus(actor, cred.User.ID))
			},
		},
		{
			name:  "LogoutAll vs DisableUser",
			admin: true,
			run: func(cred application.Credentials, actor domain.Principal) error {
				return application.DisableUserIn(ctx, NewAuthUoW(connC), pgAuthService(clock), pgStatus(actor, cred.User.ID))
			},
		},
		{
			name: "LogoutAll vs Logout",
			run: func(cred application.Credentials, _ domain.Principal) error {
				return application.LogoutIn(ctx, NewAuthUoW(connC), pgAuthService(clock), domain.Principal{
					UserID: cred.User.ID, SessionID: cred.SessionID, Roles: cred.User.Roles,
				})
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock.t = clock.t.Add(domain.RateWindow)
			email := pgEmail(t, "lo-vs")
			if _, err := application.RegisterIn(ctx, uow, svc, pgRegister(email)); err != nil {
				t.Fatal(err)
			}
			cred, err := application.LoginIn(ctx, uow, svc, pgLogin(email))
			if err != nil {
				t.Fatal(err)
			}
			var actor domain.Principal
			if tc.admin {
				clearActiveAdmins(t, ctx, app, clock)
				admin := seedPGAdmin(t, ctx, app, clock, pgEmail(t, "lo-vs-a"))
				actor = adminPrincipal(admin)
			}
			var logoutErr, otherErr error
			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				logoutErr = application.LogoutAllIn(ctx, NewAuthUoW(connB), pgAuthService(clock), domain.Principal{
					UserID: cred.User.ID, SessionID: cred.SessionID, Roles: cred.User.Roles,
				})
			}()
			go func() {
				defer wg.Done()
				otherErr = tc.run(cred, actor)
			}()
			wg.Wait()
			if isDeadlock(logoutErr) || isDeadlock(otherErr) {
				t.Fatalf("deadlock logout-all=%v other=%v", logoutErr, otherErr)
			}
			if logoutErr != nil {
				t.Fatalf("logout-all: %v", logoutErr)
			}
			if otherErr != nil && !errors.Is(otherErr, domain.ErrInvalidToken) && !errors.Is(otherErr, domain.ErrTokenReused) {
				t.Fatalf("other: %v", otherErr)
			}
			assertUserFullyRevoked(t, ctx, app, cred.User.ID)
			if _, err := read.ValidateSession(ctx, cred.SessionID); !errors.Is(err, domain.ErrInvalidToken) {
				t.Fatalf("validate: %v", err)
			}
		})
	}
}

type failRevokeAllTokens struct {
	application.TokenStore
}

func (f failRevokeAllTokens) RevokeAllForUser(ctx context.Context, userID uuid.UUID, at time.Time) error {
	return errors.New("forced token revoke fail")
}

func countLiveSessions(t *testing.T, ctx context.Context, conn *pgx.Conn, userID uuid.UUID) int {
	t.Helper()
	var n int
	if err := conn.QueryRow(ctx, `
		SELECT count(*) FROM auth.sessions
		 WHERE user_id = $1 AND revoked_at IS NULL`, userID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func assertUserFullyRevoked(t *testing.T, ctx context.Context, conn *pgx.Conn, userID uuid.UUID) {
	t.Helper()
	if n := countLiveSessions(t, ctx, conn, userID); n != 0 {
		t.Fatalf("live sessions = %d", n)
	}
	var tokens int
	if err := conn.QueryRow(ctx, `
		SELECT count(*) FROM auth.refresh_tokens t
		  JOIN auth.sessions s ON s.id = t.session_id
		 WHERE s.user_id = $1 AND t.used_at IS NULL AND t.revoked_at IS NULL`, userID).Scan(&tokens); err != nil {
		t.Fatal(err)
	}
	if tokens != 0 {
		t.Fatalf("live unused tokens = %d", tokens)
	}
}
