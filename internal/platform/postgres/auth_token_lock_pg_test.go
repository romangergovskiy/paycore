package postgres

import (
	"context"
	"crypto/sha256"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/romangergovskiy/paycore/internal/auth/application"
	"github.com/romangergovskiy/paycore/internal/auth/domain"
)

// TestAuthRefreshInsertVsBulkRevoke forces the READ COMMITTED phantom
// that deadlocks a one-pass SELECT FOR UPDATE with Refresh:
//
//	A Refresh GetForUpdate(old) and holds the row
//	B Lock* waits on old
//	A INSERT new token, Touch session, Commit
//	B's first SELECT misses the new row
//	C Refresh GetForUpdate(new) and heads for the session
//	If B then Revoke(session) and UPDATE tokens, B waits on new while
//	C waits on session — PostgreSQL deadlock.
//
// Production Lock* loops until the locked set is complete, so B holds
// the new token before the session row. C waits on the token, not the
// session. Barriers are channels + pg_stat_activity; no time.Sleep.
func TestAuthRefreshInsertVsBulkRevoke(t *testing.T) {
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

	clock := &pgClock{t: time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)}
	svc := pgAuthService(clock)
	uow := NewAuthUoW(app)

	t.Run("one SELECT FOR UPDATE misses Refresh insert", func(t *testing.T) {
		email := pgEmail(t, "miss")
		if _, err := application.RegisterIn(ctx, uow, svc, pgRegister(email)); err != nil {
			t.Fatal(err)
		}
		cred, err := application.LoginIn(ctx, uow, svc, pgLogin(email))
		if err != nil {
			t.Fatal(err)
		}
		oldHash, err := svc.Issuer.Hash(cred.RefreshToken)
		if err != nil {
			t.Fatal(err)
		}

		held, err := app.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		oldTok, err := NewAuthTokens(held).GetForUpdate(ctx, oldHash)
		if err != nil {
			_ = held.Rollback(ctx)
			t.Fatal(err)
		}

		const waiterName = "paycore-one-pass-lock"
		setAppName(t, ctx, connB, waiterName)
		waiter, err := connB.Begin(ctx)
		if err != nil {
			_ = held.Rollback(ctx)
			t.Fatal(err)
		}
		lockErr := make(chan error, 1)
		go func() {
			_, err := lockTokenRows(ctx, waiter, lockTokensBySessionSQL, "tokens lock by session", cred.SessionID)
			lockErr <- err
		}()
		waitBackendLockWait(t, ctx, app, waiterName)

		newID := mustNewID(t)
		newHash := sha256.Sum256([]byte("inserted-" + newID.String()))
		if err := NewAuthTokens(held).Create(ctx, domain.RefreshToken{
			ID: newID, SessionID: cred.SessionID, Hash: newHash[:],
			ExpiresAt: clock.t.Add(domain.RefreshTTL), CreatedAt: clock.t,
		}); err != nil {
			_ = waiter.Rollback(ctx)
			_ = held.Rollback(ctx)
			t.Fatal(err)
		}
		if err := NewAuthTokens(held).MarkUsed(ctx, oldTok.ID, newID, clock.t); err != nil {
			_ = waiter.Rollback(ctx)
			_ = held.Rollback(ctx)
			t.Fatal(err)
		}
		if err := held.Commit(ctx); err != nil {
			_ = waiter.Rollback(ctx)
			t.Fatal(err)
		}
		if err := <-lockErr; err != nil {
			_ = waiter.Rollback(ctx)
			t.Fatalf("one-pass lock: %v", err)
		}

		probe, err := connD.Begin(ctx)
		if err != nil {
			_ = waiter.Rollback(ctx)
			t.Fatal(err)
		}
		if _, err := probe.Exec(ctx, `SET LOCAL lock_timeout = '5s'`); err != nil {
			_ = probe.Rollback(ctx)
			_ = waiter.Rollback(ctx)
			t.Fatal(err)
		}
		if _, err := NewAuthTokens(probe).GetForUpdate(ctx, newHash[:]); err != nil {
			_ = probe.Rollback(ctx)
			_ = waiter.Rollback(ctx)
			t.Fatalf("new token should be free after one-pass lock: %v", err)
		}
		if err := probe.Rollback(ctx); err != nil {
			_ = waiter.Rollback(ctx)
			t.Fatal(err)
		}
		if err := waiter.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("stable LockBySession sees insert on next statement", func(t *testing.T) {
		email := pgEmail(t, "see")
		if _, err := application.RegisterIn(ctx, uow, svc, pgRegister(email)); err != nil {
			t.Fatal(err)
		}
		cred, err := application.LoginIn(ctx, uow, svc, pgLogin(email))
		if err != nil {
			t.Fatal(err)
		}
		oldHash, err := svc.Issuer.Hash(cred.RefreshToken)
		if err != nil {
			t.Fatal(err)
		}

		if _, err := connB.Exec(ctx, `SET SESSION CHARACTERISTICS AS TRANSACTION ISOLATION LEVEL REPEATABLE READ`); err != nil {
			t.Fatal(err)
		}
		defer func() {
			_, _ = connB.Exec(ctx, `SET SESSION CHARACTERISTICS AS TRANSACTION ISOLATION LEVEL READ COMMITTED`)
		}()
		var sessionIso string
		if err := connB.QueryRow(ctx, `SHOW transaction_isolation`).Scan(&sessionIso); err != nil {
			t.Fatal(err)
		}
		if sessionIso != "repeatable read" {
			t.Fatalf("session isolation %q", sessionIso)
		}

		held, err := app.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		oldTok, err := NewAuthTokens(held).GetForUpdate(ctx, oldHash)
		if err != nil {
			_ = held.Rollback(ctx)
			t.Fatal(err)
		}

		const waiterName = "paycore-stable-lock"
		setAppName(t, ctx, connB, waiterName)
		waiter, err := beginAuthTx(ctx, connB)
		if err != nil {
			_ = held.Rollback(ctx)
			t.Fatal(err)
		}
		var txIso string
		if err := waiter.QueryRow(ctx, `SHOW transaction_isolation`).Scan(&txIso); err != nil {
			_ = waiter.Rollback(ctx)
			_ = held.Rollback(ctx)
			t.Fatal(err)
		}
		if txIso != "read committed" {
			_ = waiter.Rollback(ctx)
			_ = held.Rollback(ctx)
			t.Fatalf("auth tx isolation %q", txIso)
		}

		lockErr := make(chan error, 1)
		go func() {
			lockErr <- lockTokensUntilStable(ctx, waiter, lockTokensBySessionSQL, countTokensBySessionSQL, "tokens lock by session", cred.SessionID)
		}()
		waitBackendLockWait(t, ctx, app, waiterName)

		newID := mustNewID(t)
		newHash := sha256.Sum256([]byte("stable-" + newID.String()))
		if err := NewAuthTokens(held).Create(ctx, domain.RefreshToken{
			ID: newID, SessionID: cred.SessionID, Hash: newHash[:],
			ExpiresAt: clock.t.Add(domain.RefreshTTL), CreatedAt: clock.t,
		}); err != nil {
			_ = waiter.Rollback(ctx)
			_ = held.Rollback(ctx)
			t.Fatal(err)
		}
		if err := NewAuthTokens(held).MarkUsed(ctx, oldTok.ID, newID, clock.t); err != nil {
			_ = waiter.Rollback(ctx)
			_ = held.Rollback(ctx)
			t.Fatal(err)
		}
		if err := held.Commit(ctx); err != nil {
			_ = waiter.Rollback(ctx)
			t.Fatal(err)
		}
		if err := <-lockErr; err != nil {
			_ = waiter.Rollback(ctx)
			t.Fatalf("stable lock: %v", err)
		}

		probe, err := connD.Begin(ctx)
		if err != nil {
			_ = waiter.Rollback(ctx)
			t.Fatal(err)
		}
		if _, err := probe.Exec(ctx, `SET LOCAL lock_timeout = '5s'`); err != nil {
			_ = probe.Rollback(ctx)
			_ = waiter.Rollback(ctx)
			t.Fatal(err)
		}
		_, err = NewAuthTokens(probe).GetForUpdate(ctx, newHash[:])
		_ = probe.Rollback(ctx)
		if !isLockTimeout(err) {
			_ = waiter.Rollback(ctx)
			t.Fatalf("new token must be held after COUNT saw the insert: %v", err)
		}
		if err := waiter.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("one-pass lock then session deadlocks Refresh of inserted token", func(t *testing.T) {
		aErr, bErr, cErr := runRefreshInsertRevoke(t, ctx, app, connB, connC, clock, phantomOpts{
			onePass: true,
			revoke:  phantomLogout,
		})
		if !isDeadlock(aErr) && !isDeadlock(bErr) && !isDeadlock(cErr) {
			t.Fatalf("one-pass protocol must deadlock, A=%v B=%v C=%v", aErr, bErr, cErr)
		}
	})

	t.Run("stable LockBySession prevents the deadlock", func(t *testing.T) {
		aErr, bErr, cErr := runRefreshInsertRevoke(t, ctx, app, connB, connC, clock, phantomOpts{
			revoke: phantomLogout,
		})
		assertNoPhantomDeadlock(t, aErr, bErr, cErr)
	})

	t.Run("stable LockAllForUser prevents the deadlock", func(t *testing.T) {
		aErr, bErr, cErr := runRefreshInsertRevoke(t, ctx, app, connB, connC, clock, phantomOpts{
			revoke: phantomLogoutAll,
		})
		assertNoPhantomDeadlock(t, aErr, bErr, cErr)
	})
}

func TestAuthRefreshVsRevokeRegression(t *testing.T) {
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

	clock := &pgClock{t: time.Date(2026, 11, 10, 13, 0, 0, 0, time.UTC)}
	svc := pgAuthService(clock)
	uow := NewAuthUoW(app)
	read := pgReadService(clock, app)

	t.Run("two sequential Refresh one live chain", func(t *testing.T) {
		email := pgEmail(t, "seq")
		if _, err := application.RegisterIn(ctx, uow, svc, pgRegister(email)); err != nil {
			t.Fatal(err)
		}
		first, err := application.LoginIn(ctx, uow, svc, pgLogin(email))
		if err != nil {
			t.Fatal(err)
		}
		second, err := application.RefreshIn(ctx, uow, svc, application.RefreshCommand{RefreshToken: first.RefreshToken, IP: nextTestIP()})
		if err != nil {
			t.Fatal(err)
		}
		third, err := application.RefreshIn(ctx, uow, svc, application.RefreshCommand{RefreshToken: second.RefreshToken, IP: nextTestIP()})
		if err != nil {
			t.Fatal(err)
		}
		if second.SessionID != first.SessionID || third.SessionID != first.SessionID {
			t.Fatal("session changed")
		}
		assertLiveUnusedTokens(t, ctx, app, first.SessionID, 1)
		sess, err := NewAuthSessions(app).Get(ctx, first.SessionID)
		if err != nil || !sess.Live() {
			t.Fatalf("session %+v %v", sess, err)
		}
		_, err = application.RefreshIn(ctx, uow, svc, application.RefreshCommand{RefreshToken: first.RefreshToken, IP: nextTestIP()})
		if !errors.Is(err, domain.ErrTokenReused) {
			t.Fatalf("old token: %v", err)
		}
		sess, err = NewAuthSessions(app).Get(ctx, first.SessionID)
		if err != nil || sess.Live() || sess.RevokeReason != domain.RevokeTokenReuse {
			t.Fatalf("reuse session %+v %v", sess, err)
		}
		assertLiveUnusedTokens(t, ctx, app, first.SessionID, 0)
	})

	t.Run("parallel Refresh same token at most one success", func(t *testing.T) {
		email := pgEmail(t, "par")
		if _, err := application.RegisterIn(ctx, uow, svc, pgRegister(email)); err != nil {
			t.Fatal(err)
		}
		cred, err := application.LoginIn(ctx, uow, svc, pgLogin(email))
		if err != nil {
			t.Fatal(err)
		}
		errs := make([]error, 2)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, errs[0] = application.RefreshIn(ctx, NewAuthUoW(app), pgAuthService(clock), application.RefreshCommand{
				RefreshToken: cred.RefreshToken, IP: nextTestIP(),
			})
		}()
		go func() {
			defer wg.Done()
			_, errs[1] = application.RefreshIn(ctx, NewAuthUoW(connB), pgAuthService(clock), application.RefreshCommand{
				RefreshToken: cred.RefreshToken, IP: nextTestIP(),
			})
		}()
		wg.Wait()
		ok, fail := 0, 0
		for i, err := range errs {
			switch {
			case err == nil:
				ok++
			case errors.Is(err, domain.ErrTokenReused), errors.Is(err, domain.ErrInvalidToken):
				fail++
			default:
				t.Fatalf("refresh %d: %v", i, err)
			}
		}
		if ok != 1 || fail != 1 {
			t.Fatalf("success=%d fail=%d errs=%v", ok, fail, errs)
		}
		var live int
		if err := app.QueryRow(ctx, `
			SELECT count(*) FROM auth.refresh_tokens t
			JOIN auth.sessions s ON s.id = t.session_id
			 WHERE s.id = $1
			   AND t.used_at IS NULL
			   AND t.revoked_at IS NULL
			   AND s.revoked_at IS NULL`, cred.SessionID).Scan(&live); err != nil {
			t.Fatal(err)
		}
		if live > 1 {
			t.Fatalf("live unused chains %d", live)
		}
	})

	t.Run("repeat Logout then Refresh", func(t *testing.T) {
		email := pgEmail(t, "rep")
		if _, err := application.RegisterIn(ctx, uow, svc, pgRegister(email)); err != nil {
			t.Fatal(err)
		}
		cred, err := application.LoginIn(ctx, uow, svc, pgLogin(email))
		if err != nil {
			t.Fatal(err)
		}
		p := domain.Principal{UserID: cred.User.ID, SessionID: cred.SessionID, Roles: cred.User.Roles}
		if err := application.LogoutIn(ctx, uow, svc, p); err != nil {
			t.Fatal(err)
		}
		if err := application.LogoutIn(ctx, uow, svc, p); err != nil {
			t.Fatalf("repeat logout: %v", err)
		}
		sess, err := NewAuthSessions(app).Get(ctx, cred.SessionID)
		if err != nil || sess.Live() || sess.RevokeReason != domain.RevokeLogout {
			t.Fatalf("session %+v %v", sess, err)
		}
		assertLiveUnusedTokens(t, ctx, app, cred.SessionID, 0)
		_, err = application.RefreshIn(ctx, uow, svc, application.RefreshCommand{RefreshToken: cred.RefreshToken, IP: nextTestIP()})
		if !errors.Is(err, domain.ErrInvalidToken) && !errors.Is(err, domain.ErrTokenReused) {
			t.Fatalf("refresh after logout: %v", err)
		}
		if _, err := read.ValidateSession(ctx, cred.SessionID); !errors.Is(err, domain.ErrInvalidToken) {
			t.Fatalf("validate: %v", err)
		}
	})

	for _, tc := range []struct {
		name  string
		admin bool
		run   func(cred application.Credentials, actor domain.Principal) error
	}{
		{
			name: "Refresh vs Logout",
			run: func(cred application.Credentials, _ domain.Principal) error {
				return application.LogoutIn(ctx, NewAuthUoW(connB), pgAuthService(clock), domain.Principal{
					UserID: cred.User.ID, SessionID: cred.SessionID, Roles: cred.User.Roles,
				})
			},
		},
		{
			name: "Refresh vs LogoutAll",
			run: func(cred application.Credentials, _ domain.Principal) error {
				return application.LogoutAllIn(ctx, NewAuthUoW(connB), pgAuthService(clock), domain.Principal{
					UserID: cred.User.ID, SessionID: cred.SessionID, Roles: cred.User.Roles,
				})
			},
		},
		{
			name: "Refresh vs RevokeSession",
			run: func(cred application.Credentials, _ domain.Principal) error {
				return application.RevokeSessionIn(ctx, NewAuthUoW(connB), pgAuthService(clock), application.RevokeSessionCommand{
					Actor:     domain.Principal{UserID: cred.User.ID, SessionID: cred.SessionID, Roles: cred.User.Roles},
					SessionID: cred.SessionID,
				})
			},
		},
		{
			name: "Refresh vs ChangePassword",
			run: func(cred application.Credentials, _ domain.Principal) error {
				return application.ChangePasswordIn(ctx, NewAuthUoW(connB), pgAuthService(clock), application.ChangePasswordCommand{
					Actor:       domain.Principal{UserID: cred.User.ID, SessionID: cred.SessionID, Roles: cred.User.Roles},
					OldPassword: "twelvechars!",
					NewPassword: "newpassword!!",
				})
			},
		},
		{
			name:  "Refresh vs LockUser",
			admin: true,
			run: func(cred application.Credentials, actor domain.Principal) error {
				return application.LockUserIn(ctx, NewAuthUoW(connB), pgAuthService(clock), pgStatus(actor, cred.User.ID))
			},
		},
		{
			name:  "Refresh vs DisableUser",
			admin: true,
			run: func(cred application.Credentials, actor domain.Principal) error {
				return application.DisableUserIn(ctx, NewAuthUoW(connB), pgAuthService(clock), pgStatus(actor, cred.User.ID))
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock.t = clock.t.Add(domain.RateWindow)
			email := pgEmail(t, "rv")
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
				admin := seedPGAdmin(t, ctx, app, clock, pgEmail(t, "rv-a"))
				actor = adminPrincipal(admin)
			}
			var refreshErr, revokeErr error
			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				_, refreshErr = application.RefreshIn(ctx, NewAuthUoW(app), pgAuthService(clock), application.RefreshCommand{
					RefreshToken: cred.RefreshToken, IP: nextTestIP(),
				})
			}()
			go func() {
				defer wg.Done()
				revokeErr = tc.run(cred, actor)
			}()
			wg.Wait()
			if isDeadlock(refreshErr) || isDeadlock(revokeErr) {
				t.Fatalf("deadlock refresh=%v revoke=%v", refreshErr, revokeErr)
			}
			if revokeErr != nil {
				t.Fatalf("revoke: %v", revokeErr)
			}
			if refreshErr != nil && !errors.Is(refreshErr, domain.ErrInvalidToken) && !errors.Is(refreshErr, domain.ErrTokenReused) {
				t.Fatalf("refresh: %v", refreshErr)
			}
			sess, err := NewAuthSessions(app).Get(ctx, cred.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			if sess.Live() {
				t.Fatalf("session still live after %s", tc.name)
			}
			assertLiveUnusedTokens(t, ctx, app, cred.SessionID, 0)
			if _, err := read.ValidateSession(ctx, cred.SessionID); !errors.Is(err, domain.ErrInvalidToken) {
				t.Fatalf("validate: %v", err)
			}
		})
	}
}

// TestAuthUoWReadCommitted proves Do begins READ COMMITTED even when the
// session default is REPEATABLE READ. Isolation comes from BeginTx, not
// SET TRANSACTION after the first statement and not the role default.
func TestAuthUoWReadCommitted(t *testing.T) {
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
	if _, err := app.Exec(ctx, `SET SESSION CHARACTERISTICS AS TRANSACTION ISOLATION LEVEL REPEATABLE READ`); err != nil {
		t.Fatal(err)
	}
	var sessionIso string
	if err := app.QueryRow(ctx, `SHOW transaction_isolation`).Scan(&sessionIso); err != nil {
		t.Fatal(err)
	}
	if sessionIso != "repeatable read" {
		t.Fatalf("session isolation %q", sessionIso)
	}

	var got string
	err := NewAuthUoW(app).Do(ctx, func(ctx context.Context, d application.Deps) (application.Decision, error) {
		u, ok := d.Users.(*AuthUsers)
		if !ok {
			return application.Rollback, errors.New("expected *AuthUsers")
		}
		if err := u.q.QueryRow(ctx, `SHOW transaction_isolation`).Scan(&got); err != nil {
			return application.Rollback, err
		}
		return application.Rollback, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != "read committed" {
		t.Fatalf("auth UoW isolation %q", got)
	}
}

type phantomOpts struct {
	onePass bool
	revoke  func(ctx context.Context, uow application.UnitOfWork, svc application.Service, cred application.Credentials) error
}

func phantomLogout(ctx context.Context, uow application.UnitOfWork, svc application.Service, cred application.Credentials) error {
	return application.LogoutIn(ctx, uow, svc, domain.Principal{
		UserID: cred.User.ID, SessionID: cred.SessionID, Roles: cred.User.Roles,
	})
}

func phantomLogoutAll(ctx context.Context, uow application.UnitOfWork, svc application.Service, cred application.Credentials) error {
	return application.LogoutAllIn(ctx, uow, svc, domain.Principal{
		UserID: cred.User.ID, SessionID: cred.SessionID, Roles: cred.User.Roles,
	})
}

func assertNoPhantomDeadlock(t *testing.T, aErr, bErr, cErr error) {
	t.Helper()
	if isDeadlock(aErr) || isDeadlock(bErr) || isDeadlock(cErr) {
		t.Fatalf("stable lock deadlocked A=%v B=%v C=%v", aErr, bErr, cErr)
	}
	if aErr != nil {
		t.Fatalf("refresh A: %v", aErr)
	}
	if bErr != nil {
		t.Fatalf("revoke B: %v", bErr)
	}
	if cErr == nil || (!errors.Is(cErr, domain.ErrInvalidToken) && !errors.Is(cErr, domain.ErrTokenReused)) {
		t.Fatalf("refresh C after revoke: %v", cErr)
	}
}

func runRefreshInsertRevoke(t *testing.T, ctx context.Context, app, connB, connC *pgx.Conn, clock *pgClock, opts phantomOpts) (aErr, bErr, cErr error) {
	t.Helper()
	svc := pgAuthService(clock)
	uow := NewAuthUoW(app)
	email := pgEmail(t, "ph")
	if _, err := application.RegisterIn(ctx, uow, svc, pgRegister(email)); err != nil {
		t.Fatal(err)
	}
	cred, err := application.LoginIn(ctx, uow, svc, pgLogin(email))
	if err != nil {
		t.Fatal(err)
	}

	aHold := newPark()
	bAfterLock := newPark()
	cBeforeTouch := newPark()
	defer aHold.release()
	defer bAfterLock.release()
	defer cBeforeTouch.release()

	aUoW := hookAuthUoW{db: app, wrapTokens: func(base application.TokenStore) application.TokenStore {
		return hookTokens{TokenStore: base, afterGetForUpdate: aHold.hold}
	}}
	bUoW := hookAuthUoW{db: connB, wrapTokens: func(base application.TokenStore) application.TokenStore {
		store := base
		if opts.onePass {
			at, ok := base.(*AuthTokens)
			if !ok {
				panic("auth phantom test: expected *AuthTokens")
			}
			store = onePassAuthTokens{AuthTokens: at}
		}
		return hookTokens{TokenStore: store, afterLock: bAfterLock.hold}
	}}
	cUoW := hookAuthUoW{
		db: connC,
		wrapSessions: func(base application.SessionStore) application.SessionStore {
			return hookSessions{SessionStore: base, beforeTouch: cBeforeTouch.hold}
		},
	}

	const bName = "paycore-phantom-b"
	const cName = "paycore-phantom-c"
	setAppName(t, ctx, connB, bName)
	setAppName(t, ctx, connC, cName)

	type credErr struct {
		cred application.Credentials
		err  error
	}
	aDone := make(chan credErr, 1)
	go func() {
		out, err := application.RefreshIn(ctx, aUoW, pgAuthService(clock), application.RefreshCommand{
			RefreshToken: cred.RefreshToken, IP: nextTestIP(),
		})
		aDone <- credErr{out, err}
	}()
	if err := aHold.waitArrived(ctx); err != nil {
		t.Fatal(err)
	}

	bDone := make(chan error, 1)
	go func() {
		bDone <- opts.revoke(ctx, bUoW, pgAuthService(clock), cred)
	}()
	waitBackendLockWait(t, ctx, app, bName)

	aHold.release()
	aRes := <-aDone
	aErr = aRes.err
	if aErr != nil {
		bErr = <-bDone
		return aErr, bErr, nil
	}

	if err := bAfterLock.waitArrived(ctx); err != nil {
		bErr = <-bDone
		return aErr, bErr, err
	}

	sess, err := NewAuthSessions(app).Get(ctx, cred.SessionID)
	if err != nil || !sess.Live() {
		bErr = <-bDone
		t.Fatalf("session should still be live while B holds tokens only: %+v %v", sess, err)
	}

	cDone := make(chan error, 1)
	go func() {
		_, err := application.RefreshIn(ctx, cUoW, pgAuthService(clock), application.RefreshCommand{
			RefreshToken: aRes.cred.RefreshToken, IP: nextTestIP(),
		})
		cDone <- err
	}()

	if opts.onePass {
		if err := cBeforeTouch.waitArrived(ctx); err != nil {
			bErr = <-bDone
			cErr = <-cDone
			return aErr, bErr, err
		}
		bAfterLock.release()
		waitBackendLockWait(t, ctx, app, bName)
		cBeforeTouch.release()
		bErr = <-bDone
		cErr = <-cDone
		return aErr, bErr, cErr
	}

	waitBackendLockWait(t, ctx, app, cName)
	bAfterLock.release()
	bErr = <-bDone
	cBeforeTouch.release()
	cErr = <-cDone
	if bErr == nil {
		got, err := NewAuthSessions(app).Get(ctx, cred.SessionID)
		if err != nil || got.Live() {
			t.Fatalf("session after revoke %+v %v", got, err)
		}
		assertLiveUnusedTokens(t, ctx, app, cred.SessionID, 0)
	}
	return aErr, bErr, cErr
}

type park struct {
	at     chan struct{}
	goCh   chan struct{}
	atOnce sync.Once
	goOnce sync.Once
}

func newPark() *park {
	return &park{at: make(chan struct{}), goCh: make(chan struct{})}
}

func (p *park) hold(ctx context.Context) error {
	p.atOnce.Do(func() { close(p.at) })
	select {
	case <-p.goCh:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *park) waitArrived(ctx context.Context) error {
	select {
	case <-p.at:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *park) release() {
	p.goOnce.Do(func() { close(p.goCh) })
}

type hookAuthUoW struct {
	db           beginner
	wrapTokens   func(application.TokenStore) application.TokenStore
	wrapSessions func(application.SessionStore) application.SessionStore
	wrapUsers    func(application.UserStore) application.UserStore
}

func (u hookAuthUoW) Do(ctx context.Context, fn func(context.Context, application.Deps) (application.Decision, error)) error {
	if fn == nil {
		return errors.New("auth: unit of work callback is required")
	}
	tx, err := beginAuthTx(ctx, u.db)
	if err != nil {
		return err
	}
	return runAuthTx(ctx, tx, func() (application.Decision, error) {
		d := authDeps(tx)
		if u.wrapUsers != nil {
			d.Users = u.wrapUsers(d.Users)
		}
		if u.wrapTokens != nil {
			d.Tokens = u.wrapTokens(d.Tokens)
		}
		if u.wrapSessions != nil {
			d.Sessions = u.wrapSessions(d.Sessions)
		}
		return fn(ctx, d)
	})
}

type hookTokens struct {
	application.TokenStore
	afterGetForUpdate func(context.Context) error
	afterLock         func(context.Context) error
}

func (h hookTokens) GetForUpdate(ctx context.Context, hash []byte) (domain.RefreshToken, error) {
	tok, err := h.TokenStore.GetForUpdate(ctx, hash)
	if err != nil {
		return tok, err
	}
	if h.afterGetForUpdate != nil {
		if err := h.afterGetForUpdate(ctx); err != nil {
			return domain.RefreshToken{}, err
		}
	}
	return tok, nil
}

func (h hookTokens) LockBySession(ctx context.Context, sessionID uuid.UUID) error {
	if err := h.TokenStore.LockBySession(ctx, sessionID); err != nil {
		return err
	}
	if h.afterLock != nil {
		return h.afterLock(ctx)
	}
	return nil
}

func (h hookTokens) LockAllForUser(ctx context.Context, userID uuid.UUID) error {
	if err := h.TokenStore.LockAllForUser(ctx, userID); err != nil {
		return err
	}
	if h.afterLock != nil {
		return h.afterLock(ctx)
	}
	return nil
}

type hookUsers struct {
	application.UserStore
	afterGetForUpdate func(context.Context) error
}

func (h hookUsers) GetForUpdate(ctx context.Context, id uuid.UUID) (domain.User, error) {
	u, err := h.UserStore.GetForUpdate(ctx, id)
	if err != nil {
		return u, err
	}
	if h.afterGetForUpdate != nil {
		if err := h.afterGetForUpdate(ctx); err != nil {
			return domain.User{}, err
		}
	}
	return u, nil
}

type hookSessions struct {
	application.SessionStore
	beforeTouch func(context.Context) error
}

func (h hookSessions) Touch(ctx context.Context, id uuid.UUID, at time.Time) error {
	if h.beforeTouch != nil {
		if err := h.beforeTouch(ctx); err != nil {
			return err
		}
	}
	return h.SessionStore.Touch(ctx, id, at)
}

type onePassAuthTokens struct{ *AuthTokens }

func (s onePassAuthTokens) LockBySession(ctx context.Context, sessionID uuid.UUID) error {
	_, err := lockTokenRows(ctx, s.q, lockTokensBySessionSQL, "tokens lock by session", sessionID)
	return err
}

func (s onePassAuthTokens) LockAllForUser(ctx context.Context, userID uuid.UUID) error {
	_, err := lockTokenRows(ctx, s.q, lockTokensForUserSQL, "tokens lock all for user", userID)
	return err
}

func assertLiveUnusedTokens(t *testing.T, ctx context.Context, conn *pgx.Conn, sessionID uuid.UUID, want int) {
	t.Helper()
	var n int
	if err := conn.QueryRow(ctx, `
		SELECT count(*) FROM auth.refresh_tokens
		 WHERE session_id = $1 AND used_at IS NULL AND revoked_at IS NULL`, sessionID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != want {
		t.Fatalf("live unused tokens = %d, want %d", n, want)
	}
}
