package application

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/romangergovskiy/paycore/internal/auth/domain"
)

func TestGrantRoleAdminOnly(t *testing.T) {
	w := newWorld()
	svc := w.service()
	reg, _, err := svc.Register(context.Background(), registerCmd("cust@b.c"))
	if err != nil {
		t.Fatal(err)
	}
	customer := domain.Principal{UserID: reg.ID, Roles: []domain.Role{domain.RoleCustomer}}
	_, err = svc.GrantRole(context.Background(), customer, reg.ID, domain.RoleSupport)
	if !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("customer grant: %v", err)
	}

	admin := seedUser(t, w, "admin@b.c", domain.StatusActive, domain.RoleAdmin)
	if _, err := svc.GrantRole(context.Background(), adminPrincipal(admin), reg.ID, domain.RoleSupport); err != nil {
		t.Fatal(err)
	}
	roles, err := svc.GetRoles(context.Background(), reg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !containsRole(roles, domain.RoleSupport) || !containsRole(roles, domain.RoleCustomer) {
		t.Fatalf("roles %v", roles)
	}
}

func TestGrantDoesNotRevokeSessions(t *testing.T) {
	w := newWorld()
	svc := w.service()
	reg, _, err := svc.Register(context.Background(), registerCmd("cust@b.c"))
	if err != nil {
		t.Fatal(err)
	}
	cred, _, err := svc.Login(context.Background(), loginCmd("cust@b.c"))
	if err != nil {
		t.Fatal(err)
	}
	admin := seedUser(t, w, "admin@b.c", domain.StatusActive, domain.RoleAdmin)
	if _, err := svc.GrantRole(context.Background(), adminPrincipal(admin), reg.ID, domain.RoleMerchant); err != nil {
		t.Fatal(err)
	}
	p, err := svc.ValidateSession(context.Background(), cred.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Has(domain.RoleMerchant) || !p.Has(domain.RoleCustomer) {
		t.Fatalf("current roles %+v", p)
	}
	sess, _ := w.sessions.Get(context.Background(), cred.SessionID)
	if !sess.Live() {
		t.Fatal("grant must not revoke session (identity-auth §4.1)")
	}
}

func TestGrantDuplicateAndUnknown(t *testing.T) {
	w := newWorld()
	svc := w.service()
	reg, _, _ := svc.Register(context.Background(), registerCmd("cust@b.c"))
	admin := seedUser(t, w, "admin@b.c", domain.StatusActive, domain.RoleAdmin)
	actor := adminPrincipal(admin)
	if _, err := svc.GrantRole(context.Background(), actor, reg.ID, domain.RoleSupport); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.GrantRole(context.Background(), actor, reg.ID, domain.RoleSupport); !errors.Is(err, domain.ErrInvalidRequest) {
		t.Fatalf("duplicate: %v", err)
	}
	if _, err := svc.GrantRole(context.Background(), actor, reg.ID, domain.Role("nope")); !errors.Is(err, domain.ErrInvalidRequest) {
		t.Fatalf("unknown: %v", err)
	}
}

func TestGrantMissingUserAndSelfPrivilege(t *testing.T) {
	w := newWorld()
	svc := w.service()
	admin := seedUser(t, w, "admin@b.c", domain.StatusActive, domain.RoleAdmin)
	actor := adminPrincipal(admin)
	if _, err := svc.GrantRole(context.Background(), actor, uuid.Must(uuid.NewV7()), domain.RoleSupport); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	if _, err := svc.GrantRole(context.Background(), actor, admin.ID, domain.RoleSupport); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("self privilege: %v", err)
	}
}

func TestRevokeLastAdmin(t *testing.T) {
	w := newWorld()
	svc := w.service()
	a1 := seedUser(t, w, "a1@b.c", domain.StatusActive, domain.RoleAdmin)
	a2 := seedUser(t, w, "a2@b.c", domain.StatusActive, domain.RoleAdmin)
	if _, err := svc.RevokeRole(context.Background(), adminPrincipal(a1), a1.ID, domain.RoleAdmin); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("self admin: %v", err)
	}
	if _, err := svc.RevokeRole(context.Background(), adminPrincipal(a1), a2.ID, domain.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RevokeRole(context.Background(), adminPrincipal(a1), a1.ID, domain.RoleAdmin); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("self last: %v", err)
	}
	// another admin does not exist; a1 cannot be stripped by a2 (revoked).
	a3 := seedUser(t, w, "a3@b.c", domain.StatusActive, domain.RoleAdmin)
	if _, err := svc.RevokeRole(context.Background(), adminPrincipal(a3), a1.ID, domain.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RevokeRole(context.Background(), adminPrincipal(a3), a3.ID, domain.RoleAdmin); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("cannot strip self")
	}
	locked := seedUser(t, w, "locked-admin@b.c", domain.StatusLocked, domain.RoleAdmin)
	if _, err := svc.RevokeRole(context.Background(), adminPrincipal(locked), a3.ID, domain.RoleAdmin); !errors.Is(err, domain.ErrLastAdmin) {
		t.Fatalf("stale admin principal vs last ACTIVE: %v", err)
	}
	cust, _, _ := svc.Register(context.Background(), registerCmd("c@b.c"))
	if _, err := svc.RevokeRole(context.Background(), adminPrincipal(a3), cust.ID, domain.RoleAdmin); !errors.Is(err, domain.ErrInvalidRequest) {
		t.Fatalf("no such role: %v", err)
	}
	if _, err := svc.RevokeRole(context.Background(), adminPrincipal(a3), cust.ID, domain.RoleCustomer); err != nil {
		t.Fatal(err)
	}
}

func TestRevokeNonAdminForbidden(t *testing.T) {
	w := newWorld()
	svc := w.service()
	reg, _, _ := svc.Register(context.Background(), registerCmd("c@b.c"))
	_, err := svc.RevokeRole(context.Background(), domain.Principal{
		UserID: reg.ID, Roles: []domain.Role{domain.RoleCustomer},
	}, reg.ID, domain.RoleCustomer)
	if !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("got %v", err)
	}
}

func TestGetRolesCanceled(t *testing.T) {
	w := newWorld()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := w.service().GetRoles(ctx, uuid.Must(uuid.NewV7()))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

func TestGrantCanceled(t *testing.T) {
	w := newWorld()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := w.service().GrantRole(ctx, domain.Principal{Roles: []domain.Role{domain.RoleAdmin}}, uuid.Must(uuid.NewV7()), domain.RoleSupport)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

func containsRole(roles []domain.Role, want domain.Role) bool {
	for _, r := range roles {
		if r == want {
			return true
		}
	}
	return false
}
