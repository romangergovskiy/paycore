package app

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/romangergovskiy/paycore/internal/platform/config"
)

func TestRunInvalidConfig(t *testing.T) {
	err := Run(context.Background(), []string{"PAYCORE_ENV=staging"}, io.Discard)
	if !errors.Is(err, config.ErrInvalid) {
		t.Fatalf("Run = %v, want config.ErrInvalid", err)
	}
}

func TestRunShortJWTSecret(t *testing.T) {
	err := Run(context.Background(), []string{
		"PAYCORE_AUTH_JWT_SECRET=0123456789abcdef0123456789abcde",
	}, io.Discard)
	if !errors.Is(err, config.ErrInvalid) {
		t.Fatalf("Run = %v, want config.ErrInvalid", err)
	}
}

func TestRunListenError(t *testing.T) {
	err := Run(context.Background(), []string{
		"PAYCORE_HTTP_ADDR=127.0.0.1:-1",
		"PAYCORE_AUTH_JWT_SECRET=0123456789abcdef0123456789abcdef",
	}, io.Discard)
	if err == nil || errors.Is(err, config.ErrInvalid) {
		t.Fatalf("Run = %v, want listen error", err)
	}
}

func TestRunServesAndShutsDown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logR, logW := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, []string{
			"PAYCORE_ENV=test",
			"PAYCORE_HTTP_ADDR=127.0.0.1:0",
			"PAYCORE_SHUTDOWN_TIMEOUT=2s",
			"PAYCORE_AUTH_JWT_SECRET=0123456789abcdef0123456789abcdef",
		}, logW)
		logW.Close()
	}()

	lines := make(chan map[string]any, 64)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(logR)
		for sc.Scan() {
			var rec map[string]any
			if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
				rec = map[string]any{"msg": "non-JSON log line: " + sc.Text()}
			}
			lines <- rec
		}
	}()
	addr := waitForLog(t, lines, "http server started")["addr"].(string)

	resp, err := http.Get("http://" + addr + "/livez")
	if err != nil {
		t.Fatalf("GET /livez: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/livez = %d", resp.StatusCode)
	}
	waitForLog(t, lines, "http request")

	cancel()
	waitForLog(t, lines, "shutdown complete")

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func waitForLog(t *testing.T, lines <-chan map[string]any, msg string) map[string]any {
	t.Helper()
	timeout := time.After(3 * time.Second)
	for {
		select {
		case rec, ok := <-lines:
			if !ok {
				t.Fatalf("log %q not found", msg)
			}
			if rec["msg"] == msg {
				return rec
			}
		case <-timeout:
			t.Fatalf("timed out waiting for log %q", msg)
		}
	}
}
