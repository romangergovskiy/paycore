package postgres

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/pressly/goose/v3"

	"github.com/romangergovskiy/paycore/migrations"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func init() {
	goose.SetBaseFS(migrations.FS)
}

func withGoose(ctx context.Context, dsn string, fn func(*sql.DB) error) error {
	if err := goose.SetDialect("postgres"); err != nil {
		return err
	}
	if _, err := parseDSN(dsn); err != nil {
		return err
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("ping: %w", err)
	}
	return fn(db)
}

// Up applies all pending Goose migrations as paycore_owner.
func Up(ctx context.Context, dsn string) error {
	return withGoose(ctx, dsn, func(db *sql.DB) error {
		if err := goose.UpContext(ctx, db, "."); err != nil {
			return fmt.Errorf("goose up: %w", err)
		}
		return nil
	})
}

// UpTo applies Goose migrations up to version as paycore_owner.
func UpTo(ctx context.Context, dsn string, version int64) error {
	return withGoose(ctx, dsn, func(db *sql.DB) error {
		if err := goose.UpToContext(ctx, db, ".", version); err != nil {
			return fmt.Errorf("goose up-to %d: %w", version, err)
		}
		return nil
	})
}

// Down rolls back a single Goose migration as paycore_owner.
func Down(ctx context.Context, dsn string) error {
	return withGoose(ctx, dsn, func(db *sql.DB) error {
		if err := goose.DownContext(ctx, db, "."); err != nil {
			return fmt.Errorf("goose down: %w", err)
		}
		return nil
	})
}

// Status prints migration status to stdout.
func Status(ctx context.Context, dsn string) error {
	return withGoose(ctx, dsn, func(db *sql.DB) error {
		if err := goose.StatusContext(ctx, db, "."); err != nil {
			return fmt.Errorf("goose status: %w", err)
		}
		return nil
	})
}

// CurrentVersion returns the Goose version of the database.
func CurrentVersion(ctx context.Context, dsn string) (int64, error) {
	var version int64
	err := withGoose(ctx, dsn, func(db *sql.DB) error {
		v, err := goose.GetDBVersionContext(ctx, db)
		if err != nil {
			return err
		}
		version = v
		return nil
	})
	return version, err
}
