package httpserver

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/romangergovskiy/paycore/internal/auth/domain"
	"github.com/romangergovskiy/paycore/internal/platform/apperror"
	"github.com/romangergovskiy/paycore/internal/platform/authcrypto"
)

type principalKey struct{}

// AccessParser verifies an access JWT. Roles in the token are not authorization.
type AccessParser interface {
	Parse(token string) (authcrypto.AccessToken, error)
}

// SessionValidator is application.Service.ValidateSession (store roles).
type SessionValidator interface {
	ValidateSession(ctx context.Context, sessionID uuid.UUID) (domain.Principal, error)
}

// PrincipalFromContext returns the actor placed by BearerAuth. ok is false
// when middleware did not run or authentication failed; it never panics.
func PrincipalFromContext(ctx context.Context) (domain.Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(domain.Principal)
	if !ok || p.UserID == uuid.Nil || p.SessionID == uuid.Nil {
		return domain.Principal{}, false
	}
	return p, true
}

// BearerAuth parses Authorization: Bearer, verifies the JWT, then
// ValidateSession. JWT.roles are ignored for authorization.
func BearerAuth(parser AccessParser, sessions SessionValidator) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, ok := bearerToken(r.Header.Get("Authorization"))
			if !ok {
				writeBearerError(w, r, domain.ErrInvalidToken)
				return
			}
			tok, err := parser.Parse(raw)
			if err != nil {
				writeBearerError(w, r, err)
				return
			}
			p, err := sessions.ValidateSession(r.Context(), tok.SessionID)
			if err != nil {
				writeBearerError(w, r, err)
				return
			}
			if p.UserID != tok.UserID || p.SessionID != tok.SessionID {
				writeBearerError(w, r, domain.ErrInvalidToken)
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, p)))
		})
	}
}

// RequireRole allows the request only when the context Principal has role.
// Missing Principal is 401 INVALID_TOKEN; missing role is 403 FORBIDDEN.
func RequireRole(role domain.Role) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, ok := PrincipalFromContext(r.Context())
			if !ok {
				writeBearerError(w, r, domain.ErrInvalidToken)
				return
			}
			if !p.Has(role) {
				writeBearerError(w, r, domain.ErrForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func bearerToken(header string) (string, bool) {
	scheme, token, ok := strings.Cut(header, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	if token == "" || strings.TrimSpace(token) != token || strings.ContainsAny(token, " \t") {
		return "", false
	}
	return token, true
}

func writeBearerError(w http.ResponseWriter, r *http.Request, err error) {
	mapped := mapBearerError(err)
	var e *apperror.Error
	if errors.As(mapped, &e) && e.Kind == apperror.KindUnauthorized {
		w.Header().Set("WWW-Authenticate", "Bearer")
	}
	WriteError(w, r, mapped)
}

func mapBearerError(err error) error {
	if err == nil {
		return invalidToken()
	}
	if errors.Is(err, domain.ErrTokenExpired) {
		return invalidToken()
	}
	var appErr *apperror.Error
	if errors.As(err, &appErr) {
		if appErr.Code == domain.CodeTokenExpired {
			return invalidToken()
		}
		return appErr
	}
	var domErr *domain.Error
	if errors.As(err, &domErr) {
		if domErr.Code == domain.CodeTokenExpired {
			return invalidToken()
		}
		return apperror.New(kindForAuth(domErr.Code), domErr.Code, domErr.Message)
	}
	return apperror.From(err)
}

func invalidToken() *apperror.Error {
	return apperror.New(apperror.KindUnauthorized, domain.CodeInvalidToken, domain.ErrInvalidToken.Message)
}

func kindForAuth(code string) apperror.Kind {
	switch code {
	case domain.CodeInvalidToken, domain.CodeInvalidCredentials, domain.CodeTokenExpired, domain.CodeTokenReused:
		return apperror.KindUnauthorized
	case domain.CodeForbidden:
		return apperror.KindForbidden
	default:
		return apperror.KindInternal
	}
}
