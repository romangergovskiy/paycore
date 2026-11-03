package config

import (
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load(nil)
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
		Log:             Log{Level: slog.LevelInfo, Format: LogFormatText},
		ShutdownTimeout: 20 * time.Second,
	}
	if cfg != want {
		t.Errorf("defaults:\n got %+v\nwant %+v", cfg, want)
	}
}

func TestLoadOverrides(t *testing.T) {
	cfg, err := Load([]string{
		"PAYCORE_ENV=production",
		"PAYCORE_HTTP_ADDR=127.0.0.1:9000",
		"PAYCORE_HTTP_REQUEST_TIMEOUT=2s",
		"PAYCORE_HTTP_WRITE_TIMEOUT=3s",
		"PAYCORE_HTTP_MAX_BODY_BYTES=2048",
		"PAYCORE_SHUTDOWN_TIMEOUT=1m",
		"PAYCORE_LOG_LEVEL=debug",
		"HOME=/ignored",
	})
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
	_, err := Load([]string{
		"PAYCORE_ENV=staging",
		"PAYCORE_HTTP_ADDR=8080",
		"PAYCORE_HTTP_READ_TIMEOUT=-1s",
		"PAYCORE_HTTP_MAX_BODY_BYTES=lots",
		"PAYCORE_LOG_LEVEL=loud",
		"PAYCORE_LOG_FORMAT=xml",
		"PAYCORE_HTTP_ADRR=:1",
	})
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

func TestLoadWriteTimeoutMustExceedRequestTimeout(t *testing.T) {
	_, err := Load([]string{"PAYCORE_HTTP_REQUEST_TIMEOUT=10s", "PAYCORE_HTTP_WRITE_TIMEOUT=10s"})
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "PAYCORE_HTTP_WRITE_TIMEOUT") {
		t.Fatalf("err = %v, want write timeout error", err)
	}
}
