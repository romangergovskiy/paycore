package application

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/romangergovskiy/paycore/internal/auth/domain"
)

func TestListUsersKeysetAndRoles(t *testing.T) {
	w := newWorld()
	svc := w.service()
	t0 := time.Date(2026, 11, 13, 8, 0, 0, 0, time.UTC)
	a := seedListedUser(t, w, "a@b.c", domain.StatusActive, t0, domain.RoleAdmin)
	b := seedListedUser(t, w, "b@b.c", domain.StatusLocked, t0.Add(time.Second), domain.RoleCustomer)
	c := seedListedUser(t, w, "c@b.c", domain.StatusActive, t0.Add(2*time.Second), domain.RoleCustomer, domain.RoleSupport)

	page, err := svc.ListUsers(context.Background(), ListUsersQuery{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Users) != 2 || page.Next == nil {
		t.Fatalf("page=%+v next=%v", idsOf(page.Users), page.Next)
	}
	if page.Users[0].ID != a.ID || page.Users[1].ID != b.ID {
		t.Fatalf("order %v want %s,%s", idsOf(page.Users), a.ID, b.ID)
	}
	if page.Users[0].Email != "a@b.c" || page.Users[0].Status != domain.StatusActive {
		t.Fatalf("first %+v", page.Users[0])
	}
	if !containsRole(page.Users[0].Roles, domain.RoleAdmin) {
		t.Fatalf("admin roles %v", page.Users[0].Roles)
	}
	if page.Users[0].FirstName != "Ada" {
		t.Fatalf("public fields stripped too far: %+v", page.Users[0])
	}
	if page.Next.ID != b.ID || !page.Next.CreatedAt.Equal(b.CreatedAt) {
		t.Fatalf("next %+v want %s", page.Next, b.ID)
	}

	rest, err := svc.ListUsers(context.Background(), ListUsersQuery{Limit: 2, Cursor: page.Next})
	if err != nil {
		t.Fatal(err)
	}
	if len(rest.Users) != 1 || rest.Next != nil || rest.Users[0].ID != c.ID {
		t.Fatalf("rest %+v next=%v", idsOf(rest.Users), rest.Next)
	}
	if !containsRole(rest.Users[0].Roles, domain.RoleSupport) || !containsRole(rest.Users[0].Roles, domain.RoleCustomer) {
		t.Fatalf("c roles %v", rest.Users[0].Roles)
	}

	locked := domain.StatusLocked
	only, err := svc.ListUsers(context.Background(), ListUsersQuery{Limit: 50, Status: &locked})
	if err != nil {
		t.Fatal(err)
	}
	if len(only.Users) != 1 || only.Users[0].ID != b.ID || only.Next != nil {
		t.Fatalf("status filter %+v", only)
	}
}

func TestListUsersOmitsPasswordHash(t *testing.T) {
	w := newWorld()
	seedListedUser(t, w, "a@b.c", domain.StatusActive, time.Date(2026, 11, 13, 8, 0, 0, 0, time.UTC), domain.RoleCustomer)
	page, err := w.service().ListUsers(context.Background(), ListUsersQuery{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Users) != 1 {
		t.Fatalf("users %d", len(page.Users))
	}
	raw := page.Users[0].Email + page.Users[0].FirstName + string(page.Users[0].Status)
	if strings.Contains(raw, "$argon2id$") || strings.Contains(page.Users[0].Email, "fake") {
		t.Fatalf("leaked hash into public fields: %+v", page.Users[0])
	}
	stored, err := w.users.List(context.Background(), ListUsersQuery{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 || stored[0].PasswordHash != "" {
		t.Fatalf("List must not return password_hash: %+v", stored)
	}
}

func TestListUsersRejectsBadQuery(t *testing.T) {
	w := newWorld()
	svc := w.service()
	bogus := domain.Status("NOPE")
	for _, q := range []ListUsersQuery{
		{Limit: 0},
		{Limit: 101},
		{Limit: 50, Status: &bogus},
		{Limit: 50, Cursor: &UserCursor{CreatedAt: time.Now().UTC()}},
		{Limit: 50, Cursor: &UserCursor{ID: uuid.Must(uuid.NewV7())}},
	} {
		if _, err := svc.ListUsers(context.Background(), q); !errors.Is(err, domain.ErrInvalidRequest) {
			t.Errorf("q=%+v err=%v", q, err)
		}
	}
}

func TestListUsersEmptyPage(t *testing.T) {
	w := newWorld()
	page, err := w.service().ListUsers(context.Background(), ListUsersQuery{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if page.Users == nil || len(page.Users) != 0 || page.Next != nil {
		t.Fatalf("%+v", page)
	}
}

func seedListedUser(t *testing.T, w *world, email string, st domain.Status, at time.Time, roles ...domain.Role) domain.User {
	t.Helper()
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatal(err)
	}
	u := domain.User{
		ID:           id,
		Email:        email,
		PasswordHash: fakePHC("twelvechars!"),
		FirstName:    "Ada",
		LastName:     "Lovelace",
		Status:       st,
		CreatedAt:    at,
		UpdatedAt:    at,
	}
	if err := w.users.Create(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	for _, role := range roles {
		if err := w.roles.Grant(context.Background(), domain.UserRole{
			UserID:    u.ID,
			Role:      role,
			GrantedAt: at,
		}); err != nil {
			t.Fatal(err)
		}
	}
	return u
}

func idsOf(users []domain.PublicUser) []uuid.UUID {
	out := make([]uuid.UUID, len(users))
	for i, u := range users {
		out[i] = u.ID
	}
	return out
}
