package application

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/romangergovskiy/paycore/internal/auth/domain"
)

func loginEmail(t *testing.T, w *world, email string) Credentials {
	t.Helper()
	svc := w.service()
	if _, _, err := svc.Register(context.Background(), registerCmd(email)); err != nil {
		t.Fatal(err)
	}
	got, _, err := svc.Login(context.Background(), loginCmd(email))
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func actorOf(c Credentials) domain.Principal {
	return domain.Principal{UserID: c.User.ID, SessionID: c.SessionID, Roles: c.User.Roles}
}

func liveTokenCount(w *world, userID uuid.UUID) int {
	owned := map[uuid.UUID]struct{}{}
	w.sessions.mu.Lock()
	for id, sess := range w.sessions.byID {
		if sess.UserID == userID {
			owned[id] = struct{}{}
		}
	}
	w.sessions.mu.Unlock()
	n := 0
	w.tokens.mu.Lock()
	for _, tok := range w.tokens.byID {
		if _, ok := owned[tok.SessionID]; !ok {
			continue
		}
		if tok.RevokedAt == nil {
			n++
		}
	}
	w.tokens.mu.Unlock()
	return n
}

func TestCurrentUserRolesFromStoreNotPrincipal(t *testing.T) {
	w := newWorld()
	cred := loginEmail(t, w, "me@b.c")
	planted := domain.Principal{
		UserID:    cred.User.ID,
		SessionID: cred.SessionID,
		Roles:     []domain.Role{domain.RoleAdmin, domain.RoleSupport},
	}
	got, err := w.service().CurrentUser(context.Background(), planted)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != cred.User.ID || got.Email != "me@b.c" {
		t.Fatalf("profile %+v", got)
	}
	if len(got.Roles) != 1 || got.Roles[0] != domain.RoleCustomer {
		t.Fatalf("roles from principal leaked: %v", got.Roles)
	}
	storeRoles, err := w.service().GetRoles(context.Background(), cred.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(storeRoles) != 1 || storeRoles[0] != domain.RoleCustomer {
		t.Fatalf("GetRoles %v", storeRoles)
	}
}

func TestCurrentUserInactiveIsInvalidToken(t *testing.T) {
	w := newWorld()
	cred := loginEmail(t, w, "me@b.c")
	u, _ := w.users.Get(context.Background(), cred.User.ID)
	u.Status = domain.StatusDisabled
	if err := w.users.Update(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	_, err := w.service().CurrentUser(context.Background(), actorOf(cred))
	if !errors.Is(err, domain.ErrInvalidToken) {
		t.Fatalf("got %v", err)
	}
}

func TestLogoutAllRevokesOwnSessionsLeavesOthers(t *testing.T) {
	w := newWorld()
	svc := w.service()
	first := loginEmail(t, w, "a@b.c")
	second, _, err := svc.Login(context.Background(), loginCmd("a@b.c"))
	if err != nil {
		t.Fatal(err)
	}
	other := loginEmail(t, w, "other@b.c")

	dec, err := svc.LogoutAll(context.Background(), actorOf(first))
	if dec != Commit || err != nil {
		t.Fatalf("dec=%v err=%v", dec, err)
	}
	for _, sid := range []uuid.UUID{first.SessionID, second.SessionID} {
		sess, _ := w.sessions.Get(context.Background(), sid)
		if sess.Live() || sess.RevokeReason != domain.RevokeLogoutAll {
			t.Fatalf("own session %+v", sess)
		}
		if _, err := svc.ValidateSession(context.Background(), sid); !errors.Is(err, domain.ErrInvalidToken) {
			t.Fatalf("validate %s: %v", sid, err)
		}
	}
	otherSess, _ := w.sessions.Get(context.Background(), other.SessionID)
	if !otherSess.Live() {
		t.Fatalf("foreign session revoked %+v", otherSess)
	}
	if liveTokenCount(w, first.User.ID) != 0 {
		t.Fatal("own tokens still live")
	}
	if liveTokenCount(w, other.User.ID) == 0 {
		t.Fatal("foreign tokens revoked")
	}

	dec, err = svc.LogoutAll(context.Background(), actorOf(first))
	if dec != Commit || err != nil {
		t.Fatalf("repeat dec=%v err=%v", dec, err)
	}
}

func TestLogoutAllTokensLockedBeforeSessions(t *testing.T) {
	w := newWorld()
	first := loginEmail(t, w, "a@b.c")
	w.calls = nil
	if _, err := w.service().LogoutAll(context.Background(), actorOf(first)); err != nil {
		t.Fatal(err)
	}
	lockAt, sessAt, tokAt := w.callIndex("lock-tokens-user"), w.callIndex("revoke-sessions"), w.callIndex("revoke-tokens-user")
	if lockAt < 0 || sessAt < 0 || tokAt < 0 || !(lockAt < sessAt && sessAt < tokAt) {
		t.Fatalf("order %v", w.calls)
	}
}

func TestChangePasswordWrongOldRollsBack(t *testing.T) {
	w := newWorld()
	uow := &memUoW{w: w}
	cred := loginEmail(t, w, "a@b.c")
	before, _ := w.users.Get(context.Background(), cred.User.ID)
	err := ChangePasswordIn(context.Background(), uow, w.service(), ChangePasswordCommand{
		Actor:       actorOf(cred),
		OldPassword: "wrongpassword",
		NewPassword: "newpassword12",
	})
	if !errors.Is(err, domain.ErrInvalidCredentials) {
		t.Fatalf("got %v", err)
	}
	if uow.committed || !uow.rolled {
		t.Fatalf("commit=%v rollback=%v", uow.committed, uow.rolled)
	}
	got, _ := w.users.Get(context.Background(), cred.User.ID)
	if got.PasswordHash != before.PasswordHash {
		t.Fatal("PHC changed on wrong old")
	}
	sess, _ := w.sessions.Get(context.Background(), cred.SessionID)
	if !sess.Live() {
		t.Fatal("session revoked on wrong old")
	}
}

func TestChangePasswordShortNewSkipsHash(t *testing.T) {
	w := newWorld()
	cred := loginEmail(t, w, "a@b.c")
	w.hasher.hashCalls = 0
	w.hasher.verifyCalls = 0
	dec, err := w.service().ChangePassword(context.Background(), ChangePasswordCommand{
		Actor:       actorOf(cred),
		OldPassword: "twelvechars!",
		NewPassword: "12345678901",
	})
	if dec != Rollback || !errors.Is(err, domain.ErrInvalidRequest) {
		t.Fatalf("dec=%v err=%v", dec, err)
	}
	if w.hasher.hashCalls != 0 {
		t.Fatalf("Hash called %d times", w.hasher.hashCalls)
	}
}

func TestChangePasswordSameOldAndNewRejected(t *testing.T) {
	w := newWorld()
	cred := loginEmail(t, w, "a@b.c")
	w.hasher.hashCalls = 0
	dec, err := w.service().ChangePassword(context.Background(), ChangePasswordCommand{
		Actor:       actorOf(cred),
		OldPassword: "twelvechars!",
		NewPassword: "twelvechars!",
	})
	if dec != Rollback || !errors.Is(err, domain.ErrInvalidRequest) {
		t.Fatalf("dec=%v err=%v", dec, err)
	}
	if w.hasher.hashCalls != 0 {
		t.Fatalf("Hash called %d times", w.hasher.hashCalls)
	}
}

func TestChangePasswordSuccessRevokesSessions(t *testing.T) {
	w := newWorld()
	svc := w.service()
	first := loginEmail(t, w, "a@b.c")
	second, _, err := svc.Login(context.Background(), loginCmd("a@b.c"))
	if err != nil {
		t.Fatal(err)
	}
	before, _ := w.users.Get(context.Background(), first.User.ID)
	w.calls = nil
	dec, err := svc.ChangePassword(context.Background(), ChangePasswordCommand{
		Actor:       actorOf(first),
		OldPassword: "twelvechars!",
		NewPassword: "newpassword12",
	})
	if dec != Commit || err != nil {
		t.Fatalf("dec=%v err=%v", dec, err)
	}
	got, _ := w.users.Get(context.Background(), first.User.ID)
	if got.PasswordHash == before.PasswordHash {
		t.Fatal("PHC unchanged")
	}
	want, err := w.hasher.Hash("newpassword12")
	if err != nil {
		t.Fatal(err)
	}
	if got.PasswordHash != want {
		t.Fatalf("PHC %q", got.PasswordHash)
	}
	for _, sid := range []uuid.UUID{first.SessionID, second.SessionID} {
		sess, _ := w.sessions.Get(context.Background(), sid)
		if sess.Live() || sess.RevokeReason != domain.RevokePasswordChanged {
			t.Fatalf("session %+v", sess)
		}
		if _, err := svc.ValidateSession(context.Background(), sid); !errors.Is(err, domain.ErrInvalidToken) {
			t.Fatalf("validate %s: %v", sid, err)
		}
	}
	if liveTokenCount(w, first.User.ID) != 0 {
		t.Fatal("tokens still live")
	}
	lockAt, sessAt, tokAt := w.callIndex("lock-tokens-user"), w.callIndex("revoke-sessions"), w.callIndex("revoke-tokens-user")
	rowAt := w.callIndex("for-update")
	if rowAt < 0 || lockAt < 0 || sessAt < 0 || tokAt < 0 || !(rowAt < lockAt && lockAt < sessAt && sessAt < tokAt) {
		t.Fatalf("order %v", w.calls)
	}
}

func TestChangePasswordSecondCallInvalidCredentials(t *testing.T) {
	w := newWorld()
	svc := w.service()
	uow := &memUoW{w: w}
	cred := loginEmail(t, w, "a@b.c")
	cmd := ChangePasswordCommand{
		Actor:       actorOf(cred),
		OldPassword: "twelvechars!",
		NewPassword: "newpassword12",
	}
	if err := ChangePasswordIn(context.Background(), uow, svc, cmd); err != nil {
		t.Fatal(err)
	}
	winner, _ := w.users.Get(context.Background(), cred.User.ID)
	err := ChangePasswordIn(context.Background(), uow, svc, cmd)
	if !errors.Is(err, domain.ErrInvalidCredentials) {
		t.Fatalf("got %v", err)
	}
	if uow.committed || !uow.rolled {
		t.Fatalf("commit=%v rollback=%v", uow.committed, uow.rolled)
	}
	got, _ := w.users.Get(context.Background(), cred.User.ID)
	if got.PasswordHash != winner.PasswordHash {
		t.Fatal("second call mutated PHC")
	}
}

func TestChangePasswordParallelOneWinner(t *testing.T) {
	w := newWorld()
	svc := w.service()
	cred := loginEmail(t, w, "a@b.c")
	cmd := ChangePasswordCommand{
		Actor:       actorOf(cred),
		OldPassword: "twelvechars!",
		NewPassword: "newpassword12",
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var okN, badN, other int
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := ChangePasswordIn(context.Background(), &memUoW{w: w}, svc, cmd)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				okN++
			case errors.Is(err, domain.ErrInvalidCredentials):
				badN++
			default:
				other++
			}
		}()
	}
	wg.Wait()
	if okN != 1 || badN != 1 || other != 0 {
		t.Fatalf("ok=%d invalid=%d other=%d", okN, badN, other)
	}
	if liveTokenCount(w, cred.User.ID) != 0 {
		t.Fatal("tokens still live")
	}
}

func TestListSessionsLiveOwnOnly(t *testing.T) {
	w := newWorld()
	svc := w.service()
	first := loginEmail(t, w, "a@b.c")
	second, _, err := svc.Login(context.Background(), loginCmd("a@b.c"))
	if err != nil {
		t.Fatal(err)
	}
	other := loginEmail(t, w, "other@b.c")
	if _, err := svc.Logout(context.Background(), actorOf(first)); err != nil {
		t.Fatal(err)
	}
	w.calls = nil
	got, err := svc.ListSessions(context.Background(), actorOf(second))
	if err != nil {
		t.Fatal(err)
	}
	if w.callIndex("list-by-user-live") < 0 {
		t.Fatalf("liveOnly not set: %v", w.calls)
	}
	if len(got) != 1 || got[0].ID != second.SessionID {
		t.Fatalf("sessions %+v", got)
	}
	if !got[0].Live() {
		t.Fatalf("revoked leaked %+v", got)
	}
	for _, sess := range got {
		if sess.UserID != second.User.ID || sess.ID == other.SessionID || sess.ID == first.SessionID {
			t.Fatalf("foreign or revoked %+v", sess)
		}
	}
}

func TestRevokeSessionForeignNotFound(t *testing.T) {
	w := newWorld()
	mine := loginEmail(t, w, "a@b.c")
	other := loginEmail(t, w, "other@b.c")
	dec, err := w.service().RevokeSession(context.Background(), RevokeSessionCommand{
		Actor:     actorOf(mine),
		SessionID: other.SessionID,
	})
	if dec != Rollback || !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("dec=%v err=%v", dec, err)
	}
	sess, _ := w.sessions.Get(context.Background(), other.SessionID)
	if !sess.Live() {
		t.Fatalf("foreign revoked %+v", sess)
	}
}

func TestRevokeSessionOwnAndRepeat(t *testing.T) {
	w := newWorld()
	uow := &memUoW{w: w}
	svc := w.service()
	first := loginEmail(t, w, "a@b.c")
	second, _, err := svc.Login(context.Background(), loginCmd("a@b.c"))
	if err != nil {
		t.Fatal(err)
	}
	w.calls = nil
	if err := RevokeSessionIn(context.Background(), uow, svc, RevokeSessionCommand{
		Actor:     actorOf(first),
		SessionID: second.SessionID,
	}); err != nil {
		t.Fatal(err)
	}
	if !uow.committed {
		t.Fatal("expected commit")
	}
	sess, _ := w.sessions.Get(context.Background(), second.SessionID)
	if sess.Live() || sess.RevokeReason != domain.RevokeLogout {
		t.Fatalf("revoked %+v", sess)
	}
	mine, _ := w.sessions.Get(context.Background(), first.SessionID)
	if !mine.Live() {
		t.Fatal("current session revoked")
	}
	lockAt, sessAt, tokAt := w.callIndex("lock-tokens-session"), w.callIndex("revoke-session"), w.callIndex("revoke-tokens-session")
	if lockAt < 0 || sessAt < 0 || tokAt < 0 || !(lockAt < sessAt && sessAt < tokAt) {
		t.Fatalf("order %v", w.calls)
	}

	if err := RevokeSessionIn(context.Background(), uow, svc, RevokeSessionCommand{
		Actor:     actorOf(first),
		SessionID: second.SessionID,
	}); err != nil {
		t.Fatalf("repeat: %v", err)
	}
	if !uow.committed || uow.rolled {
		t.Fatalf("repeat commit=%v rollback=%v", uow.committed, uow.rolled)
	}
}

func TestLogoutTokensBeforeSession(t *testing.T) {
	w := newWorld()
	first := loginEmail(t, w, "a@b.c")
	w.calls = nil
	if _, err := w.service().Logout(context.Background(), actorOf(first)); err != nil {
		t.Fatal(err)
	}
	lockAt, sessAt, tokAt := w.callIndex("lock-tokens-session"), w.callIndex("revoke-session"), w.callIndex("revoke-tokens-session")
	if lockAt < 0 || sessAt < 0 || tokAt < 0 || !(lockAt < sessAt && sessAt < tokAt) {
		t.Fatalf("order %v", w.calls)
	}
}

func TestEnableUserDisabledToActiveWritesHistory(t *testing.T) {
	w := newWorld()
	svc := w.service()
	admin := seedUser(t, w, "a@b.c", domain.StatusActive, domain.RoleAdmin)
	off := seedUser(t, w, "off@b.c", domain.StatusDisabled, domain.RoleCustomer)
	off.FailedLoginCount = 3
	if err := w.users.Update(context.Background(), off); err != nil {
		t.Fatal(err)
	}
	w.calls = nil
	dec, err := svc.EnableUser(context.Background(), statusCmd(adminPrincipal(admin), off.ID))
	if dec != Commit || err != nil {
		t.Fatalf("dec=%v err=%v", dec, err)
	}
	got, _ := w.users.Get(context.Background(), off.ID)
	if got.Status != domain.StatusActive || got.LockedAt != nil || got.FailedLoginCount != 0 {
		t.Fatalf("enabled %+v", got)
	}
	if len(w.history.all) != 1 {
		t.Fatalf("history rows = %d", len(w.history.all))
	}
	h := w.history.all[0]
	if h.UserID != off.ID || h.PreviousStatus != domain.StatusDisabled || h.NewStatus != domain.StatusActive {
		t.Fatalf("history %+v", h)
	}
	lockAt, rowAt := w.callIndex("lock"), w.callIndex("for-update")
	if lockAt < 0 || rowAt < 0 || rowAt <= lockAt {
		t.Fatalf("advisory after user %v", w.calls)
	}
}

func TestEnableUserLockedIsInvalidRequest(t *testing.T) {
	w := newWorld()
	svc := w.service()
	admin := seedUser(t, w, "a@b.c", domain.StatusActive, domain.RoleAdmin)
	locked := seedUser(t, w, "lock@b.c", domain.StatusLocked, domain.RoleCustomer)
	dec, err := svc.EnableUser(context.Background(), statusCmd(adminPrincipal(admin), locked.ID))
	if dec != Rollback || !errors.Is(err, domain.ErrInvalidRequest) {
		t.Fatalf("dec=%v err=%v", dec, err)
	}
	got, _ := w.users.Get(context.Background(), locked.ID)
	if got.Status != domain.StatusLocked {
		t.Fatalf("status %+v", got)
	}
	if len(w.history.all) != 0 {
		t.Fatalf("history %+v", w.history.all)
	}
}

func TestEnableUserAlreadyActiveNoHistory(t *testing.T) {
	w := newWorld()
	svc := w.service()
	admin := seedUser(t, w, "a@b.c", domain.StatusActive, domain.RoleAdmin)
	cust := seedUser(t, w, "c@b.c", domain.StatusActive, domain.RoleCustomer)
	cust.FailedLoginCount = 2
	if err := w.users.Update(context.Background(), cust); err != nil {
		t.Fatal(err)
	}
	dec, err := svc.EnableUser(context.Background(), statusCmd(adminPrincipal(admin), cust.ID))
	if dec != Commit || err != nil {
		t.Fatalf("dec=%v err=%v", dec, err)
	}
	got, _ := w.users.Get(context.Background(), cust.ID)
	if got.FailedLoginCount != 2 || got.Status != domain.StatusActive {
		t.Fatalf("noop mutated %+v", got)
	}
	if len(w.history.all) != 0 {
		t.Fatalf("history %+v", w.history.all)
	}
}

func TestUnlockIsNotEnable(t *testing.T) {
	w := newWorld()
	svc := w.service()
	admin := seedUser(t, w, "a@b.c", domain.StatusActive, domain.RoleAdmin)
	off := seedUser(t, w, "off@b.c", domain.StatusDisabled, domain.RoleCustomer)
	locked := seedUser(t, w, "lock@b.c", domain.StatusLocked, domain.RoleCustomer)

	if _, err := svc.UnlockUser(context.Background(), statusCmd(adminPrincipal(admin), off.ID)); !errors.Is(err, domain.ErrInvalidRequest) {
		t.Fatalf("unlock disabled: %v", err)
	}
	if _, err := svc.EnableUser(context.Background(), statusCmd(adminPrincipal(admin), locked.ID)); !errors.Is(err, domain.ErrInvalidRequest) {
		t.Fatalf("enable locked: %v", err)
	}
	if _, err := svc.EnableUser(context.Background(), statusCmd(adminPrincipal(admin), off.ID)); err != nil {
		t.Fatal(err)
	}
	got, _ := w.users.Get(context.Background(), off.ID)
	if got.Status != domain.StatusActive {
		t.Fatalf("enable %+v", got)
	}
	if _, err := svc.UnlockUser(context.Background(), statusCmd(adminPrincipal(admin), locked.ID)); err != nil {
		t.Fatal(err)
	}
	got, _ = w.users.Get(context.Background(), locked.ID)
	if got.Status != domain.StatusActive {
		t.Fatalf("unlock %+v", got)
	}
}

func TestUnlockTakesAdvisoryBeforeGetForUpdate(t *testing.T) {
	w := newWorld()
	svc := w.service()
	admin := seedUser(t, w, "a@b.c", domain.StatusActive, domain.RoleAdmin)
	locked := seedUser(t, w, "lock@b.c", domain.StatusLocked, domain.RoleCustomer)
	w.calls = nil
	if _, err := svc.UnlockUser(context.Background(), statusCmd(adminPrincipal(admin), locked.ID)); err != nil {
		t.Fatal(err)
	}
	lockAt, rowAt := w.callIndex("lock"), w.callIndex("for-update")
	if lockAt < 0 || rowAt < 0 || rowAt <= lockAt {
		t.Fatalf("order %v", w.calls)
	}
}

func TestLockUnlockEnableAdvisoryBeforeUser(t *testing.T) {
	w := newWorld()
	svc := w.service()
	admin := seedUser(t, w, "a@b.c", domain.StatusActive, domain.RoleAdmin)
	cust := seedUser(t, w, "c@b.c", domain.StatusActive, domain.RoleCustomer)

	w.calls = nil
	if _, err := svc.LockUser(context.Background(), statusCmd(adminPrincipal(admin), cust.ID)); err != nil {
		t.Fatal(err)
	}
	if w.callIndex("for-update") <= w.callIndex("lock") {
		t.Fatalf("lock order %v", w.calls)
	}

	w.calls = nil
	if _, err := svc.UnlockUser(context.Background(), statusCmd(adminPrincipal(admin), cust.ID)); err != nil {
		t.Fatal(err)
	}
	if w.callIndex("for-update") <= w.callIndex("lock") {
		t.Fatalf("unlock order %v", w.calls)
	}

	if _, err := svc.DisableUser(context.Background(), statusCmd(adminPrincipal(admin), cust.ID)); err != nil {
		t.Fatal(err)
	}
	w.calls = nil
	if _, err := svc.EnableUser(context.Background(), statusCmd(adminPrincipal(admin), cust.ID)); err != nil {
		t.Fatal(err)
	}
	if w.callIndex("for-update") <= w.callIndex("lock") {
		t.Fatalf("enable order %v", w.calls)
	}
}
