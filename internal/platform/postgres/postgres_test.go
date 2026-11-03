package postgres

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	tpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/romangergovskiy/paycore/internal/platform/config"
)

const (
	ownerPassword = "owner-pass"
	appPassword   = "app-pass"
	roPassword    = "ro-pass"
)

type testCluster struct {
	admin, owner, app, readonly string
}

func startCluster(t *testing.T) testCluster {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)

	ctr, err := tpostgres.Run(ctx, "postgres:16-alpine",
		tpostgres.WithDatabase("paycore"),
		tpostgres.WithUsername("postgres"),
		tpostgres.WithPassword("postgres"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(time.Minute),
		),
	)
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	t.Cleanup(func() {
		_ = ctr.Terminate(context.Background())
	})

	admin, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	base, err := url.Parse(admin)
	if err != nil {
		t.Fatal(err)
	}

	return testCluster{
		admin:    admin,
		owner:    withUser(base, RoleOwner, ownerPassword),
		app:      withUser(base, RoleApp, appPassword),
		readonly: withUser(base, RoleReadonly, roPassword),
	}
}

func TestBootstrapRequiresRoleNames(t *testing.T) {
	err := Bootstrap(context.Background(),
		"postgres://postgres:p@localhost:5432/postgres",
		"postgres://not_owner:p@localhost:5432/paycore",
		"postgres://paycore_app:p@localhost:5432/paycore",
		"postgres://paycore_readonly:p@localhost:5432/paycore",
	)
	if err == nil || !strings.Contains(err.Error(), RoleOwner) {
		t.Fatalf("err = %v", err)
	}
}

func TestMigrationsAndRoles(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	c := startCluster(t)

	if err := Bootstrap(ctx, c.admin, c.owner, c.app, c.readonly); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	if err := Up(ctx, c.owner); err != nil {
		t.Fatalf("up: %v", err)
	}
	v, err := CurrentVersion(ctx, c.owner)
	if err != nil || v != 3 {
		t.Fatalf("version after up = %d, %v", v, err)
	}
	assertSchemaInfo(t, ctx, c.app, true)
	assertFinancialSchemas(t, ctx, c.owner, true)

	if err := Down(ctx, c.owner); err != nil {
		t.Fatalf("down ledger: %v", err)
	}
	if err := Down(ctx, c.owner); err != nil {
		t.Fatalf("down accounts: %v", err)
	}
	assertFinancialSchemas(t, ctx, c.owner, false)
	assertSchemaInfo(t, ctx, c.app, true)

	if err := Down(ctx, c.owner); err != nil {
		t.Fatalf("down platform: %v", err)
	}
	v, err = CurrentVersion(ctx, c.owner)
	if err != nil || v != 0 {
		t.Fatalf("version after down = %d, %v", v, err)
	}
	assertNoPlatform(t, ctx, c.owner)

	if err := Up(ctx, c.owner); err != nil {
		t.Fatalf("up after down: %v", err)
	}
	assertSchemaInfo(t, ctx, c.app, true)
	assertFinancialSchemas(t, ctx, c.owner, true)

	t.Run("paycore_app session settings and grants", func(t *testing.T) {
		conn, err := pgx.Connect(ctx, c.app)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close(ctx)
		var idle, isolation string
		if err := conn.QueryRow(ctx, `SHOW idle_in_transaction_session_timeout`).Scan(&idle); err != nil {
			t.Fatal(err)
		}
		if err := conn.QueryRow(ctx, `SHOW default_transaction_isolation`).Scan(&isolation); err != nil {
			t.Fatal(err)
		}
		if idle != "30s" && idle != "00:00:30" {
			t.Errorf("idle_in_transaction_session_timeout = %q", idle)
		}
		if isolation != "read committed" {
			t.Errorf("isolation = %q", isolation)
		}
		if _, err := conn.Exec(ctx, `CREATE TABLE platform.forbidden (id int)`); err == nil {
			t.Error("paycore_app created a table")
		}
		if _, err := conn.Exec(ctx, `INSERT INTO platform.schema_info(key, value) VALUES ('k','v')`); err != nil {
			t.Errorf("app insert schema_info: %v", err)
		}
		if _, err := conn.Exec(ctx, `SELECT * FROM goose_db_version`); err == nil {
			t.Error("app read goose_db_version in public")
		}
	})

	t.Run("paycore_readonly cannot write", func(t *testing.T) {
		conn, err := pgx.Connect(ctx, c.readonly)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close(ctx)
		var v string
		if err := conn.QueryRow(ctx, `SELECT value FROM platform.schema_info WHERE key = 'core'`).Scan(&v); err != nil {
			t.Fatalf("readonly select: %v", err)
		}
		if _, err := conn.Exec(ctx, `INSERT INTO platform.schema_info(key, value) VALUES ('x','y')`); err == nil {
			t.Error("readonly inserted")
		}
	})

	t.Run("paycore_ledger cannot log in", func(t *testing.T) {
		u, err := url.Parse(c.app)
		if err != nil {
			t.Fatal(err)
		}
		dsn := withUser(u, RoleLedger, "anything")
		conn, err := pgx.Connect(ctx, dsn)
		if err == nil {
			conn.Close(ctx)
			t.Fatal("paycore_ledger logged in")
		}
		if !strings.Contains(strings.ToLower(err.Error()), "login") &&
			!strings.Contains(strings.ToLower(err.Error()), "not permitted") &&
			!strings.Contains(strings.ToLower(err.Error()), "authentication") {
			t.Logf("ledger login error (acceptable): %v", err)
		}
	})

	t.Run("app pool ping", func(t *testing.T) {
		pool, err := Open(ctx, config.Postgres{
			DSN:              c.app,
			MaxConns:         2,
			MinConns:         1,
			MaxConnLifetime:  time.Minute,
			MaxConnIdleTime:  time.Minute,
			LockTimeout:      3 * time.Second,
			StatementTimeout: 5 * time.Second,
		})
		if err != nil {
			t.Fatal(err)
		}
		defer pool.Close()
		var lock, stmt string
		if err := pool.QueryRow(ctx, `SHOW lock_timeout`).Scan(&lock); err != nil {
			t.Fatal(err)
		}
		if err := pool.QueryRow(ctx, `SHOW statement_timeout`).Scan(&stmt); err != nil {
			t.Fatal(err)
		}
		if lock != "3s" && lock != "00:00:03" {
			t.Errorf("pool lock_timeout = %q", lock)
		}
		if stmt != "5s" && stmt != "00:00:05" {
			t.Errorf("pool statement_timeout = %q", stmt)
		}
	})
}

func assertSchemaInfo(t *testing.T, ctx context.Context, dsn string, want bool) {
	t.Helper()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	var exists bool
	err = conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = 'platform' AND table_name = 'schema_info')`).Scan(&exists)
	if err != nil {
		t.Fatal(err)
	}
	if exists != want {
		t.Fatalf("schema_info exists = %v, want %v", exists, want)
	}
	if want {
		var value string
		if err := conn.QueryRow(ctx, `SELECT value FROM platform.schema_info WHERE key = 'core'`).Scan(&value); err != nil {
			t.Fatal(err)
		}
		if value != "initialized" {
			t.Errorf("schema_info.core = %q", value)
		}
	}
}

func assertNoPlatform(t *testing.T, ctx context.Context, dsn string) {
	t.Helper()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	var exists bool
	if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = 'platform')`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("platform schema still present after down")
	}
}

func assertFinancialSchemas(t *testing.T, ctx context.Context, dsn string, wantAccountsLedger bool) {
	t.Helper()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	rows, err := conn.Query(ctx, `SELECT nspname FROM pg_namespace WHERE nspname = ANY($1)`,
		[]string{"ledger", "accounts", "auth", "merchants", "payments", "transfers", "commissions", "limits"})
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	found := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		found[n] = true
	}
	if found["payments"] || found["transfers"] || found["commissions"] || found["limits"] {
		t.Fatalf("out-of-scope schemas present: %v", found)
	}
	if found["accounts"] != wantAccountsLedger || found["ledger"] != wantAccountsLedger {
		t.Fatalf("accounts/ledger present=%v want=%v (found %v)", found["accounts"] && found["ledger"], wantAccountsLedger, found)
	}
	if found["auth"] != wantAccountsLedger || found["merchants"] != wantAccountsLedger {
		t.Fatalf("stub auth/merchants present=%v want=%v (found %v)", found["auth"] && found["merchants"], wantAccountsLedger, found)
	}
}

func TestOpenEmptyDSN(t *testing.T) {
	pool, err := Open(context.Background(), config.Postgres{})
	if err != nil || pool != nil {
		t.Fatalf("Open empty DSN = %v %v", pool, err)
	}
}
