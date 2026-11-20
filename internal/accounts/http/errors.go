package accountshttp

import (
	"errors"
	"net/http"

	"github.com/romangergovskiy/paycore/internal/accounts/domain"
	authdomain "github.com/romangergovskiy/paycore/internal/auth/domain"
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
	var accErr *domain.Error
	if errors.As(err, &accErr) {
		return apperror.New(kindForAccount(accErr.Code), accErr.Code, accErr.Message)
	}
	var authErr *authdomain.Error
	if errors.As(err, &authErr) {
		return apperror.New(kindForAuth(authErr.Code), authErr.Code, authErr.Message)
	}
	return apperror.From(err)
}

func kindForAccount(code string) apperror.Kind {
	switch code {
	case domain.CodeInvalidAccount:
		return apperror.KindInvalid
	case domain.CodeAccountNotFound:
		return apperror.KindNotFound
	case domain.CodeCurrencyNotSupported, domain.CodeInvalidState, domain.CodeAccountNotEmpty:
		return apperror.KindUnprocessable
	default:
		return apperror.KindInternal
	}
}

func kindForAuth(code string) apperror.Kind {
	switch code {
	case authdomain.CodeInvalidRequest:
		return apperror.KindInvalid
	case authdomain.CodeInvalidToken, authdomain.CodeTokenExpired:
		return apperror.KindUnauthorized
	case authdomain.CodeForbidden:
		return apperror.KindForbidden
	default:
		return apperror.KindInternal
	}
}
