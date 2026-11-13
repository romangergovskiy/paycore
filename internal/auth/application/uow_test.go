package application

import (
	"context"
	"errors"
	"testing"

	"github.com/romangergovskiy/paycore/internal/auth/domain"
)

func TestRegisterInCommits(t *testing.T) {
	w := newWorld()
	got, err := RegisterIn(context.Background(), &memUoW{w: w}, w.service(), registerCmd("a@b.c"))
	if err != nil {
		t.Fatal(err)
	}
	if got.ID.String() == "" || got.Roles[0] != domain.RoleCustomer {
		t.Fatalf("%+v", got)
	}
}

func TestLoginInAndRefreshIn(t *testing.T) {
	w := newWorld()
	uow := &memUoW{w: w}
	svc := w.service()
	if _, err := RegisterIn(context.Background(), uow, svc, registerCmd("a@b.c")); err != nil {
		t.Fatal(err)
	}
	cred, err := LoginIn(context.Background(), uow, svc, loginCmd("a@b.c"))
	if err != nil {
		t.Fatal(err)
	}
	next, err := RefreshIn(context.Background(), uow, svc, RefreshCommand{
		RefreshToken: cred.RefreshToken,
		IP:           "203.0.113.10",
	})
	if err != nil {
		t.Fatal(err)
	}
	if next.SessionID != cred.SessionID {
		t.Fatal("session changed")
	}
	if err := LogoutIn(context.Background(), uow, svc, domain.Principal{
		UserID: cred.User.ID, SessionID: cred.SessionID,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestUoWNil(t *testing.T) {
	w := newWorld()
	if _, err := RegisterIn(context.Background(), nil, w.service(), registerCmd("a@b.c")); !errors.Is(err, errNotWired) {
		t.Fatalf("got %v", err)
	}
	if _, err := LoginIn(context.Background(), nil, w.service(), loginCmd("a@b.c")); !errors.Is(err, errNotWired) {
		t.Fatalf("got %v", err)
	}
	if _, err := RefreshIn(context.Background(), nil, w.service(), RefreshCommand{}); !errors.Is(err, errNotWired) {
		t.Fatalf("got %v", err)
	}
	if err := LogoutIn(context.Background(), nil, w.service(), domain.Principal{}); !errors.Is(err, errNotWired) {
		t.Fatalf("got %v", err)
	}
	cmd := StatusCommand{Actor: domain.Principal{Roles: []domain.Role{domain.RoleAdmin}}, Reason: "x"}
	if err := LockUserIn(context.Background(), nil, w.service(), cmd); !errors.Is(err, errNotWired) {
		t.Fatalf("lock: %v", err)
	}
	if err := UnlockUserIn(context.Background(), nil, w.service(), cmd); !errors.Is(err, errNotWired) {
		t.Fatalf("unlock: %v", err)
	}
	if err := DisableUserIn(context.Background(), nil, w.service(), cmd); !errors.Is(err, errNotWired) {
		t.Fatalf("disable: %v", err)
	}
	if err := LogoutAllIn(context.Background(), nil, w.service(), domain.Principal{}); !errors.Is(err, errNotWired) {
		t.Fatalf("logout-all: %v", err)
	}
	if err := ChangePasswordIn(context.Background(), nil, w.service(), ChangePasswordCommand{}); !errors.Is(err, errNotWired) {
		t.Fatalf("password: %v", err)
	}
	if err := RevokeSessionIn(context.Background(), nil, w.service(), RevokeSessionCommand{}); !errors.Is(err, errNotWired) {
		t.Fatalf("revoke-session: %v", err)
	}
	if err := EnableUserIn(context.Background(), nil, w.service(), cmd); !errors.Is(err, errNotWired) {
		t.Fatalf("enable: %v", err)
	}
	if _, err := BootstrapFirstAdminIn(context.Background(), nil, w.service(), "a@b.c", "twelvechars!"); !errors.Is(err, errNotWired) {
		t.Fatalf("bootstrap-admin: %v", err)
	}
}

func TestUoWCanceledBeforeDo(t *testing.T) {
	w := newWorld()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := RegisterIn(ctx, &memUoW{w: w}, w.service(), registerCmd("a@b.c"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

func TestGrantRevokeIn(t *testing.T) {
	w := newWorld()
	uow := &memUoW{w: w}
	svc := w.service()
	reg, err := RegisterIn(context.Background(), uow, svc, registerCmd("c@b.c"))
	if err != nil {
		t.Fatal(err)
	}
	admin := seedUser(t, w, "admin@b.c", domain.StatusActive, domain.RoleAdmin)
	if err := GrantRoleIn(context.Background(), uow, svc, adminPrincipal(admin), reg.ID, domain.RoleSupport); err != nil {
		t.Fatal(err)
	}
	if err := RevokeRoleIn(context.Background(), uow, svc, adminPrincipal(admin), reg.ID, domain.RoleSupport); err != nil {
		t.Fatal(err)
	}
}

func TestLockUnlockDisableIn(t *testing.T) {
	w := newWorld()
	uow := &memUoW{w: w}
	svc := w.service()
	a1 := seedUser(t, w, "a1@b.c", domain.StatusActive, domain.RoleAdmin)
	a2 := seedUser(t, w, "a2@b.c", domain.StatusActive, domain.RoleAdmin)
	if err := LockUserIn(context.Background(), uow, svc, statusCmd(adminPrincipal(a1), a2.ID)); err != nil {
		t.Fatal(err)
	}
	if err := UnlockUserIn(context.Background(), uow, svc, statusCmd(adminPrincipal(a1), a2.ID)); err != nil {
		t.Fatal(err)
	}
	if err := DisableUserIn(context.Background(), uow, svc, statusCmd(adminPrincipal(a1), a2.ID)); err != nil {
		t.Fatal(err)
	}
	got, _ := w.users.Get(context.Background(), a2.ID)
	if got.Status != domain.StatusDisabled {
		t.Fatalf("status %+v", got)
	}
	if !uow.committed || uow.rolled {
		t.Fatalf("commit=%v rollback=%v", uow.committed, uow.rolled)
	}
	if err := EnableUserIn(context.Background(), uow, svc, statusCmd(adminPrincipal(a1), a2.ID)); err != nil {
		t.Fatal(err)
	}
	got, _ = w.users.Get(context.Background(), a2.ID)
	if got.Status != domain.StatusActive {
		t.Fatalf("enabled %+v", got)
	}
}

func TestBootstrapFirstAdminIn(t *testing.T) {
	w := newWorld()
	uow := &memUoW{w: w}
	got, err := BootstrapFirstAdminIn(context.Background(), uow, w.service(), "a@b.c", "twelvechars!")
	if err != nil {
		t.Fatal(err)
	}
	if !uow.committed || got.Roles[0] != domain.RoleCustomer {
		t.Fatalf("committed=%v user %+v", uow.committed, got)
	}
	if !containsRole(got.Roles, domain.RoleAdmin) {
		t.Fatalf("roles %v", got.Roles)
	}
}
