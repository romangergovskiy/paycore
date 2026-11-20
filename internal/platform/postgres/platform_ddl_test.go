package postgres

import (
	"strings"
	"testing"

	"github.com/romangergovskiy/paycore/migrations"
)

func TestStageAMigrationFile(t *testing.T) {
	body, err := migrations.FS.ReadFile("00007_platform_idempotency_outbox.sql")
	if err != nil {
		t.Fatal(err)
	}
	lower := strings.ToLower(string(body))
	if strings.Contains(lower, "alter default privileges for role") {
		t.Fatal("00007 must not add blanket table defaults")
	}
	if strings.Contains(lower, "create function") {
		t.Fatal("00007 must not add business SQL functions")
	}
	for _, word := range []string{"kafka", "redis", "webhook_deliveries"} {
		if strings.Contains(lower, word) {
			t.Fatalf("00007 mentions %s", word)
		}
	}
}

func TestPlatformIdempotencyOutboxDDL(t *testing.T) {
	ctx, c := startMigrated(t)
	owner := mustConnect(t, ctx, c.owner)
	defer owner.Close(ctx)
	app := mustConnect(t, ctx, c.app)
	defer app.Close(ctx)
	ro := mustConnect(t, ctx, c.readonly)
	defer ro.Close(ctx)

	v, err := CurrentVersion(ctx, c.owner)
	if err != nil || v != 7 {
		t.Fatalf("version = %d, %v", v, err)
	}

	t.Run("tables exist", func(t *testing.T) {
		for _, table := range []string{"idempotency_keys", "outbox_events", "inbox_messages"} {
			var n int
			if err := app.QueryRow(ctx, `
				SELECT count(*) FROM information_schema.tables
				 WHERE table_schema = 'platform' AND table_name = $1`, table).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n != 1 {
				t.Fatalf("platform.%s missing", table)
			}
		}
	})

	t.Run("grants", func(t *testing.T) {
		for _, table := range []string{
			"platform.idempotency_keys",
			"platform.outbox_events",
			"platform.inbox_messages",
		} {
			assertPrivilege(t, ctx, owner, RoleApp, table, "SELECT", true)
			assertPrivilege(t, ctx, owner, RoleApp, table, "INSERT", true)
			assertPrivilege(t, ctx, owner, RoleApp, table, "UPDATE", true)
			assertPrivilege(t, ctx, owner, RoleApp, table, "DELETE", true)
			assertPrivilege(t, ctx, owner, RoleReadonly, table, "SELECT", true)
			assertPrivilege(t, ctx, owner, RoleReadonly, table, "INSERT", false)
			assertPrivilege(t, ctx, owner, RoleReadonly, table, "UPDATE", false)
			assertPrivilege(t, ctx, owner, RoleReadonly, table, "DELETE", false)
			assertPrivilege(t, ctx, owner, RoleLedger, table, "INSERT", false)
		}
		assertPrivilege(t, ctx, owner, RoleApp, "ledger.journal_transactions", "INSERT", false)
		assertPrivilege(t, ctx, owner, RoleApp, "ledger.journal_transactions", "UPDATE", false)
		assertPrivilege(t, ctx, owner, RoleApp, "ledger.journal_transactions", "DELETE", false)
		assertPrivilege(t, ctx, owner, RoleApp, "ledger.ledger_entries", "INSERT", false)
		assertPrivilege(t, ctx, owner, RoleApp, "ledger.account_balances", "INSERT", false)
		assertPrivilege(t, ctx, owner, RoleApp, "ledger.holds", "INSERT", false)
	})

	t.Run("paycore_app can write platform tables and cannot write ledger", func(t *testing.T) {
		id := mustNewID(t)
		hash := bytes32(t)
		if _, err := app.Exec(ctx, `
			INSERT INTO platform.idempotency_keys
			    (id, scope, endpoint, key, request_hash, status, created_at, expires_at)
			VALUES ($1, $2, 'POST /t', 'k1', $3, 'IN_PROGRESS', now(), now() + interval '24 hours')`,
			id, "user:"+mustNewID(t).String(), hash); err != nil {
			t.Fatalf("app insert key: %v", err)
		}
		if _, err := app.Exec(ctx, `
			UPDATE platform.idempotency_keys
			   SET status = 'COMPLETED', response_code = 201, response_body = '{}'::jsonb, completed_at = now()
			 WHERE id = $1`, id); err != nil {
			t.Fatalf("app update key: %v", err)
		}
		if _, err := app.Exec(ctx, `DELETE FROM platform.idempotency_keys WHERE id = $1`, id); err != nil {
			t.Fatalf("app delete key: %v", err)
		}
		oid := mustNewID(t)
		if _, err := app.Exec(ctx, `
			INSERT INTO platform.outbox_events
			    (id, aggregate_type, aggregate_id, aggregate_version, event_type, event_version)
			VALUES ($1, 'demo', $1, 1, 'demo.created', 1)`, oid); err != nil {
			t.Fatalf("app insert outbox: %v", err)
		}
		if _, err := app.Exec(ctx, `DELETE FROM platform.outbox_events WHERE id = $1`, oid); err != nil {
			t.Fatalf("app delete outbox: %v", err)
		}
		if _, err := app.Exec(ctx, `
			INSERT INTO platform.inbox_messages (consumer, message_id)
			VALUES ('c', $1)`, oid); err != nil {
			t.Fatalf("app insert inbox: %v", err)
		}
		if _, err := app.Exec(ctx, `DELETE FROM platform.inbox_messages WHERE message_id = $1`, oid); err != nil {
			t.Fatalf("app delete inbox: %v", err)
		}
		mustFail(t, ctx, app, `INSERT INTO ledger.journal_transactions (
			id, operation, reference_type, reference_id, accounting_date, status)
			VALUES ($1, 'SIM_TOPUP', 'SIM_TOPUP', $1, CURRENT_DATE, 'POSTED')`, mustNewID(t))
		mustFail(t, ctx, ro, `INSERT INTO platform.idempotency_keys
			(id, scope, endpoint, key, request_hash, status, created_at, expires_at)
			VALUES ($1, $2, 'POST /t', 'k', $3, 'IN_PROGRESS', now(), now() + interval '24 hours')`,
			mustNewID(t), "user:"+mustNewID(t).String(), hash)
	})

	t.Run("checks", func(t *testing.T) {
		hash := bytes32(t)
		mustFail(t, ctx, owner, `
			INSERT INTO platform.idempotency_keys
			    (id, scope, endpoint, key, request_hash, status, created_at, expires_at)
			VALUES ($1, 'apikey:x', 'POST /t', 'k', $2, 'IN_PROGRESS', now(), now() + interval '24 hours')`,
			mustNewID(t), hash)
		mustFail(t, ctx, owner, `
			INSERT INTO platform.idempotency_keys
			    (id, scope, endpoint, key, request_hash, status, created_at, expires_at, response_code)
			VALUES ($1, $2, 'POST /t', 'k', $3, 'COMPLETED', now(), now() + interval '24 hours', NULL)`,
			mustNewID(t), "user:"+mustNewID(t).String(), hash)
		mustFail(t, ctx, owner, `
			INSERT INTO platform.outbox_events
			    (id, aggregate_type, aggregate_id, aggregate_version, event_type, event_version, status, published_at)
			VALUES ($1, 'demo', $1, 1, 'demo.created', 1, 'PUBLISHED', NULL)`, mustNewID(t))
		mustFail(t, ctx, owner, `
			INSERT INTO platform.outbox_events
			    (id, aggregate_type, aggregate_id, aggregate_version, event_type, event_version, processing_until)
			VALUES ($1, 'demo', $1, 1, 'demo.created', 1, now())`, mustNewID(t))
	})
}

func bytes32(t *testing.T) []byte {
	t.Helper()
	b := make([]byte, 32)
	for i := range b {
		b[i] = byte(i)
	}
	return b
}
