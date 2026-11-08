package application

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/romangergovskiy/paycore/internal/auth/domain"
)

// hookUsers runs afterGetByEmail after a successful lookup and before
// GetForUpdate, without sleeping and without memUoW serializing two TXs.
type hookUsers struct {
	UserStore
	afterGetByEmail    func()
	beforeGetForUpdate func()
	getForUpdateN      int
}

func (h *hookUsers) GetByEmail(ctx context.Context, email string) (domain.User, error) {
	u, err := h.UserStore.GetByEmail(ctx, email)
	if err == nil && h.afterGetByEmail != nil {
		h.afterGetByEmail()
	}
	return u, err
}

func (h *hookUsers) GetForUpdate(ctx context.Context, id uuid.UUID) (domain.User, error) {
	h.getForUpdateN++
	if h.beforeGetForUpdate != nil {
		h.beforeGetForUpdate()
	}
	return h.UserStore.GetForUpdate(ctx, id)
}

func serviceWithUserHook(w *world, after func()) (Service, *hookUsers) {
	h := &hookUsers{UserStore: w.users, afterGetByEmail: after}
	svc := w.service()
	svc.Users = h
	return svc, h
}

func lastAttempt(t *testing.T, w *world) domain.LoginAttempt {
	t.Helper()
	if len(w.attempts.all) == 0 {
		t.Fatal("no login attempts")
	}
	return w.attempts.all[len(w.attempts.all)-1]
}

func wrongPassword(email string) LoginCommand {
	cmd := loginCmd(email)
	cmd.Password = "wrongpassword"
	return cmd
}

func lockStoredUser(t *testing.T, w *world, email string, status domain.Status, failures int) {
	t.Helper()
	u, err := w.users.GetByEmail(context.Background(), email)
	if err != nil {
		t.Fatal(err)
	}
	now := w.clock.t
	u.Status = status
	u.FailedLoginCount = failures
	if status == domain.StatusLocked {
		t := now
		u.LockedAt = &t
	} else {
		u.LockedAt = nil
	}
	if err := w.users.Update(context.Background(), u); err != nil {
		t.Fatal(err)
	}
}

func TestLogin1PasswordOkThenLockedNoSession(t *testing.T) {
	w := newWorld()
	if _, _, err := w.service().Register(context.Background(), registerCmd("a@b.c")); err != nil {
		t.Fatal(err)
	}
	svc, _ := serviceWithUserHook(w, func() {
		lockStoredUser(t, w, "a@b.c", domain.StatusLocked, 10)
	})
	cred, dec, err := svc.Login(context.Background(), loginCmd("a@b.c"))
	if !errors.Is(err, domain.ErrInvalidCredentials) {
		t.Fatalf("got %v", err)
	}
	if dec != Commit {
		t.Fatalf("decision %v", dec)
	}
	if cred.AccessToken != "" || cred.RefreshToken != "" || cred.SessionID != uuid.Nil {
		t.Fatalf("session issued: %+v", cred)
	}
	u, _ := w.users.GetByEmail(context.Background(), "a@b.c")
	if u.Status != domain.StatusLocked || u.FailedLoginCount != 10 || u.LockedAt == nil {
		t.Fatalf("lockout overwritten: %+v", u)
	}
	if len(w.sessions.byID) != 0 {
		t.Fatal("session created")
	}
}

func TestLogin2PasswordOkThenDisabledNoSession(t *testing.T) {
	w := newWorld()
	if _, _, err := w.service().Register(context.Background(), registerCmd("a@b.c")); err != nil {
		t.Fatal(err)
	}
	svc, _ := serviceWithUserHook(w, func() {
		lockStoredUser(t, w, "a@b.c", domain.StatusDisabled, 3)
	})
	_, dec, err := svc.Login(context.Background(), loginCmd("a@b.c"))
	if !errors.Is(err, domain.ErrInvalidCredentials) {
		t.Fatalf("got %v", err)
	}
	if dec != Commit {
		t.Fatalf("decision %v", dec)
	}
	u, _ := w.users.GetByEmail(context.Background(), "a@b.c")
	if u.Status != domain.StatusDisabled || u.FailedLoginCount != 3 {
		t.Fatalf("disabled overwritten: %+v", u)
	}
	if len(w.sessions.byID) != 0 {
		t.Fatal("session created")
	}
}

func TestLogin3PasswordOkStillActiveResetsFromLockedRow(t *testing.T) {
	w := newWorld()
	reg, _, err := w.service().Register(context.Background(), registerCmd("a@b.c"))
	if err != nil {
		t.Fatal(err)
	}
	u, _ := w.users.Get(context.Background(), reg.ID)
	u.FailedLoginCount = 4
	_ = w.users.Update(context.Background(), u)
	svc, _ := serviceWithUserHook(w, func() {
		cur, _ := w.users.Get(context.Background(), reg.ID)
		if cur.FailedLoginCount != 4 {
			t.Fatalf("hook saw count %d", cur.FailedLoginCount)
		}
	})
	got, dec, err := svc.Login(context.Background(), loginCmd("a@b.c"))
	if err != nil || dec != Commit {
		t.Fatalf("err=%v dec=%v", err, dec)
	}
	if got.SessionID.String() == "" || got.AccessToken == "" {
		t.Fatalf("%+v", got)
	}
	stored, _ := w.users.Get(context.Background(), reg.ID)
	if stored.FailedLoginCount != 0 || stored.LockedAt != nil || stored.Status != domain.StatusActive {
		t.Fatalf("%+v", stored)
	}
}

func TestLogin4WrongPasswordThenLockedDoesNotIncrement(t *testing.T) {
	w := newWorld()
	if _, _, err := w.service().Register(context.Background(), registerCmd("a@b.c")); err != nil {
		t.Fatal(err)
	}
	svc, hooks := serviceWithUserHook(w, func() {
		lockStoredUser(t, w, "a@b.c", domain.StatusLocked, 10)
	})
	_, dec, err := svc.Login(context.Background(), wrongPassword("a@b.c"))
	if !errors.Is(err, domain.ErrInvalidCredentials) {
		t.Fatalf("got %v", err)
	}
	if dec != Commit {
		t.Fatalf("decision %v", dec)
	}
	u, _ := w.users.GetByEmail(context.Background(), "a@b.c")
	if u.Status != domain.StatusLocked || u.FailedLoginCount != 10 {
		t.Fatalf("LOCKED count bumped: %+v", u)
	}
	if lastAttempt(t, w).FailureReason != domain.FailureLocked {
		t.Fatalf("reason %s", lastAttempt(t, w).FailureReason)
	}
	if hooks.getForUpdateN != 1 {
		t.Fatalf("GetForUpdate %d want 1", hooks.getForUpdateN)
	}
}

func TestLogin5WrongPasswordThenDisabledDoesNotIncrement(t *testing.T) {
	w := newWorld()
	if _, _, err := w.service().Register(context.Background(), registerCmd("a@b.c")); err != nil {
		t.Fatal(err)
	}
	svc, hooks := serviceWithUserHook(w, func() {
		lockStoredUser(t, w, "a@b.c", domain.StatusDisabled, 2)
	})
	_, dec, err := svc.Login(context.Background(), wrongPassword("a@b.c"))
	if !errors.Is(err, domain.ErrInvalidCredentials) {
		t.Fatalf("got %v", err)
	}
	if dec != Commit {
		t.Fatalf("decision %v", dec)
	}
	u, _ := w.users.GetByEmail(context.Background(), "a@b.c")
	if u.Status != domain.StatusDisabled || u.FailedLoginCount != 2 {
		t.Fatalf("DISABLED mutated: %+v", u)
	}
	if lastAttempt(t, w).FailureReason != domain.FailureDisabled {
		t.Fatalf("reason %s", lastAttempt(t, w).FailureReason)
	}
	if hooks.getForUpdateN != 1 {
		t.Fatalf("GetForUpdate %d want 1", hooks.getForUpdateN)
	}
}

func TestLoginLockedThenActiveIncrements(t *testing.T) {
	w := newWorld()
	if _, _, err := w.service().Register(context.Background(), registerCmd("a@b.c")); err != nil {
		t.Fatal(err)
	}
	lockStoredUser(t, w, "a@b.c", domain.StatusLocked, 2)
	svc, hooks := serviceWithUserHook(w, func() {
		lockStoredUser(t, w, "a@b.c", domain.StatusActive, 2)
	})
	_, dec, err := svc.Login(context.Background(), wrongPassword("a@b.c"))
	if !errors.Is(err, domain.ErrInvalidCredentials) || dec != Commit {
		t.Fatalf("err=%v dec=%v", err, dec)
	}
	u, _ := w.users.GetByEmail(context.Background(), "a@b.c")
	if u.Status != domain.StatusActive || u.FailedLoginCount != 3 {
		t.Fatalf("want ACTIVE count 3: %+v", u)
	}
	if lastAttempt(t, w).FailureReason != domain.FailureInvalidCredentials {
		t.Fatalf("reason %s", lastAttempt(t, w).FailureReason)
	}
	if hooks.getForUpdateN != 1 {
		t.Fatalf("GetForUpdate %d want 1", hooks.getForUpdateN)
	}
}

func TestLoginOrdinaryActiveFail(t *testing.T) {
	w := newWorld()
	if _, _, err := w.service().Register(context.Background(), registerCmd("a@b.c")); err != nil {
		t.Fatal(err)
	}
	svc, hooks := serviceWithUserHook(w, nil)
	_, dec, err := svc.Login(context.Background(), wrongPassword("a@b.c"))
	if !errors.Is(err, domain.ErrInvalidCredentials) || dec != Commit {
		t.Fatalf("err=%v dec=%v", err, dec)
	}
	u, _ := w.users.GetByEmail(context.Background(), "a@b.c")
	if u.Status != domain.StatusActive || u.FailedLoginCount != 1 {
		t.Fatalf("%+v", u)
	}
	if lastAttempt(t, w).FailureReason != domain.FailureInvalidCredentials {
		t.Fatalf("reason %s", lastAttempt(t, w).FailureReason)
	}
	if hooks.getForUpdateN != 1 {
		t.Fatalf("GetForUpdate %d want 1", hooks.getForUpdateN)
	}
}

func TestLogin6TenthFailureLocksFromRow(t *testing.T) {
	w := newWorld()
	reg, _, err := w.service().Register(context.Background(), registerCmd("a@b.c"))
	if err != nil {
		t.Fatal(err)
	}
	first, _, err := w.service().Login(context.Background(), loginCmd("a@b.c"))
	if err != nil {
		t.Fatal(err)
	}
	u, _ := w.users.Get(context.Background(), reg.ID)
	u.FailedLoginCount = domain.LockAfter - 1
	_ = w.users.Update(context.Background(), u)

	svc, hooks := serviceWithUserHook(w, nil)
	bad := wrongPassword("a@b.c")
	bad.IP = "198.51.100.9"
	_, dec, err := svc.Login(context.Background(), bad)
	if !errors.Is(err, domain.ErrInvalidCredentials) || dec != Commit {
		t.Fatalf("err=%v dec=%v", err, dec)
	}
	stored, _ := w.users.Get(context.Background(), reg.ID)
	if stored.Status != domain.StatusLocked || stored.LockedAt == nil || stored.FailedLoginCount != domain.LockAfter {
		t.Fatalf("%+v", stored)
	}
	sess, _ := w.sessions.Get(context.Background(), first.SessionID)
	if sess.Live() || sess.RevokeReason != domain.RevokeUserLocked {
		t.Fatalf("session %+v", sess)
	}
	if lastAttempt(t, w).FailureReason != domain.FailureLocked {
		t.Fatalf("reason %s", lastAttempt(t, w).FailureReason)
	}
	if hooks.getForUpdateN != 1 {
		t.Fatalf("GetForUpdate %d want 1", hooks.getForUpdateN)
	}
}

func TestLoginGrantAdminDuringRowLockRestarts(t *testing.T) {
	w := newWorld()
	admin := seedUser(t, w, "admin@b.c", domain.StatusActive, domain.RoleAdmin)
	cust := seedUser(t, w, "c@b.c", domain.StatusActive, domain.RoleCustomer)
	cust.FailedLoginCount = domain.LockAfter - 1
	if err := w.users.Update(context.Background(), cust); err != nil {
		t.Fatal(err)
	}

	svc, hooks := serviceWithUserHook(w, nil)
	hooks.beforeGetForUpdate = func() {
		hooks.beforeGetForUpdate = nil
		if _, err := w.service().GrantRole(context.Background(), adminPrincipal(admin), cust.ID, domain.RoleAdmin); err != nil {
			t.Fatal(err)
		}
	}
	_, dec, err := svc.Login(context.Background(), wrongPassword("c@b.c"))
	if dec != Rollback || !errors.Is(err, errAdminSerialRestart) {
		t.Fatalf("dec=%v err=%v", dec, err)
	}
	got, _ := w.users.Get(context.Background(), cust.ID)
	if got.Status != domain.StatusActive || got.FailedLoginCount != domain.LockAfter-1 {
		t.Fatalf("mutated before restart %+v", got)
	}

	w.calls = nil
	_, err = LoginIn(context.Background(), &memUoW{w: w}, w.service(), wrongPassword("c@b.c"))
	if !errors.Is(err, domain.ErrInvalidCredentials) {
		t.Fatalf("retry: %v", err)
	}
	got, _ = w.users.Get(context.Background(), cust.ID)
	if got.Status != domain.StatusLocked {
		t.Fatalf("retry lockout %+v", got)
	}
	lockAt, rowAt := w.callIndex("lock"), w.callIndex("for-update")
	if lockAt < 0 || rowAt <= lockAt {
		t.Fatalf("retry order %v", w.calls)
	}
}
