package postgres

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/romangergovskiy/paycore/internal/auth/application"
	"github.com/romangergovskiy/paycore/internal/auth/domain"
)

func TestAuthUsersListSQL(t *testing.T) {
	if strings.Contains(listUsersSQL, "password_hash") {
		t.Fatal("List SELECT includes password_hash")
	}
	if strings.Contains(listUsersSQL, "failed_login_count") || strings.Contains(listUsersSQL, "locked_at") {
		t.Fatal("List SELECT includes extra PII columns")
	}
	if strings.Contains(strings.ToUpper(listUsersSQL), "OFFSET") {
		t.Fatal("List uses OFFSET")
	}
	if !strings.Contains(listUsersSQL, "ORDER BY created_at ASC, id ASC") {
		t.Fatal("List sort")
	}
	if !strings.Contains(listUsersSQL, "(created_at, id) >") {
		t.Fatal("List keyset")
	}
}

func TestAuthUsersListPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	c := startCluster(t)
	if err := Bootstrap(ctx, c.admin, c.owner, c.app, c.readonly); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if err := Up(ctx, c.owner); err != nil {
		t.Fatalf("up: %v", err)
	}

	app := mustConnect(t, ctx, c.app)
	defer app.Close(ctx)

	clock := &pgClock{t: time.Date(2026, 11, 13, 8, 0, 0, 0, time.UTC)}
	users := NewAuthUsers(app)
	roles := NewAuthRoles(app)
	read := pgReadService(clock, app)

	t0 := clock.t
	a := seedAuthUser(t, ctx, app, &pgClock{t: t0}, pgEmail(t, "list-a"))
	b := seedAuthUser(t, ctx, app, &pgClock{t: t0.Add(time.Second)}, pgEmail(t, "list-b"))
	cUser := seedAuthUser(t, ctx, app, &pgClock{t: t0.Add(2 * time.Second)}, pgEmail(t, "list-c"))
	if err := roles.Grant(ctx, domain.UserRole{UserID: a.ID, Role: domain.RoleCustomer, GrantedAt: t0}); err != nil {
		t.Fatal(err)
	}
	if err := roles.Grant(ctx, domain.UserRole{UserID: a.ID, Role: domain.RoleAdmin, GrantedAt: t0}); err != nil {
		t.Fatal(err)
	}
	if err := roles.Grant(ctx, domain.UserRole{UserID: b.ID, Role: domain.RoleCustomer, GrantedAt: t0}); err != nil {
		t.Fatal(err)
	}
	if err := roles.Grant(ctx, domain.UserRole{UserID: cUser.ID, Role: domain.RoleCustomer, GrantedAt: t0}); err != nil {
		t.Fatal(err)
	}
	if err := roles.Grant(ctx, domain.UserRole{UserID: cUser.ID, Role: domain.RoleSupport, GrantedAt: t0}); err != nil {
		t.Fatal(err)
	}

	lockedAt := t0
	b.Status = domain.StatusLocked
	b.LockedAt = &lockedAt
	b.UpdatedAt = t0
	if err := users.Update(ctx, b); err != nil {
		t.Fatal(err)
	}

	t.Run("keyset Limit+1 and no password_hash", func(t *testing.T) {
		rows, err := users.List(ctx, application.ListUsersQuery{Limit: 2})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 3 {
			t.Fatalf("want Limit+1=3, got %d", len(rows))
		}
		if rows[0].ID != a.ID || rows[1].ID != b.ID || rows[2].ID != cUser.ID {
			t.Fatalf("order %s %s %s", rows[0].ID, rows[1].ID, rows[2].ID)
		}
		for _, u := range rows {
			if u.PasswordHash != "" {
				t.Fatalf("password_hash leaked: %+v", u)
			}
			if u.FailedLoginCount != 0 || u.LockedAt != nil {
				t.Fatalf("extra columns: %+v", u)
			}
		}

		page, err := read.ListUsers(ctx, application.ListUsersQuery{Limit: 2})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Users) != 2 || page.Next == nil || page.Next.ID != b.ID {
			t.Fatalf("page ids=%v next=%v", []string{page.Users[0].ID.String(), page.Users[1].ID.String()}, page.Next)
		}
		if !containsRole(page.Users[0].Roles, domain.RoleAdmin) || !containsRole(page.Users[0].Roles, domain.RoleCustomer) {
			t.Fatalf("a roles %v", page.Users[0].Roles)
		}

		rest, err := read.ListUsers(ctx, application.ListUsersQuery{Limit: 2, Cursor: page.Next})
		if err != nil {
			t.Fatal(err)
		}
		if len(rest.Users) != 1 || rest.Next != nil || rest.Users[0].ID != cUser.ID {
			t.Fatalf("rest %+v next=%v", rest.Users, rest.Next)
		}
		if !containsRole(rest.Users[0].Roles, domain.RoleSupport) {
			t.Fatalf("c roles %v", rest.Users[0].Roles)
		}
	})

	t.Run("status filter", func(t *testing.T) {
		locked := domain.StatusLocked
		page, err := read.ListUsers(ctx, application.ListUsersQuery{Limit: 50, Status: &locked})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Users) != 1 || page.Users[0].ID != b.ID || page.Users[0].Status != domain.StatusLocked {
			t.Fatalf("%+v", page)
		}
	})

	t.Run("does not write ledger", func(t *testing.T) {
		var n int
		if err := app.QueryRow(ctx, `SELECT count(*) FROM ledger.journal_transactions`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("journal_transactions %d", n)
		}
	})
}
