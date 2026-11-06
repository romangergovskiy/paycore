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
