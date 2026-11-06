package application

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/romangergovskiy/paycore/internal/auth/domain"
)

func TestTx1LoginInvalidCredentialsCommits(t *testing.T) {
	w := newWorld()
	uow := &memUoW{w: w}
	svc := w.service()
	if _, err := RegisterIn(context.Background(), uow, svc, registerCmd("a@b.c")); err != nil {
		t.Fatal(err)
	}
	bad := loginCmd("a@b.c")
	bad.Password = "wrongpassword"
	_, err := LoginIn(context.Background(), uow, svc, bad)
	if !errors.Is(err, domain.ErrInvalidCredentials) {
		t.Fatalf("got %v", err)
	}
	if !uow.committed || uow.rolled {
		t.Fatalf("commit=%v rollback=%v", uow.committed, uow.rolled)
	}
	u, err := w.users.GetByEmail(context.Background(), "a@b.c")
	if err != nil {
		t.Fatal(err)
	}
	if u.FailedLoginCount != 1 {
		t.Fatalf("failed count %d", u.FailedLoginCount)
	}
	if len(w.attempts.all) == 0 {
		t.Fatal("attempt not persisted")
	}
}

func TestTx2LoginRateLimitedCommits(t *testing.T) {
	w := newWorld()
	uow := &memUoW{w: w}
	svc := w.service()
	if _, err := RegisterIn(context.Background(), uow, svc, registerCmd("a@b.c")); err != nil {
		t.Fatal(err)
	}
	bad := loginCmd("a@b.c")
	bad.Password = "wrongpassword"
	for i := 0; i < domain.LimitLoginEmail; i++ {
		_, err := LoginIn(context.Background(), uow, svc, bad)
		if !errors.Is(err, domain.ErrInvalidCredentials) {
			t.Fatalf("i=%d %v", i, err)
		}
	}
	_, err := LoginIn(context.Background(), uow, svc, bad)
	if !errors.Is(err, domain.ErrRateLimited) {
		t.Fatalf("got %v", err)
	}
	if !uow.committed || uow.rolled {
		t.Fatalf("commit=%v rollback=%v", uow.committed, uow.rolled)
	}
	if w.limiter.count(domain.BucketLoginEmail, "a@b.c", w.clock.t) < 6 {
		t.Fatal("email limiter not persisted")
	}
}

func TestTx3TenthFailureLocksAndCommits(t *testing.T) {
	w := newWorld()
	uow := &memUoW{w: w}
	svc := w.service()
	if _, err := RegisterIn(context.Background(), uow, svc, registerCmd("a@b.c")); err != nil {
		t.Fatal(err)
	}
	first, err := LoginIn(context.Background(), uow, svc, loginCmd("a@b.c"))
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
		_, err := LoginIn(context.Background(), uow, svc, bad)
		if !errors.Is(err, domain.ErrInvalidCredentials) {
			t.Fatalf("i=%d %v", i, err)
		}
	}
	if !uow.committed || uow.rolled {
		t.Fatalf("commit=%v rollback=%v", uow.committed, uow.rolled)
	}
	u, _ := w.users.Get(context.Background(), first.User.ID)
	if u.Status != domain.StatusLocked || u.LockedAt == nil {
		t.Fatalf("user %+v", u)
	}
	sess, _ := w.sessions.Get(context.Background(), first.SessionID)
	if sess.Live() || sess.RevokeReason != domain.RevokeUserLocked {
		t.Fatalf("session %+v", sess)
	}
}

func TestTx4RefreshReuseCommitsRevoke(t *testing.T) {
	w := newWorld()
	uow := &memUoW{w: w}
	svc := w.service()
	if _, err := RegisterIn(context.Background(), uow, svc, registerCmd("a@b.c")); err != nil {
		t.Fatal(err)
	}
	first, err := LoginIn(context.Background(), uow, svc, loginCmd("a@b.c"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RefreshIn(context.Background(), uow, svc, RefreshCommand{
		RefreshToken: first.RefreshToken, IP: "203.0.113.10",
	}); err != nil {
		t.Fatal(err)
	}
	_, err = RefreshIn(context.Background(), uow, svc, RefreshCommand{
		RefreshToken: first.RefreshToken, IP: "203.0.113.10",
	})
	if !errors.Is(err, domain.ErrTokenReused) {
		t.Fatalf("got %v", err)
	}
	if !uow.committed || uow.rolled {
		t.Fatalf("commit=%v rollback=%v", uow.committed, uow.rolled)
	}
	sess, _ := w.sessions.Get(context.Background(), first.SessionID)
	if sess.Live() || sess.RevokeReason != domain.RevokeTokenReuse {
		t.Fatalf("session %+v", sess)
	}
}

func TestTx5RefreshExpiredUnusedNoRevoke(t *testing.T) {
	w := newWorld()
	uow := &memUoW{w: w}
	svc := w.service()
	if _, err := RegisterIn(context.Background(), uow, svc, registerCmd("a@b.c")); err != nil {
		t.Fatal(err)
	}
	first, err := LoginIn(context.Background(), uow, svc, loginCmd("a@b.c"))
	if err != nil {
		t.Fatal(err)
	}
	w.clock.t = w.clock.t.Add(domain.RefreshTTL).Add(time.Second)
	_, err = RefreshIn(context.Background(), uow, svc, RefreshCommand{
		RefreshToken: first.RefreshToken, IP: "203.0.113.10",
	})
	if !errors.Is(err, domain.ErrTokenExpired) {
		t.Fatalf("got %v", err)
	}
	if !uow.committed || uow.rolled {
		t.Fatalf("commit=%v rollback=%v", uow.committed, uow.rolled)
	}
	sess, _ := w.sessions.Get(context.Background(), first.SessionID)
	if !sess.Live() {
		t.Fatal("expired unused refresh must not revoke session")
	}
}

func TestTx6RefreshUnknownLeavesOtherSessions(t *testing.T) {
	w := newWorld()
	uow := &memUoW{w: w}
	svc := w.service()
	if _, err := RegisterIn(context.Background(), uow, svc, registerCmd("a@b.c")); err != nil {
		t.Fatal(err)
	}
	first, err := LoginIn(context.Background(), uow, svc, loginCmd("a@b.c"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = RefreshIn(context.Background(), uow, svc, RefreshCommand{
		RefreshToken: "unknown-token", IP: "203.0.113.10",
	})
	if !errors.Is(err, domain.ErrInvalidToken) {
		t.Fatalf("got %v", err)
	}
	if !uow.committed || uow.rolled {
		t.Fatalf("commit=%v rollback=%v", uow.committed, uow.rolled)
	}
	sess, _ := w.sessions.Get(context.Background(), first.SessionID)
	if !sess.Live() {
		t.Fatal("unknown refresh must not touch other sessions")
	}
}

func TestTx7RegisterEmailTakenCommitsLimiter(t *testing.T) {
	w := newWorld()
	uow := &memUoW{w: w}
	svc := w.service()
	if _, err := RegisterIn(context.Background(), uow, svc, registerCmd("a@b.c")); err != nil {
		t.Fatal(err)
	}
	_, err := RegisterIn(context.Background(), uow, svc, registerCmd("A@B.C"))
	if !errors.Is(err, domain.ErrEmailTaken) {
		t.Fatalf("got %v", err)
	}
	if !uow.committed || uow.rolled {
		t.Fatalf("commit=%v rollback=%v", uow.committed, uow.rolled)
	}
	if w.limiter.count(domain.BucketRegisterIP, "203.0.113.10", w.clock.t) != 2 {
		t.Fatal("limiter rolled back on EMAIL_TAKEN")
	}
	if len(w.users.byID) != 1 {
		t.Fatalf("users %d", len(w.users.byID))
	}
}

func TestTx8RegisterRateLimitedCommits(t *testing.T) {
	w := newWorld()
	uow := &memUoW{w: w}
	svc := w.service()
	for i := 0; i < domain.LimitRegisterIP; i++ {
		cmd := registerCmd("a@b.c")
		cmd.Email = string(rune('a'+i)) + "@b.c"
		if _, err := RegisterIn(context.Background(), uow, svc, cmd); err != nil {
			t.Fatalf("i=%d %v", i, err)
		}
	}
	_, err := RegisterIn(context.Background(), uow, svc, registerCmd("z@b.c"))
	if !errors.Is(err, domain.ErrRateLimited) {
		t.Fatalf("got %v", err)
	}
	if !uow.committed || uow.rolled {
		t.Fatalf("commit=%v rollback=%v", uow.committed, uow.rolled)
	}
}

func TestTx9HasherErrorRollsBack(t *testing.T) {
	w := newWorld()
	uow := &memUoW{w: w}
	w.hasher.failHash = true
	_, err := RegisterIn(context.Background(), uow, w.service(), registerCmd("a@b.c"))
	if err == nil {
		t.Fatal("expected hasher error")
	}
	if uow.committed || !uow.rolled {
		t.Fatalf("commit=%v rollback=%v — fake must not pretend to commit", uow.committed, uow.rolled)
	}
	if len(w.users.byID) != 0 {
		t.Fatal("user persisted after rollback")
	}
	if w.limiter.count(domain.BucketRegisterIP, "203.0.113.10", w.clock.t) != 0 {
		t.Fatal("limiter must roll back with infra error")
	}
}

func TestTx10CanceledRollsBack(t *testing.T) {
	w := newWorld()
	uow := &memUoW{w: w}
	svc := w.service()
	if _, err := RegisterIn(context.Background(), uow, svc, registerCmd("a@b.c")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := LoginIn(ctx, uow, svc, loginCmd("a@b.c"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	if uow.committed {
		t.Fatal("canceled login must not commit")
	}
}

func TestTx11GrantDoesNotRevokeSessions(t *testing.T) {
	w := newWorld()
	uow := &memUoW{w: w}
	svc := w.service()
	reg, err := RegisterIn(context.Background(), uow, svc, registerCmd("cust@b.c"))
	if err != nil {
		t.Fatal(err)
	}
	cred, err := LoginIn(context.Background(), uow, svc, loginCmd("cust@b.c"))
	if err != nil {
		t.Fatal(err)
	}
	admin := seedUser(t, w, "admin@b.c", domain.StatusActive, domain.RoleAdmin)
	if err := GrantRoleIn(context.Background(), uow, svc, adminPrincipal(admin), reg.ID, domain.RoleMerchant); err != nil {
		t.Fatal(err)
	}
	if !uow.committed {
		t.Fatal("grant should commit")
	}
	p, err := svc.ValidateSession(context.Background(), cred.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Has(domain.RoleMerchant) {
		t.Fatalf("roles from store: %+v", p)
	}
	sess, _ := w.sessions.Get(context.Background(), cred.SessionID)
	if !sess.Live() {
		t.Fatal("grant must not revoke sessions")
	}
}

func TestTx12NoteAttemptPropagatesAppendError(t *testing.T) {
	w := newWorld()
	uow := &memUoW{w: w}
	svc := w.service()
	if _, err := RegisterIn(context.Background(), uow, svc, registerCmd("a@b.c")); err != nil {
		t.Fatal(err)
	}
	want := errors.New("attempt store down")
	w.attempts.fail = want
	bad := loginCmd("a@b.c")
	bad.Password = "wrongpassword"
	_, err := LoginIn(context.Background(), uow, svc, bad)
	if !errors.Is(err, want) {
		t.Fatalf("got %v", err)
	}
	if uow.committed || !uow.rolled {
		t.Fatalf("commit=%v rollback=%v", uow.committed, uow.rolled)
	}
}

func TestTx13ParallelRefreshOneChain(t *testing.T) {
	w := newWorld()
	svc := w.service()
	uow := &memUoW{w: w}
	if _, err := RegisterIn(context.Background(), uow, svc, registerCmd("a@b.c")); err != nil {
		t.Fatal(err)
	}
	first, err := LoginIn(context.Background(), uow, svc, loginCmd("a@b.c"))
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	var okN, reuseN, other int
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := RefreshIn(context.Background(), &memUoW{w: w}, svc, RefreshCommand{
				RefreshToken: first.RefreshToken, IP: "203.0.113.10",
			})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				okN++
			case errors.Is(err, domain.ErrTokenReused):
				reuseN++
			default:
				other++
			}
		}()
	}
	wg.Wait()
	if okN != 1 || reuseN != 1 || other != 0 {
		t.Fatalf("ok=%d reused=%d other=%d — fake serializes whole TX; adapter must SELECT FOR UPDATE", okN, reuseN, other)
	}
	live := 0
	w.tokens.mu.Lock()
	for _, tok := range w.tokens.byID {
		if tok.UsedAt == nil && tok.RevokedAt == nil && !tok.Expired(w.clock.t) {
			live++
		}
	}
	w.tokens.mu.Unlock()
	if live > 1 {
		t.Fatalf("two live refresh chains (%d); adapter must SELECT FOR UPDATE", live)
	}
	sess, _ := w.sessions.Get(context.Background(), first.SessionID)
	if sess.Live() {
		t.Fatal("reuse of the original token must revoke the session")
	}
}

func TestTxFakeDoesNotCommitOnRollback(t *testing.T) {
	w := newWorld()
	uow := &memUoW{w: w}
	w.roles.grant = errors.New("role store down")
	_, err := RegisterIn(context.Background(), uow, w.service(), registerCmd("a@b.c"))
	if err == nil {
		t.Fatal("expected grant error")
	}
	if uow.committed || !uow.rolled {
		t.Fatalf("commit=%v rollback=%v", uow.committed, uow.rolled)
	}
	if len(w.users.byID) != 0 {
		t.Fatal("partial register must not survive rollback")
	}
}
