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
	w.roles.calls = nil
	page, err := w.service().ListUsers(context.Background(), ListUsersQuery{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if page.Users == nil || len(page.Users) != 0 || page.Next != nil {
		t.Fatalf("%+v", page)
	}
	if n := countOp(w.roles.calls, "list-by-ids"); n != 1 {
		t.Fatalf("empty page role loads: %v", w.roles.calls)
	}
	if n := countOp(w.roles.calls, "list"); n != 0 {
		t.Fatalf("per-user List on empty page: %v", w.roles.calls)
	}
}

func TestListUsersBatchesRolesOnce(t *testing.T) {
	w := newWorld()
	t0 := time.Date(2026, 11, 13, 8, 0, 0, 0, time.UTC)
	none := seedListedUser(t, w, "none@b.c", domain.StatusActive, t0)
	multi := seedListedUser(t, w, "multi@b.c", domain.StatusActive, t0.Add(time.Second), domain.RoleSupport, domain.RoleCustomer)
	w.roles.calls = nil

	page, err := w.service().ListUsers(context.Background(), ListUsersQuery{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Users) != 2 || page.Next != nil {
		t.Fatalf("page %+v next=%v", idsOf(page.Users), page.Next)
	}
	if page.Users[0].ID != none.ID || page.Users[1].ID != multi.ID {
		t.Fatalf("order %v", idsOf(page.Users))
	}
	if page.Users[0].Roles == nil || len(page.Users[0].Roles) != 0 {
		t.Fatalf("no-role user roles=%#v", page.Users[0].Roles)
	}
	if len(page.Users[1].Roles) != 2 || page.Users[1].Roles[0] != domain.RoleCustomer || page.Users[1].Roles[1] != domain.RoleSupport {
		t.Fatalf("multi roles %v", page.Users[1].Roles)
	}
	if n := countOp(w.roles.calls, "list-by-ids"); n != 1 {
		t.Fatalf("want one ListByIDs, got %v", w.roles.calls)
	}
	if n := countOp(w.roles.calls, "list"); n != 0 {
		t.Fatalf("N+1 List: %v", w.roles.calls)
	}
}

func TestListUsersBatchRoleError(t *testing.T) {
	w := newWorld()
	seedListedUser(t, w, "a@b.c", domain.StatusActive, time.Date(2026, 11, 13, 8, 0, 0, 0, time.UTC), domain.RoleCustomer)
	want := errors.New("roles down")
	w.roles.listByIDsErr = want
	_, err := w.service().ListUsers(context.Background(), ListUsersQuery{Limit: 10})
	if !errors.Is(err, want) {
		t.Fatalf("err=%v", err)
	}
}

func TestListUsersEqualCreatedAt(t *testing.T) {
	w := newWorld()
	t0 := time.Date(2026, 11, 13, 8, 0, 0, 0, time.UTC)
	lo := uuid.MustParse("00000000-0000-4000-8000-0000000000aa")
	mid := uuid.MustParse("00000000-0000-4000-8000-0000000000bb")
	hi := uuid.MustParse("00000000-0000-4000-8000-0000000000cc")
	seedListedUserID(t, w, lo, "lo@b.c", domain.StatusActive, t0, domain.RoleCustomer)
	seedListedUserID(t, w, hi, "hi@b.c", domain.StatusActive, t0, domain.RoleAdmin)
	seedListedUserID(t, w, mid, "mid@b.c", domain.StatusActive, t0)

	page, err := w.service().ListUsers(context.Background(), ListUsersQuery{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Users) != 2 || page.Next == nil {
		t.Fatalf("page ids=%v next=%v", idsOf(page.Users), page.Next)
	}
	if page.Users[0].ID != lo || page.Users[1].ID != mid {
		t.Fatalf("tie-break order %v", idsOf(page.Users))
	}
	if page.Next.ID != mid || !page.Next.CreatedAt.Equal(t0) {
		t.Fatalf("cursor from extra row? %+v", page.Next)
	}
	if len(page.Users[0].Roles) != 1 || page.Users[1].Roles == nil || len(page.Users[1].Roles) != 0 {
		t.Fatalf("roles lo=%v mid=%v", page.Users[0].Roles, page.Users[1].Roles)
	}

	rest, err := w.service().ListUsers(context.Background(), ListUsersQuery{Limit: 2, Cursor: page.Next})
	if err != nil {
		t.Fatal(err)
	}
	if len(rest.Users) != 1 || rest.Next != nil || rest.Users[0].ID != hi {
		t.Fatalf("rest %+v next=%v", idsOf(rest.Users), rest.Next)
	}
}

func TestListUsersSeesInsertsAfterCursor(t *testing.T) {
	w := newWorld()
	t0 := time.Date(2026, 11, 13, 8, 0, 0, 0, time.UTC)
	a := seedListedUser(t, w, "a@b.c", domain.StatusActive, t0, domain.RoleCustomer)
	b := seedListedUser(t, w, "b@b.c", domain.StatusActive, t0.Add(2*time.Second), domain.RoleCustomer)
	c := seedListedUser(t, w, "c@b.c", domain.StatusActive, t0.Add(4*time.Second), domain.RoleCustomer)

	page, err := w.service().ListUsers(context.Background(), ListUsersQuery{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Users) != 2 || page.Users[0].ID != a.ID || page.Users[1].ID != b.ID || page.Next == nil {
		t.Fatalf("page1 %v next=%v", idsOf(page.Users), page.Next)
	}

	between := seedListedUser(t, w, "between@b.c", domain.StatusActive, t0.Add(3*time.Second), domain.RoleCustomer)
	before := seedListedUser(t, w, "before@b.c", domain.StatusActive, t0.Add(-time.Second), domain.RoleCustomer)

	rest, err := w.service().ListUsers(context.Background(), ListUsersQuery{Limit: 2, Cursor: page.Next})
	if err != nil {
		t.Fatal(err)
	}
	if len(rest.Users) != 2 || rest.Next != nil {
		t.Fatalf("rest %v next=%v", idsOf(rest.Users), rest.Next)
	}
	if rest.Users[0].ID != between.ID || rest.Users[1].ID != c.ID {
		t.Fatalf("keyset is not a snapshot: got %v want %s,%s", idsOf(rest.Users), between.ID, c.ID)
	}
	for _, u := range rest.Users {
		if u.ID == before.ID || u.ID == a.ID || u.ID == b.ID {
			t.Fatalf("skipped/duplicated across pages: %v", idsOf(rest.Users))
		}
	}
}

func seedListedUser(t *testing.T, w *world, email string, st domain.Status, at time.Time, roles ...domain.Role) domain.User {
	t.Helper()
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatal(err)
	}
	return seedListedUserID(t, w, id, email, st, at, roles...)
}

func seedListedUserID(t *testing.T, w *world, id uuid.UUID, email string, st domain.Status, at time.Time, roles ...domain.Role) domain.User {
	t.Helper()
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

func countOp(calls []string, op string) int {
	n := 0
	for _, c := range calls {
		if c == op {
			n++
		}
	}
	return n
}
