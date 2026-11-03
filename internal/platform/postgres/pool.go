package postgres

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/romangergovskiy/paycore/internal/platform/config"
)

const tcpKeepAlive = 15 * time.Second

// Open creates a pgx pool for the application role. An empty DSN returns
// (nil, nil): the API can start without a database in local unit tests.
func Open(ctx context.Context, cfg config.Postgres) (*pgxpool.Pool, error) {
	if cfg.DSN == "" {
		return nil, nil
	}
	pc, err := pgxpool.ParseConfig(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("parse postgres DSN: %w", err)
	}
	pc.MaxConns = int32(cfg.MaxConns)
	pc.MinConns = int32(cfg.MinConns)
	pc.MaxConnLifetime = cfg.MaxConnLifetime
	pc.MaxConnIdleTime = cfg.MaxConnIdleTime
	pc.ConnConfig.ConnectTimeout = 5 * time.Second
	if pc.ConnConfig.RuntimeParams == nil {
		pc.ConnConfig.RuntimeParams = map[string]string{}
	}
	pc.ConnConfig.RuntimeParams["lock_timeout"] = cfg.LockTimeout.String()
	pc.ConnConfig.RuntimeParams["statement_timeout"] = cfg.StatementTimeout.String()
	pc.ConnConfig.DialFunc = func(ctx context.Context, network, addr string) (net.Conn, error) {
		d := net.Dialer{KeepAlive: tcpKeepAlive, Timeout: 5 * time.Second}
		return d.DialContext(ctx, network, addr)
	}

	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return nil, fmt.Errorf("open postgres pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	return pool, nil
}
