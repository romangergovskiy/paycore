package config

import (
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// testJWTSecret is 32 ASCII bytes; it is not a production secret.
const testJWTSecret = "0123456789abcdef0123456789abcdef"

func withJWTSecret(env ...string) []string {
	return append(append([]string{}, env...), varAuthJWTSecret+"="+testJWTSecret)
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load(withJWTSecret())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := Config{
		Env: EnvLocal,
		HTTP: HTTP{
			Addr:              ":8080",
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       10 * time.Second,
			WriteTimeout:      10 * time.Second,
			IdleTimeout:       60 * time.Second,
			RequestTimeout:    5 * time.Second,
			MaxBodyBytes:      1 << 20,
		},
		Log: Log{Level: slog.LevelInfo, Format: LogFormatText},
		Postgres: Postgres{
			MaxConns:         10,
			MinConns:         1,
			MaxConnLifetime:  30 * time.Minute,
			MaxConnIdleTime:  5 * time.Minute,
			LockTimeout:      3 * time.Second,
			StatementTimeout: 5 * time.Second,
		},
		Auth:            Auth{JWTSecret: testJWTSecret},
		ShutdownTimeout: 20 * time.Second,
	}
	if cfg != want {
		t.Errorf("defaults:\n got %+v\nwant %+v", cfg, want)
	}
}

func TestLoadOverrides(t *testing.T) {
	cfg, err := Load(withJWTSecret(
		"PAYCORE_ENV=production",
		"PAYCORE_HTTP_ADDR=127.0.0.1:9000",
		"PAYCORE_HTTP_REQUEST_TIMEOUT=2s",
		"PAYCORE_HTTP_WRITE_TIMEOUT=3s",
		"PAYCORE_HTTP_MAX_BODY_BYTES=2048",
		"PAYCORE_SHUTDOWN_TIMEOUT=1m",
		"PAYCORE_LOG_LEVEL=debug",
		"HOME=/ignored",
	))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Env != EnvProduction || cfg.HTTP.Addr != "127.0.0.1:9000" ||
		cfg.HTTP.RequestTimeout != 2*time.Second || cfg.HTTP.WriteTimeout != 3*time.Second ||
		cfg.HTTP.MaxBodyBytes != 2048 || cfg.ShutdownTimeout != time.Minute ||
		cfg.Log.Level != slog.LevelDebug {
		t.Errorf("overrides not applied: %+v", cfg)
	}
	if cfg.Log.Format != LogFormatJSON {
		t.Errorf("log format outside local = %q, want json", cfg.Log.Format)
	}
}

func TestLoadReportsAllErrors(t *testing.T) {
	_, err := Load(withJWTSecret(
		"PAYCORE_ENV=staging",
		"PAYCORE_HTTP_ADDR=8080",
		"PAYCORE_HTTP_READ_TIMEOUT=-1s",
		"PAYCORE_HTTP_MAX_BODY_BYTES=lots",
		"PAYCORE_LOG_LEVEL=loud",
		"PAYCORE_LOG_FORMAT=xml",
		"PAYCORE_HTTP_ADRR=:1",
	))
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
	for _, name := range []string{
		"PAYCORE_ENV", "PAYCORE_HTTP_ADDR:", "PAYCORE_HTTP_READ_TIMEOUT",
		"PAYCORE_HTTP_MAX_BODY_BYTES", "PAYCORE_LOG_LEVEL", "PAYCORE_LOG_FORMAT",
		"PAYCORE_HTTP_ADRR: unknown variable",
	} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error does not mention %s:\n%v", name, err)
		}
	}
}

func TestLoadPostgresDSN(t *testing.T) {
	cfg, err := Load(withJWTSecret(
		"PAYCORE_POSTGRES_DSN=postgres://paycore_app:secret@127.0.0.1:5432/paycore?sslmode=disable",
		"PAYCORE_POSTGRES_MAX_CONNS=4",
		"PAYCORE_POSTGRES_MIN_CONNS=0",
	))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Postgres.DSN == "" || cfg.Postgres.MaxConns != 4 || cfg.Postgres.MinConns != 0 {
		t.Errorf("postgres: %+v", cfg.Postgres)
	}

	_, err = Load(withJWTSecret("PAYCORE_POSTGRES_DSN=localhost:5432"))
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "PAYCORE_POSTGRES_DSN") {
		t.Fatalf("err = %v, want invalid DSN", err)
	}
}

func TestLoadWriteTimeoutMustExceedRequestTimeout(t *testing.T) {
	_, err := Load(withJWTSecret("PAYCORE_HTTP_REQUEST_TIMEOUT=10s", "PAYCORE_HTTP_WRITE_TIMEOUT=10s"))
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "PAYCORE_HTTP_WRITE_TIMEOUT") {
		t.Fatalf("err = %v, want write timeout error", err)
	}
}

func TestLoadJWTSecret(t *testing.T) {
	_, err := Load(nil)
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), varAuthJWTSecret) {
		t.Fatalf("missing secret: %v", err)
	}
	if strings.Contains(err.Error(), testJWTSecret) {
		t.Error("error leaked a secret value")
	}

	_, err = Load([]string{varAuthJWTSecret + "="})
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), varAuthJWTSecret) {
		t.Fatalf("empty secret: %v", err)
	}

	_, err = Load([]string{varAuthJWTSecret + "=" + testJWTSecret[:31]})
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), varAuthJWTSecret) {
		t.Fatalf("31-byte secret: %v", err)
	}

	cfg, err := Load([]string{varAuthJWTSecret + "=" + testJWTSecret})
	if err != nil {
		t.Fatalf("32-byte secret: %v", err)
	}
	if cfg.Auth.JWTSecret != testJWTSecret {
		t.Errorf("JWTSecret not stored")
	}

	_, err = Load([]string{varAuthJWTSecret + "=" + testJWTSecret, "PAYCORE_AUTH_ISSUER=other"})
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "PAYCORE_AUTH_ISSUER: unknown variable") {
		t.Fatalf("unknown AUTH var: %v", err)
	}
}
