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
	afterGetByEmail func()
}

func (h hookUsers) GetByEmail(ctx context.Context, email string) (domain.User, error) {
	u, err := h.UserStore.GetByEmail(ctx, email)
	if err == nil && h.afterGetByEmail != nil {
		h.afterGetByEmail()
	}
	return u, err
}

func serviceWithUserHook(w *world, after func()) Service {
	svc := w.service()
	svc.Users = hookUsers{UserStore: w.users, afterGetByEmail: after}
	return svc
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
	svc := serviceWithUserHook(w, func() {
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
	svc := serviceWithUserHook(w, func() {
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
	svc := serviceWithUserHook(w, func() {
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
	svc := serviceWithUserHook(w, func() {
		lockStoredUser(t, w, "a@b.c", domain.StatusLocked, 10)
	})
	bad := loginCmd("a@b.c")
	bad.Password = "wrongpassword"
	_, dec, err := svc.Login(context.Background(), bad)
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
}

func TestLogin5WrongPasswordThenDisabledDoesNotIncrement(t *testing.T) {
	w := newWorld()
	if _, _, err := w.service().Register(context.Background(), registerCmd("a@b.c")); err != nil {
		t.Fatal(err)
	}
	svc := serviceWithUserHook(w, func() {
		lockStoredUser(t, w, "a@b.c", domain.StatusDisabled, 2)
	})
	bad := loginCmd("a@b.c")
	bad.Password = "wrongpassword"
	_, dec, err := svc.Login(context.Background(), bad)
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
}

func TestLogin6TenthFailureLocksFromRow(t *testing.T) {
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
	u, _ := w.users.Get(context.Background(), reg.ID)
	u.FailedLoginCount = domain.LockAfter - 1
	_ = w.users.Update(context.Background(), u)

	bad := loginCmd("a@b.c")
	bad.IP = "198.51.100.9"
	bad.Password = "wrongpassword"
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
}
