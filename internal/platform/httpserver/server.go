// Package httpserver provides the HTTP server shared by all modules:
// timeouts, middleware, JSON error responses and health endpoints.
package httpserver

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"

	"github.com/romangergovskiy/paycore/internal/platform/apperror"
	"github.com/romangergovskiy/paycore/internal/platform/config"
)

type Server struct {
	srv        *http.Server
	mux        *http.ServeMux
	ready      atomic.Bool
	readyCheck func(context.Context) error
}

func New(cfg config.HTTP, log *slog.Logger) *Server {
	s := &Server{mux: http.NewServeMux()}
	s.ready.Store(true)
	s.mux.HandleFunc("GET /livez", s.livez)
	s.mux.HandleFunc("GET /readyz", s.readyz)

	var h http.Handler = http.HandlerFunc(s.route)
	h = withMaxBody(cfg.MaxBodyBytes, h)
	h = withDeadline(cfg.RequestTimeout, h)
	h = withRecover(h)
	h = withAccessLog(h)
	h = withRequestID(log, h)

	s.srv = &http.Server{
		Addr:              cfg.Addr,
		Handler:           h,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
		// Request contexts are not derived from the shutdown signal: in-flight
		// requests finish on their own deadline instead of being cancelled
		// halfway through a database transaction.
		BaseContext: func(net.Listener) context.Context { return context.Background() },
	}
	return s
}

// Handle registers a module route, e.g. "POST /api/v1/transfers".
func (s *Server) Handle(pattern string, h http.Handler) { s.mux.Handle(pattern, h) }

// Serve accepts connections on ln until Shutdown or Close is called.
func (s *Server) Serve(ln net.Listener) error {
	if err := s.srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// SetReady controls /readyz; it is switched off at the start of shutdown so
// that a load balancer stops routing new traffic.
func (s *Server) SetReady(ready bool) { s.ready.Store(ready) }

// SetReadyCheck, if set, is called from /readyz after the process-level ready
// flag. Use it for a database ping. It must be set before Serve.
func (s *Server) SetReadyCheck(fn func(context.Context) error) { s.readyCheck = fn }

// Shutdown stops accepting connections and waits for active requests until
// ctx is done.
func (s *Server) Shutdown(ctx context.Context) error { return s.srv.Shutdown(ctx) }

// Close drops all connections immediately.
func (s *Server) Close() error { return s.srv.Close() }

// route dispatches to the mux and replaces its plain-text 404/405 responses
// with JSON errors.
func (s *Server) route(w http.ResponseWriter, r *http.Request) {
	h, pattern := s.mux.Handler(r)
	if pattern == "" {
		probe := &headerProbe{header: http.Header{}}
		h.ServeHTTP(probe, r)
		err := apperror.New(apperror.KindNotFound, apperror.CodeNotFound, "Resource not found.")
		if probe.status == http.StatusMethodNotAllowed {
			w.Header().Set("Allow", probe.header.Get("Allow"))
			err = apperror.New(apperror.KindMethodNotAllowed, apperror.CodeMethodNotAllowed, "Method not allowed.")
		}
		WriteError(w, r, err)
		return
	}
	if info, ok := r.Context().Value(requestInfoKey{}).(*requestInfo); ok {
		info.route = pattern
	}
	s.mux.ServeHTTP(w, r)
}

// headerProbe captures the status and headers of the mux's fallback handler
// and discards its body.
type headerProbe struct {
	header http.Header
	status int
}

func (p *headerProbe) Header() http.Header         { return p.header }
func (p *headerProbe) Write(b []byte) (int, error) { return len(b), nil }
func (p *headerProbe) WriteHeader(status int)      { p.status = status }
