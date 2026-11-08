package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/romangergovskiy/paycore/internal/platform/apperror"
	"github.com/romangergovskiy/paycore/internal/platform/logging"
)

type errorBody struct {
	Error errorPayload `json:"error"`
}

type errorPayload struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id,omitempty"`
}

// WriteJSON writes v as a JSON response with the given status.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// WriteError writes err in the format described in package apperror.
// Internal errors are logged with their cause; the client gets a generic
// message and the request ID to quote.
func WriteError(w http.ResponseWriter, r *http.Request, err error) {
	e := classify(err)
	status := statusFor(e.Kind)
	if status >= http.StatusInternalServerError {
		logging.FromContext(r.Context()).LogAttrs(r.Context(), slog.LevelError, "request failed",
			slog.String("code", e.Code), slog.Any("error", err))
	}
	WriteJSON(w, status, errorBody{Error: errorPayload{
		Code:      e.Code,
		Message:   e.PublicMessage(),
		RequestID: RequestID(r.Context()),
	}})
}

func classify(err error) *apperror.Error {
	var (
		appErr   *apperror.Error
		tooLarge *http.MaxBytesError
	)
	switch {
	case errors.As(err, &appErr):
		return appErr
	case errors.As(err, &tooLarge):
		return apperror.Wrap(err, apperror.KindTooLarge, apperror.CodeRequestTooLarge, "Request body is too large.")
	case errors.Is(err, context.DeadlineExceeded):
		return apperror.Wrap(err, apperror.KindUnavailable, apperror.CodeRequestTimeout, "Request timed out. Retry later.")
	default:
		return apperror.From(err)
	}
}

func statusFor(k apperror.Kind) int {
	switch k {
	case apperror.KindInvalid:
		return http.StatusBadRequest
	case apperror.KindUnauthorized:
		return http.StatusUnauthorized
	case apperror.KindForbidden:
		return http.StatusForbidden
	case apperror.KindNotFound:
		return http.StatusNotFound
	case apperror.KindMethodNotAllowed:
		return http.StatusMethodNotAllowed
	case apperror.KindConflict:
		return http.StatusConflict
	case apperror.KindTooLarge:
		return http.StatusRequestEntityTooLarge
	case apperror.KindUnprocessable:
		return http.StatusUnprocessableEntity
	case apperror.KindUnavailable:
		return http.StatusServiceUnavailable
	case apperror.KindTooManyRequests:
		return http.StatusTooManyRequests
	default:
		return http.StatusInternalServerError
	}
}
