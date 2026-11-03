// Command migrate bootstraps roles and applies Goose SQL migrations.
//
//	migrate bootstrap   create roles and the database (superuser)
//	migrate up          apply pending migrations (paycore_owner)
//	migrate down        roll back one migration (paycore_owner)
//	migrate status      print applied versions
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
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "paycore-migrate:", err)
		if errors.Is(err, config.ErrInvalid) || errors.Is(err, errUsage) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}

var errUsage = errors.New("usage: migrate bootstrap|up|down|status")

func run(args []string) error {
	if len(args) != 1 {
		return errUsage
	}
	cfg, err := config.Load(os.Environ())
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

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
