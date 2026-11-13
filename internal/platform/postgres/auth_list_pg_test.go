package postgres

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

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
	if strings.Contains(strings.ToLower(listUsersSQL), "join") {
		t.Fatal("List JOIN would duplicate users with several roles")
	}
}

func TestAuthRolesListByIDsSQL(t *testing.T) {
	if !strings.Contains(listRolesByIDsSQL, "user_id = ANY($1::uuid[])") {
		t.Fatal("batch roles must use ANY($ids)")
	}
	if strings.Contains(strings.ToLower(listRolesByIDsSQL), "join") {
		t.Fatal("roles batch must not JOIN users")
	}
	if strings.Contains(listRolesByIDsSQL, "password_hash") {
		t.Fatal("roles SELECT includes password_hash")
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

	none := seedAuthUser(t, ctx, app, &pgClock{t: t0.Add(3 * time.Second)}, pgEmail(t, "list-none"))

	t.Run("batch roles no-role and multi-role", func(t *testing.T) {
		empty, err := roles.ListByIDs(ctx, nil)
		if err != nil || empty == nil || len(empty) != 0 {
			t.Fatalf("empty ids: %v %#v", err, empty)
		}
		empty, err = roles.ListByIDs(ctx, []uuid.UUID{})
		if err != nil || empty == nil || len(empty) != 0 {
			t.Fatalf("empty slice: %v %#v", err, empty)
		}

		got, err := roles.ListByIDs(ctx, []uuid.UUID{none.ID, a.ID, cUser.ID, uuid.MustParse("ffffffff-ffff-4fff-bfff-ffffffffffff")})
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := got[none.ID]; ok {
			t.Fatalf("no-role user in map: %v", got[none.ID])
		}
		if len(got[a.ID]) != 2 || got[a.ID][0] != domain.RoleAdmin || got[a.ID][1] != domain.RoleCustomer {
			t.Fatalf("a roles %v", got[a.ID])
		}
		if len(got[cUser.ID]) != 2 || got[cUser.ID][0] != domain.RoleCustomer || got[cUser.ID][1] != domain.RoleSupport {
			t.Fatalf("c roles %v", got[cUser.ID])
		}

		page, err := read.ListUsers(ctx, application.ListUsersQuery{Limit: 50})
		if err != nil {
			t.Fatal(err)
		}
		var seenNone, seenA bool
		for _, u := range page.Users {
			if u.ID == none.ID {
				seenNone = true
				if u.Roles == nil || len(u.Roles) != 0 {
					t.Fatalf("no-role public roles %#v", u.Roles)
				}
			}
			if u.ID == a.ID {
				seenA = true
				if len(u.Roles) != 2 {
					t.Fatalf("multi-role user duplicated or truncated: %+v", u)
				}
			}
		}
		if !seenNone || !seenA {
			t.Fatalf("missing users in page of %d", len(page.Users))
		}
	})

	t.Run("equal created_at keyset", func(t *testing.T) {
		at := t0.Add(10 * time.Second)
		lo := uuid.MustParse("00000000-0000-4000-8000-0000000000aa")
		mid := uuid.MustParse("00000000-0000-4000-8000-0000000000bb")
		hi := uuid.MustParse("00000000-0000-4000-8000-0000000000cc")
		seedAuthUserID(t, ctx, users, lo, pgEmail(t, "eq-lo"), at)
		seedAuthUserID(t, ctx, users, mid, pgEmail(t, "eq-mid"), at)
		seedAuthUserID(t, ctx, users, hi, pgEmail(t, "eq-hi"), at)
		if err := roles.Grant(ctx, domain.UserRole{UserID: lo, Role: domain.RoleSupport, GrantedAt: at}); err != nil {
			t.Fatal(err)
		}
		if err := roles.Grant(ctx, domain.UserRole{UserID: lo, Role: domain.RoleCustomer, GrantedAt: at}); err != nil {
			t.Fatal(err)
		}

		cur := &application.UserCursor{CreatedAt: none.CreatedAt, ID: none.ID}
		page, err := read.ListUsers(ctx, application.ListUsersQuery{Limit: 2, Cursor: cur})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Users) != 2 || page.Next == nil {
			t.Fatalf("page ids=%v next=%v", publicIDs(page.Users), page.Next)
		}
		if page.Users[0].ID != lo || page.Users[1].ID != mid {
			t.Fatalf("tie-break %v", publicIDs(page.Users))
		}
		if page.Next.ID != mid || !page.Next.CreatedAt.Equal(at) {
			t.Fatalf("cursor from extra row? %+v", page.Next)
		}
		if len(page.Users[0].Roles) != 2 || page.Users[0].Roles[0] != domain.RoleCustomer || page.Users[0].Roles[1] != domain.RoleSupport {
			t.Fatalf("lo roles %v", page.Users[0].Roles)
		}
		if page.Users[1].Roles == nil || len(page.Users[1].Roles) != 0 {
			t.Fatalf("mid roles %#v", page.Users[1].Roles)
		}

		rest, err := read.ListUsers(ctx, application.ListUsersQuery{Limit: 2, Cursor: page.Next})
		if err != nil {
			t.Fatal(err)
		}
		if len(rest.Users) != 1 || rest.Next != nil || rest.Users[0].ID != hi {
			t.Fatalf("rest %v next=%v", publicIDs(rest.Users), rest.Next)
		}
	})

	t.Run("insert after cursor is visible", func(t *testing.T) {
		base := t0.Add(20 * time.Second)
		x := seedAuthUser(t, ctx, app, &pgClock{t: base}, pgEmail(t, "ks-x"))
		y := seedAuthUser(t, ctx, app, &pgClock{t: base.Add(2 * time.Second)}, pgEmail(t, "ks-y"))
		z := seedAuthUser(t, ctx, app, &pgClock{t: base.Add(4 * time.Second)}, pgEmail(t, "ks-z"))
		cur := &application.UserCursor{CreatedAt: t0.Add(10 * time.Second), ID: uuid.MustParse("00000000-0000-4000-8000-0000000000cc")}
		page, err := read.ListUsers(ctx, application.ListUsersQuery{Limit: 2, Cursor: cur})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Users) != 2 || page.Users[0].ID != x.ID || page.Users[1].ID != y.ID || page.Next == nil {
			t.Fatalf("page1 %v next=%v", publicIDs(page.Users), page.Next)
		}

		between := seedAuthUser(t, ctx, app, &pgClock{t: base.Add(3 * time.Second)}, pgEmail(t, "ks-between"))
		before := seedAuthUser(t, ctx, app, &pgClock{t: base.Add(-time.Second)}, pgEmail(t, "ks-before"))

		rest, err := read.ListUsers(ctx, application.ListUsersQuery{Limit: 2, Cursor: page.Next})
		if err != nil {
			t.Fatal(err)
		}
		if len(rest.Users) != 2 || rest.Next != nil {
			t.Fatalf("rest %v next=%v", publicIDs(rest.Users), rest.Next)
		}
		if rest.Users[0].ID != between.ID || rest.Users[1].ID != z.ID {
			t.Fatalf("not a snapshot: got %v want %s,%s", publicIDs(rest.Users), between.ID, z.ID)
		}
		for _, u := range rest.Users {
			if u.ID == before.ID || u.ID == x.ID || u.ID == y.ID {
				t.Fatalf("skip/dup across pages: %v", publicIDs(rest.Users))
			}
		}
	})

	t.Run("explain and indexes", func(t *testing.T) {
		idx := listAuthIndexes(t, ctx, app)
		t.Logf("auth indexes:\n%s", idx)
		if !strings.Contains(idx, "users_pkey") {
			t.Fatal("missing users_pkey")
		}
		if !strings.Contains(idx, "user_roles_pkey") {
			t.Fatal("missing user_roles_pkey")
		}

		usersPlan := explainSQL(t, ctx, app, listUsersSQL, nil, nil, nil, 3)
		t.Logf("users list EXPLAIN (ANALYZE, BUFFERS):\n%s", usersPlan)
		rolesPlan := explainSQL(t, ctx, app, listRolesByIDsSQL, []uuid.UUID{a.ID, b.ID, cUser.ID, none.ID})
		t.Logf("roles ListByIDs EXPLAIN (ANALYZE, BUFFERS):\n%s", rolesPlan)
		if strings.Contains(usersPlan, "password_hash") {
			t.Fatal("EXPLAIN mentions password_hash")
		}
	})
}

func seedAuthUserID(t *testing.T, ctx context.Context, users *AuthUsers, id uuid.UUID, email string, at time.Time) domain.User {
	t.Helper()
	u := domain.User{
		ID:           id,
		Email:        email,
		PasswordHash: testArgon2idPHC,
		Status:       domain.StatusActive,
		CreatedAt:    at,
		UpdatedAt:    at,
	}
	if err := users.Create(ctx, u); err != nil {
		t.Fatal(err)
	}
	return u
}

func publicIDs(users []domain.PublicUser) []uuid.UUID {
	out := make([]uuid.UUID, len(users))
	for i, u := range users {
		out[i] = u.ID
	}
	return out
}

func explainSQL(t *testing.T, ctx context.Context, conn *pgx.Conn, sql string, args ...any) string {
	t.Helper()
	rows, err := conn.Query(ctx, "EXPLAIN (ANALYZE, BUFFERS) "+sql, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func listAuthIndexes(t *testing.T, ctx context.Context, conn *pgx.Conn) string {
	t.Helper()
	rows, err := conn.Query(ctx, `
SELECT tablename, indexname, indexdef
  FROM pg_indexes
 WHERE schemaname = 'auth' AND tablename IN ('users', 'user_roles')
 ORDER BY tablename, indexname`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var table, name, def string
		if err := rows.Scan(&table, &name, &def); err != nil {
			t.Fatal(err)
		}
		b.WriteString(table)
		b.WriteByte('\t')
		b.WriteString(name)
		b.WriteByte('\t')
		b.WriteString(def)
		b.WriteByte('\n')
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return b.String()
}
