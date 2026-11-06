package postgres

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/romangergovskiy/paycore/internal/auth/application"
	"github.com/romangergovskiy/paycore/internal/auth/domain"
)

var testIPSeq atomic.Uint32

func TestAuthPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	c := startCluster(t)
	if err := Bootstrap(ctx, c.admin, c.owner, c.app, c.readonly); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if err := Up(ctx, c.owner); err != nil {
		t.Fatalf("up: %v", err)
	}

	owner := mustConnect(t, ctx, c.owner)
	defer owner.Close(ctx)
	app := mustConnect(t, ctx, c.app)
	defer app.Close(ctx)
	other := mustConnect(t, ctx, c.app)
	defer other.Close(ctx)

	uow := NewAuthUoW(app)
	uowOther := NewAuthUoW(other)
	clock := &pgClock{t: time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)}
	svc := pgAuthService(clock)

	t.Run("missing rows", func(t *testing.T) {
		id := mustNewID(t)
		if _, err := NewAuthUsers(app).Get(ctx, id); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("users get: %v", err)
		}
		if _, err := NewAuthUsers(app).GetByEmail(ctx, "missing@users.test"); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("users email: %v", err)
		}
		if _, err := NewAuthUsers(app).GetForUpdate(ctx, id); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("users lock: %v", err)
		}
		if err := NewAuthUsers(app).Update(ctx, domain.User{
			ID: id, Email: "missing@users.test", PasswordHash: testArgon2idPHC, Status: domain.StatusActive,
		}); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("users update: %v", err)
		}
		if _, err := NewAuthSessions(app).Get(ctx, id); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("sessions: %v", err)
		}
		sum := sha256.Sum256([]byte("no-such-refresh"))
		if _, err := NewAuthTokens(app).GetByHash(ctx, sum[:]); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("tokens: %v", err)
		}
		if _, err := NewAuthTokens(app).GetForUpdate(ctx, sum[:]); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("tokens lock: %v", err)
		}
	})

	t.Run("password hash check is schema not a use-case rule", func(t *testing.T) {
		u := domain.User{
			ID: mustNewID(t), Email: pgEmail(t, "hash"), PasswordHash: "plaintext",
			Status: domain.StatusActive, CreatedAt: clock.t, UpdatedAt: clock.t,
		}
		err := NewAuthUsers(app).Create(ctx, u)
		if err == nil {
			t.Fatal("accepted non-argon2 hash")
		}
		if errors.Is(err, domain.ErrEmailTaken) || errors.Is(err, domain.ErrInvalidRequest) {
			t.Fatalf("mapped CHECK to business error: %v", err)
		}
	})

	t.Run("register commit and unique email", func(t *testing.T) {
		email := pgEmail(t, "reg")
		cmd := pgRegister(email)
		got, err := application.RegisterIn(ctx, uow, svc, cmd)
		if err != nil {
			t.Fatal(err)
		}
		if got.Email != email || len(got.Roles) != 1 || got.Roles[0] != domain.RoleCustomer {
			t.Fatalf("got %+v", got)
		}
		stored, err := NewAuthUsers(app).Get(ctx, got.ID)
		if err != nil || stored.PasswordHash == cmd.Password {
			t.Fatalf("stored %+v %v", stored, err)
		}
		_, err = application.RegisterIn(ctx, uow, svc, cmd)
		if !errors.Is(err, domain.ErrEmailTaken) {
			t.Fatalf("second register: %v", err)
		}
		dup := domain.User{
			ID: mustNewID(t), Email: email, PasswordHash: testArgon2idPHC,
			Status: domain.StatusActive, CreatedAt: clock.t, UpdatedAt: clock.t,
		}
		if err := NewAuthUsers(app).Create(ctx, dup); !errors.Is(err, domain.ErrEmailTaken) {
			t.Fatalf("unique email: %v", err)
		}
		var n int
		if err := app.QueryRow(ctx, `SELECT count(*) FROM auth.users WHERE email = $1`, email).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("users with email = %d", n)
		}
		assertCounter(t, ctx, app, domain.BucketRegisterIP, cmd.IP, clock.t, 2)
	})

	t.Run("login failure commits attempt and counter", func(t *testing.T) {
		email := pgEmail(t, "fail")
		if _, err := application.RegisterIn(ctx, uow, svc, pgRegister(email)); err != nil {
			t.Fatal(err)
		}
		cmd := pgLogin(email)
		cmd.Password = "wrongpassword!"
		_, err := application.LoginIn(ctx, uow, svc, cmd)
		if !errors.Is(err, domain.ErrInvalidCredentials) {
			t.Fatalf("got %v", err)
		}
		u, err := NewAuthUsers(app).GetByEmail(ctx, email)
		if err != nil || u.FailedLoginCount != 1 || u.Status != domain.StatusActive {
			t.Fatalf("user %+v %v", u, err)
		}
		var attempts int
		if err := app.QueryRow(ctx, `SELECT count(*) FROM auth.login_attempts WHERE email = $1 AND success = false`, email).Scan(&attempts); err != nil {
			t.Fatal(err)
		}
		if attempts != 1 {
			t.Fatalf("attempts %d", attempts)
		}
		assertCounter(t, ctx, app, domain.BucketLoginEmail, email, clock.t, 1)
	})

	t.Run("login refresh logout reuse", func(t *testing.T) {
		email := pgEmail(t, "walk")
		if _, err := application.RegisterIn(ctx, uow, svc, pgRegister(email)); err != nil {
			t.Fatal(err)
		}
		first, err := application.LoginIn(ctx, uow, svc, pgLogin(email))
		if err != nil {
			t.Fatal(err)
		}
		u, err := NewAuthUsers(app).Get(ctx, first.User.ID)
		if err != nil || u.FailedLoginCount != 0 {
			t.Fatalf("cleared failures %+v %v", u, err)
		}
		second, err := application.RefreshIn(ctx, uow, svc, application.RefreshCommand{RefreshToken: first.RefreshToken, IP: "203.0.113.10"})
		if err != nil {
			t.Fatal(err)
		}
		if second.SessionID != first.SessionID || second.RefreshToken == first.RefreshToken {
			t.Fatalf("rotation %+v", second)
		}
		third, err := application.RefreshIn(ctx, uow, svc, application.RefreshCommand{RefreshToken: second.RefreshToken, IP: "203.0.113.10"})
		if err != nil {
			t.Fatal(err)
		}
		if err := application.LogoutIn(ctx, uow, svc, domain.Principal{UserID: first.User.ID, SessionID: first.SessionID}); err != nil {
			t.Fatal(err)
		}
		sess, err := NewAuthSessions(app).Get(ctx, first.SessionID)
		if err != nil || sess.Live() || sess.RevokeReason != domain.RevokeLogout {
			t.Fatalf("session %+v %v", sess, err)
		}
		_, err = application.RefreshIn(ctx, uow, svc, application.RefreshCommand{RefreshToken: first.RefreshToken, IP: "203.0.113.10"})
		if !errors.Is(err, domain.ErrTokenReused) {
			t.Fatalf("old refresh: %v", err)
		}
		_, err = application.RefreshIn(ctx, uow, svc, application.RefreshCommand{RefreshToken: third.RefreshToken, IP: "203.0.113.10"})
		if !errors.Is(err, domain.ErrInvalidToken) && !errors.Is(err, domain.ErrTokenReused) {
			t.Fatalf("new refresh after logout: %v", err)
		}
	})

	t.Run("refresh reuse revokes session in one tx", func(t *testing.T) {
		email := pgEmail(t, "reuse")
		if _, err := application.RegisterIn(ctx, uow, svc, pgRegister(email)); err != nil {
			t.Fatal(err)
		}
		cred, err := application.LoginIn(ctx, uow, svc, pgLogin(email))
		if err != nil {
			t.Fatal(err)
		}
		next, err := application.RefreshIn(ctx, uow, svc, application.RefreshCommand{RefreshToken: cred.RefreshToken, IP: "203.0.113.10"})
		if err != nil {
			t.Fatal(err)
		}
		_, err = application.RefreshIn(ctx, uow, svc, application.RefreshCommand{RefreshToken: cred.RefreshToken, IP: "203.0.113.10"})
		if !errors.Is(err, domain.ErrTokenReused) {
			t.Fatalf("got %v", err)
		}
		sess, err := NewAuthSessions(app).Get(ctx, cred.SessionID)
		if err != nil || sess.Live() || sess.RevokeReason != domain.RevokeTokenReuse {
			t.Fatalf("session %+v %v", sess, err)
		}
		var live int
		if err := app.QueryRow(ctx, `
			SELECT count(*) FROM auth.refresh_tokens
			 WHERE session_id = $1 AND revoked_at IS NULL`, cred.SessionID).Scan(&live); err != nil {
			t.Fatal(err)
		}
		if live != 0 {
			t.Fatalf("live refresh rows %d (new token %s)", live, next.RefreshToken)
		}
	})

	t.Run("expired unused leaves session live", func(t *testing.T) {
		email := pgEmail(t, "exp")
		if _, err := application.RegisterIn(ctx, uow, svc, pgRegister(email)); err != nil {
			t.Fatal(err)
		}
		cred, err := application.LoginIn(ctx, uow, svc, pgLogin(email))
		if err != nil {
			t.Fatal(err)
		}
		clock.t = clock.t.Add(domain.RefreshTTL + time.Second)
		defer func() { clock.t = time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC) }()
		_, err = application.RefreshIn(ctx, uow, svc, application.RefreshCommand{RefreshToken: cred.RefreshToken, IP: "203.0.113.40"})
		if !errors.Is(err, domain.ErrTokenExpired) {
			t.Fatalf("got %v", err)
		}
		sess, err := NewAuthSessions(app).Get(ctx, cred.SessionID)
		if err != nil || !sess.Live() {
			t.Fatalf("session %+v %v", sess, err)
		}
	})

	t.Run("unknown refresh commits counter", func(t *testing.T) {
		ip := "198.51.100.9"
		_, err := application.RefreshIn(ctx, uow, svc, application.RefreshCommand{RefreshToken: "no-such-token", IP: ip})
		if !errors.Is(err, domain.ErrInvalidToken) {
			t.Fatalf("got %v", err)
		}
		assertCounter(t, ctx, app, domain.BucketRefreshIP, ip, clock.t, 1)
	})

	t.Run("rate limit sixth login email", func(t *testing.T) {
		email := pgEmail(t, "rl")
		if _, err := application.RegisterIn(ctx, uow, svc, pgRegister(email)); err != nil {
			t.Fatal(err)
		}
		var last error
		for i := 0; i < 6; i++ {
			cmd := pgLogin(email)
			cmd.Password = "wrongpassword!"
			cmd.IP = fmt.Sprintf("203.0.113.%d", 50+i)
			_, last = application.LoginIn(ctx, uow, svc, cmd)
		}
		if !errors.Is(last, domain.ErrRateLimited) {
			t.Fatalf("6th login: %v", last)
		}
		assertCounterAtLeast(t, ctx, app, domain.BucketLoginEmail, email, clock.t, 6)
	})

	t.Run("tenth failure locks", func(t *testing.T) {
		email := pgEmail(t, "lock")
		if _, err := application.RegisterIn(ctx, uow, svc, pgRegister(email)); err != nil {
			t.Fatal(err)
		}
		u, err := NewAuthUsers(app).GetByEmail(ctx, email)
		if err != nil {
			t.Fatal(err)
		}
		cred, err := application.LoginIn(ctx, uow, svc, pgLogin(email))
		if err != nil {
			t.Fatal(err)
		}
		u.FailedLoginCount = domain.LockAfter - 1
		u.UpdatedAt = clock.t
		if err := NewAuthUsers(app).Update(ctx, u); err != nil {
			t.Fatal(err)
		}
		clock.t = clock.t.Add(domain.RateWindow)
		cmd := pgLogin(email)
		cmd.Password = "wrongpassword!"
		_, err = application.LoginIn(ctx, uow, svc, cmd)
		if !errors.Is(err, domain.ErrInvalidCredentials) {
			t.Fatalf("got %v", err)
		}
		got, err := NewAuthUsers(app).Get(ctx, u.ID)
		if err != nil || got.Status != domain.StatusLocked || got.FailedLoginCount != domain.LockAfter || got.LockedAt == nil {
			t.Fatalf("locked %+v %v", got, err)
		}
		sess, err := NewAuthSessions(app).Get(ctx, cred.SessionID)
		if err != nil || sess.Live() || sess.RevokeReason != domain.RevokeUserLocked {
			t.Fatalf("session %+v %v", sess, err)
		}
	})

	t.Run("rollback discards writes", func(t *testing.T) {
		id := mustNewID(t)
		email := pgEmail(t, "rb")
		err := uow.Do(ctx, func(ctx context.Context, d application.Deps) (application.Decision, error) {
			u := domain.User{
				ID: id, Email: email, PasswordHash: testArgon2idPHC,
				Status: domain.StatusActive, CreatedAt: clock.t, UpdatedAt: clock.t,
			}
			if err := d.Users.Create(ctx, u); err != nil {
				return application.Rollback, err
			}
			if _, err := d.Limiter.Hit(ctx, domain.BucketRegisterIP, "203.0.113.70", clock.t); err != nil {
				return application.Rollback, err
			}
			return application.Rollback, errors.New("forced")
		})
		if err == nil || err.Error() != "forced" {
			t.Fatalf("got %v", err)
		}
		if _, err := NewAuthUsers(app).Get(ctx, id); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("user persisted: %v", err)
		}
	})

	t.Run("cancel after writes does not commit", func(t *testing.T) {
		id := mustNewID(t)
		email := pgEmail(t, "cancel")
		runCtx, stop := context.WithCancel(ctx)
		err := uow.Do(runCtx, func(ctx context.Context, d application.Deps) (application.Decision, error) {
			u := domain.User{
				ID: id, Email: email, PasswordHash: testArgon2idPHC,
				Status: domain.StatusActive, CreatedAt: clock.t, UpdatedAt: clock.t,
			}
			if err := d.Users.Create(ctx, u); err != nil {
				return application.Rollback, err
			}
			stop()
			return application.Commit, nil
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v", err)
		}
		if _, err := NewAuthUsers(app).Get(ctx, id); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("canceled commit persisted: %v", err)
		}
	})

	t.Run("same tx not only same pool", func(t *testing.T) {
		id := mustNewID(t)
		email := pgEmail(t, "tx")
		err := uow.Do(ctx, func(ctx context.Context, d application.Deps) (application.Decision, error) {
			u := domain.User{
				ID: id, Email: email, PasswordHash: testArgon2idPHC,
				Status: domain.StatusActive, CreatedAt: clock.t, UpdatedAt: clock.t,
			}
			if err := d.Users.Create(ctx, u); err != nil {
				return application.Rollback, err
			}
			if _, err := NewAuthUsers(other).Get(ctx, id); !errors.Is(err, domain.ErrNotFound) {
				return application.Rollback, fmt.Errorf("other conn saw uncommitted user: %v", err)
			}
			got, err := d.Users.Get(ctx, id)
			if err != nil || got.ID != id {
				return application.Rollback, fmt.Errorf("same tx get: %+v %v", got, err)
			}
			return application.Commit, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		got, err := NewAuthUsers(other).Get(ctx, id)
		if err != nil || got.ID != id {
			t.Fatalf("after commit: %+v %v", got, err)
		}
	})

	t.Run("limiter uses date_bin epoch windows", func(t *testing.T) {
		key := pgEmail(t, "win")
		at := time.Date(2026, 11, 6, 10, 7, 0, 0, time.UTC)
		err := uow.Do(ctx, func(ctx context.Context, d application.Deps) (application.Decision, error) {
			n, err := d.Limiter.Hit(ctx, domain.BucketLoginIP, key, at)
			if err != nil || n != 1 {
				return application.Rollback, fmt.Errorf("first %d %v", n, err)
			}
			n, err = d.Limiter.Hit(ctx, domain.BucketLoginIP, key, at.Add(7*time.Minute))
			if err != nil || n != 2 {
				return application.Rollback, fmt.Errorf("same window %d %v", n, err)
			}
			n, err = d.Limiter.Hit(ctx, domain.BucketLoginIP, key, at.Add(15*time.Minute))
			if err != nil || n != 1 {
				return application.Rollback, fmt.Errorf("next window %d %v", n, err)
			}
			return application.Commit, nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})

	t.Run("auth does not write ledger", func(t *testing.T) {
		email := pgEmail(t, "led")
		if _, err := application.RegisterIn(ctx, uow, svc, pgRegister(email)); err != nil {
			t.Fatal(err)
		}
		if _, err := application.LoginIn(ctx, uow, svc, pgLogin(email)); err != nil {
			t.Fatal(err)
		}
		var n int
		if err := app.QueryRow(ctx, `SELECT count(*) FROM ledger.journal_transactions`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("journal_transactions %d", n)
		}
	})

	t.Run("concurrent refresh one unused token", func(t *testing.T) {
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
		run := func(i int, w application.UnitOfWork, ip string) {
			defer wg.Done()
			_, errs[i] = application.RefreshIn(ctx, w, pgAuthService(clock), application.RefreshCommand{
				RefreshToken: cred.RefreshToken, IP: ip,
			})
		}
		wg.Add(2)
		go run(0, uow, "203.0.113.80")
		go run(1, uowOther, "203.0.113.81")
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
			 WHERE s.user_id = $1
			   AND t.used_at IS NULL
			   AND t.revoked_at IS NULL
			   AND s.revoked_at IS NULL`, cred.User.ID).Scan(&live); err != nil {
			t.Fatal(err)
		}
		if live > 1 {
			t.Fatalf("live refresh chains %d", live)
		}
	})

	t.Run("concurrent lockout does not double increment", func(t *testing.T) {
		email := pgEmail(t, "clock")
		if _, err := application.RegisterIn(ctx, uow, svc, pgRegister(email)); err != nil {
			t.Fatal(err)
		}
		u, err := NewAuthUsers(app).GetByEmail(ctx, email)
		if err != nil {
			t.Fatal(err)
		}
		u.FailedLoginCount = domain.LockAfter - 1
		u.UpdatedAt = clock.t
		if err := NewAuthUsers(app).Update(ctx, u); err != nil {
			t.Fatal(err)
		}
		clock.t = clock.t.Add(domain.RateWindow)
		var wg sync.WaitGroup
		errs := make([]error, 2)
		run := func(i int, w application.UnitOfWork, ip string) {
			defer wg.Done()
			cmd := pgLogin(email)
			cmd.Password = "wrongpassword!"
			cmd.IP = ip
			_, errs[i] = application.LoginIn(ctx, w, pgAuthService(clock), cmd)
		}
		wg.Add(2)
		go run(0, uow, "203.0.113.90")
		go run(1, uowOther, "203.0.113.91")
		wg.Wait()
		for i, err := range errs {
			if !errors.Is(err, domain.ErrInvalidCredentials) {
				t.Fatalf("login %d: %v", i, err)
			}
		}
		got, err := NewAuthUsers(app).Get(ctx, u.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != domain.StatusLocked || got.FailedLoginCount != domain.LockAfter {
			t.Fatalf("after concurrent lockout %+v", got)
		}
	})
}

type pgClock struct{ t time.Time }

func (c *pgClock) Now() time.Time { return c.t }

type pgHasher struct{}

func (pgHasher) Hash(password string) (string, error) {
	return "$argon2id$v=19$m=19456,t=2,p=1$fake$" + password, nil
}

func (h pgHasher) Verify(password, encoded string) (bool, error) {
	if encoded == h.DummyHash() {
		return false, nil
	}
	want, err := h.Hash(password)
	if err != nil {
		return false, err
	}
	return encoded == want, nil
}

func (pgHasher) DummyHash() string {
	return "$argon2id$v=19$m=19456,t=2,p=1$dummy$xxxxxxxxxxxxxxxx"
}

type pgIssuer struct {
	mu sync.Mutex
	n  int
}

func (i *pgIssuer) Issue() (string, []byte, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.n++
	plain := fmt.Sprintf("refresh-%d-%d", i.n, time.Now().UnixNano())
	sum := sha256.Sum256([]byte(plain))
	return plain, sum[:], nil
}

func (*pgIssuer) Hash(plaintext string) ([]byte, error) {
	sum := sha256.Sum256([]byte(plaintext))
	return sum[:], nil
}

type pgSigner struct{}

func (pgSigner) Sign(p domain.Principal, now, exp time.Time) (string, error) {
	return fmt.Sprintf("access:%s:%s:%d", p.UserID, p.SessionID, exp.Unix()), nil
}

func pgAuthService(clock *pgClock) application.Service {
	return application.Service{
		Hasher: pgHasher{},
		Issuer: &pgIssuer{},
		Access: pgSigner{},
		Clock:  clock,
	}
}

func pgEmail(t *testing.T, prefix string) string {
	t.Helper()
	return fmt.Sprintf("%s-%s@users.test", prefix, mustNewID(t).String())
}

func pgRegister(email string) application.RegisterCommand {
	return application.RegisterCommand{
		Email:     email,
		Password:  "twelvechars!",
		FirstName: "Ada",
		LastName:  "Lovelace",
		IP:        nextTestIP(),
	}
}

func pgLogin(email string) application.LoginCommand {
	return application.LoginCommand{
		Email:     email,
		Password:  "twelvechars!",
		IP:        nextTestIP(),
		UserAgent: "paycore-test",
	}
}

func nextTestIP() string {
	n := testIPSeq.Add(1)
	return fmt.Sprintf("198.51.%d.%d", 1+(n/250)%250, 1+n%250)
}

func assertCounter(t *testing.T, ctx context.Context, conn *pgx.Conn, bucket domain.RateBucket, key string, at time.Time, want int) {
	t.Helper()
	var n int
	err := conn.QueryRow(ctx, `
		SELECT count FROM auth.rate_limit_counters
		 WHERE bucket = $1 AND counter_key = $2
		   AND window_start = date_bin('15 minutes', $3::timestamptz, TIMESTAMPTZ '1970-01-01 00:00:00+00')`,
		string(bucket), key, at.UTC(),
	).Scan(&n)
	if err != nil || n != want {
		t.Fatalf("counter %s %s = %d %v, want %d", bucket, key, n, err, want)
	}
}

func assertCounterAtLeast(t *testing.T, ctx context.Context, conn *pgx.Conn, bucket domain.RateBucket, key string, at time.Time, want int) {
	t.Helper()
	var n int
	err := conn.QueryRow(ctx, `
		SELECT count FROM auth.rate_limit_counters
		 WHERE bucket = $1 AND counter_key = $2
		   AND window_start = date_bin('15 minutes', $3::timestamptz, TIMESTAMPTZ '1970-01-01 00:00:00+00')`,
		string(bucket), key, at.UTC(),
	).Scan(&n)
	if err != nil || n < want {
		t.Fatalf("counter %s %s = %d %v, want >= %d", bucket, key, n, err, want)
	}
}
