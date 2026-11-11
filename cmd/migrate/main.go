// Command migrate bootstraps roles, applies Goose SQL migrations, and
// creates the first ACTIVE ADMIN.
//
//	migrate bootstrap         create roles and the database (superuser)
//	migrate bootstrap-admin   first ACTIVE ADMIN (paycore_app)
//	migrate up                apply pending migrations (paycore_owner)
//	migrate down              roll back one migration (paycore_owner)
//	migrate status            print applied versions
//
// Exit codes: 0 success, 1 runtime error, 2 invalid configuration.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/romangergovskiy/paycore/internal/platform/config"
	"github.com/romangergovskiy/paycore/internal/platform/postgres"
)

func main() {
	err := run(os.Args[1:], os.Environ())
	if err != nil {
		fmt.Fprintln(os.Stderr, "paycore-migrate:", err)
	}
	os.Exit(exitStatus(err))
}

func exitStatus(err error) int {
	if err == nil {
		return 0
	}
	if errors.Is(err, config.ErrInvalid) || errors.Is(err, errUsage) {
		return 2
	}
	return 1
}

var errUsage = errors.New("usage: migrate bootstrap|bootstrap-admin|up|down|status")

func run(args, environ []string) error {
	if len(args) != 1 {
		return errUsage
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	switch args[0] {
	case "bootstrap-admin":
		return runBootstrapAdmin(ctx, environ)
	case "bootstrap", "up", "down", "status":
	default:
		return errUsage
	}

	cfg, err := config.Load(withoutBootstrapAdminVars(environ))
	if err != nil {
		return err
	}
	switch args[0] {
	case "bootstrap":
		if cfg.Postgres.AdminDSN == "" || cfg.Postgres.MigrateDSN == "" ||
			cfg.Postgres.DSN == "" || cfg.Postgres.ReadonlyDSN == "" {
			return fmt.Errorf("%w: bootstrap needs PAYCORE_POSTGRES_ADMIN_DSN, PAYCORE_POSTGRES_MIGRATE_DSN, PAYCORE_POSTGRES_DSN and PAYCORE_POSTGRES_READONLY_DSN", config.ErrInvalid)
		}
		return postgres.Bootstrap(ctx, cfg.Postgres.AdminDSN, cfg.Postgres.MigrateDSN, cfg.Postgres.DSN, cfg.Postgres.ReadonlyDSN)
	case "up":
		if cfg.Postgres.MigrateDSN == "" {
			return fmt.Errorf("%w: %s is required", config.ErrInvalid, "PAYCORE_POSTGRES_MIGRATE_DSN")
		}
		return postgres.Up(ctx, cfg.Postgres.MigrateDSN)
	case "down":
		if cfg.Postgres.MigrateDSN == "" {
			return fmt.Errorf("%w: %s is required", config.ErrInvalid, "PAYCORE_POSTGRES_MIGRATE_DSN")
		}
		return postgres.Down(ctx, cfg.Postgres.MigrateDSN)
	case "status":
		if cfg.Postgres.MigrateDSN == "" {
			return fmt.Errorf("%w: %s is required", config.ErrInvalid, "PAYCORE_POSTGRES_MIGRATE_DSN")
		}
		return postgres.Status(ctx, cfg.Postgres.MigrateDSN)
	default:
		return errUsage
	}
}
