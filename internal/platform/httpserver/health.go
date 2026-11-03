package httpserver

import (
	"net/http"

	"github.com/romangergovskiy/paycore/internal/platform/apperror"
)

type status struct {
	Status string `json:"status"`
}

// livez reports that the process is running. It never checks dependencies,
// so an unavailable database does not cause restarts.
func (s *Server) livez(w http.ResponseWriter, _ *http.Request) {
	WriteJSON(w, http.StatusOK, status{Status: "ok"})
}

// readyz reports whether the instance should receive traffic.
func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	if !s.ready.Load() {
		WriteError(w, r, apperror.New(apperror.KindUnavailable, apperror.CodeUnavailable, "Service is shutting down."))
		return
	}
	WriteJSON(w, http.StatusOK, status{Status: "ok"})
}
