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

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/romangergovskiy/paycore/internal/auth/application"
	authhttp "github.com/romangergovskiy/paycore/internal/auth/http"
	"github.com/romangergovskiy/paycore/internal/platform/authcrypto"
	"github.com/romangergovskiy/paycore/internal/platform/config"
	"github.com/romangergovskiy/paycore/internal/platform/httpserver"
	"github.com/romangergovskiy/paycore/internal/platform/logging"
	"github.com/romangergovskiy/paycore/internal/platform/postgres"
)

// authRuntime is the Identity/Auth composition root.
type authRuntime struct {
	uow    application.UnitOfWork
	svc    application.Service // hasher, issuer, signer, clock; stores come from uow
	read   application.Service // Users/Sessions/Roles on the pool for ValidateSession
	parser httpserver.AccessParser
}

func newAuthRuntime(pool *pgxpool.Pool, jwtSecret string) (*authRuntime, error) {
	if pool == nil {
		return nil, nil
	}
	signer, err := authcrypto.NewHS256([]byte(jwtSecret))
	if err != nil {
		return nil, err
	}
	base := application.Service{
		Hasher: authcrypto.NewArgon2idHasher(),
		Issuer: authcrypto.NewOpaqueRefresh(),
		Access: signer,
		Clock:  authcrypto.NewSystemClock(),
	}
	read := base
	read.Users = postgres.NewAuthUsers(pool)
	read.Sessions = postgres.NewAuthSessions(pool)
	read.Roles = postgres.NewAuthRoles(pool)
	return &authRuntime{
		uow:    postgres.NewAuthUoW(pool),
		svc:    base,
		read:   read,
		parser: signer,
	}, nil
}

// Version is set at build time with -ldflags "-X .../internal/app.Version=...".
var Version = "dev"

// Run blocks until ctx is cancelled or the server fails. Configuration errors
// wrap config.ErrInvalid.
func Run(ctx context.Context, environ []string, logOut io.Writer) error {
	cfg, err := config.Load(environ)
	if err != nil {
		return err
	}
	if cfg.Env == config.EnvProduction && cfg.Postgres.DSN == "" {
		return fmt.Errorf("%w: PAYCORE_POSTGRES_DSN is required in production", config.ErrInvalid)
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

	auth, err := newAuthRuntime(pool, cfg.Auth.JWTSecret)
	if err != nil {
		return err
	}

	ln, err := net.Listen("tcp", cfg.HTTP.Addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.HTTP.Addr, err)
	}
	srv := httpserver.New(cfg.HTTP, log)
	if pool != nil {
		srv.SetReadyCheck(pool.Ping)
	}
	if auth != nil {
		authhttp.Mount(srv, authhttp.New(auth.uow, auth.svc, auth.parser, auth.read))
		log.Info("auth runtime ready")
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
