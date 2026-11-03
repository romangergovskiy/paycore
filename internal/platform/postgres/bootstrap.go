package postgres

import (
	"context"
	"fmt"
	"regexp"

	"github.com/jackc/pgx/v5"
)

const (
	RoleOwner    = "paycore_owner"
	RoleLedger   = "paycore_ledger"
	RoleApp      = "paycore_app"
	RoleReadonly = "paycore_readonly"
)

var identRE = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// Bootstrap creates the four database roles from D-33 and the application
// database, using a superuser connection. It is idempotent.
func Bootstrap(ctx context.Context, adminDSN, migrateDSN, appDSN, readonlyDSN string) error {
	migrateURL, err := parseDSN(migrateDSN)
	if err != nil {
		return fmt.Errorf("migrate DSN: %w", err)
	}
	appURL, err := parseDSN(appDSN)
	if err != nil {
		return fmt.Errorf("app DSN: %w", err)
	}
	roURL, err := parseDSN(readonlyDSN)
	if err != nil {
		return fmt.Errorf("readonly DSN: %w", err)
	}
	adminURL, err := parseDSN(adminDSN)
	if err != nil {
		return fmt.Errorf("admin DSN: %w", err)
	}

	ownerPass, ok := migrateURL.User.Password()
	if !ok || ownerPass == "" {
		return fmt.Errorf("migrate DSN: password for %s is required", RoleOwner)
	}
	appPass, ok := appURL.User.Password()
	if !ok || appPass == "" {
		return fmt.Errorf("app DSN: password for %s is required", RoleApp)
	}
	roPass, ok := roURL.User.Password()
	if !ok || roPass == "" {
		return fmt.Errorf("readonly DSN: password for %s is required", RoleReadonly)
	}
	if migrateURL.User.Username() != RoleOwner {
		return fmt.Errorf("migrate DSN: user must be %s", RoleOwner)
	}
	if appURL.User.Username() != RoleApp {
		return fmt.Errorf("app DSN: user must be %s", RoleApp)
	}
	if roURL.User.Username() != RoleReadonly {
		return fmt.Errorf("readonly DSN: user must be %s", RoleReadonly)
	}

	targetDB := dbName(migrateURL)
	if !identRE.MatchString(targetDB) {
		return fmt.Errorf("database name %q is not a safe identifier", targetDB)
	}
	if dbName(appURL) != targetDB || dbName(roURL) != targetDB {
		return fmt.Errorf("app, migrate and readonly DSNs must name the same database")
	}

	conn, err := pgx.Connect(ctx, withDatabase(adminURL, "postgres"))
	if err != nil {
		return fmt.Errorf("connect as admin: %w", err)
	}
	defer conn.Close(ctx)

	if err := ensureLoginRole(ctx, conn, RoleOwner, ownerPass); err != nil {
		return err
	}
	if err := ensureLoginRole(ctx, conn, RoleApp, appPass); err != nil {
		return err
	}
	if err := ensureLoginRole(ctx, conn, RoleReadonly, roPass); err != nil {
		return err
	}
	if _, err := conn.Exec(ctx, `
		DO $do$ BEGIN
			IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'paycore_ledger') THEN
				CREATE ROLE paycore_ledger NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE;
			ELSE
				ALTER ROLE paycore_ledger WITH NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE;
			END IF;
		END $do$`); err != nil {
		return fmt.Errorf("ensure %s: %w", RoleLedger, err)
	}

	if _, err := conn.Exec(ctx, `ALTER ROLE paycore_app SET idle_in_transaction_session_timeout = '30s'`); err != nil {
		return fmt.Errorf("set idle_in_transaction_session_timeout: %w", err)
	}
	if _, err := conn.Exec(ctx, `ALTER ROLE paycore_app SET default_transaction_isolation TO 'read committed'`); err != nil {
		return fmt.Errorf("set default_transaction_isolation: %w", err)
	}

	var exists bool
	if err := conn.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname = $1)`, targetDB).Scan(&exists); err != nil {
		return fmt.Errorf("lookup database: %w", err)
	}
	if !exists {
		if _, err := conn.Exec(ctx, `CREATE DATABASE `+targetDB+` OWNER paycore_owner`); err != nil {
			return fmt.Errorf("create database: %w", err)
		}
	} else {
		if _, err := conn.Exec(ctx, `ALTER DATABASE `+targetDB+` OWNER TO paycore_owner`); err != nil {
			return fmt.Errorf("alter database owner: %w", err)
		}
	}

	if _, err := conn.Exec(ctx, `REVOKE ALL ON DATABASE `+targetDB+` FROM PUBLIC`); err != nil {
		return fmt.Errorf("revoke public on database: %w", err)
	}
	if _, err := conn.Exec(ctx, `GRANT CONNECT, CREATE, TEMP ON DATABASE `+targetDB+` TO paycore_owner`); err != nil {
		return fmt.Errorf("grant owner on database: %w", err)
	}
	if _, err := conn.Exec(ctx, `GRANT CONNECT ON DATABASE `+targetDB+` TO paycore_app, paycore_readonly`); err != nil {
		return fmt.Errorf("grant connect: %w", err)
	}
	return nil
}

func ensureLoginRole(ctx context.Context, conn *pgx.Conn, role, password string) error {
	sql := fmt.Sprintf(`
		DO $do$ BEGIN
			IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = %s) THEN
				CREATE ROLE %s LOGIN PASSWORD %s NOSUPERUSER NOCREATEDB NOCREATEROLE;
			ELSE
				ALTER ROLE %s WITH LOGIN PASSWORD %s NOSUPERUSER NOCREATEDB NOCREATEROLE;
			END IF;
		END $do$`,
		quoteLiteral(role), role, quoteLiteral(password), role, quoteLiteral(password))
	if _, err := conn.Exec(ctx, sql); err != nil {
		return fmt.Errorf("ensure role %s: %w", role, err)
	}
	return nil
}
