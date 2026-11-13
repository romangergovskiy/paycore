package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/romangergovskiy/paycore/internal/auth/application"
	"github.com/romangergovskiy/paycore/internal/platform/authcrypto"
	"github.com/romangergovskiy/paycore/internal/platform/config"
	"github.com/romangergovskiy/paycore/internal/platform/postgres"
)

const (
	envBootstrapAdminEmail    = "PAYCORE_BOOTSTRAP_ADMIN_EMAIL"
	envBootstrapAdminPassword = "PAYCORE_BOOTSTRAP_ADMIN_PASSWORD"
	envPostgresDSN            = "PAYCORE_POSTGRES_DSN"
)

func withoutBootstrapAdminVars(environ []string) []string {
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		k, _, _ := strings.Cut(kv, "=")
		if k == envBootstrapAdminEmail || k == envBootstrapAdminPassword {
			continue
		}
		out = append(out, kv)
	}
	return out
}

type bootstrapAdminInput struct {
	dsn, email, password string
}

func parseBootstrapAdminEnv(environ []string) (bootstrapAdminInput, error) {
	vars := make(map[string]string, len(environ))
	for _, kv := range environ {
		k, v, _ := strings.Cut(kv, "=")
		vars[k] = v
	}
	in := bootstrapAdminInput{
		dsn:      vars[envPostgresDSN],
		email:    vars[envBootstrapAdminEmail],
		password: vars[envBootstrapAdminPassword],
	}
	var errs []error
	if in.dsn == "" {
		errs = append(errs, fmt.Errorf("%s is required", envPostgresDSN))
	} else {
		u, err := url.Parse(in.dsn)
		if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Host == "" {
			errs = append(errs, fmt.Errorf("%s must be a postgres:// URL with host", envPostgresDSN))
		}
	}
	if strings.TrimSpace(in.email) == "" {
		errs = append(errs, fmt.Errorf("%s is required", envBootstrapAdminEmail))
	}
	if in.password == "" {
		errs = append(errs, fmt.Errorf("%s is required", envBootstrapAdminPassword))
	}
	if len(errs) > 0 {
		return bootstrapAdminInput{}, fmt.Errorf("%w:\n%w", config.ErrInvalid, errors.Join(errs...))
	}
	return in, nil
}

func runBootstrapAdmin(ctx context.Context, environ []string) error {
	in, err := parseBootstrapAdminEnv(environ)
	if err != nil {
		return err
	}
	pool, err := postgres.Open(ctx, config.Postgres{
		DSN:              in.dsn,
		MaxConns:         10,
		MinConns:         1,
		MaxConnLifetime:  30 * time.Minute,
		MaxConnIdleTime:  5 * time.Minute,
		LockTimeout:      3 * time.Second,
		StatementTimeout: 5 * time.Second,
	})
	if err != nil {
		return err
	}
	if pool == nil {
		return fmt.Errorf("%w: %s is required", config.ErrInvalid, envPostgresDSN)
	}
	defer pool.Close()

	svc := application.Service{
		Hasher: authcrypto.NewArgon2idHasher(),
		Clock:  authcrypto.NewSystemClock(),
	}
	_, err = application.BootstrapFirstAdminIn(ctx, postgres.NewAuthUoW(pool), svc, in.email, in.password)
	return err
}
