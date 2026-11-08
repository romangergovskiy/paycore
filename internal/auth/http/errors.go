package authhttp

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/romangergovskiy/paycore/internal/auth/domain"
	"github.com/romangergovskiy/paycore/internal/platform/apperror"
	"github.com/romangergovskiy/paycore/internal/platform/httpserver"
)

func writeError(w http.ResponseWriter, r *http.Request, err error) {
	mapped := mapError(err)
	var e *apperror.Error
	if errors.As(mapped, &e) && e.Kind == apperror.KindUnauthorized {
		w.Header().Set("WWW-Authenticate", "Bearer")
	}
	httpserver.WriteError(w, r, mapped)
}

func mapError(err error) error {
	if err == nil {
		return nil
	}
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return err
	}
	var appErr *apperror.Error
	if errors.As(err, &appErr) {
		return appErr
	}
	var domErr *domain.Error
	if errors.As(err, &domErr) {
		return apperror.New(kindFor(domErr.Code), domErr.Code, domErr.Message)
	}
	if isJSONDecodeError(err) {
		return invalidJSON()
	}
	return apperror.From(err)
}

func isJSONDecodeError(err error) bool {
	var syn *json.SyntaxError
	var typ *json.UnmarshalTypeError
	if errors.As(err, &syn) || errors.As(err, &typ) {
		return true
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	return strings.HasPrefix(err.Error(), "json: unknown field ")
}

func invalidJSON() *apperror.Error {
	return apperror.New(apperror.KindInvalid, domain.CodeInvalidRequest, domain.ErrInvalidRequest.Message)
}

func kindFor(code string) apperror.Kind {
	switch code {
	case domain.CodeInvalidRequest:
		return apperror.KindInvalid
	case domain.CodeInvalidCredentials, domain.CodeInvalidToken, domain.CodeTokenExpired, domain.CodeTokenReused:
		return apperror.KindUnauthorized
	case domain.CodeForbidden:
		return apperror.KindForbidden
	case domain.CodeNotFound:
		return apperror.KindNotFound
	case domain.CodeEmailTaken:
		return apperror.KindConflict
	case domain.CodeRateLimited:
		return apperror.KindTooManyRequests
	default:
		return apperror.KindInternal
	}
}
