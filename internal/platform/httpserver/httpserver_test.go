package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/romangergovskiy/paycore/internal/platform/apperror"
	"github.com/romangergovskiy/paycore/internal/platform/config"
)

func testConfig() config.HTTP {
	return config.HTTP{
		Addr:              "127.0.0.1:0",
		ReadHeaderTimeout: time.Second,
		ReadTimeout:       time.Second,
		WriteTimeout:      2 * time.Second,
		IdleTimeout:       time.Second,
		RequestTimeout:    time.Second,
		MaxBodyBytes:      16,
	}
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func newTestServer(t *testing.T, cfg config.HTTP) (*Server, *syncBuffer) {
	t.Helper()
	logs := &syncBuffer{}
	return New(cfg, slog.New(slog.NewJSONHandler(logs, nil))), logs
}

func serve(s *Server, method, target string, body io.Reader, header http.Header) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, body)
	for k, v := range header {
		req.Header[k] = v
	}
	rec := httptest.NewRecorder()
	s.srv.Handler.ServeHTTP(rec, req)
	return rec
}

func decodeError(t *testing.T, rec *httptest.ResponseRecorder) errorPayload {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q", ct)
	}
	var body errorBody
	dec := json.NewDecoder(rec.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	return body.Error
}

func TestHealth(t *testing.T) {
	s, _ := newTestServer(t, testConfig())

	for _, path := range []string{"/livez", "/readyz"} {
		rec := serve(s, http.MethodGet, path, nil, nil)
		if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"status":"ok"}` {
			t.Errorf("%s = %d %s", path, rec.Code, rec.Body)
		}
	}

	s.SetReady(false)
	rec := serve(s, http.MethodGet, "/readyz", nil, nil)
	if rec.Code != http.StatusServiceUnavailable || decodeError(t, rec).Code != apperror.CodeUnavailable {
		t.Errorf("/readyz while draining = %d %s", rec.Code, rec.Body)
	}
	if rec := serve(s, http.MethodGet, "/livez", nil, nil); rec.Code != http.StatusOK {
		t.Errorf("/livez while draining = %d", rec.Code)
	}

	s.SetReady(true)
	s.SetReadyCheck(func(context.Context) error { return context.DeadlineExceeded })
	rec = serve(s, http.MethodGet, "/readyz", nil, nil)
	if rec.Code != http.StatusServiceUnavailable || decodeError(t, rec).Code != apperror.CodeUnavailable {
		t.Errorf("/readyz with failed check = %d %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "deadline") {
		t.Error("ping error leaked to the client")
	}
}

func TestNotFoundAndMethodNotAllowedAreJSON(t *testing.T) {
	s, _ := newTestServer(t, testConfig())

	rec := serve(s, http.MethodGet, "/nope", nil, nil)
	e := decodeError(t, rec)
	if rec.Code != http.StatusNotFound || e.Code != apperror.CodeNotFound {
		t.Errorf("404 = %d %+v", rec.Code, e)
	}
	if e.RequestID == "" || e.RequestID != rec.Header().Get(requestIDHeader) {
		t.Errorf("request_id %q does not match header %q", e.RequestID, rec.Header().Get(requestIDHeader))
	}

	rec = serve(s, http.MethodPost, "/livez", nil, nil)
	if rec.Code != http.StatusMethodNotAllowed || decodeError(t, rec).Code != apperror.CodeMethodNotAllowed {
		t.Errorf("405 = %d %s", rec.Code, rec.Body)
	}
	if allow := rec.Header().Get("Allow"); !strings.Contains(allow, http.MethodGet) {
		t.Errorf("Allow = %q", allow)
	}
}

func TestRequestID(t *testing.T) {
	s, _ := newTestServer(t, testConfig())

	rec := serve(s, http.MethodGet, "/livez", nil, http.Header{requestIDHeader: {"client-id-1"}})
	if got := rec.Header().Get(requestIDHeader); got != "client-id-1" {
		t.Errorf("valid incoming ID replaced: %q", got)
	}

	rec = serve(s, http.MethodGet, "/livez", nil, http.Header{requestIDHeader: {"bad id\n"}})
	if got := rec.Header().Get(requestIDHeader); got == "" || got == "bad id\n" {
		t.Errorf("invalid incoming ID accepted: %q", got)
	}
}

func TestTooManyRequestsJSON(t *testing.T) {
	s, _ := newTestServer(t, testConfig())
	s.Handle("POST /limited", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		WriteError(w, r, apperror.New(apperror.KindTooManyRequests, "AUTH_RATE_LIMITED", "Too many attempts. Retry later."))
	}))

	rec := serve(s, http.MethodPost, "/limited", nil, nil)
	e := decodeError(t, rec)
	if rec.Code != http.StatusTooManyRequests || e.Code != "AUTH_RATE_LIMITED" {
		t.Errorf("429 = %d %+v", rec.Code, e)
	}
	if e.Message != "Too many attempts. Retry later." || e.RequestID == "" {
		t.Errorf("D-39 body = %+v", e)
	}
	if rec.Header().Get("WWW-Authenticate") != "" {
		t.Errorf("429 must not send WWW-Authenticate: %q", rec.Header().Get("WWW-Authenticate"))
	}
}

func TestAppErrorMapping(t *testing.T) {
	s, _ := newTestServer(t, testConfig())
	s.Handle("GET /refund", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		WriteError(w, r, apperror.New(apperror.KindUnprocessable, "MERCHANT_INSUFFICIENT_FUNDS", "Not enough funds."))
	}))

	rec := serve(s, http.MethodGet, "/refund", nil, nil)
	e := decodeError(t, rec)
	if rec.Code != http.StatusUnprocessableEntity || e.Code != "MERCHANT_INSUFFICIENT_FUNDS" || e.Message != "Not enough funds." {
		t.Errorf("got %d %+v", rec.Code, e)
	}
}

func TestPanicIsInternalErrorWithoutDetails(t *testing.T) {
	s, logs := newTestServer(t, testConfig())
	s.Handle("GET /boom", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("secret detail")
	}))

	rec := serve(s, http.MethodGet, "/boom", nil, nil)
	e := decodeError(t, rec)
	if rec.Code != http.StatusInternalServerError || e.Code != apperror.CodeInternal {
		t.Fatalf("got %d %+v", rec.Code, e)
	}
	if strings.Contains(rec.Body.String(), "secret detail") {
		t.Error("panic value leaked to the client")
	}
	if !strings.Contains(logs.String(), "secret detail") || !strings.Contains(logs.String(), `"route":"GET /boom"`) {
		t.Errorf("panic or route not logged:\n%s", logs)
	}
}

func TestBodyLimit(t *testing.T) {
	s, _ := newTestServer(t, testConfig())
	s.Handle("POST /echo", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadAll(r.Body); err != nil {
			WriteError(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, status{Status: "ok"})
	}))

	rec := serve(s, http.MethodPost, "/echo", strings.NewReader(strings.Repeat("x", 17)), nil)
	if rec.Code != http.StatusRequestEntityTooLarge || decodeError(t, rec).Code != apperror.CodeRequestTooLarge {
		t.Errorf("got %d %s", rec.Code, rec.Body)
	}
}

func TestRequestDeadline(t *testing.T) {
	cfg := testConfig()
	cfg.RequestTimeout = 20 * time.Millisecond
	s, _ := newTestServer(t, cfg)
	s.Handle("GET /slow", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
		WriteError(w, r, r.Context().Err())
	}))

	rec := serve(s, http.MethodGet, "/slow", nil, nil)
	if rec.Code != http.StatusServiceUnavailable || decodeError(t, rec).Code != apperror.CodeRequestTimeout {
		t.Errorf("got %d %s", rec.Code, rec.Body)
	}
}

func TestShutdownWaitsForInFlightRequest(t *testing.T) {
	s, _ := newTestServer(t, testConfig())
	started := make(chan struct{})
	s.Handle("GET /work", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		time.Sleep(100 * time.Millisecond)
		if r.Context().Err() != nil {
			t.Error("request context cancelled by shutdown")
		}
		WriteJSON(w, http.StatusOK, status{Status: "done"})
	}))

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- s.Serve(ln) }()

	type result struct {
		code int
		err  error
	}
	got := make(chan result, 1)
	go func() {
		resp, err := http.Get("http://" + ln.Addr().String() + "/work")
		if err != nil {
			got <- result{err: err}
			return
		}
		resp.Body.Close()
		got <- result{code: resp.StatusCode}
	}()

	<-started
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if r := <-got; r.err != nil || r.code != http.StatusOK {
		t.Errorf("in-flight request: %+v", r)
	}
	if err := <-served; err != nil {
		t.Errorf("Serve after Shutdown = %v, want nil", err)
	}
}
