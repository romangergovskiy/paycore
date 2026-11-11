package application

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/romangergovskiy/paycore/internal/auth/domain"
)

const bootstrapPassword = "twelvechars!"

func TestBootstrapFirstAdminCreatesCustomerAndAdmin(t *testing.T) {
	w := newWorld()
	svc := w.service()
	got, dec, err := svc.BootstrapFirstAdmin(context.Background(), "Ada@Pay.example", bootstrapPassword)
	if dec != Commit || err != nil {
		t.Fatalf("dec=%v err=%v", dec, err)
	}
	if got.Email != "ada@pay.example" || got.Status != domain.StatusActive {
		t.Fatalf("user %+v", got)
	}
	if len(got.Roles) != 2 || !containsRole(got.Roles, domain.RoleCustomer) || !containsRole(got.Roles, domain.RoleAdmin) {
		t.Fatalf("roles %v", got.Roles)
	}
	stored, err := w.users.Get(context.Background(), got.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.PasswordHash == bootstrapPassword || !strings.HasPrefix(stored.PasswordHash, "$argon2id$") {
		t.Fatalf("stored hash %q", stored.PasswordHash)
	}
	roles, err := w.roles.List(context.Background(), got.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(roles) != 2 || !containsRole(roles, domain.RoleCustomer) || !containsRole(roles, domain.RoleAdmin) {
		t.Fatalf("store roles %v", roles)
	}
	w.roles.mu.Lock()
	urs := w.roles.byUser[got.ID]
	w.roles.mu.Unlock()
	for _, ur := range urs {
		if ur.GrantedBy != nil {
			t.Fatalf("granted_by must be NULL, got %+v", ur)
		}
	}
	if len(w.sessions.byID) != 0 || len(w.tokens.byID) != 0 {
		t.Fatalf("bootstrap opened a session")
	}
	if len(w.history.all) != 0 || len(w.attempts.all) != 0 {
		t.Fatalf("bootstrap wrote history or attempts")
	}
}

func TestBootstrapFirstAdminAdvisoryBeforeCount(t *testing.T) {
	w := newWorld()
	w.calls = nil
	_, dec, err := w.service().BootstrapFirstAdmin(context.Background(), "a@b.c", bootstrapPassword)
	if dec != Commit || err != nil {
		t.Fatalf("dec=%v err=%v", dec, err)
	}
	lockAt, countAt, hashAt, createAt := w.callIndex("lock"), w.callIndex("count"), w.callIndex("hash"), w.callIndex("create")
	if lockAt < 0 || countAt <= lockAt || hashAt <= countAt || createAt <= hashAt {
		t.Fatalf("order %v", w.calls)
	}
	if w.callIndex("for-update") >= 0 {
		t.Fatalf("user FOR UPDATE before/during bootstrap %v", w.calls)
	}
}

func TestBootstrapFirstAdminRepeatIsNoOp(t *testing.T) {
	w := newWorld()
	svc := w.service()
	first, _, err := svc.BootstrapFirstAdmin(context.Background(), "one@b.c", bootstrapPassword)
	if err != nil {
		t.Fatal(err)
	}
	w.hasher.hashCalls = 0
	w.calls = nil
	got, dec, err := svc.BootstrapFirstAdmin(context.Background(), "other@b.c", "differentpass1")
	if dec != Commit || err != nil {
		t.Fatalf("dec=%v err=%v", dec, err)
	}
	if got.ID != uuid.Nil {
		t.Fatalf("no-op returned user %+v", got)
	}
	if w.hasher.hashCalls != 0 {
		t.Fatalf("no-op hashed password")
	}
	n, err := w.roles.CountActiveWithRole(context.Background(), domain.RoleAdmin)
	if err != nil || n != 1 {
		t.Fatalf("admins=%d err=%v", n, err)
	}
	if _, err := w.users.GetByEmail(context.Background(), "other@b.c"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("second email inserted: %v", err)
	}
	stored, err := w.users.Get(context.Background(), first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.PasswordHash != fakePHC(bootstrapPassword) {
		t.Fatalf("no-op changed PHC")
	}
	if w.callIndex("lock") < 0 || w.callIndex("count") <= w.callIndex("lock") {
		t.Fatalf("no-op order %v", w.calls)
	}
	if w.callIndex("create") >= 0 || w.callIndex("hash") >= 0 {
		t.Fatalf("no-op wrote %v", w.calls)
	}
	if _, dec, err := svc.BootstrapFirstAdmin(context.Background(), "third@b.c", "short"); dec != Commit || err != nil {
		t.Fatalf("no-op must ignore password policy dec=%v err=%v", dec, err)
	}
}

func TestBootstrapFirstAdminOccupiedEmailRollsBack(t *testing.T) {
	w := newWorld()
	svc := w.service()
	reg, _, err := svc.Register(context.Background(), registerCmd("taken@b.c"))
	if err != nil {
		t.Fatal(err)
	}
	uow := &memUoW{w: w}
	_, err = BootstrapFirstAdminIn(context.Background(), uow, svc, "taken@b.c", bootstrapPassword)
	if !errors.Is(err, domain.ErrEmailTaken) {
		t.Fatalf("got %v", err)
	}
	if !uow.rolled || uow.committed {
		t.Fatalf("committed=%v rolled=%v", uow.committed, uow.rolled)
	}
	n, err := w.roles.CountActiveWithRole(context.Background(), domain.RoleAdmin)
	if err != nil || n != 0 {
		t.Fatalf("admins=%d err=%v", n, err)
	}
	roles, err := w.roles.List(context.Background(), reg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if containsRole(roles, domain.RoleAdmin) {
		t.Fatalf("added ADMIN to occupied customer: %v", roles)
	}
	if _, err := w.users.Get(context.Background(), reg.ID); err != nil {
		t.Fatalf("customer row lost: %v", err)
	}
}

func TestBootstrapFirstAdminGrantFailureRollsBackUser(t *testing.T) {
	w := newWorld()
	w.roles.failGrant = 2
	uow := &memUoW{w: w}
	_, err := BootstrapFirstAdminIn(context.Background(), uow, w.service(), "a@b.c", bootstrapPassword)
	if err == nil {
		t.Fatal("expected grant error")
	}
	if !uow.rolled {
		t.Fatal("expected rollback")
	}
	if _, err := w.users.GetByEmail(context.Background(), "a@b.c"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("partial user survived: %v", err)
	}
	n, err := w.roles.CountActiveWithRole(context.Background(), domain.RoleAdmin)
	if err != nil || n != 0 {
		t.Fatalf("admins=%d err=%v", n, err)
	}
}

func TestBootstrapFirstAdminDoesNotLogSecrets(t *testing.T) {
	w := newWorld()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	password := "super-secret-12"
	_, _, err := w.service().BootstrapFirstAdmin(context.Background(), "a@b.c", password)
	if err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.Contains(out, password) || strings.Contains(out, "$argon2id$") || strings.Contains(out, fakePHC(password)) {
		t.Fatalf("log leaked secret: %s", out)
	}
}

func TestBootstrapFirstAdminInvalidInput(t *testing.T) {
	w := newWorld()
	svc := w.service()
	if _, dec, err := svc.BootstrapFirstAdmin(context.Background(), "bad", bootstrapPassword); dec != Rollback || !errors.Is(err, domain.ErrInvalidRequest) {
		t.Fatalf("email dec=%v err=%v", dec, err)
	}
	if _, dec, err := svc.BootstrapFirstAdmin(context.Background(), "a@b.c", "12345678901"); dec != Rollback || !errors.Is(err, domain.ErrInvalidRequest) {
		t.Fatalf("password dec=%v err=%v", dec, err)
	}
	if n, err := w.roles.CountActiveWithRole(context.Background(), domain.RoleAdmin); err != nil || n != 0 {
		t.Fatalf("admins=%d err=%v", n, err)
	}
}

func TestRegisterStillCannotAssignAdmin(t *testing.T) {
	w := newWorld()
	svc := w.service()
	if _, _, err := svc.BootstrapFirstAdmin(context.Background(), "admin@b.c", bootstrapPassword); err != nil {
		t.Fatal(err)
	}
	cmd := registerCmd("cust@b.c")
	cmd.Roles = []domain.Role{domain.RoleAdmin}
	if _, _, err := svc.Register(context.Background(), cmd); !errors.Is(err, domain.ErrInvalidRequest) {
		t.Fatalf("register roles: %v", err)
	}
	got, _, err := svc.Register(context.Background(), registerCmd("cust@b.c"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Roles) != 1 || got.Roles[0] != domain.RoleCustomer {
		t.Fatalf("roles %v", got.Roles)
	}
}
