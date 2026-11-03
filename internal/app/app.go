// Package app wires the service together and owns its lifecycle: load
// configuration, start the HTTP server, and shut down gracefully when ctx is
// cancelled.
package app

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"

	"github.com/romangergovskiy/paycore/internal/platform/config"
	"github.com/romangergovskiy/paycore/internal/platform/httpserver"
	"github.com/romangergovskiy/paycore/internal/platform/logging"
	"github.com/romangergovskiy/paycore/internal/platform/postgres"
)

// Version is set at build time with -ldflags "-X .../internal/app.Version=...".
var Version = "dev"

// Run blocks until ctx is cancelled or the server fails. Configuration errors
// wrap config.ErrInvalid.
func Run(ctx context.Context, environ []string, logOut io.Writer) error {
	cfg, err := config.Load(environ)
	if err != nil {
		return err
	}

	log := logging.New(logOut, logging.Format(cfg.Log.Format), cfg.Log.Level).With(
		slog.String("service", "paycore-api"),
		slog.String("env", string(cfg.Env)),
		slog.String("version", Version),
	)
	slog.SetDefault(log)

	pool, err := postgres.Open(ctx, cfg.Postgres)
	if err != nil {
		return err
	}
	if pool != nil {
		defer pool.Close()
		log.Info("postgres connected", slog.String("dsn", postgres.RedactDSN(cfg.Postgres.DSN)))
	}

	ln, err := net.Listen("tcp", cfg.HTTP.Addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.HTTP.Addr, err)
	}
	srv := httpserver.New(cfg.HTTP, log)
	if pool != nil {
		srv.SetReadyCheck(pool.Ping)
	}

	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	log.Info("http server started", slog.String("addr", ln.Addr().String()))

	select {
	case err := <-served:
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
	}

	log.Info("shutdown started", slog.Duration("timeout", cfg.ShutdownTimeout))
	srv.SetReady(false)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		_ = srv.Close()
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	if err := <-served; err != nil {
		return fmt.Errorf("http server: %w", err)
	}

	log.Info("shutdown complete")
	return nil
}
