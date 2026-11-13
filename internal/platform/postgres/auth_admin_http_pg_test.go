package postgres

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/romangergovskiy/paycore/internal/auth/application"
	"github.com/romangergovskiy/paycore/internal/auth/domain"
	authhttp "github.com/romangergovskiy/paycore/internal/auth/http"
	"github.com/romangergovskiy/paycore/internal/platform/authcrypto"
	"github.com/romangergovskiy/paycore/internal/platform/config"
	"github.com/romangergovskiy/paycore/internal/platform/httpserver"
)

const adminHTTPReason = `{"reason":"policy-violation"}`

var adminHTTPOps = []struct {
	name, method, path, body string
}{
	{"list", http.MethodGet, "/api/v1/admin/users", ""},
	{"grant", http.MethodPost, "/api/v1/admin/users/{id}/roles", `{"role":"SUPPORT"}`},
	{"revoke-role", http.MethodPost, "/api/v1/admin/users/{id}/roles/revoke", `{"role":"SUPPORT"}`},
	{"lock", http.MethodPost, "/api/v1/admin/users/{id}/lock", adminHTTPReason},
	{"unlock", http.MethodPost, "/api/v1/admin/users/{id}/unlock", adminHTTPReason},
	{"disable", http.MethodPost, "/api/v1/admin/users/{id}/disable", adminHTTPReason},
	{"enable", http.MethodPost, "/api/v1/admin/users/{id}/enable", adminHTTPReason},
}

func adminOpPath(path string, id uuid.UUID) string {
	return strings.ReplaceAll(path, "{id}", id.String())
}

// TestAdminHTTPPostgres walks every Admin HTTP route through production
// Compose: HTTP → BearerAuth → RequireRole(ADMIN) → handler → application
// *In → Auth UoW → PostgreSQL (paycore_app) → HTTP. Argon2id, HS256, and
// OpaqueRefresh are the production types. No mocks of PG, no t.Skip, no
// time.Sleep. Admin writes use NewAuthUoW, not LogoutRefreshRetry.
func TestAdminHTTPPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	c := startCluster(t)
	if err := Bootstrap(ctx, c.admin, c.owner, c.app, c.readonly); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if err := Up(ctx, c.owner); err != nil {
		t.Fatalf("up: %v", err)
	}

	pool, err := Open(ctx, config.Postgres{
		DSN:              c.app,
		MaxConns:         12,
		MinConns:         2,
		MaxConnLifetime:  time.Minute,
		MaxConnIdleTime:  time.Minute,
		LockTimeout:      8 * time.Second,
		StatementTimeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	conn := mustConnect(t, ctx, c.app)
	t.Cleanup(func() { _ = conn.Close(context.Background()) })

	env := newAdminHTTPEnv(t, ctx, pool, conn)
	ledgerBefore := ledgerCounts(t, ctx, conn)

	adminEmail := pgEmail(t, "adm-http")
	env.trackSecret(httpPGPassword)
	created, err := application.BootstrapFirstAdminIn(ctx, env.uow, env.write, adminEmail, httpPGPassword)
	if err != nil {
		t.Fatalf("bootstrap-admin: %v", err)
	}
	if created.ID == uuid.Nil {
		t.Fatal("bootstrap-admin returned empty user")
	}
	adminCred := env.mustLogin(adminEmail, httpPGPassword)
	adminID := adminCred.User.ID
	adminAuth := "Bearer " + adminCred.AccessToken

	t.Run("unauthenticated each admin endpoint", func(t *testing.T) {
		env.t = t
		vid := env.mustRegister(pgEmail(t, "unauth-t")).ID
		before := env.userSnapshot(vid)
		for _, op := range adminHTTPOps {
			rec := env.do(op.method, adminOpPath(op.path, vid), op.body, "", nextTestIP())
			assertAuthError(t, rec, http.StatusUnauthorized, domain.CodeInvalidToken)
			env.assertNoSecrets(rec.Body.String())
		}
		env.assertUserUnchanged(vid, before)
		missing := env.do(http.MethodGet, testAdminPath, "", adminAuth, nextTestIP())
		if missing.Code != http.StatusNotFound {
			t.Fatalf("production mux mounted __test/require-admin: %d %s", missing.Code, missing.Body)
		}
	})

	t.Run("CUSTOMER 403 each admin endpoint", func(t *testing.T) {
		env.t = t
		email := pgEmail(t, "cust-403")
		cust := env.mustRegister(email)
		tok := env.mustLogin(email, httpPGPassword).AccessToken
		before := env.userSnapshot(cust.ID)
		for _, op := range adminHTTPOps {
			rec := env.do(op.method, adminOpPath(op.path, cust.ID), op.body, "Bearer "+tok, nextTestIP())
			assertAuthError(t, rec, http.StatusForbidden, domain.CodeForbidden)
			if rec.Header().Get("WWW-Authenticate") != "" {
				t.Errorf("403 sent WWW-Authenticate on %s", op.name)
			}
			env.assertNoSecrets(rec.Body.String())
		}
		env.assertUserUnchanged(cust.ID, before)
	})

	t.Run("empty list", func(t *testing.T) {
		env.t = t
		rec := env.do(http.MethodGet, "/api/v1/admin/users?status=LOCKED&limit=50", "", adminAuth, nextTestIP())
		page := decodeAdminList(t, rec)
		if len(page.Users) != 0 {
			t.Fatalf("want empty LOCKED list, got %+v", page.Users)
		}
		if page.NextCursor != "" {
			t.Fatalf("next_cursor on empty page: %s", rec.Body)
		}
		if !strings.Contains(rec.Body.String(), `"users":[]`) && !strings.Contains(rec.Body.String(), `"users": []`) {
			t.Fatalf("users must be []: %s", rec.Body)
		}
		assertAdminListShape(t, rec.Body.String())
	})

	t.Run("ADMIN live session 2xx", func(t *testing.T) {
		env.t = t
		email := pgEmail(t, "adm-2xx")
		victim := env.mustRegister(email)
		list := env.do(http.MethodGet, "/api/v1/admin/users?limit=50", "", adminAuth, nextTestIP())
		if list.Code != http.StatusOK {
			t.Fatalf("list = %d %s", list.Code, list.Body)
		}
		assertAdminListShape(t, list.Body.String())

		grant := env.do(http.MethodPost, "/api/v1/admin/users/"+victim.ID.String()+"/roles", `{"role":"SUPPORT"}`, adminAuth, nextTestIP())
		if grant.Code != http.StatusNoContent || grant.Body.Len() != 0 {
			t.Fatalf("grant = %d %q", grant.Code, grant.Body)
		}
		if !containsRole(env.userRoles(victim.ID), domain.RoleSupport) {
			t.Fatal("SUPPORT not persisted")
		}

		revoke := env.do(http.MethodPost, "/api/v1/admin/users/"+victim.ID.String()+"/roles/revoke", `{"role":"SUPPORT"}`, adminAuth, nextTestIP())
		if revoke.Code != http.StatusNoContent {
			t.Fatalf("revoke = %d %s", revoke.Code, revoke.Body)
		}
		if containsRole(env.userRoles(victim.ID), domain.RoleSupport) {
			t.Fatal("SUPPORT still present")
		}

		lock := env.do(http.MethodPost, "/api/v1/admin/users/"+victim.ID.String()+"/lock", adminHTTPReason, adminAuth, nextTestIP())
		if lock.Code != http.StatusNoContent {
			t.Fatalf("lock = %d %s", lock.Code, lock.Body)
		}
		unlock := env.do(http.MethodPost, "/api/v1/admin/users/"+victim.ID.String()+"/unlock", adminHTTPReason, adminAuth, nextTestIP())
		if unlock.Code != http.StatusNoContent {
			t.Fatalf("unlock = %d %s", unlock.Code, unlock.Body)
		}
		disable := env.do(http.MethodPost, "/api/v1/admin/users/"+victim.ID.String()+"/disable", adminHTTPReason, adminAuth, nextTestIP())
		if disable.Code != http.StatusNoContent {
			t.Fatalf("disable = %d %s", disable.Code, disable.Body)
		}
		enable := env.do(http.MethodPost, "/api/v1/admin/users/"+victim.ID.String()+"/enable", adminHTTPReason, adminAuth, nextTestIP())
		if enable.Code != http.StatusNoContent {
			t.Fatalf("enable = %d %s", enable.Code, enable.Body)
		}
		if env.userStatus(victim.ID) != domain.StatusActive {
			t.Fatalf("status after enable = %s", env.userStatus(victim.ID))
		}
		env.assertLogsClean()
	})

	t.Run("JWT.roles is not authorization", func(t *testing.T) {
		env.t = t
		email := pgEmail(t, "jwt-roles")
		env.mustRegister(email)
		cred := env.mustLogin(email, httpPGPassword)
		parsed, err := env.signer.Parse(cred.AccessToken)
		if err != nil {
			t.Fatal(err)
		}
		planted, err := env.signer.Sign(domain.Principal{
			UserID:    parsed.UserID,
			SessionID: parsed.SessionID,
			Roles:     []domain.Role{domain.RoleAdmin, domain.RoleCustomer},
		}, time.Now().UTC(), time.Now().UTC().Add(domain.AccessTTL))
		if err != nil {
			t.Fatal(err)
		}
		env.trackSecret(planted)
		before := env.userSnapshot(parsed.UserID)
		rec := env.do(http.MethodGet, "/api/v1/admin/users", "", "Bearer "+planted, nextTestIP())
		assertAuthError(t, rec, http.StatusForbidden, domain.CodeForbidden)
		if containsRole(env.userRoles(parsed.UserID), domain.RoleAdmin) {
			t.Fatal("store gained ADMIN from JWT claims")
		}
		env.assertUserUnchanged(parsed.UserID, before)
	})

	t.Run("revoked session 401 while JWT unexpired", func(t *testing.T) {
		env.t = t
		email := pgEmail(t, "revoked-sess")
		u := env.mustRegister(email)
		env.mustGrant(adminAuth, u.ID, domain.RoleAdmin)
		cred := env.mustLogin(email, httpPGPassword)
		ok := env.do(http.MethodGet, "/api/v1/admin/users", "", "Bearer "+cred.AccessToken, nextTestIP())
		if ok.Code != http.StatusOK {
			t.Fatalf("live admin = %d %s", ok.Code, ok.Body)
		}
		parsed, err := env.signer.Parse(cred.AccessToken)
		if err != nil {
			t.Fatal(err)
		}
		out := env.do(http.MethodPost, "/api/v1/auth/logout", "", "Bearer "+cred.AccessToken, nextTestIP())
		if out.Code != http.StatusNoContent {
			t.Fatalf("logout = %d %s", out.Code, out.Body)
		}
		assertSessionRevoked(t, ctx, conn, parsed.SessionID, domain.RevokeLogout)
		denied := env.do(http.MethodPost, "/api/v1/admin/users/"+u.ID.String()+"/lock", adminHTTPReason, "Bearer "+cred.AccessToken, nextTestIP())
		assertAuthError(t, denied, http.StatusUnauthorized, domain.CodeInvalidToken)
		env.mustRevokeRole(adminAuth, u.ID, domain.RoleAdmin)
	})

	t.Run("role changes apply on later requests", func(t *testing.T) {
		env.t = t
		email := pgEmail(t, "roles-later")
		u := env.mustRegister(email)
		cred := env.mustLogin(email, httpPGPassword)
		denied := env.do(http.MethodGet, "/api/v1/admin/users", "", "Bearer "+cred.AccessToken, nextTestIP())
		assertAuthError(t, denied, http.StatusForbidden, domain.CodeForbidden)

		env.mustGrant(adminAuth, u.ID, domain.RoleAdmin)
		if !containsRole(env.userRoles(u.ID), domain.RoleAdmin) {
			t.Fatal("ADMIN grant not in DB")
		}
		allowed := env.do(http.MethodGet, "/api/v1/admin/users", "", "Bearer "+cred.AccessToken, nextTestIP())
		if allowed.Code != http.StatusOK {
			t.Fatalf("after grant = %d %s", allowed.Code, allowed.Body)
		}

		env.mustRevokeRole(adminAuth, u.ID, domain.RoleAdmin)
		again := env.do(http.MethodGet, "/api/v1/admin/users", "", "Bearer "+cred.AccessToken, nextTestIP())
		assertAuthError(t, again, http.StatusForbidden, domain.CodeForbidden)
		if containsRole(env.userRoles(u.ID), domain.RoleAdmin) {
			t.Fatal("ADMIN still in DB")
		}
	})

	t.Run("roles grant revoke regrant missing atomic", func(t *testing.T) {
		env.t = t
		email := pgEmail(t, "roles-crud")
		u := env.mustRegister(email)
		beforeRoles := env.userRoles(u.ID)

		grant := env.do(http.MethodPost, "/api/v1/admin/users/"+u.ID.String()+"/roles", `{"role":"SUPPORT"}`, adminAuth, nextTestIP())
		if grant.Code != http.StatusNoContent {
			t.Fatalf("grant = %d %s", grant.Code, grant.Body)
		}
		got := env.userRoles(u.ID)
		if !containsRole(got, domain.RoleSupport) || !containsRole(got, domain.RoleCustomer) {
			t.Fatalf("after grant %v", got)
		}

		dup := env.do(http.MethodPost, "/api/v1/admin/users/"+u.ID.String()+"/roles", `{"role":"SUPPORT"}`, adminAuth, nextTestIP())
		assertAuthError(t, dup, http.StatusBadRequest, domain.CodeInvalidRequest)
		if roleCount(env.userRoles(u.ID), domain.RoleSupport) != 1 {
			t.Fatalf("duplicate grant wrote extra row: %v", env.userRoles(u.ID))
		}

		rev := env.do(http.MethodPost, "/api/v1/admin/users/"+u.ID.String()+"/roles/revoke", `{"role":"SUPPORT"}`, adminAuth, nextTestIP())
		if rev.Code != http.StatusNoContent {
			t.Fatalf("revoke = %d %s", rev.Code, rev.Body)
		}
		if containsRole(env.userRoles(u.ID), domain.RoleSupport) {
			t.Fatal("SUPPORT remained")
		}

		missing := env.do(http.MethodPost, "/api/v1/admin/users/"+u.ID.String()+"/roles/revoke", `{"role":"SUPPORT"}`, adminAuth, nextTestIP())
		assertAuthError(t, missing, http.StatusBadRequest, domain.CodeInvalidRequest)
		if containsRole(env.userRoles(u.ID), domain.RoleSupport) {
			t.Fatal("missing revoke inserted SUPPORT")
		}

		unknown := env.do(http.MethodPost, "/api/v1/admin/users/"+u.ID.String()+"/roles", `{"role":"ROOT"}`, adminAuth, nextTestIP())
		assertAuthError(t, unknown, http.StatusBadRequest, domain.CodeInvalidRequest)
		if !rolesEqual(env.userRoles(u.ID), beforeRoles) {
			t.Fatalf("unknown role mutated: %v want %v", env.userRoles(u.ID), beforeRoles)
		}
	})

	t.Run("last ACTIVE ADMIN cannot self-revoke or lock", func(t *testing.T) {
		env.t = t
		if n := countActiveAdmins(t, ctx, conn); n != 1 {
			t.Fatalf("want exactly 1 ACTIVE ADMIN before self tests, got %d", n)
		}
		before := env.userSnapshot(adminID)
		selfRev := env.do(http.MethodPost, "/api/v1/admin/users/"+adminID.String()+"/roles/revoke", `{"role":"ADMIN"}`, adminAuth, nextTestIP())
		assertAuthError(t, selfRev, http.StatusForbidden, domain.CodeForbidden)
		if !containsRole(env.userRoles(adminID), domain.RoleAdmin) {
			t.Fatal("self-revoke dropped ADMIN")
		}

		selfLock := env.do(http.MethodPost, "/api/v1/admin/users/"+adminID.String()+"/lock", adminHTTPReason, adminAuth, nextTestIP())
		assertAuthError(t, selfLock, http.StatusBadRequest, domain.CodeInvalidRequest)
		selfDis := env.do(http.MethodPost, "/api/v1/admin/users/"+adminID.String()+"/disable", adminHTTPReason, adminAuth, nextTestIP())
		assertAuthError(t, selfDis, http.StatusBadRequest, domain.CodeInvalidRequest)
		env.assertUserUnchanged(adminID, before)
		if countActiveAdmins(t, ctx, conn) != 1 {
			t.Fatal("last ADMIN invariant broken")
		}
	})

	t.Run("users list pagination roles secrets equal created_at", func(t *testing.T) {
		env.t = t
		at := time.Date(2099, 6, 1, 12, 0, 0, 0, time.UTC)
		var ids []uuid.UUID
		for i := 0; i < 4; i++ {
			u := env.mustRegister(pgEmail(t, "page-"+strconv.Itoa(i)))
			ids = append(ids, u.ID)
			env.setCreatedAt(u.ID, at.Add(time.Duration(i)*time.Second))
		}
		env.mustGrant(adminAuth, ids[0], domain.RoleSupport)
		env.mustGrant(adminAuth, ids[0], domain.RoleOperator)

		sentinel := env.createdAt(ids[0])
		cur := encodeAdminCursor(t, sentinel.Add(-time.Second), uuid.MustParse("00000000-0000-4000-8000-000000000001"))
		var seen []uuid.UUID
		path := "/api/v1/admin/users?limit=2&cursor=" + cur
		for i := 0; i < 8; i++ {
			rec := env.do(http.MethodGet, path, "", adminAuth, nextTestIP())
			page := decodeAdminList(t, rec)
			assertAdminListShape(t, rec.Body.String())
			for _, u := range page.Users {
				seen = append(seen, u.ID)
			}
			if page.NextCursor == "" {
				break
			}
			path = "/api/v1/admin/users?limit=2&cursor=" + page.NextCursor
		}
		got := idsIn(seen, ids)
		if len(got) != 4 {
			t.Fatalf("paged ids %v want 4 of %v (seen %v)", got, ids, seen)
		}
		if !uuidSeqEqual(got, ids) {
			t.Fatalf("skip/dup or order: got %v want %v", got, ids)
		}

		list0 := env.do(http.MethodGet, "/api/v1/admin/users?limit=50", "", adminAuth, nextTestIP())
		page0 := decodeAdminList(t, list0)
		var found0 bool
		for _, u := range page0.Users {
			if u.ID != ids[0] {
				continue
			}
			found0 = true
			if u.Email == "" || u.Status != domain.StatusActive {
				t.Fatalf("list item %+v", u)
			}
			if !containsRole(u.Roles, domain.RoleSupport) || !containsRole(u.Roles, domain.RoleOperator) || !containsRole(u.Roles, domain.RoleCustomer) {
				t.Fatalf("roles not loaded: %v", u.Roles)
			}
			if containsRole(u.Roles, domain.Role("password_hash")) {
				t.Fatal("secret as role")
			}
		}
		if !found0 {
			t.Fatal("multi-role user missing from list")
		}

		eq := time.Date(2099, 7, 1, 0, 0, 0, 0, time.UTC)
		var eqIDs []uuid.UUID
		for i := 0; i < 3; i++ {
			u := env.mustRegister(pgEmail(t, "eq-"+strconv.Itoa(i)))
			eqIDs = append(eqIDs, u.ID)
			env.setCreatedAt(u.ID, eq)
		}
		eqIDs = env.idsByKeyset(eqIDs)
		eqCur := encodeAdminCursor(t, eq.Add(-time.Second), uuid.MustParse("00000000-0000-4000-8000-000000000001"))
		var eqSeen []uuid.UUID
		eqPath := "/api/v1/admin/users?limit=1&cursor=" + eqCur
		for i := 0; i < 12; i++ {
			rec := env.do(http.MethodGet, eqPath, "", adminAuth, nextTestIP())
			page := decodeAdminList(t, rec)
			assertAdminListShape(t, rec.Body.String())
			for _, u := range page.Users {
				eqSeen = append(eqSeen, u.ID)
			}
			if page.NextCursor == "" {
				break
			}
			eqPath = "/api/v1/admin/users?limit=1&cursor=" + page.NextCursor
		}
		eqGot := idsIn(eqSeen, eqIDs)
		if len(eqGot) != 3 || !uuidSeqEqual(eqGot, eqIDs) {
			t.Fatalf("equal created_at skip/dup: got %v want %v (seen %v)", eqGot, eqIDs, eqSeen)
		}
		env.mustRevokeRole(adminAuth, ids[0], domain.RoleSupport)
		env.mustRevokeRole(adminAuth, ids[0], domain.RoleOperator)
	})

	t.Run("status lock unlock disable enable and illegal", func(t *testing.T) {
		env.t = t
		email := pgEmail(t, "status-life")
		u := env.mustRegister(email)
		cred := env.mustLogin(email, httpPGPassword)
		me := env.do(http.MethodGet, "/api/v1/auth/me", "", "Bearer "+cred.AccessToken, nextTestIP())
		if me.Code != http.StatusOK {
			t.Fatalf("me before lock = %d %s", me.Code, me.Body)
		}

		hist0 := env.historyCount(u.ID)
		lock := env.do(http.MethodPost, "/api/v1/admin/users/"+u.ID.String()+"/lock", adminHTTPReason, adminAuth, nextTestIP())
		if lock.Code != http.StatusNoContent {
			t.Fatalf("lock = %d %s", lock.Code, lock.Body)
		}
		if env.userStatus(u.ID) != domain.StatusLocked {
			t.Fatalf("status = %s", env.userStatus(u.ID))
		}
		if env.historyCount(u.ID) != hist0+1 {
			t.Fatalf("lock history %d → %d", hist0, env.historyCount(u.ID))
		}
		if env.liveSessions(u.ID) != 0 {
			t.Fatal("lock left live sessions")
		}
		denied := env.do(http.MethodGet, "/api/v1/auth/me", "", "Bearer "+cred.AccessToken, nextTestIP())
		assertAuthError(t, denied, http.StatusUnauthorized, domain.CodeInvalidToken)
		loginLocked := env.do(http.MethodPost, "/api/v1/auth/login", loginBody(email, httpPGPassword), "", nextTestIP())
		assertAuthError(t, loginLocked, http.StatusUnauthorized, domain.CodeInvalidCredentials)

		enableLocked := env.do(http.MethodPost, "/api/v1/admin/users/"+u.ID.String()+"/enable", adminHTTPReason, adminAuth, nextTestIP())
		assertAuthError(t, enableLocked, http.StatusBadRequest, domain.CodeInvalidRequest)
		if env.userStatus(u.ID) != domain.StatusLocked {
			t.Fatalf("enable LOCKED mutated: %s", env.userStatus(u.ID))
		}

		unlock := env.do(http.MethodPost, "/api/v1/admin/users/"+u.ID.String()+"/unlock", adminHTTPReason, adminAuth, nextTestIP())
		if unlock.Code != http.StatusNoContent {
			t.Fatalf("unlock = %d %s", unlock.Code, unlock.Body)
		}
		if env.userStatus(u.ID) != domain.StatusActive {
			t.Fatalf("after unlock %s", env.userStatus(u.ID))
		}
		histUnlock := env.historyCount(u.ID)
		unlockAgain := env.do(http.MethodPost, "/api/v1/admin/users/"+u.ID.String()+"/unlock", adminHTTPReason, adminAuth, nextTestIP())
		if unlockAgain.Code != http.StatusNoContent {
			t.Fatalf("unlock ACTIVE = %d %s", unlockAgain.Code, unlockAgain.Body)
		}
		if env.historyCount(u.ID) != histUnlock {
			t.Fatal("ACTIVE unlock wrote history")
		}
		relogin := env.mustLogin(email, httpPGPassword)

		disable := env.do(http.MethodPost, "/api/v1/admin/users/"+u.ID.String()+"/disable", adminHTTPReason, adminAuth, nextTestIP())
		if disable.Code != http.StatusNoContent {
			t.Fatalf("disable = %d %s", disable.Code, disable.Body)
		}
		if env.userStatus(u.ID) != domain.StatusDisabled {
			t.Fatalf("after disable %s", env.userStatus(u.ID))
		}
		disabledMe := env.do(http.MethodGet, "/api/v1/auth/me", "", "Bearer "+relogin.AccessToken, nextTestIP())
		assertAuthError(t, disabledMe, http.StatusUnauthorized, domain.CodeInvalidToken)
		loginOff := env.do(http.MethodPost, "/api/v1/auth/login", loginBody(email, httpPGPassword), "", nextTestIP())
		assertAuthError(t, loginOff, http.StatusUnauthorized, domain.CodeInvalidCredentials)

		unlockOff := env.do(http.MethodPost, "/api/v1/admin/users/"+u.ID.String()+"/unlock", adminHTTPReason, adminAuth, nextTestIP())
		assertAuthError(t, unlockOff, http.StatusBadRequest, domain.CodeInvalidRequest)
		lockOff := env.do(http.MethodPost, "/api/v1/admin/users/"+u.ID.String()+"/lock", adminHTTPReason, adminAuth, nextTestIP())
		assertAuthError(t, lockOff, http.StatusBadRequest, domain.CodeInvalidRequest)
		if env.userStatus(u.ID) != domain.StatusDisabled {
			t.Fatalf("illegal transition moved DISABLED: %s", env.userStatus(u.ID))
		}

		histDis := env.historyCount(u.ID)
		enable := env.do(http.MethodPost, "/api/v1/admin/users/"+u.ID.String()+"/enable", adminHTTPReason, adminAuth, nextTestIP())
		if enable.Code != http.StatusNoContent {
			t.Fatalf("enable = %d %s", enable.Code, enable.Body)
		}
		if env.userStatus(u.ID) != domain.StatusActive {
			t.Fatalf("after enable %s", env.userStatus(u.ID))
		}
		if env.historyCount(u.ID) != histDis+1 {
			t.Fatal("enable must write history on DISABLED→ACTIVE")
		}
		histEn := env.historyCount(u.ID)
		enableAgain := env.do(http.MethodPost, "/api/v1/admin/users/"+u.ID.String()+"/enable", adminHTTPReason, adminAuth, nextTestIP())
		if enableAgain.Code != http.StatusNoContent {
			t.Fatalf("enable ACTIVE = %d %s", enableAgain.Code, enableAgain.Body)
		}
		if env.historyCount(u.ID) != histEn {
			t.Fatal("ACTIVE enable wrote history")
		}
		after := env.mustLogin(email, httpPGPassword)
		meOK := env.do(http.MethodGet, "/api/v1/auth/me", "", "Bearer "+after.AccessToken, nextTestIP())
		if meOK.Code != http.StatusOK {
			t.Fatalf("me after enable = %d %s", meOK.Code, meOK.Body)
		}
	})

	t.Run("failed use case leaves no partial writes", func(t *testing.T) {
		env.t = t
		email := pgEmail(t, "no-partial")
		u := env.mustRegister(email)
		before := env.userSnapshot(u.ID)

		badUUID := env.do(http.MethodPost, "/api/v1/admin/users/not-a-uuid/lock", adminHTTPReason, adminAuth, nextTestIP())
		assertAuthError(t, badUUID, http.StatusBadRequest, domain.CodeInvalidRequest)

		missing := env.do(http.MethodPost, "/api/v1/admin/users/"+uuid.Must(uuid.NewV7()).String()+"/lock", adminHTTPReason, adminAuth, nextTestIP())
		assertAuthError(t, missing, http.StatusNotFound, domain.CodeNotFound)

		emptyReason := env.do(http.MethodPost, "/api/v1/admin/users/"+u.ID.String()+"/lock", `{"reason":""}`, adminAuth, nextTestIP())
		assertAuthError(t, emptyReason, http.StatusBadRequest, domain.CodeInvalidRequest)
		env.assertUserUnchanged(u.ID, before)

		last := env.userSnapshot(adminID)
		self := env.do(http.MethodPost, "/api/v1/admin/users/"+adminID.String()+"/lock", adminHTTPReason, adminAuth, nextTestIP())
		assertAuthError(t, self, http.StatusBadRequest, domain.CodeInvalidRequest)
		env.assertUserUnchanged(adminID, last)
		if countActiveAdmins(t, ctx, conn) < 1 {
			t.Fatal("last ADMIN lost")
		}
	})

	t.Run("concurrent ADMIN via existing advisory lock", func(t *testing.T) {
		env.t = t
		email := pgEmail(t, "adv-wait")
		victim := env.mustRegister(email)
		before := env.userSnapshot(victim.ID)

		holder := mustConnect(t, ctx, c.app)
		defer holder.Close(ctx)
		held := beginAdminAdvisory(t, ctx, holder)
		defer func() { _ = held.Rollback(ctx) }()

		done := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			done <- env.do(http.MethodPost, "/api/v1/admin/users/"+victim.ID.String()+"/lock", adminHTTPReason, adminAuth, nextTestIP())
		}()
		waitAdvisoryWaiters(t, ctx, held, 1)
		env.assertUserUnchanged(victim.ID, before)
		if err := held.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
		rec := <-done
		if rec.Code != http.StatusNoContent {
			t.Fatalf("lock after advisory release = %d %s", rec.Code, rec.Body)
		}
		if env.userStatus(victim.ID) != domain.StatusLocked {
			t.Fatalf("status = %s", env.userStatus(victim.ID))
		}
		unlock := env.do(http.MethodPost, "/api/v1/admin/users/"+victim.ID.String()+"/unlock", adminHTTPReason, adminAuth, nextTestIP())
		if unlock.Code != http.StatusNoContent {
			t.Fatalf("unlock = %d %s", unlock.Code, unlock.Body)
		}
		assertAdvisoryReleased(t, ctx, conn)
	})

	t.Run("cannot lock last ACTIVE ADMIN via a race", func(t *testing.T) {
		env.t = t
		aEmail := pgEmail(t, "race-a")
		bEmail := pgEmail(t, "race-b")
		a := env.mustRegister(aEmail)
		b := env.mustRegister(bEmail)
		env.mustGrant(adminAuth, a.ID, domain.RoleAdmin)
		env.mustGrant(adminAuth, b.ID, domain.RoleAdmin)
		aTok := env.mustLogin(aEmail, httpPGPassword).AccessToken
		bTok := env.mustLogin(bEmail, httpPGPassword).AccessToken

		strip := env.do(http.MethodPost, "/api/v1/admin/users/"+adminID.String()+"/roles/revoke", `{"role":"ADMIN"}`, "Bearer "+aTok, nextTestIP())
		if strip.Code != http.StatusNoContent {
			t.Fatalf("strip fixture ADMIN = %d %s", strip.Code, strip.Body)
		}
		if countActiveAdmins(t, ctx, conn) != 2 {
			t.Fatalf("ACTIVE ADMIN = %d, want 2", countActiveAdmins(t, ctx, conn))
		}

		var ready, done sync.WaitGroup
		start := make(chan struct{})
		out := make([]*httptest.ResponseRecorder, 2)
		ready.Add(2)
		done.Add(2)
		go func() {
			defer done.Done()
			ready.Done()
			<-start
			out[0] = env.do(http.MethodPost, "/api/v1/admin/users/"+b.ID.String()+"/lock", adminHTTPReason, "Bearer "+aTok, nextTestIP())
		}()
		go func() {
			defer done.Done()
			ready.Done()
			<-start
			out[1] = env.do(http.MethodPost, "/api/v1/admin/users/"+a.ID.String()+"/lock", adminHTTPReason, "Bearer "+bTok, nextTestIP())
		}()
		ready.Wait()
		close(start)
		done.Wait()

		ok, fail := 0, 0
		for i, rec := range out {
			switch rec.Code {
			case http.StatusNoContent:
				ok++
			case http.StatusBadRequest:
				fail++
				assertAuthError(t, rec, http.StatusBadRequest, domain.CodeInvalidRequest)
			case http.StatusForbidden:
				fail++
			default:
				t.Fatalf("race %d = %d %s", i, rec.Code, rec.Body)
			}
		}
		if ok < 1 || fail < 1 {
			t.Fatalf("lock race success=%d fail=%d", ok, fail)
		}
		n := countActiveAdmins(t, ctx, conn)
		if n < 1 {
			t.Fatalf("race locked every ACTIVE ADMIN: %d", n)
		}
		sa, sb := env.userStatus(a.ID), env.userStatus(b.ID)
		locked := 0
		if sa == domain.StatusLocked {
			locked++
		}
		if sb == domain.StatusLocked {
			locked++
		}
		if locked != 1 {
			t.Fatalf("locked both or neither: a=%s b=%s", sa, sb)
		}

		survivorTok := aTok
		survivorID, victimID := a.ID, b.ID
		if sa == domain.StatusLocked {
			survivorTok = bTok
			survivorID, victimID = b.ID, a.ID
		}
		restore := env.do(http.MethodPost, "/api/v1/admin/users/"+adminID.String()+"/roles", `{"role":"ADMIN"}`, "Bearer "+survivorTok, nextTestIP())
		if restore.Code != http.StatusNoContent {
			t.Fatalf("restore fixture ADMIN = %d %s", restore.Code, restore.Body)
		}
		unlock := env.do(http.MethodPost, "/api/v1/admin/users/"+victimID.String()+"/unlock", adminHTTPReason, adminAuth, nextTestIP())
		if unlock.Code != http.StatusNoContent {
			t.Fatalf("unlock loser = %d %s", unlock.Code, unlock.Body)
		}
		env.mustRevokeRole(adminAuth, survivorID, domain.RoleAdmin)
		env.mustRevokeRole(adminAuth, victimID, domain.RoleAdmin)
		if countActiveAdmins(t, ctx, conn) != 1 {
			t.Fatalf("cleanup ACTIVE ADMIN = %d", countActiveAdmins(t, ctx, conn))
		}
	})

	t.Run("cannot disable or revoke last ACTIVE ADMIN via a race", func(t *testing.T) {
		env.t = t
		aEmail := pgEmail(t, "race-dis-a")
		bEmail := pgEmail(t, "race-dis-b")
		a := env.mustRegister(aEmail)
		b := env.mustRegister(bEmail)
		env.mustGrant(adminAuth, a.ID, domain.RoleAdmin)
		env.mustGrant(adminAuth, b.ID, domain.RoleAdmin)
		aTok := env.mustLogin(aEmail, httpPGPassword).AccessToken
		bTok := env.mustLogin(bEmail, httpPGPassword).AccessToken
		strip := env.do(http.MethodPost, "/api/v1/admin/users/"+adminID.String()+"/roles/revoke", `{"role":"ADMIN"}`, "Bearer "+aTok, nextTestIP())
		if strip.Code != http.StatusNoContent {
			t.Fatalf("strip = %d %s", strip.Code, strip.Body)
		}
		if countActiveAdmins(t, ctx, conn) != 2 {
			t.Fatalf("ACTIVE ADMIN = %d, want 2", countActiveAdmins(t, ctx, conn))
		}

		var ready, done sync.WaitGroup
		start := make(chan struct{})
		out := make([]*httptest.ResponseRecorder, 2)
		ready.Add(2)
		done.Add(2)
		go func() {
			defer done.Done()
			ready.Done()
			<-start
			out[0] = env.do(http.MethodPost, "/api/v1/admin/users/"+b.ID.String()+"/disable", adminHTTPReason, "Bearer "+aTok, nextTestIP())
		}()
		go func() {
			defer done.Done()
			ready.Done()
			<-start
			out[1] = env.do(http.MethodPost, "/api/v1/admin/users/"+a.ID.String()+"/roles/revoke", `{"role":"ADMIN"}`, "Bearer "+bTok, nextTestIP())
		}()
		ready.Wait()
		close(start)
		done.Wait()

		ok, fail := 0, 0
		for i, rec := range out {
			switch rec.Code {
			case http.StatusNoContent:
				ok++
			case http.StatusBadRequest:
				fail++
				assertAuthError(t, rec, http.StatusBadRequest, domain.CodeInvalidRequest)
			case http.StatusForbidden:
				fail++
			default:
				t.Fatalf("race %d = %d %s", i, rec.Code, rec.Body)
			}
		}
		if ok != 1 || fail != 1 {
			t.Fatalf("disable/revoke race success=%d fail=%d", ok, fail)
		}

		sa, sb := env.userStatus(a.ID), env.userStatus(b.ID)
		aAdmin := containsRole(env.userRoles(a.ID), domain.RoleAdmin)
		bAdmin := containsRole(env.userRoles(b.ID), domain.RoleAdmin)
		n := countActiveAdmins(t, ctx, conn)
		if n != 1 {
			t.Fatalf("ACTIVE ADMIN = %d, want 1", n)
		}

		survivorTok := aTok
		survivorID, otherID := a.ID, b.ID
		switch {
		case out[0].Code == http.StatusNoContent:
			if sb != domain.StatusDisabled || sa != domain.StatusActive {
				t.Fatalf("disable winner: a=%s b=%s", sa, sb)
			}
			if !aAdmin || !bAdmin {
				t.Fatalf("disable must not strip ADMIN: a=%v b=%v", aAdmin, bAdmin)
			}
		case out[1].Code == http.StatusNoContent:
			if sa != domain.StatusActive || sb != domain.StatusActive {
				t.Fatalf("revoke winner mutated status a=%s b=%s", sa, sb)
			}
			if aAdmin || !bAdmin {
				t.Fatalf("revoke winner: aAdmin=%v bAdmin=%v", aAdmin, bAdmin)
			}
			survivorTok, survivorID, otherID = bTok, b.ID, a.ID
		default:
			t.Fatal("neither disable nor revoke succeeded")
		}

		restore := env.do(http.MethodPost, "/api/v1/admin/users/"+adminID.String()+"/roles", `{"role":"ADMIN"}`, "Bearer "+survivorTok, nextTestIP())
		if restore.Code != http.StatusNoContent {
			t.Fatalf("restore = %d %s", restore.Code, restore.Body)
		}
		if env.userStatus(otherID) == domain.StatusDisabled {
			en := env.do(http.MethodPost, "/api/v1/admin/users/"+otherID.String()+"/enable", adminHTTPReason, adminAuth, nextTestIP())
			if en.Code != http.StatusNoContent {
				t.Fatalf("enable other = %d %s", en.Code, en.Body)
			}
		}
		if containsRole(env.userRoles(otherID), domain.RoleAdmin) {
			env.mustRevokeRole(adminAuth, otherID, domain.RoleAdmin)
		}
		env.mustRevokeRole(adminAuth, survivorID, domain.RoleAdmin)
		if countActiveAdmins(t, ctx, conn) != 1 {
			t.Fatalf("cleanup ACTIVE ADMIN = %d", countActiveAdmins(t, ctx, conn))
		}
	})

	t.Run("auth admin http does not touch ledger", func(t *testing.T) {
		env.t = t
		got := ledgerCounts(t, ctx, conn)
		if got != ledgerBefore {
			t.Fatalf("ledger changed: before=%+v after=%+v", ledgerBefore, got)
		}
		if got.journals != 0 || got.entries != 0 || got.balances != 0 {
			t.Fatalf("auth wrote journals/entries/balances: %+v", got)
		}
	})
}

type adminHTTPEnv struct {
	t      *testing.T
	ctx    context.Context
	logs   *syncBuf
	h      http.Handler
	uow    application.UnitOfWork
	write  application.Service
	signer *authcrypto.HS256
	app    *pgx.Conn
	mu     sync.Mutex
	banned []string
}

type userSnap struct {
	status  domain.Status
	roles   []domain.Role
	history int
}

func newAdminHTTPEnv(t *testing.T, ctx context.Context, pool *pgxpool.Pool, app *pgx.Conn) *adminHTTPEnv {
	t.Helper()
	signer, err := authcrypto.NewHS256([]byte(httpPGJWTSecret))
	if err != nil {
		t.Fatal(err)
	}
	base := application.Service{
		Hasher: authcrypto.NewArgon2idHasher(),
		Issuer: authcrypto.NewOpaqueRefresh(),
		Access: signer,
		Clock:  authcrypto.NewSystemClock(),
	}
	read := base
	read.Users = NewAuthUsers(pool)
	read.Sessions = NewAuthSessions(pool)
	read.Roles = NewAuthRoles(pool)
	uow := NewAuthUoW(pool)

	logs := &syncBuf{}
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	srv := httpserver.New(config.HTTP{
		Addr:           "127.0.0.1:0",
		RequestTimeout: 60 * time.Second,
		MaxBodyBytes:   1 << 20,
	}, log)
	authhttp.Mount(srv, authhttp.Compose(uow, LogoutRefreshRetry(uow), base, signer, read))

	return &adminHTTPEnv{
		t:      t,
		ctx:    ctx,
		logs:   logs,
		h:      srv.Handler(),
		uow:    uow,
		write:  base,
		signer: signer,
		app:    app,
		banned: []string{
			httpPGPassword, httpPGJWTSecret, appPassword, ownerPassword,
			"Authorization", "$argon2id$", "pgx",
		},
	}
}

func (e *adminHTTPEnv) trackSecret(ss ...string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.banned = append(e.banned, ss...)
}

func (e *adminHTTPEnv) do(method, path, body, auth, ip string) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(e.ctx)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	req.RemoteAddr = ip + ":54321"
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

func (e *adminHTTPEnv) assertNoSecrets(haystacks ...string) {
	e.t.Helper()
	e.mu.Lock()
	banned := append([]string(nil), e.banned...)
	e.mu.Unlock()
	for _, hay := range haystacks {
		for _, s := range banned {
			if s == "" || s == "Authorization" {
				continue
			}
			if strings.Contains(hay, s) {
				e.t.Errorf("value leaked %q in %s", s, hay)
			}
		}
		if strings.Contains(hay, "Authorization") && strings.Contains(hay, "Bearer ") {
			e.t.Errorf("Authorization leaked: %s", hay)
		}
	}
}

func (e *adminHTTPEnv) assertLogsClean() {
	e.t.Helper()
	dump := e.logs.String()
	e.assertNoSecrets(dump)
	if strings.Contains(dump, "Authorization") {
		e.t.Errorf("logs leaked Authorization: %s", dump)
	}
}

func (e *adminHTTPEnv) mustRegister(email string) httpUser {
	e.t.Helper()
	e.trackSecret(httpPGPassword)
	rec := e.do(http.MethodPost, "/api/v1/auth/register", registerBody(email, httpPGPassword), "", nextTestIP())
	if rec.Code != http.StatusCreated {
		e.t.Fatalf("register %s = %d %s", email, rec.Code, rec.Body)
	}
	return decodeUser(e.t, rec.Body)
}

func (e *adminHTTPEnv) mustLogin(email, password string) httpCreds {
	e.t.Helper()
	e.trackSecret(password)
	rec := e.do(http.MethodPost, "/api/v1/auth/login", loginBody(email, password), "", nextTestIP())
	if rec.Code != http.StatusOK {
		e.t.Fatalf("login %s = %d %s", email, rec.Code, rec.Body)
	}
	cred := decodeCreds(e.t, rec.Body)
	e.trackSecret(cred.AccessToken, cred.RefreshToken)
	return cred
}

func (e *adminHTTPEnv) mustGrant(auth string, id uuid.UUID, role domain.Role) {
	e.t.Helper()
	rec := e.do(http.MethodPost, "/api/v1/admin/users/"+id.String()+"/roles", `{"role":"`+string(role)+`"}`, auth, nextTestIP())
	if rec.Code != http.StatusNoContent {
		e.t.Fatalf("grant %s = %d %s", role, rec.Code, rec.Body)
	}
}

func (e *adminHTTPEnv) mustRevokeRole(auth string, id uuid.UUID, role domain.Role) {
	e.t.Helper()
	rec := e.do(http.MethodPost, "/api/v1/admin/users/"+id.String()+"/roles/revoke", `{"role":"`+string(role)+`"}`, auth, nextTestIP())
	if rec.Code != http.StatusNoContent {
		e.t.Fatalf("revoke %s = %d %s", role, rec.Code, rec.Body)
	}
}

func (e *adminHTTPEnv) userStatus(id uuid.UUID) domain.Status {
	e.t.Helper()
	var st string
	if err := e.app.QueryRow(e.ctx, `SELECT status FROM auth.users WHERE id = $1`, id).Scan(&st); err != nil {
		e.t.Fatal(err)
	}
	return domain.Status(st)
}

func (e *adminHTTPEnv) userRoles(id uuid.UUID) []domain.Role {
	e.t.Helper()
	rows, err := e.app.Query(e.ctx, `
		SELECT role_code FROM auth.user_roles WHERE user_id = $1 ORDER BY role_code`, id)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var out []domain.Role
	for rows.Next() {
		var code string
		if err := rows.Scan(&code); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, domain.Role(code))
	}
	if err := rows.Err(); err != nil {
		e.t.Fatal(err)
	}
	return out
}

func (e *adminHTTPEnv) historyCount(id uuid.UUID) int {
	e.t.Helper()
	var n int
	if err := e.app.QueryRow(e.ctx, `
		SELECT count(*) FROM auth.user_status_history WHERE user_id = $1`, id).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *adminHTTPEnv) liveSessions(id uuid.UUID) int {
	e.t.Helper()
	var n int
	if err := e.app.QueryRow(e.ctx, `
		SELECT count(*) FROM auth.sessions WHERE user_id = $1 AND revoked_at IS NULL`, id).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *adminHTTPEnv) createdAt(id uuid.UUID) time.Time {
	e.t.Helper()
	var at time.Time
	if err := e.app.QueryRow(e.ctx, `SELECT created_at FROM auth.users WHERE id = $1`, id).Scan(&at); err != nil {
		e.t.Fatal(err)
	}
	return at.UTC()
}

func (e *adminHTTPEnv) setCreatedAt(id uuid.UUID, at time.Time) {
	e.t.Helper()
	if _, err := e.app.Exec(e.ctx, `UPDATE auth.users SET created_at = $2 WHERE id = $1`, id, at.UTC()); err != nil {
		e.t.Fatal(err)
	}
}

func (e *adminHTTPEnv) idsByKeyset(ids []uuid.UUID) []uuid.UUID {
	e.t.Helper()
	rows, err := e.app.Query(e.ctx, `
		SELECT id FROM auth.users WHERE id = ANY($1::uuid[])
		 ORDER BY created_at ASC, id ASC`, ids)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		e.t.Fatal(err)
	}
	return out
}

func (e *adminHTTPEnv) userSnapshot(id uuid.UUID) userSnap {
	e.t.Helper()
	return userSnap{status: e.userStatus(id), roles: e.userRoles(id), history: e.historyCount(id)}
}

func (e *adminHTTPEnv) assertUserUnchanged(id uuid.UUID, before userSnap) {
	e.t.Helper()
	got := e.userSnapshot(id)
	if got.status != before.status {
		e.t.Fatalf("status changed %s → %s", before.status, got.status)
	}
	if !rolesEqual(got.roles, before.roles) {
		e.t.Fatalf("roles changed %v → %v", before.roles, got.roles)
	}
	if got.history != before.history {
		e.t.Fatalf("history %d → %d", before.history, got.history)
	}
}

type adminListJSON struct {
	Users []struct {
		ID        uuid.UUID     `json:"id"`
		Email     string        `json:"email"`
		Status    domain.Status `json:"status"`
		Roles     []domain.Role `json:"roles"`
		CreatedAt time.Time     `json:"created_at"`
	} `json:"users"`
	NextCursor string `json:"next_cursor"`
}

func decodeAdminList(t *testing.T, rec *httptest.ResponseRecorder) adminListJSON {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d %s", rec.Code, rec.Body)
	}
	var page adminListJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("list json: %v %s", err, rec.Body)
	}
	if page.Users == nil {
		t.Fatalf("users null: %s", rec.Body)
	}
	return page
}

func encodeAdminCursor(t *testing.T, created time.Time, id uuid.UUID) string {
	t.Helper()
	payload, err := json.Marshal(map[string]string{
		"created_at": created.UTC().Format(time.RFC3339Nano),
		"id":         id.String(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(payload)
}

func assertAdminListShape(t *testing.T, raw string) {
	t.Helper()
	var env map[string]any
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		t.Fatal(err)
	}
	for k := range env {
		if k != "users" && k != "next_cursor" {
			t.Errorf("unexpected list key %q", k)
		}
	}
	users, _ := env["users"].([]any)
	for i, row := range users {
		m, _ := row.(map[string]any)
		for k := range m {
			switch k {
			case "id", "email", "status", "roles", "created_at":
			default:
				t.Errorf("user[%d] extra field %q", i, k)
			}
		}
		for _, leak := range []string{
			"password_hash", "failed_login_count", "locked_at", "first_name", "last_name",
			"password", "access_token", "refresh_token",
		} {
			if _, ok := m[leak]; ok {
				t.Errorf("user[%d] has %s", i, leak)
			}
		}
	}
	for _, leak := range []string{"password_hash", "$argon2id$", "failed_login_count", "refresh_token", "access_token"} {
		if strings.Contains(raw, leak) {
			t.Errorf("list leaked %s: %s", leak, raw)
		}
	}
}

func rolesEqual(a, b []domain.Role) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func roleCount(roles []domain.Role, want domain.Role) int {
	n := 0
	for _, r := range roles {
		if r == want {
			n++
		}
	}
	return n
}

func idsIn(seen, want []uuid.UUID) []uuid.UUID {
	allow := make(map[uuid.UUID]struct{}, len(want))
	for _, id := range want {
		allow[id] = struct{}{}
	}
	var out []uuid.UUID
	found := make(map[uuid.UUID]int)
	for _, id := range seen {
		if _, ok := allow[id]; !ok {
			continue
		}
		found[id]++
		out = append(out, id)
	}
	return out
}

func uuidSeqEqual(a, b []uuid.UUID) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
