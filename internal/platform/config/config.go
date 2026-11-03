// Package config loads the service configuration from PAYCORE_* environment
// variables. Every variable is optional and has a default; invalid values and
// unknown PAYCORE_* names are reported together and wrap ErrInvalid.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

const prefix = "PAYCORE_"

// ErrInvalid is wrapped by every error returned from Load.
var ErrInvalid = errors.New("invalid configuration")

type Env string

const (
	EnvLocal      Env = "local"
	EnvTest       Env = "test"
	EnvProduction Env = "production"
)

type LogFormat string

const (
	LogFormatText LogFormat = "text"
	LogFormatJSON LogFormat = "json"
)

type Config struct {
	Env             Env
	HTTP            HTTP
	Log             Log
	Postgres        Postgres
	ShutdownTimeout time.Duration
}

// Postgres holds pool and DSN settings. The API uses DSN (paycore_app).
// MigrateDSN, AdminDSN and ReadonlyDSN are for cmd/migrate and tests.
type Postgres struct {
	DSN              string
	MigrateDSN       string
	AdminDSN         string
	ReadonlyDSN      string
	MaxConns         int32
	MinConns         int32
	MaxConnLifetime  time.Duration
	MaxConnIdleTime  time.Duration
	LockTimeout      time.Duration
	StatementTimeout time.Duration
}

type HTTP struct {
	Addr              string
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	// RequestTimeout is the deadline of every request context
	// (docs/database/concurrency.md §10).
	RequestTimeout time.Duration
	MaxBodyBytes   int64
}

type Log struct {
	Level  slog.Level
	Format LogFormat
}

const (
	varEnv              = prefix + "ENV"
	varHTTPAddr         = prefix + "HTTP_ADDR"
	varHTTPReadHeader   = prefix + "HTTP_READ_HEADER_TIMEOUT"
	varHTTPRead         = prefix + "HTTP_READ_TIMEOUT"
	varHTTPWrite        = prefix + "HTTP_WRITE_TIMEOUT"
	varHTTPIdle         = prefix + "HTTP_IDLE_TIMEOUT"
	varHTTPRequest      = prefix + "HTTP_REQUEST_TIMEOUT"
	varHTTPMaxBodyBytes = prefix + "HTTP_MAX_BODY_BYTES"
	varShutdownTimeout  = prefix + "SHUTDOWN_TIMEOUT"
	varLogLevel         = prefix + "LOG_LEVEL"
	varLogFormat        = prefix + "LOG_FORMAT"
	varPostgresDSN      = prefix + "POSTGRES_DSN"
	varPostgresMigrate  = prefix + "POSTGRES_MIGRATE_DSN"
	varPostgresAdmin    = prefix + "POSTGRES_ADMIN_DSN"
	varPostgresReadonly = prefix + "POSTGRES_READONLY_DSN"
	varPostgresMaxConns = prefix + "POSTGRES_MAX_CONNS"
	varPostgresMinConns = prefix + "POSTGRES_MIN_CONNS"
	varPostgresLifetime = prefix + "POSTGRES_MAX_CONN_LIFETIME"
	varPostgresIdle     = prefix + "POSTGRES_MAX_CONN_IDLE_TIME"
	varPostgresLock     = prefix + "POSTGRES_LOCK_TIMEOUT"
	varPostgresStmt     = prefix + "POSTGRES_STATEMENT_TIMEOUT"
)

var known = []string{
	varEnv, varHTTPAddr, varHTTPReadHeader, varHTTPRead, varHTTPWrite, varHTTPIdle,
	varHTTPRequest, varHTTPMaxBodyBytes, varShutdownTimeout, varLogLevel, varLogFormat,
	varPostgresDSN, varPostgresMigrate, varPostgresAdmin, varPostgresReadonly,
	varPostgresMaxConns, varPostgresMinConns, varPostgresLifetime, varPostgresIdle,
	varPostgresLock, varPostgresStmt,
}

// Load builds a Config from environ, given in the os.Environ "KEY=value" form.
func Load(environ []string) (Config, error) {
	vars := make(map[string]string, len(environ))
	for _, kv := range environ {
		k, v, _ := strings.Cut(kv, "=")
		vars[k] = v
	}

	p := parser{vars: vars}
	cfg := Config{
		Env: Env(p.str(varEnv, string(EnvLocal))),
		HTTP: HTTP{
			Addr:              p.str(varHTTPAddr, ":8080"),
			ReadHeaderTimeout: p.duration(varHTTPReadHeader, 5*time.Second),
			ReadTimeout:       p.duration(varHTTPRead, 10*time.Second),
			WriteTimeout:      p.duration(varHTTPWrite, 10*time.Second),
			IdleTimeout:       p.duration(varHTTPIdle, 60*time.Second),
			RequestTimeout:    p.duration(varHTTPRequest, 5*time.Second),
			MaxBodyBytes:      p.positiveInt(varHTTPMaxBodyBytes, 1<<20),
		},
		ShutdownTimeout: p.duration(varShutdownTimeout, 20*time.Second),
		Log: Log{
			Level: p.level(varLogLevel, slog.LevelInfo),
		},
		Postgres: Postgres{
			DSN:              p.dsn(varPostgresDSN),
			MigrateDSN:       p.dsn(varPostgresMigrate),
			AdminDSN:         p.dsn(varPostgresAdmin),
			ReadonlyDSN:      p.dsn(varPostgresReadonly),
			MaxConns:         int32(p.positiveInt(varPostgresMaxConns, 10)),
			MinConns:         int32(p.nonNegativeInt(varPostgresMinConns, 1)),
			MaxConnLifetime:  p.duration(varPostgresLifetime, 30*time.Minute),
			MaxConnIdleTime:  p.duration(varPostgresIdle, 5*time.Minute),
			LockTimeout:      p.duration(varPostgresLock, 3*time.Second),
			StatementTimeout: p.duration(varPostgresStmt, 5*time.Second),
		},
	}

	switch cfg.Env {
	case EnvLocal, EnvTest, EnvProduction:
	default:
		p.fail(varEnv, "must be one of local, test, production")
	}

	defaultFormat := LogFormatJSON
	if cfg.Env == EnvLocal {
		defaultFormat = LogFormatText
	}
	cfg.Log.Format = LogFormat(p.str(varLogFormat, string(defaultFormat)))
	if cfg.Log.Format != LogFormatText && cfg.Log.Format != LogFormatJSON {
		p.fail(varLogFormat, "must be text or json")
	}

	if _, _, err := net.SplitHostPort(cfg.HTTP.Addr); err != nil {
		p.fail(varHTTPAddr, "must be host:port, e.g. :8080")
	}
	// The write timeout must leave room to send the error response after the
	// request deadline fires.
	if cfg.HTTP.WriteTimeout > 0 && cfg.HTTP.RequestTimeout > 0 && cfg.HTTP.WriteTimeout <= cfg.HTTP.RequestTimeout {
		p.fail(varHTTPWrite, "must be greater than "+varHTTPRequest)
	}
	if cfg.Postgres.MinConns > cfg.Postgres.MaxConns {
		p.fail(varPostgresMinConns, "must be less than or equal to "+varPostgresMaxConns)
	}

	var unknown []string
	for k := range vars {
		if strings.HasPrefix(k, prefix) && !slices.Contains(known, k) {
			unknown = append(unknown, k)
		}
	}
	slices.Sort(unknown)
	for _, k := range unknown {
		p.fail(k, "unknown variable")
	}

	if len(p.errs) > 0 {
		return Config{}, fmt.Errorf("%w:\n%w", ErrInvalid, errors.Join(p.errs...))
	}
	return cfg, nil
}

type parser struct {
	vars map[string]string
	errs []error
}

func (p *parser) fail(name, reason string) {
	p.errs = append(p.errs, fmt.Errorf("%s: %s", name, reason))
}

func (p *parser) str(name, def string) string {
	if v, ok := p.vars[name]; ok && v != "" {
		return v
	}
	return def
}

func (p *parser) duration(name string, def time.Duration) time.Duration {
	v, ok := p.vars[name]
	if !ok || v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		p.fail(name, fmt.Sprintf("must be a positive duration such as 5s, got %q", v))
		return 0
	}
	return d
}

func (p *parser) dsn(name string) string {
	v := p.str(name, "")
	if v == "" {
		return ""
	}
	u, err := url.Parse(v)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Host == "" {
		p.fail(name, "must be a postgres:// URL with host, e.g. postgres://paycore_app:pass@127.0.0.1:5432/paycore")
		return v
	}
	return v
}

func (p *parser) nonNegativeInt(name string, def int64) int64 {
	v, ok := p.vars[name]
	if !ok || v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		p.fail(name, fmt.Sprintf("must be a non-negative integer, got %q", v))
		return 0
	}
	return n
}

func (p *parser) positiveInt(name string, def int64) int64 {
	v, ok := p.vars[name]
	if !ok || v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		p.fail(name, fmt.Sprintf("must be a positive integer, got %q", v))
		return 0
	}
	return n
}

func (p *parser) level(name string, def slog.Level) slog.Level {
	v, ok := p.vars[name]
	if !ok || v == "" {
		return def
	}
	var l slog.Level
	if err := l.UnmarshalText([]byte(v)); err != nil {
		p.fail(name, fmt.Sprintf("must be debug, info, warn or error, got %q", v))
		return def
	}
	return l
}
