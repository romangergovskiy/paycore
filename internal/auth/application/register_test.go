package application

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/romangergovskiy/paycore/internal/auth/domain"
)

func TestRegisterCustomerOnly(t *testing.T) {
	w := newWorld()
	got, _, err := w.service().Register(context.Background(), registerCmd("Ada@Pay.example"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Email != "ada@pay.example" || got.Status != domain.StatusActive {
		t.Fatalf("%+v", got)
	}
	if len(got.Roles) != 1 || got.Roles[0] != domain.RoleCustomer {
		t.Fatalf("roles=%v", got.Roles)
	}
	stored, err := w.users.Get(context.Background(), got.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(stored.PasswordHash, "$argon2id$") {
		t.Fatalf("hash %q", stored.PasswordHash)
	}
	if stored.PasswordHash == "twelvechars!" {
		t.Fatal("stored plaintext password")
	}
}

func TestRegisterRejectsRolesInCommand(t *testing.T) {
	w := newWorld()
	cmd := registerCmd("a@b.c")
	cmd.Roles = []domain.Role{domain.RoleAdmin}
	_, _, err := w.service().Register(context.Background(), cmd)
	if !errors.Is(err, domain.ErrInvalidRequest) {
		t.Fatalf("got %v", err)
	}
}

func TestRegisterPasswordPolicy(t *testing.T) {
	w := newWorld()
	short := registerCmd("a@b.c")
	short.Password = "12345678901"
	if _, _, err := w.service().Register(context.Background(), short); !errors.Is(err, domain.ErrInvalidRequest) {
		t.Fatalf("11 runes: %v", err)
	}
	long := registerCmd("a@b.c")
	long.Password = strings.Repeat("a", 129)
	if _, _, err := w.service().Register(context.Background(), long); !errors.Is(err, domain.ErrInvalidRequest) {
		t.Fatalf("129 runes: %v", err)
	}
}

func TestRegisterInvalidInput(t *testing.T) {
	w := newWorld()
	svc := w.service()
	cases := []RegisterCommand{
		{Email: "bad", Password: "twelvechars!", IP: "1.1.1.1"},
		{Email: "a@b.c", Password: "twelvechars!", IP: ""},
		{Email: "a@b.c", Password: "twelvechars!", IP: "1.1.1.1", FirstName: strings.Repeat("n", 101)},
	}
	for _, cmd := range cases {
		if _, _, err := svc.Register(context.Background(), cmd); !errors.Is(err, domain.ErrInvalidRequest) {
			t.Errorf("%+v: %v", cmd, err)
		}
	}
}

func TestRegisterEmailTaken(t *testing.T) {
	w := newWorld()
	svc := w.service()
	if _, _, err := svc.Register(context.Background(), registerCmd("a@b.c")); err != nil {
		t.Fatal(err)
	}
	_, _, err := svc.Register(context.Background(), registerCmd("A@B.C"))
	if !errors.Is(err, domain.ErrEmailTaken) {
		t.Fatalf("got %v", err)
	}
	if w.limiter.count(domain.BucketRegisterIP, "203.0.113.10", w.clock.t) != 2 {
		t.Fatalf("limiter should persist on EMAIL_TAKEN")
	}
}

func TestRegisterRateLimited(t *testing.T) {
	w := newWorld()
	svc := w.service()
	for i := 0; i < domain.LimitRegisterIP; i++ {
		cmd := registerCmd("user@x.example")
		cmd.Email = strings.ReplaceAll(cmd.Email, "user", string(rune('a'+i))+"user")
		if _, _, err := svc.Register(context.Background(), cmd); err != nil {
			t.Fatalf("i=%d %v", i, err)
		}
	}
	_, _, err := svc.Register(context.Background(), registerCmd("last@x.example"))
	if !errors.Is(err, domain.ErrRateLimited) {
		t.Fatalf("got %v", err)
	}
}

func TestRegisterHasherError(t *testing.T) {
	w := newWorld()
	w.hasher.failHash = true
	_, _, err := w.service().Register(context.Background(), registerCmd("a@b.c"))
	if err == nil || errors.Is(err, domain.ErrInvalidRequest) {
		t.Fatalf("got %v", err)
	}
	if len(w.users.byID) != 0 {
		t.Fatal("user created after hasher error")
	}
}

func TestRegisterPartialRoleGrant(t *testing.T) {
	w := newWorld()
	w.roles.grant = errors.New("role store down")
	_, _, err := w.service().Register(context.Background(), registerCmd("a@b.c"))
	if err == nil {
		t.Fatal("expected error")
	}
	if len(w.users.byID) != 1 {
		t.Fatalf("partial user writes without UoW: %d", len(w.users.byID))
	}
}

func TestRegisterCanceled(t *testing.T) {
	w := newWorld()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := w.service().Register(ctx, registerCmd("a@b.c"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

func TestRegisterNotWired(t *testing.T) {
	_, _, err := (Service{}).Register(context.Background(), registerCmd("a@b.c"))
	if !errors.Is(err, errNotWired) {
		t.Fatalf("got %v", err)
	}
}
