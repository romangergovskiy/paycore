package postgres

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/romangergovskiy/paycore/migrations"
)

func TestAuthMigrationFile(t *testing.T) {
	up, err := migrations.FS.ReadFile("00005_auth.sql")
	if err != nil {
		t.Fatal(err)
	}
	body := string(up)
	lower := strings.ToLower(body)
	if strings.Contains(lower, "drop table auth.users") {
		t.Fatal("00005 must not DROP auth.users")
	}
	if strings.Contains(lower, "insert into auth.users") {
		t.Fatal("00005 must not seed users")
	}
	if strings.Contains(body, "PAYCORE_BOOTSTRAP") || strings.Contains(lower, "bootstrap_admin") {
		t.Fatal("00005 must not seed bootstrap ADMIN")
	}
	if strings.Contains(lower, "on delete cascade") {
		t.Fatal("00005 must not use ON DELETE CASCADE")
	}
}

func TestAuthDDL(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	c := startCluster(t)
	if err := Bootstrap(ctx, c.admin, c.owner, c.app, c.readonly); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if err := Up(ctx, c.owner); err != nil {
		t.Fatalf("up: %v", err)
	}

	owner := mustConnect(t, ctx, c.owner)
	defer owner.Close(ctx)
	app := mustConnect(t, ctx, c.app)
	defer app.Close(ctx)
	ro := mustConnect(t, ctx, c.readonly)
	defer ro.Close(ctx)

	hash32 := bytes.Repeat([]byte{0xab}, 32)
	hash31 := bytes.Repeat([]byte{0xab}, 31)

	t.Run("1. users table not dropped", func(t *testing.T) {
		var n int
		if err := owner.QueryRow(ctx, `
			SELECT count(*) FROM information_schema.tables
			 WHERE table_schema = 'auth' AND table_name = 'users'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("auth.users tables = %d", n)
		}
		var fk int
		if err := owner.QueryRow(ctx, `
			SELECT count(*) FROM information_schema.table_constraints
			 WHERE table_schema = 'accounts' AND table_name = 'accounts'
			   AND constraint_type = 'FOREIGN KEY'`).Scan(&fk); err != nil {
			t.Fatal(err)
		}
		if fk < 1 {
			t.Fatal("accounts FKs missing after auth ALTER")
		}
	})

	t.Run("2. users columns exist", func(t *testing.T) {
		var email, hash, status string
		if err := owner.QueryRow(ctx, `
			SELECT column_name FROM information_schema.columns
			 WHERE table_schema = 'auth' AND table_name = 'users' AND column_name = 'email'`).Scan(&email); err != nil {
			t.Fatal(err)
		}
		if err := owner.QueryRow(ctx, `
			SELECT column_name FROM information_schema.columns
			 WHERE table_schema = 'auth' AND table_name = 'users' AND column_name = 'password_hash'`).Scan(&hash); err != nil {
			t.Fatal(err)
		}
		if err := owner.QueryRow(ctx, `
			SELECT column_name FROM information_schema.columns
			 WHERE table_schema = 'auth' AND table_name = 'users' AND column_name = 'status'`).Scan(&status); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("3. email unique and lowercase", func(t *testing.T) {
		id := mustNewID(t)
		insertAuthUser(t, ctx, owner, id.String())
		mustFail(t, ctx, owner, `
			INSERT INTO auth.users (id, email, password_hash, status)
			VALUES ($1, $2, $3, 'ACTIVE')`, mustNewID(t), strings.ToLower(id.String())+"@users.test", testArgon2idPHC)
		mustFail(t, ctx, owner, `
			INSERT INTO auth.users (id, email, password_hash, status)
			VALUES ($1, 'NotLower@example.com', $2, 'ACTIVE')`, mustNewID(t), testArgon2idPHC)
	})

	t.Run("4. password_hash argon2id check", func(t *testing.T) {
		mustFail(t, ctx, owner, `
			INSERT INTO auth.users (id, email, password_hash, status)
			VALUES ($1, 'plain@users.test', 'secret', 'ACTIVE')`, mustNewID(t))
		mustFail(t, ctx, owner, `
			INSERT INTO auth.users (id, email, password_hash, status)
			VALUES ($1, 'bcrypt@users.test', '$2a$10$aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', 'ACTIVE')`, mustNewID(t))
	})

	t.Run("5. status and locked_at", func(t *testing.T) {
		mustFail(t, ctx, owner, `
			INSERT INTO auth.users (id, email, password_hash, status)
			VALUES ($1, 'badstatus@users.test', $2, 'PENDING')`, mustNewID(t), testArgon2idPHC)
		mustFail(t, ctx, owner, `
			INSERT INTO auth.users (id, email, password_hash, status, locked_at)
			VALUES ($1, 'locked-no-at@users.test', $2, 'LOCKED', NULL)`, mustNewID(t), testArgon2idPHC)
		mustFail(t, ctx, owner, `
			INSERT INTO auth.users (id, email, password_hash, status, locked_at)
			VALUES ($1, 'active-with-at@users.test', $2, 'ACTIVE', now())`, mustNewID(t), testArgon2idPHC)
	})

	t.Run("6. failed_login_count", func(t *testing.T) {
		mustFail(t, ctx, owner, `
			INSERT INTO auth.users (id, email, password_hash, status, failed_login_count)
			VALUES ($1, 'negfail@users.test', $2, 'ACTIVE', -1)`, mustNewID(t), testArgon2idPHC)
	})

	t.Run("7. name length", func(t *testing.T) {
		mustFail(t, ctx, owner, `
			INSERT INTO auth.users (id, email, password_hash, status, first_name)
			VALUES ($1, 'emptyname@users.test', $2, 'ACTIVE', '')`, mustNewID(t), testArgon2idPHC)
		mustFail(t, ctx, owner, `
			INSERT INTO auth.users (id, email, password_hash, status, last_name)
			VALUES ($1, 'longname@users.test', $2, 'ACTIVE', $3)`, mustNewID(t), testArgon2idPHC, strings.Repeat("n", 101))
	})

	t.Run("8. six roles seeded", func(t *testing.T) {
		var n int
		if err := owner.QueryRow(ctx, `SELECT count(*) FROM auth.roles`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 6 {
			t.Fatalf("roles = %d", n)
		}
		rows, err := owner.Query(ctx, `SELECT code FROM auth.roles ORDER BY code`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		want := []string{"ADMIN", "CUSTOMER", "MERCHANT", "OPERATOR", "RISK_ANALYST", "SUPPORT"}
		var got []string
		for rows.Next() {
			var code string
			if err := rows.Scan(&code); err != nil {
				t.Fatal(err)
			}
			got = append(got, code)
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("roles = %v want %v", got, want)
		}
	})

	t.Run("9. role seed idempotent", func(t *testing.T) {
		if _, err := owner.Exec(ctx, `
			INSERT INTO auth.roles (code, description) VALUES
				('CUSTOMER', 'Own accounts and customer operations')
			ON CONFLICT (code) DO NOTHING`); err != nil {
			t.Fatal(err)
		}
		var n int
		if err := owner.QueryRow(ctx, `SELECT count(*) FROM auth.roles`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 6 {
			t.Fatalf("roles after reseed = %d", n)
		}
	})

	t.Run("10. no admin user seed", func(t *testing.T) {
		var admins int
		if err := owner.QueryRow(ctx, `
			SELECT count(*) FROM auth.user_roles WHERE role_code = 'ADMIN'`).Scan(&admins); err != nil {
			t.Fatal(err)
		}
		if admins != 0 {
			t.Fatalf("ADMIN user_roles = %d", admins)
		}
	})

	t.Run("11. user_roles pk fk and app write", func(t *testing.T) {
		u := mustNewID(t)
		insertAuthUser(t, ctx, owner, u.String())
		if _, err := app.Exec(ctx, `
			INSERT INTO auth.user_roles (user_id, role_code) VALUES ($1, 'CUSTOMER')`, u); err != nil {
			t.Fatalf("app insert user_roles: %v", err)
		}
		mustFail(t, ctx, app, `
			INSERT INTO auth.user_roles (user_id, role_code) VALUES ($1, 'CUSTOMER')`, u)
		mustFail(t, ctx, app, `
			INSERT INTO auth.user_roles (user_id, role_code) VALUES ($1, 'NOPE')`, u)
		if _, err := app.Exec(ctx, `DELETE FROM auth.user_roles WHERE user_id = $1`, u); err != nil {
			t.Fatalf("app delete user_roles: %v", err)
		}
	})

	t.Run("12. sessions revoke check and live index", func(t *testing.T) {
		u := mustNewID(t)
		insertAuthUser(t, ctx, owner, u.String())
		sid := mustNewID(t)
		if _, err := app.Exec(ctx, `
			INSERT INTO auth.sessions (id, user_id) VALUES ($1, $2)`, sid, u); err != nil {
			t.Fatalf("NULL/NULL: %v", err)
		}
		mustFail(t, ctx, app, `
			UPDATE auth.sessions SET revoke_reason = 'LOGOUT' WHERE id = $1`, sid)
		mustFail(t, ctx, app, `
			INSERT INTO auth.sessions (id, user_id, revoked_at)
			VALUES ($1, $2, now())`, mustNewID(t), u)
		if _, err := app.Exec(ctx, `
			UPDATE auth.sessions SET revoked_at = now(), revoke_reason = 'LOGOUT' WHERE id = $1`, sid); err != nil {
			t.Fatalf("revoked+LOGOUT: %v", err)
		}
		mustFail(t, ctx, app, `
			INSERT INTO auth.sessions (id, user_id, revoked_at, revoke_reason)
			VALUES ($1, $2, now(), 'NOPE')`, mustNewID(t), u)
		var idx int
		if err := owner.QueryRow(ctx, `
			SELECT count(*) FROM pg_indexes
			 WHERE schemaname = 'auth' AND indexname = 'ix_sessions_user_live'`).Scan(&idx); err != nil {
			t.Fatal(err)
		}
		if idx != 1 {
			t.Fatal("missing ix_sessions_user_live")
		}
	})

	t.Run("13. refresh token_hash length 32", func(t *testing.T) {
		u := mustNewID(t)
		insertAuthUser(t, ctx, owner, u.String())
		sid := mustNewID(t)
		if _, err := app.Exec(ctx, `INSERT INTO auth.sessions (id, user_id) VALUES ($1, $2)`, sid, u); err != nil {
			t.Fatal(err)
		}
		mustFail(t, ctx, app, `
			INSERT INTO auth.refresh_tokens (id, session_id, token_hash, expires_at)
			VALUES ($1, $2, $3, now() + interval '30 days')`, mustNewID(t), sid, hash31)
		if _, err := app.Exec(ctx, `
			INSERT INTO auth.refresh_tokens (id, session_id, token_hash, expires_at)
			VALUES ($1, $2, $3, now() + interval '30 days')`, mustNewID(t), sid, hash32); err != nil {
			t.Fatalf("32-byte hash: %v", err)
		}
	})

	t.Run("14. token_hash immutable trigger", func(t *testing.T) {
		u := mustNewID(t)
		insertAuthUser(t, ctx, owner, u.String())
		sid := mustNewID(t)
		tid := mustNewID(t)
		if _, err := app.Exec(ctx, `INSERT INTO auth.sessions (id, user_id) VALUES ($1, $2)`, sid, u); err != nil {
			t.Fatal(err)
		}
		if _, err := app.Exec(ctx, `
			INSERT INTO auth.refresh_tokens (id, session_id, token_hash, expires_at)
			VALUES ($1, $2, $3, now() + interval '30 days')`, tid, sid, bytes.Repeat([]byte{0x14}, 32)); err != nil {
			t.Fatal(err)
		}
		mustFail(t, ctx, app, `UPDATE auth.refresh_tokens SET token_hash = $1 WHERE id = $2`,
			bytes.Repeat([]byte{0xcd}, 32), tid)
		if _, err := app.Exec(ctx, `UPDATE auth.refresh_tokens SET revoked_at = now() WHERE id = $1`, tid); err != nil {
			t.Fatalf("update revoked_at: %v", err)
		}
	})

	t.Run("15. trigger search_path", func(t *testing.T) {
		for _, name := range []string{"forbid_login_attempt_mutation", "forbid_refresh_token_hash_change"} {
			var cfg string
			if err := owner.QueryRow(ctx, `
				SELECT coalesce(array_to_string(proconfig, ','), '')
				  FROM pg_proc p
				  JOIN pg_namespace n ON n.oid = p.pronamespace
				 WHERE n.nspname = 'auth' AND p.proname = $1`, name).Scan(&cfg); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if !strings.Contains(cfg, "pg_catalog") || !strings.Contains(cfg, "pg_temp") {
				t.Fatalf("%s search_path = %q", name, cfg)
			}
			if strings.Contains(cfg, "public") {
				t.Fatalf("%s search_path includes public: %q", name, cfg)
			}
		}
	})

	t.Run("16. replaced_by and expires_at", func(t *testing.T) {
		u := mustNewID(t)
		insertAuthUser(t, ctx, owner, u.String())
		sid := mustNewID(t)
		if _, err := app.Exec(ctx, `INSERT INTO auth.sessions (id, user_id) VALUES ($1, $2)`, sid, u); err != nil {
			t.Fatal(err)
		}
		mustFail(t, ctx, app, `
			INSERT INTO auth.refresh_tokens (id, session_id, token_hash, expires_at, created_at)
			VALUES ($1, $2, $3, now(), now() + interval '1 hour')`, mustNewID(t), sid, bytes.Repeat([]byte{0x16}, 32))
		mustFail(t, ctx, app, `
			INSERT INTO auth.refresh_tokens (id, session_id, token_hash, expires_at, used_at)
			VALUES ($1, $2, $3, now() + interval '30 days', now())`, mustNewID(t), sid, bytes.Repeat([]byte{0x17}, 32))
	})

	t.Run("17. login_attempts append-only", func(t *testing.T) {
		id := mustNewID(t)
		if _, err := app.Exec(ctx, `
			INSERT INTO auth.login_attempts (id, email, success, failure_reason)
			VALUES ($1, 'a@users.test', false, 'INVALID_CREDENTIALS')`, id); err != nil {
			t.Fatalf("insert attempt: %v", err)
		}
		mustFail(t, ctx, owner, `UPDATE auth.login_attempts SET success = true WHERE id = $1`, id)
		mustFail(t, ctx, owner, `DELETE FROM auth.login_attempts WHERE id = $1`, id)
		mustFail(t, ctx, app, `DELETE FROM auth.login_attempts WHERE id = $1`, id)
		mustFail(t, ctx, owner, `TRUNCATE auth.login_attempts`)

		if _, err := app.Exec(ctx, `
			INSERT INTO auth.login_attempts (id, email, success, failure_reason)
			VALUES ($1, 'ok@users.test', true, NULL)`, mustNewID(t)); err != nil {
			t.Fatalf("success+NULL: %v", err)
		}
		mustFail(t, ctx, app, `
			INSERT INTO auth.login_attempts (id, email, success, failure_reason)
			VALUES ($1, 'ok-reason@users.test', true, 'INVALID_CREDENTIALS')`, mustNewID(t))
		mustFail(t, ctx, app, `
			INSERT INTO auth.login_attempts (id, email, success, failure_reason)
			VALUES ($1, 'fail-null@users.test', false, NULL)`, mustNewID(t))
		if _, err := app.Exec(ctx, `
			INSERT INTO auth.login_attempts (id, email, success, failure_reason)
			VALUES ($1, 'fail-ok@users.test', false, 'RATE_LIMITED')`, mustNewID(t)); err != nil {
			t.Fatalf("fail+valid reason: %v", err)
		}
		mustFail(t, ctx, app, `
			INSERT INTO auth.login_attempts (id, email, success, failure_reason)
			VALUES ($1, 'fail-unknown@users.test', false, 'NOPE')`, mustNewID(t))
	})

	t.Run("18. rate_limit_counters", func(t *testing.T) {
		if _, err := app.Exec(ctx, `
			INSERT INTO auth.rate_limit_counters (bucket, counter_key, window_start, count)
			VALUES ('LOGIN_EMAIL', 'a@users.test', date_bin('15 minutes', now(), TIMESTAMPTZ '1970-01-01'), 1)`); err != nil {
			t.Fatalf("insert counter: %v", err)
		}
		mustFail(t, ctx, app, `
			INSERT INTO auth.rate_limit_counters (bucket, counter_key, window_start, count)
			VALUES ('OTHER', 'x', now(), 1)`)
		mustFail(t, ctx, app, `
			INSERT INTO auth.rate_limit_counters (bucket, counter_key, window_start, count)
			VALUES ('LOGIN_IP', '127.0.0.1', now(), -1)`)
	})

	t.Run("19. action_confirmations exists", func(t *testing.T) {
		var n int
		if err := owner.QueryRow(ctx, `
			SELECT count(*) FROM auth.action_confirmations`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("action_confirmations seed = %d", n)
		}
		u := mustNewID(t)
		insertAuthUser(t, ctx, owner, u.String())
		if _, err := app.Exec(ctx, `
			INSERT INTO auth.action_confirmations (id, user_id, action_type, code_hash, attempts_left, expires_at)
			VALUES ($1, $2, 'PASSWORD_CHANGE', $3, 3, now() + interval '10 minutes')`, mustNewID(t), u, hash32); err != nil {
			t.Fatalf("insert confirmation: %v", err)
		}
	})

	t.Run("20. grants", func(t *testing.T) {
		var usageApp, usageRO, usageLedger bool
		if err := owner.QueryRow(ctx, `SELECT has_schema_privilege('paycore_app', 'auth', 'USAGE')`).Scan(&usageApp); err != nil {
			t.Fatal(err)
		}
		if err := owner.QueryRow(ctx, `SELECT has_schema_privilege('paycore_readonly', 'auth', 'USAGE')`).Scan(&usageRO); err != nil {
			t.Fatal(err)
		}
		if err := owner.QueryRow(ctx, `SELECT has_schema_privilege('paycore_ledger', 'auth', 'USAGE')`).Scan(&usageLedger); err != nil {
			t.Fatal(err)
		}
		if !usageApp || !usageRO {
			t.Fatalf("USAGE app=%v readonly=%v", usageApp, usageRO)
		}
		if usageLedger {
			t.Fatal("paycore_ledger has USAGE on auth")
		}
		assertPrivilege(t, ctx, owner, "paycore_app", "auth.users", "INSERT", true)
		assertPrivilege(t, ctx, owner, "paycore_app", "auth.users", "UPDATE", true)
		assertPrivilege(t, ctx, owner, "paycore_app", "auth.users", "DELETE", false)
		assertPrivilege(t, ctx, owner, "paycore_app", "auth.roles", "SELECT", true)
		assertPrivilege(t, ctx, owner, "paycore_app", "auth.roles", "INSERT", false)
		assertPrivilege(t, ctx, owner, "paycore_app", "auth.user_roles", "DELETE", true)
		assertPrivilege(t, ctx, owner, "paycore_app", "auth.login_attempts", "INSERT", true)
		assertPrivilege(t, ctx, owner, "paycore_app", "auth.login_attempts", "UPDATE", false)
		assertPrivilege(t, ctx, owner, "paycore_app", "auth.user_status_history", "INSERT", true)
		assertPrivilege(t, ctx, owner, "paycore_app", "auth.user_status_history", "UPDATE", false)
		assertPrivilege(t, ctx, owner, "paycore_app", "auth.user_status_history", "DELETE", false)
		assertPrivilege(t, ctx, owner, "paycore_readonly", "auth.user_status_history", "SELECT", true)
		assertPrivilege(t, ctx, owner, "paycore_readonly", "auth.user_status_history", "INSERT", false)
		assertPrivilege(t, ctx, owner, "paycore_app", "auth.refresh_tokens", "DELETE", true)
		assertPrivilege(t, ctx, owner, "paycore_readonly", "auth.users", "SELECT", true)
		assertPrivilege(t, ctx, owner, "paycore_readonly", "auth.users", "INSERT", false)
		u := mustNewID(t)
		if _, err := app.Exec(ctx, `
			INSERT INTO auth.users (id, email, password_hash, status)
			VALUES ($1, $2, $3, 'ACTIVE')`, u, strings.ToLower(u.String())+"@app.test", testArgon2idPHC); err != nil {
			t.Fatalf("app insert user: %v", err)
		}
		mustFail(t, ctx, app, `DELETE FROM auth.users WHERE id = $1`, u)
		mustFail(t, ctx, app, `INSERT INTO auth.roles (code, description) VALUES ('X', 'no')`)
	})

	t.Run("21. readonly select no write", func(t *testing.T) {
		var n int
		if err := ro.QueryRow(ctx, `SELECT count(*) FROM auth.roles`).Scan(&n); err != nil {
			t.Fatalf("readonly select roles: %v", err)
		}
		if n != 6 {
			t.Fatalf("readonly roles = %d", n)
		}
		mustFail(t, ctx, ro, `
			INSERT INTO auth.users (id, email, password_hash, status)
			VALUES ($1, 'ro@users.test', $2, 'ACTIVE')`, mustNewID(t), testArgon2idPHC)
		mustFail(t, ctx, ro, `INSERT INTO auth.roles (code, description) VALUES ('X', 'no')`)
	})

	t.Run("22. down refuses data", func(t *testing.T) {
		c2 := startCluster(t)
		if err := Bootstrap(ctx, c2.admin, c2.owner, c2.app, c2.readonly); err != nil {
			t.Fatalf("bootstrap: %v", err)
		}
		if err := Up(ctx, c2.owner); err != nil {
			t.Fatalf("up: %v", err)
		}
		conn := mustConnect(t, ctx, c2.owner)
		defer conn.Close(ctx)
		insertAuthUser(t, ctx, conn, mustNewID(t).String())
		if err := Down(ctx, c2.owner); err != nil {
			t.Fatalf("00006 down with empty history: %v", err)
		}
		v, verr := CurrentVersion(ctx, c2.owner)
		if verr != nil || v != 5 {
			t.Fatalf("version after 00006 down = %d, %v", v, verr)
		}
		err := Down(ctx, c2.owner)
		if err == nil {
			t.Fatal("down succeeded with users present")
		}
		if !strings.Contains(strings.ToLower(err.Error()), "refused") {
			t.Fatalf("down error = %v", err)
		}
		v, verr = CurrentVersion(ctx, c2.owner)
		if verr != nil || v != 5 {
			t.Fatalf("version after refused down = %d, %v", v, verr)
		}
	})

	t.Run("23. down succeeds without identity rows", func(t *testing.T) {
		c2 := startCluster(t)
		if err := Bootstrap(ctx, c2.admin, c2.owner, c2.app, c2.readonly); err != nil {
			t.Fatalf("bootstrap: %v", err)
		}
		if err := Up(ctx, c2.owner); err != nil {
			t.Fatalf("up: %v", err)
		}
		if err := Down(ctx, c2.owner); err != nil {
			t.Fatalf("00006 down: %v", err)
		}
		if err := Down(ctx, c2.owner); err != nil {
			t.Fatalf("00005 down: %v", err)
		}
		v, err := CurrentVersion(ctx, c2.owner)
		if err != nil || v != 4 {
			t.Fatalf("version after down = %d, %v", v, err)
		}
		conn := mustConnect(t, ctx, c2.owner)
		defer conn.Close(ctx)
		var users int
		if err := conn.QueryRow(ctx, `
			SELECT count(*) FROM information_schema.tables
			 WHERE table_schema = 'auth' AND table_name = 'users'`).Scan(&users); err != nil {
			t.Fatal(err)
		}
		if users != 1 {
			t.Fatal("auth.users dropped on down")
		}
		var emailCol int
		if err := conn.QueryRow(ctx, `
			SELECT count(*) FROM information_schema.columns
			 WHERE table_schema = 'auth' AND table_name = 'users' AND column_name = 'email'`).Scan(&emailCol); err != nil {
			t.Fatal(err)
		}
		if emailCol != 0 {
			t.Fatal("email column remained after down")
		}
		var roles int
		if err := conn.QueryRow(ctx, `
			SELECT count(*) FROM information_schema.tables
			 WHERE table_schema = 'auth' AND table_name = 'roles'`).Scan(&roles); err != nil {
			t.Fatal(err)
		}
		if roles != 0 {
			t.Fatal("auth.roles remained after down")
		}
	})

	t.Run("24. down refuses status history rows", func(t *testing.T) {
		c2 := startCluster(t)
		if err := Bootstrap(ctx, c2.admin, c2.owner, c2.app, c2.readonly); err != nil {
			t.Fatalf("bootstrap: %v", err)
		}
		if err := Up(ctx, c2.owner); err != nil {
			t.Fatalf("up: %v", err)
		}
		conn := mustConnect(t, ctx, c2.owner)
		defer conn.Close(ctx)
		uid := mustNewID(t)
		insertAuthUser(t, ctx, conn, uid.String())
		if _, err := conn.Exec(ctx, `
			INSERT INTO auth.user_status_history (
			  id, user_id, changed_by, previous_status, new_status, reason
			) VALUES ($1, $2, $2, 'ACTIVE', 'LOCKED', 'policy')`, mustNewID(t), uid); err != nil {
			t.Fatal(err)
		}
		err := Down(ctx, c2.owner)
		if err == nil {
			t.Fatal("00006 down succeeded with history rows")
		}
		if !strings.Contains(strings.ToLower(err.Error()), "refused") {
			t.Fatalf("down error = %v", err)
		}
		v, verr := CurrentVersion(ctx, c2.owner)
		if verr != nil || v != 6 {
			t.Fatalf("version after refused 00006 down = %d, %v", v, verr)
		}
	})
}
