package application

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/romangergovskiy/paycore/internal/auth/domain"
)

func statusCmd(actor domain.Principal, userID uuid.UUID) StatusCommand {
	return StatusCommand{Actor: actor, UserID: userID, Reason: "policy"}
}

func TestLockUserLastAdmin(t *testing.T) {
	w := newWorld()
	svc := w.service()
	admin := seedUser(t, w, "a@b.c", domain.StatusActive, domain.RoleAdmin)
	dec, err := svc.LockUser(context.Background(), statusCmd(adminPrincipal(admin), admin.ID))
	if dec != Rollback || !errors.Is(err, domain.ErrLastAdmin) {
		t.Fatalf("dec=%v err=%v", dec, err)
	}
	got, err := w.users.Get(context.Background(), admin.ID)
	if err != nil || got.Status != domain.StatusActive || got.LockedAt != nil {
		t.Fatalf("status changed %+v %v", got, err)
	}
}

func TestDisableUserLastAdmin(t *testing.T) {
	w := newWorld()
	svc := w.service()
	admin := seedUser(t, w, "a@b.c", domain.StatusActive, domain.RoleAdmin)
	dec, err := svc.DisableUser(context.Background(), statusCmd(adminPrincipal(admin), admin.ID))
	if dec != Rollback || !errors.Is(err, domain.ErrLastAdmin) {
		t.Fatalf("dec=%v err=%v", dec, err)
	}
	got, _ := w.users.Get(context.Background(), admin.ID)
	if got.Status != domain.StatusActive {
		t.Fatalf("status %+v", got)
	}
}

func TestLockUserOneOfTwo(t *testing.T) {
	w := newWorld()
	svc := w.service()
	a1 := seedUser(t, w, "a1@b.c", domain.StatusActive, domain.RoleAdmin)
	a2 := seedUser(t, w, "a2@b.c", domain.StatusActive, domain.RoleAdmin)
	cred, _, err := svc.Login(context.Background(), loginCmd("a2@b.c"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.LockUser(context.Background(), statusCmd(adminPrincipal(a1), a2.ID)); err != nil {
		t.Fatal(err)
	}
	got, _ := w.users.Get(context.Background(), a2.ID)
	if got.Status != domain.StatusLocked || got.LockedAt == nil {
		t.Fatalf("locked %+v", got)
	}
	sess, _ := w.sessions.Get(context.Background(), cred.SessionID)
	if sess.Live() || sess.RevokeReason != domain.RevokeUserLocked {
		t.Fatalf("session %+v", sess)
	}
	if _, err := svc.LockUser(context.Background(), statusCmd(adminPrincipal(a1), a1.ID)); !errors.Is(err, domain.ErrLastAdmin) {
		t.Fatalf("last: %v", err)
	}
	left, _ := w.users.Get(context.Background(), a1.ID)
	if left.Status != domain.StatusActive {
		t.Fatalf("remaining %+v", left)
	}
}

func TestLockUserIdempotentAlreadyLocked(t *testing.T) {
	w := newWorld()
	svc := w.service()
	a1 := seedUser(t, w, "a1@b.c", domain.StatusActive, domain.RoleAdmin)
	locked := seedUser(t, w, "lock@b.c", domain.StatusLocked, domain.RoleCustomer)
	dec, err := svc.LockUser(context.Background(), statusCmd(adminPrincipal(a1), locked.ID))
	if dec != Commit || err != nil {
		t.Fatalf("dec=%v err=%v", dec, err)
	}
	got, _ := w.users.Get(context.Background(), locked.ID)
	if got.Status != domain.StatusLocked {
		t.Fatalf("status %+v", got)
	}
}

func TestLockUserDisabledRejected(t *testing.T) {
	w := newWorld()
	svc := w.service()
	admin := seedUser(t, w, "a@b.c", domain.StatusActive, domain.RoleAdmin)
	off := seedUser(t, w, "off@b.c", domain.StatusDisabled, domain.RoleCustomer)
	if _, err := svc.LockUser(context.Background(), statusCmd(adminPrincipal(admin), off.ID)); !errors.Is(err, domain.ErrInvalidRequest) {
		t.Fatalf("got %v", err)
	}
}

func TestUnlockUserLockedResetsFailures(t *testing.T) {
	w := newWorld()
	svc := w.service()
	admin := seedUser(t, w, "a@b.c", domain.StatusActive, domain.RoleAdmin)
	target := seedUser(t, w, "t@b.c", domain.StatusLocked, domain.RoleCustomer)
	target.FailedLoginCount = 9
	if err := w.users.Update(context.Background(), target); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.UnlockUser(context.Background(), statusCmd(adminPrincipal(admin), target.ID)); err != nil {
		t.Fatal(err)
	}
	got, _ := w.users.Get(context.Background(), target.ID)
	if got.Status != domain.StatusActive || got.LockedAt != nil || got.FailedLoginCount != 0 {
		t.Fatalf("unlocked %+v", got)
	}
}

func TestUnlockUserDoesNotSkipDisabled(t *testing.T) {
	w := newWorld()
	svc := w.service()
	admin := seedUser(t, w, "a@b.c", domain.StatusActive, domain.RoleAdmin)
	off := seedUser(t, w, "off@b.c", domain.StatusDisabled, domain.RoleCustomer)
	dec, err := svc.UnlockUser(context.Background(), statusCmd(adminPrincipal(admin), off.ID))
	if dec != Rollback || !errors.Is(err, domain.ErrInvalidRequest) {
		t.Fatalf("dec=%v err=%v", dec, err)
	}
	got, _ := w.users.Get(context.Background(), off.ID)
	if got.Status != domain.StatusDisabled {
		t.Fatalf("status %+v", got)
	}
}

func TestUnlockUserAlreadyActiveIdempotent(t *testing.T) {
	w := newWorld()
	svc := w.service()
	admin := seedUser(t, w, "a@b.c", domain.StatusActive, domain.RoleAdmin)
	cust := seedUser(t, w, "c@b.c", domain.StatusActive, domain.RoleCustomer)
	cust.FailedLoginCount = 4
	if err := w.users.Update(context.Background(), cust); err != nil {
		t.Fatal(err)
	}
	dec, err := svc.UnlockUser(context.Background(), statusCmd(adminPrincipal(admin), cust.ID))
	if dec != Commit || err != nil {
		t.Fatalf("dec=%v err=%v", dec, err)
	}
	got, _ := w.users.Get(context.Background(), cust.ID)
	if got.FailedLoginCount != 4 || got.Status != domain.StatusActive {
		t.Fatalf("noop mutated %+v", got)
	}
}

func TestDisableUserFromLocked(t *testing.T) {
	w := newWorld()
	svc := w.service()
	admin := seedUser(t, w, "a@b.c", domain.StatusActive, domain.RoleAdmin)
	target := seedUser(t, w, "t@b.c", domain.StatusLocked, domain.RoleCustomer)
	if _, err := svc.DisableUser(context.Background(), statusCmd(adminPrincipal(admin), target.ID)); err != nil {
		t.Fatal(err)
	}
	got, _ := w.users.Get(context.Background(), target.ID)
	if got.Status != domain.StatusDisabled || got.LockedAt != nil {
		t.Fatalf("disabled %+v", got)
	}
}

func TestDisableUserRevokesSessions(t *testing.T) {
	w := newWorld()
	svc := w.service()
	admin := seedUser(t, w, "a@b.c", domain.StatusActive, domain.RoleAdmin)
	if _, _, err := svc.Register(context.Background(), registerCmd("c@b.c")); err != nil {
		t.Fatal(err)
	}
	cred, _, err := svc.Login(context.Background(), loginCmd("c@b.c"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.DisableUser(context.Background(), statusCmd(adminPrincipal(admin), cred.User.ID)); err != nil {
		t.Fatal(err)
	}
	sess, _ := w.sessions.Get(context.Background(), cred.SessionID)
	if sess.Live() || sess.RevokeReason != domain.RevokeUserDisabled {
		t.Fatalf("session %+v", sess)
	}
}

func TestDisableUserIdempotent(t *testing.T) {
	w := newWorld()
	svc := w.service()
	admin := seedUser(t, w, "a@b.c", domain.StatusActive, domain.RoleAdmin)
	off := seedUser(t, w, "off@b.c", domain.StatusDisabled, domain.RoleCustomer)
	dec, err := svc.DisableUser(context.Background(), statusCmd(adminPrincipal(admin), off.ID))
	if dec != Commit || err != nil {
		t.Fatalf("dec=%v err=%v", dec, err)
	}
}

func TestStatusCommandForbiddenAndInvalid(t *testing.T) {
	w := newWorld()
	svc := w.service()
	admin := seedUser(t, w, "a@b.c", domain.StatusActive, domain.RoleAdmin)
	cust := seedUser(t, w, "c@b.c", domain.StatusActive, domain.RoleCustomer)
	customer := domain.Principal{UserID: cust.ID, Roles: []domain.Role{domain.RoleCustomer}}
	if _, err := svc.LockUser(context.Background(), statusCmd(customer, cust.ID)); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("customer: %v", err)
	}
	empty := statusCmd(adminPrincipal(admin), cust.ID)
	empty.Reason = "  "
	if _, err := svc.LockUser(context.Background(), empty); !errors.Is(err, domain.ErrInvalidRequest) {
		t.Fatalf("reason: %v", err)
	}
	if _, err := svc.LockUser(context.Background(), statusCmd(adminPrincipal(admin), uuid.Nil)); !errors.Is(err, domain.ErrInvalidRequest) {
		t.Fatalf("id: %v", err)
	}
	if _, err := svc.UnlockUser(context.Background(), statusCmd(adminPrincipal(admin), uuid.Must(uuid.NewV7()))); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
}

func TestLockUserAlwaysTakesAdminLock(t *testing.T) {
	w := newWorld()
	svc := w.service()
	admin := seedUser(t, w, "a@b.c", domain.StatusActive, domain.RoleAdmin)
	cust := seedUser(t, w, "c@b.c", domain.StatusActive, domain.RoleCustomer)
	w.roles.calls = nil
	w.calls = nil
	if _, err := svc.LockUser(context.Background(), statusCmd(adminPrincipal(admin), cust.ID)); err != nil {
		t.Fatal(err)
	}
	got, _ := w.users.Get(context.Background(), cust.ID)
	if got.Status != domain.StatusLocked {
		t.Fatalf("status %+v", got)
	}
	lockAt, rowAt := w.callIndex("lock"), w.callIndex("for-update")
	if lockAt < 0 || rowAt < 0 || rowAt <= lockAt {
		t.Fatalf("customer lock skipped advisory or reversed order %v", w.calls)
	}
}

func TestLockUserAdminTakesLockBeforeRow(t *testing.T) {
	w := newWorld()
	svc := w.service()
	a1 := seedUser(t, w, "a1@b.c", domain.StatusActive, domain.RoleAdmin)
	a2 := seedUser(t, w, "a2@b.c", domain.StatusActive, domain.RoleAdmin)
	w.calls = nil
	if _, err := svc.LockUser(context.Background(), statusCmd(adminPrincipal(a1), a2.ID)); err != nil {
		t.Fatal(err)
	}
	lockAt, rowAt := w.callIndex("lock"), w.callIndex("for-update")
	if lockAt < 0 || rowAt < 0 || rowAt <= lockAt {
		t.Fatalf("order %v", w.calls)
	}
}

func TestStatusRejectsStaleInitiator(t *testing.T) {
	w := newWorld()
	svc := w.service()
	a1 := seedUser(t, w, "a1@b.c", domain.StatusActive, domain.RoleAdmin)
	a2 := seedUser(t, w, "a2@b.c", domain.StatusActive, domain.RoleAdmin)
	if err := w.roles.Revoke(context.Background(), a1.ID, domain.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.LockUser(context.Background(), statusCmd(adminPrincipal(a1), a2.ID)); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("stale: %v", err)
	}
	got, _ := w.users.Get(context.Background(), a2.ID)
	if got.Status != domain.StatusActive {
		t.Fatalf("target %+v", got)
	}
}

func TestLockUserCustomerRejectsStaleInitiator(t *testing.T) {
	w := newWorld()
	svc := w.service()
	a1 := seedUser(t, w, "a1@b.c", domain.StatusActive, domain.RoleAdmin)
	_ = seedUser(t, w, "a2@b.c", domain.StatusActive, domain.RoleAdmin)
	cust := seedUser(t, w, "c@b.c", domain.StatusActive, domain.RoleCustomer)
	if err := w.roles.Revoke(context.Background(), a1.ID, domain.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.LockUser(context.Background(), statusCmd(adminPrincipal(a1), cust.ID)); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("stale: %v", err)
	}
	got, _ := w.users.Get(context.Background(), cust.ID)
	if got.Status != domain.StatusActive {
		t.Fatalf("target %+v", got)
	}
}

func TestLockUserCanceled(t *testing.T) {
	w := newWorld()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := w.service().LockUser(ctx, statusCmd(domain.Principal{Roles: []domain.Role{domain.RoleAdmin}}, uuid.Must(uuid.NewV7())))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

func TestLockUserAdminLockError(t *testing.T) {
	w := newWorld()
	svc := w.service()
	a1 := seedUser(t, w, "a1@b.c", domain.StatusActive, domain.RoleAdmin)
	a2 := seedUser(t, w, "a2@b.c", domain.StatusActive, domain.RoleAdmin)
	w.roles.lockErr = errors.New("lock failed")
	dec, err := svc.LockUser(context.Background(), statusCmd(adminPrincipal(a1), a2.ID))
	if dec != Rollback || !errors.Is(err, w.roles.lockErr) {
		t.Fatalf("dec=%v err=%v", dec, err)
	}
	got, _ := w.users.Get(context.Background(), a2.ID)
	if got.Status != domain.StatusActive {
		t.Fatalf("status %+v", got)
	}
}

func TestLockUserUpdateErrorRollsBack(t *testing.T) {
	w := newWorld()
	uow := &memUoW{w: w}
	admin := seedUser(t, w, "a@b.c", domain.StatusActive, domain.RoleAdmin)
	cust := seedUser(t, w, "c@b.c", domain.StatusActive, domain.RoleCustomer)
	w.users.update = errors.New("users update failed")
	err := LockUserIn(context.Background(), uow, w.service(), statusCmd(adminPrincipal(admin), cust.ID))
	if !errors.Is(err, w.users.update) {
		t.Fatalf("got %v", err)
	}
	if uow.committed || !uow.rolled {
		t.Fatalf("commit=%v rollback=%v", uow.committed, uow.rolled)
	}
	got, _ := w.users.Get(context.Background(), cust.ID)
	if got.Status != domain.StatusActive {
		t.Fatalf("partial lock %+v", got)
	}
}

func TestLockUserWritesHistory(t *testing.T) {
	w := newWorld()
	svc := w.service()
	admin := seedUser(t, w, "a@b.c", domain.StatusActive, domain.RoleAdmin)
	cust := seedUser(t, w, "c@b.c", domain.StatusActive, domain.RoleCustomer)
	if _, err := svc.LockUser(context.Background(), statusCmd(adminPrincipal(admin), cust.ID)); err != nil {
		t.Fatal(err)
	}
	if len(w.history.all) != 1 {
		t.Fatalf("history rows = %d", len(w.history.all))
	}
	h := w.history.all[0]
	if h.UserID != cust.ID || h.ChangedBy != admin.ID || h.PreviousStatus != domain.StatusActive ||
		h.NewStatus != domain.StatusLocked || h.Reason != "policy" {
		t.Fatalf("history %+v", h)
	}
}

func TestLockUserIdempotentSkipsHistory(t *testing.T) {
	w := newWorld()
	svc := w.service()
	admin := seedUser(t, w, "a@b.c", domain.StatusActive, domain.RoleAdmin)
	locked := seedUser(t, w, "lock@b.c", domain.StatusLocked, domain.RoleCustomer)
	if _, err := svc.LockUser(context.Background(), statusCmd(adminPrincipal(admin), locked.ID)); err != nil {
		t.Fatal(err)
	}
	if len(w.history.all) != 0 {
		t.Fatalf("noop wrote history %+v", w.history.all)
	}
}

func TestLockUserLastAdminSkipsHistory(t *testing.T) {
	w := newWorld()
	svc := w.service()
	admin := seedUser(t, w, "a@b.c", domain.StatusActive, domain.RoleAdmin)
	if _, err := svc.LockUser(context.Background(), statusCmd(adminPrincipal(admin), admin.ID)); !errors.Is(err, domain.ErrLastAdmin) {
		t.Fatalf("got %v", err)
	}
	if len(w.history.all) != 0 {
		t.Fatalf("last admin wrote history %+v", w.history.all)
	}
}

func TestLockUserHistoryErrorRollsBack(t *testing.T) {
	w := newWorld()
	uow := &memUoW{w: w}
	admin := seedUser(t, w, "a@b.c", domain.StatusActive, domain.RoleAdmin)
	cust := seedUser(t, w, "c@b.c", domain.StatusActive, domain.RoleCustomer)
	w.history.fail = errors.New("history failed")
	err := LockUserIn(context.Background(), uow, w.service(), statusCmd(adminPrincipal(admin), cust.ID))
	if !errors.Is(err, w.history.fail) {
		t.Fatalf("got %v", err)
	}
	if uow.committed || !uow.rolled {
		t.Fatalf("commit=%v rollback=%v", uow.committed, uow.rolled)
	}
	got, _ := w.users.Get(context.Background(), cust.ID)
	if got.Status != domain.StatusActive {
		t.Fatalf("partial lock %+v", got)
	}
	if len(w.history.all) != 0 {
		t.Fatalf("history after rollback %+v", w.history.all)
	}
}

func TestUnlockAndDisableWriteHistory(t *testing.T) {
	w := newWorld()
	svc := w.service()
	admin := seedUser(t, w, "a@b.c", domain.StatusActive, domain.RoleAdmin)
	locked := seedUser(t, w, "lock@b.c", domain.StatusLocked, domain.RoleCustomer)
	if _, err := svc.UnlockUser(context.Background(), statusCmd(adminPrincipal(admin), locked.ID)); err != nil {
		t.Fatal(err)
	}
	if len(w.history.all) != 1 || w.history.all[0].PreviousStatus != domain.StatusLocked ||
		w.history.all[0].NewStatus != domain.StatusActive {
		t.Fatalf("unlock history %+v", w.history.all)
	}
	cust := seedUser(t, w, "c@b.c", domain.StatusActive, domain.RoleCustomer)
	if _, err := svc.DisableUser(context.Background(), statusCmd(adminPrincipal(admin), cust.ID)); err != nil {
		t.Fatal(err)
	}
	if len(w.history.all) != 2 || w.history.all[1].NewStatus != domain.StatusDisabled {
		t.Fatalf("disable history %+v", w.history.all)
	}
}
