package application

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/romangergovskiy/paycore/internal/auth/domain"
)

func TestLoginSuccess(t *testing.T) {
	w := newWorld()
	svc := w.service()
	reg, _, err := svc.Register(context.Background(), registerCmd("a@b.c"))
	if err != nil {
		t.Fatal(err)
	}
	got, _, err := svc.Login(context.Background(), loginCmd("A@B.C"))
	if err != nil {
		t.Fatal(err)
	}
	if got.SessionID == uuid.Nil || got.AccessToken == "" {
		t.Fatalf("%+v", got)
	}
	if got.TokenType != tokenTypeBearer || got.ExpiresIn != domain.AccessExpiresIn {
		t.Fatalf("token %s expires %d", got.TokenType, got.ExpiresIn)
	}
	if got.RefreshToken == "" || got.User.ID != reg.ID {
		t.Fatalf("%+v", got)
	}
	if got.User.Roles[0] != domain.RoleCustomer {
		t.Fatalf("roles %v", got.User.Roles)
	}
	stored, _ := w.users.Get(context.Background(), reg.ID)
	if stored.FailedLoginCount != 0 {
		t.Fatalf("failed count %d", stored.FailedLoginCount)
	}
	if w.signer.last.SessionID != got.SessionID {
		t.Fatal("signer principal")
	}
}

func TestLoginUnknownEmailSameCodeAsBadPassword(t *testing.T) {
	w := newWorld()
	svc := w.service()
	if _, _, err := svc.Register(context.Background(), registerCmd("a@b.c")); err != nil {
		t.Fatal(err)
	}
	_, _, unknown := svc.Login(context.Background(), loginCmd("nope@b.c"))
	bad := loginCmd("a@b.c")
	bad.Password = "wrongpassword"
	_, _, wrong := svc.Login(context.Background(), bad)
	if !errors.Is(unknown, domain.ErrInvalidCredentials) || !errors.Is(wrong, domain.ErrInvalidCredentials) {
		t.Fatalf("unknown=%v wrong=%v", unknown, wrong)
	}
	if w.hasher.verifyCalls < 2 || w.hasher.lastVerify == "" {
		t.Fatal("dummy/real verify not called")
	}
}

func TestLoginDummyVerifyOnUnknownEmail(t *testing.T) {
	w := newWorld()
	_, _, err := w.service().Login(context.Background(), loginCmd("ghost@b.c"))
	if !errors.Is(err, domain.ErrInvalidCredentials) {
		t.Fatalf("got %v", err)
	}
	if w.hasher.lastVerify != w.hasher.DummyHash() {
		t.Fatalf("verify hash %q", w.hasher.lastVerify)
	}
}

func TestLoginLockedAndDisabled(t *testing.T) {
	w := newWorld()
	seedUser(t, w, "lock@b.c", domain.StatusLocked, domain.RoleCustomer)
	seedUser(t, w, "off@b.c", domain.StatusDisabled, domain.RoleCustomer)
	svc := w.service()
	_, _, locked := svc.Login(context.Background(), loginCmd("lock@b.c"))
	_, _, disabled := svc.Login(context.Background(), loginCmd("off@b.c"))
	if !errors.Is(locked, domain.ErrInvalidCredentials) || !errors.Is(disabled, domain.ErrInvalidCredentials) {
		t.Fatalf("locked=%v disabled=%v", locked, disabled)
	}
}

func TestLoginEmailRateLimit(t *testing.T) {
	w := newWorld()
	svc := w.service()
	if _, _, err := svc.Register(context.Background(), registerCmd("a@b.c")); err != nil {
		t.Fatal(err)
	}
	bad := loginCmd("a@b.c")
	bad.Password = "wrongpassword"
	for i := 0; i < domain.LimitLoginEmail; i++ {
		_, _, err := svc.Login(context.Background(), bad)
		if !errors.Is(err, domain.ErrInvalidCredentials) {
			t.Fatalf("i=%d %v", i, err)
		}
	}
	_, _, err := svc.Login(context.Background(), bad)
	if !errors.Is(err, domain.ErrRateLimited) {
		t.Fatalf("6th: %v", err)
	}
}

func TestLoginLockAfterConsecutiveFailures(t *testing.T) {
	w := newWorld()
	svc := w.service()
	reg, _, err := svc.Register(context.Background(), registerCmd("a@b.c"))
	if err != nil {
		t.Fatal(err)
	}
	first, _, err := svc.Login(context.Background(), loginCmd("a@b.c"))
	if err != nil {
		t.Fatal(err)
	}
	bad := loginCmd("a@b.c")
	bad.IP = "198.51.100.9"
	bad.Password = "wrongpassword"
	for i := 0; i < domain.LockAfter; i++ {
		if i > 0 && i%domain.LimitLoginEmail == 0 {
			w.clock.t = w.clock.t.Add(domain.RateWindow)
		}
		_, _, err := svc.Login(context.Background(), bad)
		if !errors.Is(err, domain.ErrInvalidCredentials) {
			t.Fatalf("i=%d %v", i, err)
		}
	}
	u, _ := w.users.Get(context.Background(), reg.ID)
	if u.Status != domain.StatusLocked || u.LockedAt == nil {
		t.Fatalf("status %+v", u)
	}
	sess, _ := w.sessions.Get(context.Background(), first.SessionID)
	if sess.Live() {
		t.Fatal("session should be revoked on lock")
	}
	w.clock.t = w.clock.t.Add(domain.RateWindow)
	_, _, again := svc.Login(context.Background(), loginCmd("a@b.c"))
	if !errors.Is(again, domain.ErrInvalidCredentials) {
		t.Fatalf("locked login: %v", again)
	}
}

func TestLoginInvalidInput(t *testing.T) {
	w := newWorld()
	svc := w.service()
	_, _, err := svc.Login(context.Background(), LoginCommand{Email: "x", Password: "twelvechars!", IP: "1.1.1.1"})
	if !errors.Is(err, domain.ErrInvalidRequest) {
		t.Fatalf("got %v", err)
	}
	_, _, err = svc.Login(context.Background(), LoginCommand{Email: "a@b.c", Password: "twelvechars!", IP: ""})
	if !errors.Is(err, domain.ErrInvalidRequest) {
		t.Fatalf("got %v", err)
	}
}

func TestLoginHasherError(t *testing.T) {
	w := newWorld()
	if _, _, err := w.service().Register(context.Background(), registerCmd("a@b.c")); err != nil {
		t.Fatal(err)
	}
	w.hasher.failVerify = true
	_, _, err := w.service().Login(context.Background(), loginCmd("a@b.c"))
	if err == nil {
		t.Fatal("expected verify error")
	}
}

func TestLoginSignerPartial(t *testing.T) {
	w := newWorld()
	svc := w.service()
	if _, _, err := svc.Register(context.Background(), registerCmd("a@b.c")); err != nil {
		t.Fatal(err)
	}
	w.signer.fail = true
	_, _, err := svc.Login(context.Background(), loginCmd("a@b.c"))
	if err == nil {
		t.Fatal("expected sign error")
	}
	if len(w.sessions.byID) != 1 {
		t.Fatalf("partial session without UoW: %d", len(w.sessions.byID))
	}
}

func TestLoginLimiterError(t *testing.T) {
	w := newWorld()
	w.limiter.err = errors.New("limiter down")
	_, _, err := w.service().Login(context.Background(), loginCmd("a@b.c"))
	if err == nil {
		t.Fatal("expected limiter error")
	}
}

func TestLoginLastAdminNotLockedAfterFailures(t *testing.T) {
	w := newWorld()
	svc := w.service()
	admin := seedUser(t, w, "admin@b.c", domain.StatusActive, domain.RoleAdmin)
	bad := loginCmd("admin@b.c")
	bad.Password = "wrongpassword"
	for i := 0; i < domain.LockAfter; i++ {
		if i > 0 && i%domain.LimitLoginEmail == 0 {
			w.clock.t = w.clock.t.Add(domain.RateWindow)
		}
		_, _, err := svc.Login(context.Background(), bad)
		if !errors.Is(err, domain.ErrInvalidCredentials) {
			t.Fatalf("i=%d %v", i, err)
		}
	}
	got, _ := w.users.Get(context.Background(), admin.ID)
	if got.Status != domain.StatusActive || got.LockedAt != nil {
		t.Fatalf("last ADMIN locked %+v", got)
	}
	if got.FailedLoginCount != domain.LockAfter {
		t.Fatalf("fail count %d", got.FailedLoginCount)
	}
	if len(w.attempts.all) < domain.LockAfter {
		t.Fatalf("attempts %d", len(w.attempts.all))
	}
}

func TestLoginLastAdminLockoutCommitsAttempt(t *testing.T) {
	w := newWorld()
	uow := &memUoW{w: w}
	admin := seedUser(t, w, "admin@b.c", domain.StatusActive, domain.RoleAdmin)
	admin.FailedLoginCount = domain.LockAfter - 1
	if err := w.users.Update(context.Background(), admin); err != nil {
		t.Fatal(err)
	}
	bad := loginCmd("admin@b.c")
	bad.Password = "wrongpassword"
	_, err := LoginIn(context.Background(), uow, w.service(), bad)
	if !errors.Is(err, domain.ErrInvalidCredentials) {
		t.Fatalf("got %v", err)
	}
	if !uow.committed || uow.rolled {
		t.Fatalf("commit=%v rollback=%v", uow.committed, uow.rolled)
	}
	got, _ := w.users.Get(context.Background(), admin.ID)
	if got.Status != domain.StatusActive || got.FailedLoginCount != domain.LockAfter {
		t.Fatalf("user %+v", got)
	}
}

func TestLoginNonAdminDoesNotTakeAdminLock(t *testing.T) {
	w := newWorld()
	svc := w.service()
	if _, _, err := svc.Register(context.Background(), registerCmd("c@b.c")); err != nil {
		t.Fatal(err)
	}
	w.roles.calls = nil
	bad := loginCmd("c@b.c")
	bad.Password = "wrongpassword"
	_, _, err := svc.Login(context.Background(), bad)
	if !errors.Is(err, domain.ErrInvalidCredentials) {
		t.Fatalf("got %v", err)
	}
	if callIndex(w.roles.calls, "lock") != -1 {
		t.Fatalf("customer lockout took advisory: %v", w.roles.calls)
	}
}

func TestLoginAdminTakesLockBeforeRow(t *testing.T) {
	w := newWorld()
	svc := w.service()
	seedUser(t, w, "admin@b.c", domain.StatusActive, domain.RoleAdmin)
	w.calls = nil
	bad := loginCmd("admin@b.c")
	bad.Password = "wrongpassword"
	_, _, err := svc.Login(context.Background(), bad)
	if !errors.Is(err, domain.ErrInvalidCredentials) {
		t.Fatalf("got %v", err)
	}
	lockAt, rowAt := w.callIndex("lock"), w.callIndex("for-update")
	if lockAt < 0 || rowAt < 0 || rowAt <= lockAt {
		t.Fatalf("order %v", w.calls)
	}
}

func TestLoginAlreadyLockedAdminDoesNotUpdate(t *testing.T) {
	w := newWorld()
	svc := w.service()
	seedUser(t, w, "a1@b.c", domain.StatusActive, domain.RoleAdmin)
	locked := seedUser(t, w, "lock@b.c", domain.StatusLocked, domain.RoleAdmin)
	locked.FailedLoginCount = 3
	if err := w.users.Update(context.Background(), locked); err != nil {
		t.Fatal(err)
	}
	bad := loginCmd("lock@b.c")
	bad.Password = "wrongpassword"
	_, _, err := svc.Login(context.Background(), bad)
	if !errors.Is(err, domain.ErrInvalidCredentials) {
		t.Fatalf("got %v", err)
	}
	got, _ := w.users.Get(context.Background(), locked.ID)
	if got.Status != domain.StatusLocked || got.FailedLoginCount != 3 {
		t.Fatalf("updated inactive %+v", got)
	}
}

func TestLoginOneOfTwoAdminsCanLockOut(t *testing.T) {
	w := newWorld()
	svc := w.service()
	a1 := seedUser(t, w, "a1@b.c", domain.StatusActive, domain.RoleAdmin)
	a2 := seedUser(t, w, "a2@b.c", domain.StatusActive, domain.RoleAdmin)
	a1.FailedLoginCount = domain.LockAfter - 1
	a2.FailedLoginCount = domain.LockAfter - 1
	if err := w.users.Update(context.Background(), a1); err != nil {
		t.Fatal(err)
	}
	if err := w.users.Update(context.Background(), a2); err != nil {
		t.Fatal(err)
	}
	bad1 := loginCmd("a1@b.c")
	bad1.Password = "wrongpassword"
	bad2 := loginCmd("a2@b.c")
	bad2.Password = "wrongpassword"
	if _, _, err := svc.Login(context.Background(), bad1); !errors.Is(err, domain.ErrInvalidCredentials) {
		t.Fatalf("a1: %v", err)
	}
	if _, _, err := svc.Login(context.Background(), bad2); !errors.Is(err, domain.ErrInvalidCredentials) {
		t.Fatalf("a2: %v", err)
	}
	u1, _ := w.users.Get(context.Background(), a1.ID)
	u2, _ := w.users.Get(context.Background(), a2.ID)
	locked := 0
	if u1.Status == domain.StatusLocked {
		locked++
	}
	if u2.Status == domain.StatusLocked {
		locked++
	}
	if locked != 1 {
		t.Fatalf("locked=%d u1=%s u2=%s", locked, u1.Status, u2.Status)
	}
	if u1.Status != domain.StatusActive && u2.Status != domain.StatusActive {
		t.Fatal("no ACTIVE ADMIN left")
	}
}

func TestLoginCanceled(t *testing.T) {
	w := newWorld()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := w.service().Login(ctx, loginCmd("a@b.c"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}
